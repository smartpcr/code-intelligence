package insights

// Stage 10.2 -- Aged mute insights report.
//
// This file implements the second Insights surface projection
// (the first being the percentile [Freshness] banner in
// [freshness.go]). The report scans the `clean_code.override`
// log, applies LATEST-ROW-WINS per `(rule_id, scope_filter)`
// to derive the currently-active mute state, then filters to
// the rows whose `mute=true` AND `created_at` is older than
// the operator-supplied threshold (default 90 days).
//
// # eval.gate carve-out (architecture Sec 6.2)
//
// The aged-mute report is INSIGHTS-ONLY. There is NO automatic
// flip, no enforcement timer, no scheduled job that promotes
// an aged mute to "expired" -- the iter-1 evaluator item 5 +
// tech-spec Sec 10A "mute lifecycle" pin BOTH require that v1
// has NO TTL enforcement in code. Operators unmute by
// appending an `override(mute=false)` row through the
// `mgmt.override` write verb; the unmute supersedes the mute
// under [Steward.LatestMatchingOverride]'s glob-matching
// MAX(created_at) read AND drops the (rule_id, scope) pair
// off this report on the next call.
//
// # Why a separate package-level type vs an extension of
// [Freshness]
//
// [Freshness] is a value-receiver, pure-function projection
// that holds the freshness window + clock and does no I/O. The
// aged-mute report needs an injected reader (the underlying
// override log) AND a clock. Composing those into one
// [AgedMutes] struct keeps the freshness check decoupled from
// the override-table scan -- a backend that only wires the
// dashboard surfaces (cross_repo/portfolio) does not need to
// implement the override reader, and a backend that wires
// aged-mutes does not need to know about percentile
// freshness.
//
// # Decoupling from the steward package
//
// The insights package intentionally has ZERO non-stdlib
// dependencies (see the import-isolation matrix in
// `services/clean-code/docs/follow-up-workstreams.md`). To
// preserve that, [OverrideRecord] is a NARROW VIEW of an
// override row -- a value type with only the fields the
// aged-mute projection consumes. The composition root adapts
// `steward.Override` rows to [OverrideRecord]s in the
// management package (which already imports both insights and
// steward), so insights never gains a direct import of
// steward.

import (
	"context"
	"sort"
	"time"
)

// AgedMuteDefaultThresholdDays is the default operator
// threshold for "aged" classification. The Stage 10.2 brief
// pins it at 90 days; the operator can override per-call via
// [AgedMutes.ReportWithThreshold] / the
// `mgmt.read.insights.aged_mutes(threshold_days?)` verb's
// optional argument.
//
// 90 days mirrors the tech-spec Sec 10A guidance that a mute
// older than one calendar quarter SHOULD be reviewed -- v1
// only SURFACES the review item, it does not act on it.
const AgedMuteDefaultThresholdDays = 90

// OverrideScope is the exact-match scope-filter triple that
// identifies a single mute target. Mirrors the JSON shape of
// `clean_code.override.scope_filter` (architecture Sec 5.3.6
// line 1167): `{repo_id, scope_kind, scope_signature_glob}`.
//
// The aged-mute report groups by `(rule_id, OverrideScope)`
// using BYTE-IDENTICAL equality on all three fields -- an
// operator-issued unmute (`mute=false`) must therefore replay
// the SAME triple to supersede a prior mute (this matches the
// steward writer's storage shape; the value is stored
// verbatim with no normalisation).
//
// NOT a glob-match: this report's "latest-row-wins" reduction
// works at the storage-key granularity, not at the candidate-
// scope evaluation granularity. The gate-time helper
// [steward.Steward.LatestMatchingOverride] performs glob
// matching against a concrete candidate scope; this report
// instead reports per registered scope filter (the same
// granularity an operator typed when they wrote the
// override).
type OverrideScope struct {
	RepoID             string
	ScopeKind          string
	ScopeSignatureGlob string
}

