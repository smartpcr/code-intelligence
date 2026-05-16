package repoindexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// EventChannel is the single PostgreSQL `LISTEN/NOTIFY` channel
// every Stage 3+ worker publishes against (per
// implementation-plan.md §3.1: "downstream workers `LISTEN` on
// the `agent_memory_events` channel"). Pinning the channel name
// here means downstream subscribers (`internal/embedding`,
// `internal/concept`, mgmt-api/recall paths) all reference the
// same `repoindexer.EventChannel` symbol -- a single `grep -F
// "agent_memory_events"` finds every coupling.
const EventChannel = "agent_memory_events"

// Closed set of event kinds the Repo Indexer publishes. Stages
// 3.3/3.4 add more constants for embedding-published and delta-
// ingested events; the closed-set discipline mirrors the ENUM
// approach the schema migrations take so a typo cannot leak a
// novel event kind onto the channel.
const (
	// EventKindRepoRegistered fires the FIRST time an
	// ingest_jobs row reaches `done` for a given (repo_id,
	// mode='full', from_sha, to_sha) tuple. The predicate is
	// evaluated inside the same atomic tx as the
	// status='done' UPDATE (see worker.markDoneAndPublish), so
	// it stays consistent with the row's terminal state and
	// survives mid-pipeline retries: a transient failure after
	// EnsureCommit succeeded -- which under the prior
	// `EnsureCommit.Inserted` predicate would suppress the
	// event because the commit row already exists -- still
	// fires `repo.registered` on the eventual successful
	// retry. It is the architecture.md §4.1-step-4 "Repo
	// Indexer publishes a `repo.registered` event with the
	// indexed SHA" signal.
	EventKindRepoRegistered = "repo.registered"
	// EventKindRepoFullIngested fires on EVERY successful
	// full-mode completion (cold registration AND idempotent
	// re-ingest of an already-known SHA). Downstream consumers
	// that don't care about cold-vs-replay subscribe to this
	// kind and ignore `repo.registered`.
	EventKindRepoFullIngested = "repo.full_ingested"
)

// EventPublisher publishes Repo Indexer lifecycle events. The
// production implementation (`PGNotifyPublisher`) shells out to
// `pg_notify(channel, payload)`; tests inject a
// `*recordingEventPublisher` so they can assert on the captured
// payloads without opening a LISTEN connection.
//
// Two publish surfaces are exposed:
//
//   - `Publish` runs in autocommit and is intended for one-off
//     publishes that do not need to be atomic with another
//     database mutation (e.g. the integration test that drives
//     a LISTEN'ing connection through a single payload).
//
//   - `PublishTx` accepts a *sql.Tx so the NOTIFY enrolls in the
//     caller's transaction. PostgreSQL queues NOTIFY payloads
//     until the tx commits, so this is the load-bearing primitive
//     the worker uses to deliver `repo.registered` /
//     `repo.full_ingested` ATOMICALLY with the
//     `ingest_jobs.status='done'` transition. If `PublishTx`
//     returns an error the caller MUST roll the tx back so no
//     event is delivered for a job that did not reach `done`,
//     and conversely the `done` transition is never committed
//     for a job whose events failed to enqueue.
//
// Both methods are responsible for marshalling `ev` to the
// stable JSON payload (the closed set `{kind, repo_id, sha,
// job_id, time}` plus future additive fields).
type EventPublisher interface {
	Publish(ctx context.Context, ev Event) error
	PublishTx(ctx context.Context, tx *sql.Tx, ev Event) error
}

// Event is the payload shape every publisher accepts. The
// JSON wire format is stable (lower-snake-case field names)
// so downstream subscribers can decode without depending on
// this Go module.
type Event struct {
	Kind   string    `json:"kind"`
	RepoID string    `json:"repo_id"`
	SHA    string    `json:"sha"`
	JobID  string    `json:"job_id"`
	Time   time.Time `json:"time"`
}

// MarshalPayload renders the event to the JSON payload the
// publisher hands to `pg_notify`. Exposed (not a private helper)
// so unit tests can assert on the on-the-wire format without
// re-implementing the marshalling rule.
func (e Event) MarshalPayload() (string, error) {
	if e.Kind == "" {
		return "", errors.New("repoindexer: Event.MarshalPayload: empty kind")
	}
	// pg_notify rejects payloads > 8000 bytes; we encode just
	// the closed set above which is well under that. Use the
	// stdlib encoder so the field order matches the struct tag
	// declaration (encoding/json preserves struct field order).
	b, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("repoindexer: Event.MarshalPayload: %w", err)
	}
	return string(b), nil
}

