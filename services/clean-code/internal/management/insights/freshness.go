// Package insights owns the Insights-surface projections the
// Management read verbs `mgmt.read.cross_repo` and
// `mgmt.read.portfolio` (architecture Sec 6.3, Stage 6.3)
// attach to their response envelopes. The freshness banner
// implemented here is one of those projections; aged-mute
// rollups and other dashboard-side rollups land alongside it
// in follow-up files.
//
// # Why a separate package
//
// The insights projections are consumed BY the Management
// surface (`internal/management/reader.go`) but are
// architecturally a distinct concern: the Reader is the
// row-level identity / active-row contract layer (architecture
// G2 / Sec 5.2.1), and the Insights projections are the
// "envelope decoration" layer that runs AFTER the Reader has
// resolved the canonical row. Splitting them keeps the
// Reader's per-verb method bodies focused on the canonical
// projection ("here is the row I read") and pushes envelope
// decoration into a single composable struct ([Freshness])
// that the Reader holds as a field. That layout also matches
// the implementation-plan's Stage 7.3 dependency declaration:
// "phase-evaluator-surface-and-management-surface/stage-
// management-read-verbs-and-insights-projections" -- the
// Insights projections layer is a Stage 6.3 target so that
// later stages (7.3 percentile-freshness banner, 7.4 aged-
// mutes rollup) can extend the surface without touching the
// Reader struct.
//
// # eval.gate carve-out (iter 1 evaluator item 8)
//
// The freshness banner is INSIGHTS-ONLY. `eval.gate`
// (architecture Sec 6.2) refuses to accept
// `degraded_reason='percentile_stale'` -- the gate's
// degraded-reason taxonomy is the four-value set
// {samples_pending, policy_signature_invalid,
// xrepo_edges_unavailable, ast_subprocess_unavailable}
// (architecture Sec 8.2). The string constant
// [DegradedReasonPercentileStale] is exported here so the
// gate-side validator can explicitly compare and reject
// (verified by Stage 6.1 test scenario
// `percentile-stale-not-on-gate`); see also Stage 7.3
// scenario `gate-never-emits-percentile-stale`.
package insights

import (
	"time"
)

// FreshnessWindowSeconds is the canonical `built_at` staleness
// threshold the Insights surface compares against. Pinned at
// 3600s (one hour) per tech-spec Sec 8.2 line referencing
// `freshness_window_seconds=3600`. Exported as a typed integer
// so the composition root can build a [time.Duration] without
// re-deriving the conversion. Stage 7.3 may switch this to a
// per-deployment knob; v1 keeps it a package constant so the
// freshness contract is greppable across the codebase.
const FreshnessWindowSeconds = 3600

// DegradedReasonPercentileStale is the canonical degraded
// reason string emitted on the Insights envelope when a
// `cross_repo_percentile` / `portfolio_snapshot` row's
// `built_at` is older than [FreshnessWindowSeconds] at the
// time of read. INSIGHTS-ONLY: see package doc + tech-spec
// C18 / architecture Sec 7.5. Pinned as a string constant so
// `eval.gate`'s degraded-reason validator can compare against
// it without importing this package's [Freshness] type.
const DegradedReasonPercentileStale = "percentile_stale"

// Clock is the time-source seam [Freshness] uses to compute
// staleness. Tests inject a fake clock so a fixture row's
// `built_at` can be classified deterministically without
// sleeping for an hour. Production wires [SystemClock].
type Clock interface {
	Now() time.Time
}

// SystemClock is the production [Clock] -- a thin wrapper
// over [time.Now] so a Reader holding a [Freshness] doesn't
// need to capture the global function. Stateless: every
// composition-root call shares one zero-value instance.
type SystemClock struct{}

// Now returns the current wall-clock time in the system's
// configured timezone. Callers MUST NOT depend on a specific
// timezone -- the freshness comparison only inspects the
// duration between [Status.BuiltAt] and [Status.EvaluatedAt],
// both of which are absolute instants.
func (SystemClock) Now() time.Time { return time.Now() }

