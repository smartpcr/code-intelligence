// Package loadtest is the Stage 8.4 calibration-harness
// orchestrator.
//
// The harness composes a [calibration.Config] (which carries
// the ┬º8.3-derived [reliability.LoadProfile]) and a set of
// per-verb [scenarios.Scenario] drivers, runs them concurrently
// against an [scenarios.AgentClient] / [scenarios.ManagementClient]
// for the configured duration, and emits a single
// [calibration.Report] the operator persists into the story
// docs as `load-test-iter1.md`.
//
// Scheduling model: open-loop. Each scenario has its own
// dedicated ticker firing at 1/sustainedRPS; each tick attempts
// to acquire a slot from a per-verb semaphore (sized by
// `Config.MaxInflightPerVerb`) and launches a worker goroutine.
// When the semaphore is full the tick is recorded as a "dropped
// tick" instead of stalling ΓÇö this preserves the open-loop
// invariant: planned arrival rate is honoured regardless of
// downstream latency, and the artifact surfaces drift between
// requested and achieved RPS rather than the closed-loop
// failure mode where one slow request silently throttles
// every subsequent tick.
package loadtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/calibration"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/loadtest/scenarios"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// Harness is the calibration-run orchestrator. Construct via
// [NewHarness] and call [Harness.Run].
type Harness struct {
	cfg       calibration.Config
	scenarios []scenarios.Scenario

	// now is the wall-clock source. Production wires
	// time.Now; tests inject a deterministic clock.
	now func() time.Time

	// newRNG mints the rng each scenario worker pulls fresh
	// integers from. Seeded from cfg.RandomSeed for
	// reproducibility.
	newRNG func() scenarios.RNG

	// effectiveSeed is the seed the harness actually used
	// (cfg.RandomSeed when non-zero, else the wall-clock
	// seed drawn at construction time). Echoed onto the
	// report so an operator can replay the run.
	effectiveSeed int64

	// sampleObserver is an optional per-sample callback fired
	// after every Scenario.Execute completes. Used by the
	// cmd binary to feed an obs.Histogram so the harness's
	// /metrics surface produces an approximate per-verb
	// latency signal bucketed at the ┬º8.3 SLO thresholds.
	// The /metrics histogram does NOT reproduce the
	// artifact's exact nearest-rank percentiles ΓÇö see the
	// docstring on `cmd/loadtest-harness/main.go::loadtestHarnessRunDurationSeconds`
	// and on `WithSampleObserver` below for the
	// bucket-quantization caveat. Nil = no-op (the in-process
	// tests don't need metrics).
	sampleObserver func(sample scenarios.Sample)

	// artifactNotes are operator-supplied strings the
	// binary injects unconditionally (e.g. a pointer at the
	// operator-workflow doc, the labelled-queries fixture
	// path). Appended to report.Notes after the
	// dropped-tick / degraded notes so a stable footer is
	// visible at the bottom of the rendered artifact.
	artifactNotes []string
}

// Option mutates Harness fields at construction time.
type Option func(*Harness)

// WithClock overrides the wall-clock source. Tests pass a
// `func() time.Time` that advances under their control.
func WithClock(now func() time.Time) Option {
	return func(h *Harness) { h.now = now }
}

// WithRNG overrides the random-source factory. Used by tests
// that want every scenario to see the same deterministic
// sequence.
func WithRNG(factory func() scenarios.RNG) Option {
	return func(h *Harness) { h.newRNG = factory }
}