// PGNotifyPublisher is the production EventPublisher. It exposes
// two publish modes:
//
//   - `Publish` issues `SELECT pg_notify($1, $2)` in autocommit on
//     the supplied *sql.DB.
//   - `PublishTx` issues the same statement on a *sql.Tx so the
//     NOTIFY is queued by PostgreSQL until that tx commits.
//
// Wiring rule: pg_notify payloads are only delivered to a
// LISTEN'ing connection after the issuing tx commits. The Stage
// 3.1 worker uses `PublishTx` so the `repo.registered` /
// `repo.full_ingested` events are guaranteed to be delivered
// IFF the matching `ingest_jobs.status='done'` UPDATE commits.
// Tests that just want to verify channel delivery use `Publish`
// because they don't need atomicity with another mutation.
type PGNotifyPublisher struct {
	db *sql.DB
	// logger is the structured logger the publisher emits one
	// record per Publish call to. The shape mirrors the
	// graphwriter audit log so operators see a uniform
	// {op, repo_id, kind, sha} tuple across services.
	logger *slog.Logger
	// channel is overridable so tests can target a per-test
	// channel name and avoid cross-test event leaks when
	// running `go test -parallel`. Empty means `EventChannel`.
	channel string
}

// NewPGNotifyPublisher constructs a publisher over `db`. A nil
// `db` panics (the publisher cannot operate without a database
// handle); a nil logger is replaced with `slog.Default()`. The
// channel defaults to `EventChannel` -- tests override with the
// optional `WithChannel` helper.
func NewPGNotifyPublisher(db *sql.DB, logger *slog.Logger) *PGNotifyPublisher {
	if db == nil {
		panic("repoindexer: NewPGNotifyPublisher: nil *sql.DB")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PGNotifyPublisher{db: db, logger: logger, channel: EventChannel}
}

// WithChannel returns a shallow copy of the publisher with its
// channel name overridden. The original publisher is unchanged
// so concurrent callers see no surprise channel switch.
func (p *PGNotifyPublisher) WithChannel(name string) *PGNotifyPublisher {
	cp := *p
	cp.channel = name
	return &cp
}

// execer is the smallest interface both *sql.DB and *sql.Tx
// satisfy that PGNotifyPublisher needs. Pulling it out lets the
// Publish / PublishTx code paths share a single helper without
// reflection or duplicated branching on the caller's type.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Publish marshals `ev` to JSON and issues `SELECT pg_notify(...)`.
// In autocommit on `p.db`. On success, one Info-level log record is
// emitted; on failure, one Error-level record. The shape is
// intentionally uniform with the graphwriter audit middleware so
// operators can grep for `{op="publish_event", kind=..., repo_id=...}`
// across the whole service.
func (p *PGNotifyPublisher) Publish(ctx context.Context, ev Event) error {
	return p.publishVia(ctx, p.db, ev)
}

// PublishTx issues the NOTIFY through the supplied *sql.Tx so the
// payload only enters PostgreSQL's notification queue when the
// caller commits the tx. This is the primitive the Stage 3.1
// worker uses to keep event delivery atomic with the
// `ingest_jobs.status='done'` transition.
func (p *PGNotifyPublisher) PublishTx(ctx context.Context, tx *sql.Tx, ev Event) error {
	if tx == nil {
		return errors.New("repoindexer: PGNotifyPublisher.PublishTx: nil *sql.Tx")
	}
	return p.publishVia(ctx, tx, ev)
}

func (p *PGNotifyPublisher) publishVia(ctx context.Context, x execer, ev Event) error {
	payload, err := ev.MarshalPayload()
	if err != nil {
		p.logger.Error("repoindexer.publisher.failed",
			slog.String("op", "publish_event"),
			slog.String("kind", ev.Kind),
			slog.String("repo_id", ev.RepoID),
			slog.String("sha", ev.SHA),
			slog.String("error", err.Error()),
		)
		return err
	}
	if _, err := x.ExecContext(ctx,
		`SELECT pg_notify($1, $2)`, p.channel, payload,
	); err != nil {
		wrapped := fmt.Errorf("repoindexer: pg_notify(%s): %w", p.channel, err)
		p.logger.Error("repoindexer.publisher.failed",
			slog.String("op", "publish_event"),
			slog.String("kind", ev.Kind),
			slog.String("repo_id", ev.RepoID),
			slog.String("sha", ev.SHA),
			slog.String("channel", p.channel),
			slog.String("error", wrapped.Error()),
		)
		return wrapped
	}
	p.logger.Info("repoindexer.publisher",
		slog.String("op", "publish_event"),
		slog.String("kind", ev.Kind),
		slog.String("repo_id", ev.RepoID),
		slog.String("sha", ev.SHA),
		slog.String("channel", p.channel),
	)
	return nil
}
