// Stage 10.3 canonical-states conformance test.
//
// Pins the four state enums named in the workstream brief
// to their architecturally-canonical values so a future
// rename, addition, or deletion is caught at test time:
//
//   - `Commit.scan_status`  -- {pending, scanning, scanned, failed}
//     (architecture Sec 5.1.2 line 891, code:
//      `internal/repo_indexer.ScanStatus`)
//   - `ScanRun.status`      -- {running, succeeded, failed}
//     (architecture Sec 5.7 line 1308, code:
//      `internal/metric_ingestor.ScanRunStatus`)
//   - `Verdict`             -- {pass, warn, block}
//     (architecture Sec 5.4.3 line 1237, code:
//      `internal/domain.Verdict`)
//   - `Override` has NO `expires_at` field
//     (architecture Sec 5.3.6 lines 1187-1197 -- the Override
//      row is append-only and carries no TTL; the
//      `mgmt.override` lifecycle is "append a fresh row to
//      flip mute on/off", NOT "set an expiry". Code:
//      `internal/policy/steward.Override`.)
//
// The fourth pin is the iter-1 evaluator item 9
// regression guard: the v1 surface deliberately omits a TTL
// because a TTL would create a silent un-mute (the operator
// authorised a mute, then the system silently revoked it
// when the clock advanced -- a clean-code-grade audit trail
// requires every mute lifecycle change to be a recorded
// operator action). A future contributor adding an
// `ExpiresAt` field to the struct would tag this test red.
package conformance

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/domain"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/repo_indexer"
)

// TestCommitScanStatus_CanonicalEnum asserts the
// `repo_indexer.AllScanStatuses` slice equals the closed
// architecture-canonical set
// `{pending, scanning, scanned, failed}` (architecture
// Sec 5.1.2 line 891). The assertion is order-pinned --
// architecture Sec 5.1.2 lists the four values in this
// exact order, and the [repo_indexer.ScanStatus] package
// godoc commits to "declared order matches the canonical
// lifecycle diagram pending -> scanning -> scanned|failed".
// A future re-ordering would silently change the order
// downstream readers see and is therefore a contract
// breach worth surfacing here.
func TestCommitScanStatus_CanonicalEnum(t *testing.T) {
	want := []repo_indexer.ScanStatus{
		repo_indexer.ScanStatusPending,
		repo_indexer.ScanStatusScanning,
		repo_indexer.ScanStatusScanned,
		repo_indexer.ScanStatusFailed,
	}
	got := repo_indexer.AllScanStatuses()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf(
			"Commit.scan_status enum has drifted from architecture Sec 5.1.2:\n  got:  %v\n  want: %v\n\nTo fix, either (a) restore the closed set in `internal/repo_indexer/scan_status.go` if the drift was accidental, or (b) co-edit architecture Sec 5.1.2 line 891, the PostgreSQL enum `clean_code.commit_scan_status` (migration 0001), AND this test's `want` slice if the change is intentional.",
			stringifyScanStatuses(got),
			stringifyScanStatuses(want),
		)
	}
	// Pin the underlying string wire literals so a
	// future contributor cannot quietly rename a const
	// (e.g. `ScanStatusPending = "queued"`) -- the
	// architecture freeze is on the wire literal, not the
	// Go identifier.
	literals := []string{"pending", "scanning", "scanned", "failed"}
	for i, s := range got {
		if string(s) != literals[i] {
			t.Errorf("ScanStatus[%d] wire literal drift: got %q want %q", i, string(s), literals[i])
		}
	}
}

// TestScanRunStatus_CanonicalEnum asserts
// `metric_ingestor.AllScanRunStatuses` is the closed
// architecture-canonical set `{running, succeeded, failed}`
// (architecture Sec 5.7 line 1308). The check is on SET
// equality plus a literal pin: the architecture lists the
// three values without committing to a particular order on
// the wire, but the wire literals themselves are pinned by
// migration 0001's `clean_code.scan_run_status` enum.
func TestScanRunStatus_CanonicalEnum(t *testing.T) {
	got := stringifyScanRunStatuses(metric_ingestor.AllScanRunStatuses())
	sort.Strings(got)
	want := []string{"failed", "running", "succeeded"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf(
			"ScanRun.status enum has drifted from architecture Sec 5.7:\n  got (sorted):  %v\n  want (sorted): %v\n\nTo fix, either (a) restore the closed set in `internal/metric_ingestor/state.go` if the drift was accidental, or (b) co-edit architecture Sec 5.7 line 1308, the PostgreSQL enum `clean_code.scan_run_status` (migration 0001), AND this test's `want` slice if the change is intentional.",
			got, want,
		)
	}
}

