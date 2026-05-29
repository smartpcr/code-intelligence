package defects_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ingest/defects"
)

// fixedRepoID is a stable non-zero UUID for the happy-path
// tests so the validator's RepoID guard does not trip.
var fixedRepoID = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))

// validSHA returns a canonical 40-char hex SHA built from
// the repeated `c` rune. The validator's per-row SHA contract
// requires hex only.
func validSHA(c byte) string {
	return strings.Repeat(string(c), 40)
}

// goodPayload returns a Payload that passes every Validate
// gate. Tests mutate one field at a time to exercise each
// sentinel.
func goodPayload() *defects.Payload {
	return &defects.Payload{
		RepoID: fixedRepoID,
		Rows: []defects.PayloadRow{
			{
				SHA:      validSHA('a'),
				FilePath: "internal/foo.go",
				DefectID: "JIRA-1234",
				Severity: "critical",
			},
			{
				SHA:      validSHA('b'),
				FilePath: "internal/bar.go",
				DefectID: "JIRA-5678",
				Severity: "minor",
			},
		},
	}
}

// TestPayload_Validate_HappyPath pins the canonical
// happy-path: a well-formed payload returns nil.
func TestPayload_Validate_HappyPath(t *testing.T) {
	t.Parallel()
	if err := goodPayload().Validate(); err != nil {
		t.Fatalf("Validate happy payload: %v", err)
	}
}

// TestPayload_Validate_RejectsNil pins the nil-receiver
// guard. A nil [*Payload] is a programmer-error scenario
// (the decoder never produces it) but the validator should
// not segfault.
func TestPayload_Validate_RejectsNil(t *testing.T) {
	t.Parallel()
	var p *defects.Payload
	if err := p.Validate(); err == nil {
		t.Fatalf("Validate(nil): want error, got nil")
	}
}

// TestPayload_Validate_RejectsEmptyRepoID pins the
// [defects.ErrEmptyRepoID] sentinel surface.
func TestPayload_Validate_RejectsEmptyRepoID(t *testing.T) {
	t.Parallel()
	p := goodPayload()
	p.RepoID = uuid.Nil
	err := p.Validate()
	if err == nil {
		t.Fatalf("Validate(zero RepoID): want error, got nil")
	}
	if !errors.Is(err, defects.ErrEmptyRepoID) {
		t.Errorf("Validate(zero RepoID) = %v; want errors.Is ErrEmptyRepoID", err)
	}
}

// TestPayload_Validate_RejectsEmptyRows pins the
// [defects.ErrEmptyRows] sentinel surface. A payload with a
// nil or zero-length Rows slice fails validation.
func TestPayload_Validate_RejectsEmptyRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rows []defects.PayloadRow
	}{
		{"nil-slice", nil},
		{"empty-slice", []defects.PayloadRow{}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := goodPayload()
			p.Rows = tc.rows
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate(empty rows): want error, got nil")
			}
			if !errors.Is(err, defects.ErrEmptyRows) {
				t.Errorf("Validate(empty rows) = %v; want errors.Is ErrEmptyRows", err)
			}
		})
	}
}

// TestPayload_Validate_RejectsEmptySHA pins
// [defects.ErrEmptySHA] for whitespace-only and empty values.
func TestPayload_Validate_RejectsEmptySHA(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sha  string
	}{
		{"empty", ""},
		{"whitespace-only", "    "},
		{"tab-only", "\t"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := goodPayload()
			p.Rows[0].SHA = tc.sha
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate(empty SHA): want error, got nil")
			}
			if !errors.Is(err, defects.ErrEmptySHA) {
				t.Errorf("Validate(empty SHA) = %v; want errors.Is ErrEmptySHA", err)
			}
		})
	}
}

// TestPayload_Validate_RejectsInvalidSHA pins
// [defects.ErrInvalidSHA] for non-40-hex shapes that
// short-circuit the whitespace check (non-empty but malformed).
func TestPayload_Validate_RejectsInvalidSHA(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		sha  string
	}{
		{"too-short", strings.Repeat("a", 39)},
		{"too-long", strings.Repeat("a", 41)},
		{"non-hex", strings.Repeat("z", 40)},
		{"mixed-non-hex", "ggggggggggggggggggggggggggggggggggggggggg"},
		{"contains-space", "a" + strings.Repeat("a", 38) + " "},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := goodPayload()
			p.Rows[0].SHA = tc.sha
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate(invalid SHA %q): want error, got nil", tc.sha)
			}
			if !errors.Is(err, defects.ErrInvalidSHA) {
				t.Errorf("Validate(invalid SHA %q) = %v; want errors.Is ErrInvalidSHA", tc.sha, err)
			}
		})
	}
}