// WithSampleObserver installs a per-sample callback. The
// callback is invoked AFTER each Scenario.Execute returns,
// before the sample is added to the per-verb aggregator. The
// callback runs on the scenario worker goroutine; the harness
// does NOT serialise it across verbs, so the callback MUST be
// goroutine-safe (typical implementation: a single
// obs.Histogram whose Observe is mutex-guarded).
//
// Used by the cmd binary to drive the
// `loadtest_harness_request_duration_seconds` histogram so the
// /metrics surface offers an approximate per-verb percentile
// signal whose bucket boundaries align with the ┬º8.3 SLO
// thresholds. The /metrics histogram does NOT reproduce the
// on-disk artifact's exact percentiles ΓÇö see the docstring on
// `cmd/loadtest-harness/main.go::loadtestHarnessRunDurationSeconds`
// for the bucket-quantization caveat.
func WithSampleObserver(observe func(sample scenarios.Sample)) Option {
	return func(h *Harness) { h.sampleObserver = observe }
}

// WithArtifactNote appends a stable operator-supplied note to
// the rendered artifact. Multiple calls accumulate; notes are
// emitted after the dynamic dropped-tick / degraded notes so
// the operator workflow / fixture references render in a
// predictable footer position. Useful for the cmd binary to
// embed cross-references that survive every run (e.g. a
// pointer at the operator-workflow doc).
func WithArtifactNote(note string) Option {
	return func(h *Harness) {
		if note == "" {
			return
		}
		h.artifactNotes = append(h.artifactNotes, note)
	}
}