// TestVerdict_CanonicalEnum asserts `domain.AllVerdicts` is
// the closed architecture-canonical set
// `{pass, warn, block}` (architecture Sec 5.4.3 line 1237).
// The assertion is order-pinned -- the `domain.AllVerdicts`
// godoc commits to "exhaustive, ordered list" with the same
// ordering Sec 5.4.3 publishes.
//
// This test is also the iter-1 evaluator item 6 regression
// guard: a stray verdict value like `fail`, `gated`,
// `degraded`, or `error` would break the `eval.gate`
// contract; the test exists so a slip lands red here
// instead of in production logs.
func TestVerdict_CanonicalEnum(t *testing.T) {
	want := []domain.Verdict{
		domain.VerdictPass,
		domain.VerdictWarn,
		domain.VerdictBlock,
	}
	got := domain.AllVerdicts()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf(
			"Verdict enum has drifted from architecture Sec 5.4.3:\n  got:  %v\n  want: %v\n\nTo fix, either (a) restore the closed set in `internal/domain/verdict.go` if the drift was accidental, or (b) co-edit architecture Sec 5.4.3 line 1237, the PostgreSQL enum `clean_code.evaluation_verdict` (migration 0003), AND this test's `want` slice if the change is intentional.",
			stringifyVerdicts(got),
			stringifyVerdicts(want),
		)
	}
	// Pin the underlying string wire literals.
	literals := []string{"pass", "warn", "block"}
	for i, v := range got {
		if string(v) != literals[i] {
			t.Errorf("Verdict[%d] wire literal drift: got %q want %q", i, string(v), literals[i])
		}
	}
}

// TestOverride_NoExpiresAt asserts the
// `steward.Override` struct has NO `expires_at` /
// `ExpiresAt` field on the Go struct AND no
// `expires_at` JSON tag on any field. The v1 contract is
// "append-only mute -- no TTL" (architecture Sec 5.3.6,
// iter-1 evaluator item 9); a future contributor adding a
// TTL field would silently un-mute operator-authorised
// overrides when the clock advanced, breaking the audit
// trail. This test is the structural guard.
//
// # Why we check both Go name AND JSON tag
//
// A future contributor might rename the Go field to
// something innocuous (`ExpiryWindow`, `LifetimeSeconds`)
// while keeping the JSON wire literal as `expires_at` --
// the JSON-tag scan catches that. Conversely, a Go field
// named `ExpiresAt` with `json:"-"` (i.e. wire-omitted)
// is STILL a structural betrayal of the append-only
// contract; the Go-name scan catches that.
//
// # Tag-name parsing
//
// Go struct tags are space-separated `key:"value"` pairs;
// the JSON tag value may itself be `name,omitempty,...`
// -- we extract the first comma-separated segment, which
// is the wire name. A `json:"-"` field has wire name `-`
// (i.e. omitted), which we treat as "no wire name" and
// skip from the expires check.
func TestOverride_NoExpiresAt(t *testing.T) {
	typ := reflect.TypeOf(steward.Override{})
	var violations []string

	const forbiddenName = "ExpiresAt"
	const forbiddenJSON = "expires_at"

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		// Reject the Go field name `ExpiresAt` exactly.
		// We deliberately do NOT do a substring `expires`
		// match (a future legitimate field like
		// `RuleExpiryPolicy` should not false-trip).
		if f.Name == forbiddenName {
			violations = append(violations,
				"Go field `"+f.Name+"` is the canonical anti-name; the v1 Override is append-only and carries no TTL (architecture Sec 5.3.6)")
		}
		// Reject the JSON tag `expires_at` exactly (first
		// comma-separated segment).
		jsonTag := f.Tag.Get("json")
		wireName, _, _ := strings.Cut(jsonTag, ",")
		if wireName == forbiddenJSON {
			violations = append(violations,
				"Go field `"+f.Name+"` carries JSON tag `expires_at`; the v1 Override wire shape has no TTL (architecture Sec 5.3.6, iter-1 evaluator item 9)")
		}
		// Reject DB column tags too if any future migration
		// adds them. The current Override struct does NOT
		// use a `db:"..."` tag (it persists via positional
		// args in `sql_store.go`), but a defensive check is
		// cheap and pins the contract end-to-end.
		dbTag := f.Tag.Get("db")
		if dbTag == forbiddenJSON {
			violations = append(violations,
				"Go field `"+f.Name+"` carries DB tag `expires_at`; the v1 Override SQL shape has no TTL column (architecture Sec 5.3.6)")
		}
	}
	if len(violations) > 0 {
		t.Fatalf(
			"steward.Override has drifted from the append-only-no-TTL contract:\n  - %s\n\n"+
				"The v1 Override row is append-only -- to un-mute a rule, append a fresh row with `mute=false` (architecture Sec 5.3.6, `mgmt.override` verb in Sec 6.3). A TTL would silently un-mute an operator-authorised override when the clock advanced, breaking the audit trail. To fix, REMOVE the field; if the operator workflow genuinely needs a TTL, surface it via an architecture change AND a co-edit to this test.",
			strings.Join(violations, "\n  - "),
		)
	}
}

// stringifyScanStatuses converts a slice of [repo_indexer.ScanStatus]
// to []string for use in DeepEqual diagnostics. The
// underlying [repo_indexer.ScanStatus] type is a string
// alias so the conversion is byte-equal.
func stringifyScanStatuses(in []repo_indexer.ScanStatus) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

// stringifyScanRunStatuses converts a slice of [metric_ingestor.ScanRunStatus]
// to []string for use in DeepEqual diagnostics.
func stringifyScanRunStatuses(in []metric_ingestor.ScanRunStatus) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

// stringifyVerdicts converts a slice of [domain.Verdict]
// to []string for use in DeepEqual diagnostics.
func stringifyVerdicts(in []domain.Verdict) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}
