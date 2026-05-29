package repo_indexer_test

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/repo_indexer"
)

// TestAllScanStatuses_ExactlyCanonicalSet pins the closed-set
// invariant for [repo_indexer.ScanStatus]. The Stage 3.1 test
// scenario `commit-states-only-canonical` requires that the
// enum carries EXACTLY `{pending, scanning, scanned, failed}`
// with no `complete`, `superseded`, `orphaned`, or `queued`
// member.
//
// The implementation-plan scenario text reads
// `reflect.TypeOf(ScanStatus(0)).String()` -- that idiom does
// not enumerate enum values in Go (it returns the type name);
// the intent is clearly "no non-canonical values exist", which
// we exercise here via [AllScanStatuses].
func TestAllScanStatuses_ExactlyCanonicalSet(t *testing.T) {
	got := repo_indexer.AllScanStatuses()

	want := []repo_indexer.ScanStatus{
		repo_indexer.ScanStatusPending,
		repo_indexer.ScanStatusScanning,
		repo_indexer.ScanStatusScanned,
		repo_indexer.ScanStatusFailed,
	}

	if len(got) != len(want) {
		t.Fatalf("AllScanStatuses returned %d values; want %d", len(got), len(want))
	}

	gotStr := make([]string, len(got))
	wantStr := make([]string, len(want))
	for i := range got {
		gotStr[i] = string(got[i])
		wantStr[i] = string(want[i])
	}
	sort.Strings(gotStr)
	sort.Strings(wantStr)
	if strings.Join(gotStr, ",") != strings.Join(wantStr, ",") {
		t.Fatalf("AllScanStatuses set mismatch:\n got=%v\nwant=%v", gotStr, wantStr)
	}

	// Forbidden literals MUST NOT appear under any name.
	forbidden := []string{"complete", "superseded", "orphaned", "queued"}
	for _, bad := range forbidden {
		for _, s := range got {
			if string(s) == bad {
				t.Fatalf("AllScanStatuses unexpectedly contains forbidden value %q", bad)
			}
		}
	}
}

// TestAllScanStatuses_ReturnsFreshCopy ensures a caller
// mutating the returned slice cannot leak back into the
// package's closed-set guard.
func TestAllScanStatuses_ReturnsFreshCopy(t *testing.T) {
	a := repo_indexer.AllScanStatuses()
	b := repo_indexer.AllScanStatuses()
	if &a[0] == &b[0] {
		t.Fatal("AllScanStatuses returned aliased slice; expected fresh copy each call")
	}
	a[0] = repo_indexer.ScanStatus("MUTATED")
	if b[0] != repo_indexer.ScanStatusPending {
		t.Fatalf("mutation of a leaked into b: b[0]=%q want %q", b[0], repo_indexer.ScanStatusPending)
	}
	// And a third fresh call must still observe the canonical value.
	c := repo_indexer.AllScanStatuses()
	if c[0] != repo_indexer.ScanStatusPending {
		t.Fatalf("mutation of a leaked into package state: c[0]=%q want %q", c[0], repo_indexer.ScanStatusPending)
	}
}

// TestScanStatus_String pins the wire literal each constant
// resolves to.
func TestScanStatus_String(t *testing.T) {
	cases := []struct {
		s    repo_indexer.ScanStatus
		want string
	}{
		{repo_indexer.ScanStatusPending, "pending"},
		{repo_indexer.ScanStatusScanning, "scanning"},
		{repo_indexer.ScanStatusScanned, "scanned"},
		{repo_indexer.ScanStatusFailed, "failed"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("ScanStatus(%q).String() = %q; want %q", tc.s, got, tc.want)
		}
	}
}

// TestScanStatus_Validate exercises both the accept path
// (every canonical value validates) and the reject path
// (non-canonical literals fail with a wrapped
// [ErrInvalidScanStatus]).
func TestScanStatus_Validate(t *testing.T) {
	for _, s := range repo_indexer.AllScanStatuses() {
		if err := s.Validate(); err != nil {
			t.Errorf("%q.Validate() returned %v; want nil", s, err)
		}
	}

	reject := []string{"", "complete", "superseded", "orphaned", "queued", "PENDING", "scanning ", "Scanning"}
	for _, lit := range reject {
		s := repo_indexer.ScanStatus(lit)
		err := s.Validate()
		if err == nil {
			t.Errorf("ScanStatus(%q).Validate() unexpectedly succeeded", lit)
			continue
		}
		if !errors.Is(err, repo_indexer.ErrInvalidScanStatus) {
			t.Errorf("ScanStatus(%q).Validate() returned %v; want ErrInvalidScanStatus", lit, err)
		}
	}
}

// TestScanStatus_IsTerminal pins the two terminal labels.
func TestScanStatus_IsTerminal(t *testing.T) {
	terminal := map[repo_indexer.ScanStatus]bool{
		repo_indexer.ScanStatusPending:  false,
		repo_indexer.ScanStatusScanning: false,
		repo_indexer.ScanStatusScanned:  true,
		repo_indexer.ScanStatusFailed:   true,
	}
	for s, want := range terminal {
		if got := s.IsTerminal(); got != want {
			t.Errorf("%q.IsTerminal() = %v; want %v", s, got, want)
		}
	}
}