// NewHarness wires a Harness from a validated config and a
// list of Scenarios. Returns an error when the config is
// invalid or no scenarios were supplied.
func NewHarness(cfg calibration.Config, scen []scenarios.Scenario, opts ...Option) (*Harness, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("loadtest: %w", err)
	}
	if len(scen) == 0 {
		return nil, errors.New("loadtest: NewHarness requires at least one scenario")
	}
	for i, s := range scen {
		if s == nil {
			return nil, fmt.Errorf("loadtest: scenarios[%d] is nil", i)
		}
		if _, ok := cfg.Profile.Verb(s.Verb()); !ok {
			return nil, fmt.Errorf("loadtest: scenario %d verb %q is not in profile %q", i, s.Verb(), cfg.Profile.Name)
		}
	}
	h := &Harness{
		cfg:       cfg,
		scenarios: scen,
		now:       time.Now,
	}
	seed := cfg.RandomSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	h.effectiveSeed = seed
	// One PCG source per harness; each scenario worker pulls
	// a per-worker `*rand.Rand` so the runtime does not
	// serialise on a shared source. rand.NewSource is
	// goroutine-unsafe but each *rand.Rand we hand out is
	// local to a scenario worker ΓÇö no shared state.
	src := rand.NewSource(seed)
	mu := &sync.Mutex{}
	h.newRNG = func() scenarios.RNG {
		mu.Lock()
		workerSeed := src.Int63()
		mu.Unlock()
		return rand.New(rand.NewSource(workerSeed))
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// aggregator is the per-verb sample sink. Goroutine-safe:
// addSample / addDrop / snapshot all guard with mu (or atomic
// in the case of drops, which can be incremented from many
// goroutines without taking the slice lock).
type aggregator struct {
	verb string

	mu        sync.Mutex
	latencies []time.Duration
	succeeded int
	failed    int
	degraded  int
	// degradedReasons counts the per-reason occurrences for
	// degraded responses that carried a non-empty
	// `Sample.DegradedReason`. The harness's note emitter
	// renders these onto the artifact's degraded-responses
	// note line in descending order so the dominant
	// backpressure mode is the first thing an operator
	// sees. Bounded at 64 distinct reasons per verb to keep
	// memory tight under an unbounded-cardinality wire bug;
	// further reasons are aggregated into a single
	// "<other>" bucket.
	degradedReasons map[string]int

	// ranks / hits feed the learning-quality computation;
	// only the recall scenario populates them.
	ranks []int
	hits  []bool

	dropped atomic.Int64
}

const maxDegradedReasons = 64

func (a *aggregator) addSample(s scenarios.Sample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.latencies = append(a.latencies, s.Latency())
	if s.Err != nil {
		a.failed++
	} else {
		a.succeeded++
	}
	if s.Degraded {
		a.degraded++
		if s.DegradedReason != "" {
			if a.degradedReasons == nil {
				a.degradedReasons = make(map[string]int, 4)
			}
			if _, present := a.degradedReasons[s.DegradedReason]; present || len(a.degradedReasons) < maxDegradedReasons {
				a.degradedReasons[s.DegradedReason]++
			} else {
				a.degradedReasons["<other>"]++
			}
		}
	}
	// Always include measured ranks ΓÇö including misses
	// (RecallRank=0) ΓÇö so the median doesn't silently
	// improve when a labelled query fails to find the
	// expected node. AggregateLearningQuality buckets 0 into
	// the worst-rank slot (K+1).
	if s.RankMeasured {
		a.ranks = append(a.ranks, s.RecallRank)
	}
	if s.ConceptHitMeasured {
		a.hits = append(a.hits, s.ConceptHit)
	}
}

func (a *aggregator) addDrop() { a.dropped.Add(1) }

// Run drives every scenario for the configured duration and
// returns the calibration report. The returned
// Report.RandomSeed echoes the (possibly auto-generated) seed
// so an operator can replay the run.
//
// Context discipline: there are TWO contexts in play.
//   - `ctx` (the caller's) is the request context ΓÇö it cancels
//     when the operator interrupts (SIGINT/SIGTERM) or when the
//     caller explicitly cancels. In-flight requests use this
//     context, so a cancel propagates immediately. The harness
//     does NOT cancel ctx itself.
//   - `schedulerCtx` (an internal derivative) is the SCHEDULER
//     context ΓÇö it cancels when the planned duration elapses,
//     stopping new ticks. In-flight requests are NOT cancelled
//     by the scheduler deadline; they complete (or fail) on
//     their own (subject to per-scenario RequestTimeout). This
//     is why the harness measures the FULL latency distribution
//     including the requests that overflow the planned window ΓÇö
//     truncating those would understate the tail.
func (h *Harness) Run(ctx context.Context) (calibration.Report, error) {
	duration := h.cfg.EffectiveDuration()
	startedAt := h.now()
	schedulerCtx, cancelScheduler := context.WithDeadline(ctx, startedAt.Add(duration))
	defer cancelScheduler()

	report := calibration.Report{
		ProfileName:      h.cfg.Profile.Name,
		StartedAt:        startedAt,
		PlannedDuration:  duration,
		RepoID:           h.cfg.RepoID,
		SeededFixtureLOC: h.cfg.SeededFixtureLOC,
		RandomSeed:       h.effectiveSeed,
		ErrorBudgetRatio: h.cfg.Profile.ErrorBudgetRatio,
		Provenance:       h.cfg.Provenance,
	}

	// One aggregator per scenario.
	aggs := make(map[string]*aggregator, len(h.scenarios))
	for _, s := range h.scenarios {
		aggs[s.Verb()] = &aggregator{verb: s.Verb()}
	}

	var wg sync.WaitGroup
	for _, scen := range h.scenarios {
		scen := scen
		verbProfile, _ := h.cfg.Profile.Verb(scen.Verb())
		agg := aggs[scen.Verb()]
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.driveScenario(ctx, schedulerCtx, scen, verbProfile, agg)
		}()
	}
	wg.Wait()

	report.FinishedAt = h.now()
	report.ActualDuration = report.FinishedAt.Sub(report.StartedAt)

	// Detect aborted runs so the binary can return a distinct
	// exit code and the artifact carries an audit trail. The
	// scheduler's WithDeadline returns DeadlineExceeded when
	// the planned duration elapsed naturally (NOT an abort);
	// any other ctx error (Canceled, parent deadline propagated
	// in before our deadline) IS an abort.
	switch err := ctx.Err(); {
	case err == nil:
		report.CompletionReason = "completed"
	case errors.Is(err, context.Canceled):
		report.Aborted = true
		report.CompletionReason = "aborted-context-cancelled"
		report.Notes = append(report.Notes,
			"run aborted by caller (SIGINT/SIGTERM/cancel); per-verb percentiles cover a partial window and must NOT be promoted to a ┬º8.3 baseline")
	case errors.Is(err, context.DeadlineExceeded):
		// Caller-supplied deadline fired before our planned
		// duration elapsed ΓÇö still an abort from the harness's
		// perspective.
		if report.ActualDuration < report.PlannedDuration {
			report.Aborted = true
			report.CompletionReason = "aborted-deadline-exceeded"
			report.Notes = append(report.Notes,
				"caller's context deadline fired before the harness's planned duration elapsed; per-verb percentiles cover a partial window")
		} else {
			report.CompletionReason = "completed"
		}
	default:
		report.Aborted = true
		report.CompletionReason = fmt.Sprintf("aborted-%v", err)
	}

	// Aggregate per-verb results in the profile's declared
	// order so the report is stable.
	recallSeen := false
	for _, v := range h.cfg.Profile.Verbs {
		agg, ok := aggs[v.Verb]
		if !ok {
			continue
		}
		agg.mu.Lock()
		latencies := append([]time.Duration(nil), agg.latencies...)
		succeeded := agg.succeeded
		failed := agg.failed
		degraded := agg.degraded
		degradedReasons := copyReasonMap(agg.degradedReasons)
		ranks := append([]int(nil), agg.ranks...)
		hits := append([]bool(nil), agg.hits...)
		agg.mu.Unlock()
		dropped := int(agg.dropped.Load())

		vr := calibration.AggregateVerb(v, latencies, succeeded, failed, dropped, report.PlannedDuration, report.ActualDuration, h.cfg.Profile.ErrorBudgetRatio)
		report.Verbs = append(report.Verbs, vr)
		if !vr.BudgetMet {
			report.BudgetBreaches = append(report.BudgetBreaches, v.Verb)
		}
		if v.Verb == reliability.VerbAgentRecall {
			recallSeen = true
			report.LearningQuality = calibration.AggregateLearningQuality(h.cfg.Profile.LearningQuality, ranks, hits)
		}
		if degraded > 0 {
			note := fmt.Sprintf("verb `%s` returned degraded=true on %d/%d responses",
				v.Verb, degraded, succeeded+failed)
			if reasons := formatDegradedReasons(degradedReasons); reasons != "" {
				note += "; reasons: " + reasons
			}
			report.Notes = append(report.Notes, note)
		}
		if dropped > 0 {
			report.Notes = append(report.Notes,
				fmt.Sprintf("verb `%s` dropped %d ticks (per-verb in-flight cap %d hit); achieved RPS %.3f vs requested %.3f",
					v.Verb, dropped, h.cfg.MaxInflightPerVerb, vr.AchievedRPS, vr.RequestedRPS))
		}
	}
	if !recallSeen {
		report.LearningQuality = calibration.AggregateLearningQuality(h.cfg.Profile.LearningQuality, nil, nil)
		report.Notes = append(report.Notes,
			"recall scenario not driven; learning-quality metrics are n/a for this run")
	}

	// Operator-supplied stable notes (e.g. workflow-doc
	// cross-references) ΓÇö appended after the dynamic notes so
	// the footer position is deterministic across runs.
	//
	// We deliberately do NOT sort report.Notes here. The
	// append order is already deterministic for a given run
	// (abort note first, then per-verb degraded/dropped notes
	// in `cfg.Profile.Verbs` order, then the recall-not-driven
	// note, then operator artifactNotes in registration
	// order), and an alphabetical sort would shuffle the
	// operator-supplied notes into the middle of the dynamic
	// notes ΓÇö contradicting WithArtifactNote's documented
	// "predictable footer position" contract.
	report.Notes = append(report.Notes, h.artifactNotes...)

	return report, nil
}