// OverrideRecord is the narrow view of an override row the
// aged-mute projection consumes. Keeping the fields slim AND
// using plain Go types (no `uuid.UUID`, no JSON tags) is what
// lets the insights package stay leaf-level: see the package
// doc above for the import-isolation rationale.
//
// `OverrideID` is the stringified `clean_code.override.override_id`
// (uuid). The aged-mute report uses it for tie-breaking among
// rows that share `(rule_id, scope, created_at)` -- the
// canonical SQL contract is "MAX(created_at) DESC, MAX(override_id)
// DESC" (see `Store.LatestMatchingOverride` docstring) and
// this report mirrors that ordering verbatim.
//
// `CreatedAt` MUST be the UTC instant the writer stamped
// (`s.clock().UTC()` in the steward). The aged-mute filter
// computes `now - CreatedAt`; a non-UTC timestamp would yield
// the same comparison (instants are absolute) but the
// rendered AgeDays field would be off by the configured
// timezone, which is misleading on a dashboard.
type OverrideRecord struct {
	OverrideID string
	RuleID     string
	Scope      OverrideScope
	Mute       bool
	Reason     string
	ActorID    string
	CreatedAt  time.Time
}

// OverrideReader is the narrow READ SEAM the aged-mute
// projection uses to fetch override rows. The implementation
// MUST return EVERY override row (both `mute=true` AND
// `mute=false`) so the reducer here can apply latest-row-wins
// per `(rule_id, scope)` -- if `mute=false` rows are stripped,
// the unmute-supersedes-mute semantic breaks and aged mutes
// continue to appear in the report after operators unmute them
// (this is the failure mode the `unmute-removes-from-report`
// scenario guards against).
//
// Order of returned rows is unspecified -- this report does
// its own (rule_id, scope) grouping and per-group MAX
// reduction, so a backend that streams in
// `(created_at DESC, override_id DESC)` order (the SQLStore
// shape) and a backend that returns rows insertion-ordered
// (the InMemoryStore shape) yield bit-identical reports.
//
// Backends MUST propagate `ctx.Err()` on cancellation so an
// operator dashboard tab that closes mid-scan does not pin a
// goroutine to a full table scan.
type OverrideReader interface {
	ListAllOverrides(ctx context.Context) ([]OverrideRecord, error)
}

// AgedMute is one entry in the aged-mute report. The fields
// mirror [OverrideRecord] verbatim PLUS `AgeDays` -- the
// floor-of-days age the dashboard renders so an operator can
// see "97 days" without re-deriving the duration from
// `CreatedAt`.
//
// `Reason` is echoed verbatim from the override row; the
// architecture Sec 5.3.6 contract requires it to be non-empty
// when `mute=true` (the only rows that ever appear here), so a
// renderer can use `Reason` as the primary label without a
// null check.
type AgedMute struct {
	OverrideID string
	RuleID     string
	Scope      OverrideScope
	Reason     string
	ActorID    string
	CreatedAt  time.Time
	AgeDays    int
}

// AgedMutes is the Insights-surface projection. It holds the
// override-row reader, the clock seam, and the canonical
// threshold; methods are SAFE FOR CONCURRENT USE (the struct
// is read-only after construction; the reader / clock
// implementations enforce their own concurrency contracts).
//
// `Reader` MUST be non-nil; [Report] / [ReportWithThreshold]
// return [ErrAgedMuteReaderUnavailable] when it is. A nil
// `Clock` falls back to [SystemClock] (mirrors [Freshness.now]
// -- the hot read path stays crash-free under a wiring bug).
type AgedMutes struct {
	Reader    OverrideReader
	Clock     Clock
	Threshold time.Duration
}

// NewAgedMutes wires an [AgedMutes] with the canonical
// production defaults: 90-day threshold + [SystemClock]. The
// composition root calls this once and passes the result to
// the management Reader via [management.WithAgedMutes].
//
// A nil `r` is permitted at construction -- the resulting
// AgedMutes can be wired into a Reader for "feature absent"
// scaffold-mode bring-ups; calls to [Report] then return
// [ErrAgedMuteReaderUnavailable] (the HTTP layer maps this to
// 503 per the management surface convention).
func NewAgedMutes(r OverrideReader) *AgedMutes {
	return &AgedMutes{
		Reader:    r,
		Clock:     SystemClock{},
		Threshold: time.Duration(AgedMuteDefaultThresholdDays) * 24 * time.Hour,
	}
}