// TestCanTransition_CanonicalDiagram pins the canonical
// transition diagram from the Stage 3.1 brief:
//
//	pending  -> scanning
//	scanning -> scanned
//	scanning -> failed
//
// All other (from, to) pairs return false. Tested
// exhaustively across the cross-product so a future edit
// adding a forbidden edge surfaces here.
func TestCanTransition_CanonicalDiagram(t *testing.T) {
	allowed := map[[2]repo_indexer.ScanStatus]bool{
		{repo_indexer.ScanStatusPending, repo_indexer.ScanStatusScanning}: true,
		{repo_indexer.ScanStatusScanning, repo_indexer.ScanStatusScanned}: true,
		{repo_indexer.ScanStatusScanning, repo_indexer.ScanStatusFailed}:  true,
	}

	all := repo_indexer.AllScanStatuses()
	for _, from := range all {
		for _, to := range all {
			pair := [2]repo_indexer.ScanStatus{from, to}
			want := allowed[pair]
			got := repo_indexer.CanTransition(from, to)
			if got != want {
				t.Errorf("CanTransition(%q, %q) = %v; want %v", from, to, got, want)
			}
		}
	}
}

// TestCanTransition_RejectsTerminalAndSelfEdges pins three
// failure modes that have caused real-world bugs in similar
// pipelines:
//   - terminal -> any  (a "rescan" that mutates a scanned row
//     would orphan the previous metric_sample set).
//   - self-edge      (an UPDATE that does not change the value
//     is a writer bug, not a no-op).
//   - pending -> terminal without visiting `scanning` (drops
//     the in-flight observability window).
func TestCanTransition_RejectsTerminalAndSelfEdges(t *testing.T) {
	bad := [][2]repo_indexer.ScanStatus{
		// terminal -> any
		{repo_indexer.ScanStatusScanned, repo_indexer.ScanStatusPending},
		{repo_indexer.ScanStatusScanned, repo_indexer.ScanStatusScanning},
		{repo_indexer.ScanStatusScanned, repo_indexer.ScanStatusFailed},
		{repo_indexer.ScanStatusFailed, repo_indexer.ScanStatusPending},
		{repo_indexer.ScanStatusFailed, repo_indexer.ScanStatusScanning},
		{repo_indexer.ScanStatusFailed, repo_indexer.ScanStatusScanned},
		// self-edges
		{repo_indexer.ScanStatusPending, repo_indexer.ScanStatusPending},
		{repo_indexer.ScanStatusScanning, repo_indexer.ScanStatusScanning},
		{repo_indexer.ScanStatusScanned, repo_indexer.ScanStatusScanned},
		{repo_indexer.ScanStatusFailed, repo_indexer.ScanStatusFailed},
		// pending -> terminal (skips the in-flight state)
		{repo_indexer.ScanStatusPending, repo_indexer.ScanStatusScanned},
		{repo_indexer.ScanStatusPending, repo_indexer.ScanStatusFailed},
	}
	for _, p := range bad {
		if repo_indexer.CanTransition(p[0], p[1]) {
			t.Errorf("CanTransition(%q, %q) returned true; want false (off-diagram edge)", p[0], p[1])
		}
	}
}

// TestValidateTransition_RejectsUnknownInputs covers the
// pre-membership check: a non-canonical `from` or `to`
// surfaces as [ErrInvalidScanStatus], NOT as a transition
// error.
func TestValidateTransition_RejectsUnknownInputs(t *testing.T) {
	bad := repo_indexer.ScanStatus("complete")

	if err := repo_indexer.ValidateTransition(bad, repo_indexer.ScanStatusScanning); !errors.Is(err, repo_indexer.ErrInvalidScanStatus) {
		t.Errorf("ValidateTransition(bad-from): err=%v; want ErrInvalidScanStatus", err)
	}
	if err := repo_indexer.ValidateTransition(repo_indexer.ScanStatusPending, bad); !errors.Is(err, repo_indexer.ErrInvalidScanStatus) {
		t.Errorf("ValidateTransition(bad-to): err=%v; want ErrInvalidScanStatus", err)
	}
}

// TestValidateTransition_RejectsOffDiagramEdges pins the
// canonical-edge guard: an unknown `(from, to)` pair where
// BOTH values are canonical returns
// [ErrInvalidScanStatusTransition].
func TestValidateTransition_RejectsOffDiagramEdges(t *testing.T) {
	err := repo_indexer.ValidateTransition(repo_indexer.ScanStatusPending, repo_indexer.ScanStatusScanned)
	if !errors.Is(err, repo_indexer.ErrInvalidScanStatusTransition) {
		t.Errorf("ValidateTransition(pending->scanned): err=%v; want ErrInvalidScanStatusTransition", err)
	}
}

// TestValidateTransition_AcceptsCanonicalEdges pins the
// three accept paths.
func TestValidateTransition_AcceptsCanonicalEdges(t *testing.T) {
	good := [][2]repo_indexer.ScanStatus{
		{repo_indexer.ScanStatusPending, repo_indexer.ScanStatusScanning},
		{repo_indexer.ScanStatusScanning, repo_indexer.ScanStatusScanned},
		{repo_indexer.ScanStatusScanning, repo_indexer.ScanStatusFailed},
	}
	for _, p := range good {
		if err := repo_indexer.ValidateTransition(p[0], p[1]); err != nil {
			t.Errorf("ValidateTransition(%q, %q) = %v; want nil", p[0], p[1], err)
		}
	}
}