// Freshness encapsulates the percentile-staleness check the
// Insights verbs attach to their response envelope. One
// instance is held by [management.Reader] and shared across
// concurrent reads -- the struct is read-only after
// construction, so it is safe for concurrent use.
//
// Window and Clock are exported so the composition root /
// integration tests can build a custom shape; the canonical
// production construction is [NewPercentileFreshness] which
// supplies [FreshnessWindowSeconds] + [SystemClock].
type Freshness struct {
	// Window is the inclusive staleness threshold. A row
	// whose `built_at` is at least `Window` seconds older
	// than [Clock.Now] is reported as stale. The boundary
	// case (age == Window) is treated as FRESH so a snapshot
	// that just hit the threshold is not prematurely flagged.
	Window time.Duration
	// Clock is the time-source the staleness compute uses.
	// MUST be non-nil; [NewPercentileFreshness] supplies
	// [SystemClock] when callers do not specify one.
	Clock Clock
}

// NewPercentileFreshness returns a [Freshness] wired with the
// production defaults: [FreshnessWindowSeconds]-second window
// and a [SystemClock]. The composition root calls this once
// and passes the result to [management.NewReader] via
// [management.WithInsightsFreshness]; when no
// [management.WithInsightsFreshness] option is passed,
// [management.NewReader] auto-constructs a default
// [Freshness] from this helper so a composition-root miss
// CANNOT silently let a stale snapshot render as fresh.
func NewPercentileFreshness() *Freshness {
	return &Freshness{
		Window: time.Duration(FreshnessWindowSeconds) * time.Second,
		Clock:  SystemClock{},
	}
}

// Status is the freshness verdict the Insights envelope
// carries. `Degraded=false` means the row is within the
// freshness window; `Degraded=true` means the row is stale
// and the verb's response envelope MUST stamp
// `degraded=true, degraded_reason='percentile_stale'` to
// the wire.
type Status struct {
	// Degraded is true iff `now() - BuiltAt > Window`.
	Degraded bool
	// Reason carries [DegradedReasonPercentileStale] when
	// `Degraded=true`, else the empty string. Kept as a
	// string (not an enum) so the wire shape matches the
	// `clean_code.degraded_reason` SQL enum label verbatim.
	Reason string
	// BuiltAt is the row's `built_at` instant verbatim --
	// echoed so a log line / metric label can attribute
	// the staleness verdict to a specific snapshot.
	BuiltAt time.Time
	// EvaluatedAt is the [Clock.Now] reading used for the
	// comparison. Echoed so a debug dump can verify the
	// fake clock plumbed through correctly.
	EvaluatedAt time.Time
	// Window is the [Freshness.Window] copied into the
	// status so a downstream consumer can render "stale
	// after Xs" without re-reading the parent struct.
	Window time.Duration
}

// Evaluate classifies `builtAt` against the configured
// window. Returns a [Status] with `Degraded=true` and
// [DegradedReasonPercentileStale] when stale, else
// `Degraded=false` and an empty `Reason`.
//
// Comparison semantics: `Degraded = (now - builtAt) > Window`.
// A `builtAt` in the future (clock skew between writer and
// reader) is treated as fresh -- the resulting negative
// duration is never `> Window` (Window is non-negative); the
// Insights surface does not police clock drift.
//
// Empty input (zero `builtAt`): a zero `time.Time` is the
// "no row" sentinel some backends return when the underlying
// table is empty. We treat it as STALE so an unpopulated
// dashboard does not silently render misleading "fresh"
// metrics. Callers that distinguish "no row" from "stale row"
// should branch BEFORE calling Evaluate.
func (f *Freshness) Evaluate(builtAt time.Time) Status {
	now := f.now()
	if builtAt.IsZero() {
		return Status{
			Degraded:    true,
			Reason:      DegradedReasonPercentileStale,
			BuiltAt:     builtAt,
			EvaluatedAt: now,
			Window:      f.Window,
		}
	}
	age := now.Sub(builtAt)
	if age > f.Window {
		return Status{
			Degraded:    true,
			Reason:      DegradedReasonPercentileStale,
			BuiltAt:     builtAt,
			EvaluatedAt: now,
			Window:      f.Window,
		}
	}
	return Status{
		Degraded:    false,
		Reason:      "",
		BuiltAt:     builtAt,
		EvaluatedAt: now,
		Window:      f.Window,
	}
}

// now resolves the configured [Clock], defaulting to
// [SystemClock] when the field is nil. A nil clock is treated
// as a wiring bug worth surviving rather than panicking --
// every caller of [Evaluate] is on the read hot path and a
// crash there would take the Insights dashboard down.
func (f *Freshness) now() time.Time {
	if f.Clock == nil {
		return SystemClock{}.Now()
	}
	return f.Clock.Now()
}