// errAgedMuteReader is the package-private error returned when
// [AgedMutes.Reader] is nil. Exposed via the exported sentinel
// [ErrAgedMuteReaderUnavailable] so callers can branch via
// `errors.Is` rather than string-matching.
type errAgedMuteReader struct{}

func (errAgedMuteReader) Error() string {
	return "insights: aged-mute reader not wired (composition-root bug)"
}

// ErrAgedMuteReaderUnavailable is returned by [AgedMutes.Report]
// and [AgedMutes.ReportWithThreshold] when the receiver was
// constructed without a backing [OverrideReader]. The HTTP
// layer maps this to a 503 Service Unavailable -- the verb is
// mounted but the substrate is not, mirroring
// [management.ErrBackendUnavailable].
var ErrAgedMuteReaderUnavailable error = errAgedMuteReader{}

// Report returns every aged mute under the configured
// [AgedMutes.Threshold]. The result list is sorted
// deterministically by (RuleID, Scope.RepoID, Scope.ScopeKind,
// Scope.ScopeSignatureGlob) ascending so two callers that read
// the same backend state in succession see byte-identical JSON.
//
// Returns an empty (non-nil) slice when no override matches.
// Errors:
//
//   - [ErrAgedMuteReaderUnavailable] when [Reader] is nil.
//   - The raw [OverrideReader.ListAllOverrides] error
//     (wrapped) when the backend scan fails.
//   - `ctx.Err()` is propagated by the backend; callers should
//     check via `errors.Is` (the wrapping preserves the
//     sentinel).
func (a *AgedMutes) Report(ctx context.Context) ([]AgedMute, error) {
	if a == nil {
		return nil, ErrAgedMuteReaderUnavailable
	}
	return a.ReportWithThreshold(ctx, a.Threshold)
}

// ReportWithThreshold is the operator-overridable variant of
// [Report]. The `threshold` argument MUST be positive; a
// non-positive duration (zero or negative) is treated as the
// default 90-day threshold so a missing-arg HTTP call cannot
// silently surface every mute (which would be the
// "threshold=0 -> every mute is aged" failure mode).
//
// Mirrors the
// `mgmt.read.insights.aged_mutes(threshold_days?)` verb's
// optional argument: the verb resolves a present
// `threshold_days` to `time.Duration(d) * 24 * time.Hour` and
// passes it through; an absent argument calls [Report]
// instead.
func (a *AgedMutes) ReportWithThreshold(ctx context.Context, threshold time.Duration) ([]AgedMute, error) {
	if a == nil || a.Reader == nil {
		return nil, ErrAgedMuteReaderUnavailable
	}
	if threshold <= 0 {
		threshold = time.Duration(AgedMuteDefaultThresholdDays) * 24 * time.Hour
	}
	records, err := a.Reader.ListAllOverrides(ctx)
	if err != nil {
		return nil, err
	}
	now := a.now()
	report := reduceAndFilter(records, now, threshold)
	return report, nil
}

// now resolves the configured [Clock], defaulting to
// [SystemClock] when nil. Mirrors [Freshness.now] -- the hot
// read path stays crash-free under a wiring bug rather than
// panicking and taking down the Insights dashboard.
func (a *AgedMutes) now() time.Time {
	if a.Clock == nil {
		return SystemClock{}.Now()
	}
	return a.Clock.Now()
}