// copyReasonMap snapshots a degradedReasons map under the
// aggregator's lock so the caller can release the lock before
// rendering the note. Returns nil for a nil/empty input.
func copyReasonMap(src map[string]int) map[string]int {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]int, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// formatDegradedReasons renders the per-reason occurrence map
// onto a single comma-separated string in descending count
// order (ties broken by reason name for deterministic diffs):
//
//	"writer_backpressure=12, qdrant_unreachable=3"
//
// Returns "" when the map is empty so the note emitter can fall
// back to the boolean-only shape for scenarios whose response
// envelope carries no reason string (today: all agent.* verbs).
func formatDegradedReasons(reasons map[string]int) string {
	if len(reasons) == 0 {
		return ""
	}
	type entry struct {
		reason string
		count  int
	}
	entries := make([]entry, 0, len(reasons))
	for k, v := range reasons {
		entries = append(entries, entry{k, v})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].reason < entries[j].reason
	})
	var b []byte
	for i, e := range entries {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = append(b, e.reason...)
		b = append(b, '=')
		b = append(b, []byte(fmt.Sprintf("%d", e.count))...)
	}
	return string(b)
}

// driveScenario is the open-loop ticker for one scenario.
// Runs until schedulerCtx is done (planned-duration deadline)
// or reqCtx is cancelled (caller-driven shutdown).
//
// Two contexts in play here:
//   - schedulerCtx: governs the TICKER loop. Cancels at the
//     planned-duration deadline; new ticks stop firing.
//   - reqCtx: governs IN-FLIGHT requests. Outlives
//     schedulerCtx by design ΓÇö requests started before the
//     deadline complete (or fail on their own RequestTimeout)
//     rather than being unilaterally cancelled by the harness.
//     This matches the open-loop invariant: an in-flight request
//     at deadline t was a legitimate part of the load profile;
//     its latency belongs in the histogram.
//
// Why per-scenario tickers (not one big ticker the router
// dispatches): each scenario has its own arrival rate (50 RPS
// vs 0.83 RPS for ingest_spans). A single router would either
// round-robin (wrong cadence) or pump at the LCM (defeats the
// arrival shaping). Per-scenario tickers keep the math local
// and goroutine-cheap.
func (h *Harness) driveScenario(reqCtx, schedulerCtx context.Context, scen scenarios.Scenario, verb reliability.VerbProfile, agg *aggregator) {
	interval := verb.Interval()
	if interval <= 0 {
		// Defensive ΓÇö the profile validator already rejects
		// SustainedRPS <= 0; this would mean the operator
		// constructed a profile bypassing Validate.
		return
	}

	// Per-verb semaphore: a buffered channel sized at
	// MaxInflightPerVerb. Acquiring is a non-blocking send;
	// a full channel means we drop the tick.
	sem := make(chan struct{}, h.cfg.MaxInflightPerVerb)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var workerWG sync.WaitGroup

	fire := func() {
		select {
		case sem <- struct{}{}:
			workerWG.Add(1)
			go func() {
				defer workerWG.Done()
				defer func() { <-sem }()
				rng := h.newRNG()
				sample := scen.Execute(reqCtx, rng)
				if h.sampleObserver != nil {
					// Per-sample callback (e.g.
					// obs.Histogram.Observe). Fired
					// before the aggregator add so a
					// crashing observer doesn't lose
					// the sample from the artifact ΓÇö
					// the aggregator add is the source
					// of truth.
					func() {
						defer func() { _ = recover() }()
						h.sampleObserver(sample)
					}()
				}
				agg.addSample(sample)
			}()
		default:
			agg.addDrop()
		}
	}

	// Fire the t=0 arrival immediately so the first interval
	// is not lost waiting on the ticker.
	fire()

	for {
		select {
		case <-schedulerCtx.Done():
			// Stop firing new ticks. Wait for in-flight
			// workers to drain ΓÇö they use reqCtx, not
			// schedulerCtx, so they complete normally
			// (subject to their own timeout) instead of
			// returning ctx.Err() noise that would inflate
			// the failure count.
			workerWG.Wait()
			return
		case <-ticker.C:
			fire()
		}
	}
}