// TestPayload_Validate_AcceptsUppercaseHexSHA pins the
// case-insensitive SHA regex: Git emits lowercase but
// upstream consumers MAY upper-case.
func TestPayload_Validate_AcceptsUppercaseHexSHA(t *testing.T) {
	t.Parallel()
	p := goodPayload()
	p.Rows[0].SHA = strings.ToUpper(validSHA('a'))
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate(uppercase hex): %v", err)
	}
}

// TestPayload_Validate_RejectsEmptyFilePath pins
// [defects.ErrEmptyFilePath].
func TestPayload_Validate_RejectsEmptyFilePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  string
	}{
		{"empty", ""},
		{"whitespace", "   "},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := goodPayload()
			p.Rows[0].FilePath = tc.val
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate(empty FilePath): want error, got nil")
			}
			if !errors.Is(err, defects.ErrEmptyFilePath) {
				t.Errorf("Validate(empty FilePath) = %v; want errors.Is ErrEmptyFilePath", err)
			}
		})
	}
}

// TestPayload_Validate_RejectsEmptyDefectID pins
// [defects.ErrEmptyDefectID].
func TestPayload_Validate_RejectsEmptyDefectID(t *testing.T) {
	t.Parallel()
	p := goodPayload()
	p.Rows[0].DefectID = ""
	err := p.Validate()
	if err == nil {
		t.Fatalf("Validate(empty DefectID): want error, got nil")
	}
	if !errors.Is(err, defects.ErrEmptyDefectID) {
		t.Errorf("Validate(empty DefectID) = %v; want errors.Is ErrEmptyDefectID", err)
	}
}

// TestPayload_Validate_RejectsEmptySeverity pins
// [defects.ErrEmptySeverity]. Note v1 does not pin a closed
// severity enum (tracker-specific), so this test only
// asserts the non-emptiness gate.
func TestPayload_Validate_RejectsEmptySeverity(t *testing.T) {
	t.Parallel()
	p := goodPayload()
	p.Rows[0].Severity = ""
	err := p.Validate()
	if err == nil {
		t.Fatalf("Validate(empty Severity): want error, got nil")
	}
	if !errors.Is(err, defects.ErrEmptySeverity) {
		t.Errorf("Validate(empty Severity) = %v; want errors.Is ErrEmptySeverity", err)
	}
}

// TestPayload_Validate_AcceptsArbitrarySeverityLiteral pins
// the v1 contract that severity is opaque to the validator.
// Tracker-specific values (`S0`, `blocker`, `info`) all
// pass; v2 will canonicalise.
func TestPayload_Validate_AcceptsArbitrarySeverityLiteral(t *testing.T) {
	t.Parallel()
	for _, sev := range []string{"S0", "blocker", "critical", "major", "minor", "info", "low"} {
		sev := sev
		t.Run(sev, func(t *testing.T) {
			t.Parallel()
			p := goodPayload()
			p.Rows[0].Severity = sev
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate(severity=%q): %v", sev, err)
			}
		})
	}
}

// TestPayload_Validate_RowErrorReportsIndex pins the
// `rows[i]: %w` wrap from the index-aware error message so an
// operator looking at the 400 body can locate the offending
// row without re-running the publisher.
func TestPayload_Validate_RowErrorReportsIndex(t *testing.T) {
	t.Parallel()
	p := goodPayload()
	p.Rows[1].DefectID = "" // bad row at index 1
	err := p.Validate()
	if err == nil {
		t.Fatalf("Validate(bad row 1): want error, got nil")
	}
	if !strings.Contains(err.Error(), "rows[1]") {
		t.Errorf("Validate error %q does not name the offending row index", err.Error())
	}
	if !errors.Is(err, defects.ErrEmptyDefectID) {
		t.Errorf("Validate(bad row 1) does not unwrap to ErrEmptyDefectID: %v", err)
	}
}

// TestScanRunKindExternalPerRow_Literal pins the canon-guard
// constant. The literal MUST be string-equal to
// `external_per_row` so the closed-set assertion in
// `webhook.canonicalScanRunKindForVerb` accepts the verb's
// registration.
func TestScanRunKindExternalPerRow_Literal(t *testing.T) {
	t.Parallel()
	if defects.ScanRunKindExternalPerRow != "external_per_row" {
		t.Errorf("ScanRunKindExternalPerRow = %q; want %q",
			defects.ScanRunKindExternalPerRow, "external_per_row")
	}
}