// reduceAndFilter applies the Stage 10.2 reduction:
//
//  1. Group `records` by `(rule_id, scope)`.
//  2. Per group, pick the LATEST row by `(CreatedAt,
//     OverrideID)` -- mirrors the SQL contract
//     `MAX(created_at) DESC, override_id DESC` that
//     [Store.LatestMatchingOverride] documents. Tie-break on
//     `OverrideID` lexicographic ascending picks the larger uuid
//     as the winner so a clock-skew tie is broken
//     deterministically.
//  3. Drop groups whose winner has `Mute=false` -- this is the
//     unmute-supersedes-mute step; an unmute appended AFTER a
//     mute drops the (rule, scope) pair off the report.
//  4. Drop groups whose winner's age `now - CreatedAt <=
//     threshold` (inclusive: an exact-boundary row is NOT
//     aged; mirrors the [Freshness] window inclusive
//     contract).
//  5. Sort the remaining winners by (RuleID, Scope.RepoID,
//     Scope.ScopeKind, Scope.ScopeSignatureGlob) ascending so
//     the result is deterministic.
//
// Pure function: no I/O, no clock, no concurrency. Exported
// internally for the test suite to feed in fixture records.
func reduceAndFilter(records []OverrideRecord, now time.Time, threshold time.Duration) []AgedMute {
	type groupKey struct {
		ruleID string
		scope  OverrideScope
	}
	latest := make(map[groupKey]OverrideRecord, len(records))
	for _, r := range records {
		k := groupKey{ruleID: r.RuleID, scope: r.Scope}
		existing, ok := latest[k]
		if !ok {
			latest[k] = r
			continue
		}
		if recordWins(r, existing) {
			latest[k] = r
		}
	}
	out := make([]AgedMute, 0, len(latest))
	for _, r := range latest {
		if !r.Mute {
			continue
		}
		age := now.Sub(r.CreatedAt)
		if age <= threshold {
			continue
		}
		out = append(out, AgedMute{
			OverrideID: r.OverrideID,
			RuleID:     r.RuleID,
			Scope:      r.Scope,
			Reason:     r.Reason,
			ActorID:    r.ActorID,
			CreatedAt:  r.CreatedAt,
			AgeDays:    int(age / (24 * time.Hour)),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return lessAgedMute(out[i], out[j])
	})
	return out
}

// recordWins reports whether `cand` should replace `cur` as
// the latest-row-wins winner for a group. Mirrors the SQL
// `ORDER BY created_at DESC, override_id DESC LIMIT 1` shape:
// later instants beat earlier; on a tie, larger OverrideID
// (lexicographic) wins. Sorting ascending then picking last
// would yield the same result; we keep a single-pass O(n)
// reducer instead because the steward emits at most O(rules *
// scopes) rows and the aged-mute report runs on the operator
// dashboard hot path.
func recordWins(cand, cur OverrideRecord) bool {
	if cand.CreatedAt.After(cur.CreatedAt) {
		return true
	}
	if cand.CreatedAt.Equal(cur.CreatedAt) && cand.OverrideID > cur.OverrideID {
		return true
	}
	return false
}

// lessAgedMute orders two AgedMute entries by the
// architecture-friendly canonical key
// (RuleID, Scope.RepoID, Scope.ScopeKind, Scope.ScopeSignatureGlob).
// The tail tie-breaker is OverrideID so two rows that share the
// quadruple (which the schema permits: two muted scopes with
// the same scope_filter triple would only occur if an earlier
// (mute=true) -> (mute=false) -> (mute=true) sequence somehow
// reduced to two winners, which it cannot under
// [reduceAndFilter]'s single-winner-per-group reduction;
// still, a defensive tie-breaker keeps the sort total).
func lessAgedMute(a, b AgedMute) bool {
	if a.RuleID != b.RuleID {
		return a.RuleID < b.RuleID
	}
	if a.Scope.RepoID != b.Scope.RepoID {
		return a.Scope.RepoID < b.Scope.RepoID
	}
	if a.Scope.ScopeKind != b.Scope.ScopeKind {
		return a.Scope.ScopeKind < b.Scope.ScopeKind
	}
	if a.Scope.ScopeSignatureGlob != b.Scope.ScopeSignatureGlob {
		return a.Scope.ScopeSignatureGlob < b.Scope.ScopeSignatureGlob
	}
	return a.OverrideID < b.OverrideID
}
