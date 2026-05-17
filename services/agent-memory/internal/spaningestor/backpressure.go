package spaningestor

// Backpressure supervisor: a single goroutine the Ingestor.Run
// spawns when `health` is non-nil. It samples queue depth at
// `cfg.HealthSupervisorInterval` and applies the
// (BackpressureThreshold, BackpressureSustain) hysteresis to
// decide when to flip `repo_health.degraded`.
//
// Per-repo state
// --------------
// The Ingestor is provisioned per-repo at the binary level
// (cmd/span-ingestor's composition root creates one Ingestor
// per service.name → repo_id mapping). But a SINGLE Ingestor
// in v1 serves all repos in its queue — the queue is not
// per-repo. So the supervisor reasons about TOTAL queue depth,
// not per-repo depth. The repo_id it writes to repo_health is
// the SET of repos that contributed spans to the queue during
// the sustain window.
//
// Why per-repo (instead of a single global degraded flag): the
// agent.recall handler keys off the request's repo_id (the
// tech-spec §C22 protocol). A single global flag would force
// every recall to surface degraded even when only one repo is
// emitting heavy spans. So we maintain a per-repo bookkeeping
// (set into degradedState) so the flag we write is per-repo
// even though the queue is shared.

import (
	"context"
	"log/slog"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// backpressureState is the per-repo book-keeping the supervisor
// updates on each tick.
type backpressureState struct {
	// degraded is the current FLAG state we believe the
	// database row holds. Starts false (not degraded).
	degraded bool
	// overSince is the wall-clock instant the queue first
	// rose AT OR ABOVE BackpressureThreshold since the last
	// state transition. Zero when below threshold.
	overSince time.Time
	// underSince is the wall-clock instant the queue first
	// dropped BELOW BackpressureThreshold since the last
	// state transition. Zero when at or above threshold.
	underSince time.Time
}

// runSupervisor is the goroutine that maintains repo_health
// rows according to the sustained-queue-depth heuristic AND
// flushes TTL-expired pending children from the
// out-of-order-reconciliation cache (evaluator iter-2 #2). It
// returns when `ctx.Done()` fires.
//
// We sample at HealthSupervisorInterval rather than reacting
// to channel-depth changes because the channel surface only
// exposes `len(ch)` snapshots, not change events. A 1s
// interval is fine for the 30s sustain window AND for the
// 10min pending-child TTL — both transitions are slow
// relative to the tick rate.
func (i *Ingestor) runSupervisor(ctx context.Context) {
	ticker := time.NewTicker(i.cfg.HealthSupervisorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := i.now()
			if i.health != nil {
				i.supervisorTick(ctx, now)
			}
			// Always flush expired pending children when the
			// cache is enabled, regardless of health wiring;
			// the cost is one mutex acquire per tick when
			// nothing has expired.
			i.flushExpiredPendingChildren(ctx)
		}
	}
}

// supervisorTick is the one-step state machine. Factored out
// of runSupervisor for direct testability — unit tests drive
// it with synthesized timestamps and assert the resulting
// UpsertRepoHealth call shape.
func (i *Ingestor) supervisorTick(ctx context.Context, now time.Time) {
	depth := i.QueueDepth()
	// We attribute the degraded flag to every repo that has
	// at least one span in flight (i.e., that the Ingestor
	// has ingested since starting). A repo that hasn't sent
	// any traffic this lifetime cannot be backpressured — we
	// would only create noise by marking it.
	repos := i.attributableRepos()
	if len(repos) == 0 {
		return
	}

	for _, repoID := range repos {
		state, ok := i.degradedState[repoID]
		if !ok {
			state = &backpressureState{}
			i.degradedState[repoID] = state
		}
		i.tickRepo(ctx, repoID, state, depth, now)
	}
}

// tickRepo runs the hysteresis for a single repo. Pulled out
// of supervisorTick for test clarity.
func (i *Ingestor) tickRepo(
	ctx context.Context,
	repoID string,
	state *backpressureState,
	depth int,
	now time.Time,
) {
	if depth >= i.cfg.BackpressureThreshold {
		// Threshold tripped. Reset the under-counter (any
		// recovery progress is invalidated by this tick).
		state.underSince = time.Time{}
		if state.overSince.IsZero() {
			state.overSince = now
		}
		if !state.degraded && now.Sub(state.overSince) >= i.cfg.BackpressureSustain {
			i.writeHealth(ctx, repoID, true, now)
			state.degraded = true
			state.overSince = time.Time{}
		}
		return
	}
	// Below threshold. Reset the over-counter.
	state.overSince = time.Time{}
	if !state.degraded {
		return
	}
	if state.underSince.IsZero() {
		state.underSince = now
	}
	if now.Sub(state.underSince) >= i.cfg.BackpressureClearance {
		i.writeHealth(ctx, repoID, false, now)
		state.degraded = false
		state.underSince = time.Time{}
	}
}

func (i *Ingestor) writeHealth(ctx context.Context, repoID string, degraded bool, now time.Time) {
	in := graphwriter.HealthInput{
		RepoID:     repoID,
		Degraded:   degraded,
		Source:     ingestorSource,
		ObservedAt: now,
	}
	if degraded {
		in.Reason = DegradedReasonBackpressure
	}
	if _, err := i.health.UpsertRepoHealth(ctx, in); err != nil {
		i.logger.Error("spaningestor.health_upsert_failed",
			slog.String("repo_id", repoID),
			slog.Bool("degraded", degraded),
			slog.String("err", err.Error()))
		return
	}
	i.logger.Info("spaningestor.health_upsert",
		slog.String("repo_id", repoID),
		slog.Bool("degraded", degraded),
		slog.Int("queue_depth", i.QueueDepth()),
		slog.Int("threshold", i.cfg.BackpressureThreshold),
	)
}

// attributableRepos enumerates repos with non-zero CURRENT
// in-flight spans. Evaluator iter-1 #6: the previous
// implementation returned every repo with non-zero
// `span_ingested_total` since startup (a lifetime counter),
// which falsely degraded repos that had emitted a single span
// hours ago when an unrelated noisy repo filled the queue.
// Using the in-flight counter scopes the degraded flag to
// repos contributing to the current sustained backlog.
//
// Repos that historically ingested but have zero in-flight at
// the tick boundary are SKIPPED — their previous degraded
// state lives in `degradedState` so a clear-cycle still fires
// if they were marked degraded earlier. The supervisor calls
// this for fresh attribution; clearance is handled separately
// in `tickRepoForce` (run for repos already in degradedState
// even if their in-flight has gone to zero).
func (i *Ingestor) attributableRepos() []string {
	m := i.metrics
	if m == nil {
		return nil
	}
	inflight := m.InflightSnapshot()
	out := make([]string, 0, len(inflight)+len(i.degradedState))
	seen := make(map[string]struct{}, len(inflight)+len(i.degradedState))
	for repoID, cnt := range inflight {
		if cnt <= 0 {
			continue
		}
		if _, dup := seen[repoID]; dup {
			continue
		}
		seen[repoID] = struct{}{}
		out = append(out, repoID)
	}
	// Repos already marked degraded MUST keep ticking so the
	// clearance window can fire even after their traffic has
	// gone quiet — otherwise a degraded repo would stay
	// degraded forever once it stopped emitting.
	for repoID := range i.degradedState {
		if _, dup := seen[repoID]; dup {
			continue
		}
		seen[repoID] = struct{}{}
		out = append(out, repoID)
	}
	return out
}
