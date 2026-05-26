package main

import "testing"

// TestScanRunShaBindingForKind_CoversCanonicalEnum pins the
// iter-7-evaluator-feedback-#2 fix: the application-layer
// `scanRunShaBindingForKind` map MUST cover every canonical
// `clean_code.scan_run_kind` enum value (migration 0001 line
// 117) AND assign each kind its semantically correct
// sha_binding so the resulting INSERT honours
// `scan_run_sha_binding_consistent`. Without this pin, a
// future kind added to `validScanRunKinds` would silently
// fall through the binding switch and HTTP 500.
func TestScanRunShaBindingForKind_CoversCanonicalEnum(t *testing.T) {
	want := map[string]string{
		"full":             "single",
		"delta":            "single",
		"external_single":  "single",
		"external_per_row": "per_row",
		"retract":          "single",
	}
	if len(scanRunShaBindingForKind) != len(want) {
		t.Fatalf("scanRunShaBindingForKind has %d entries, want %d (canonical "+
			"scan_run_kind enum has 5 values per migration 0001)",
			len(scanRunShaBindingForKind), len(want))
	}
	for kind, expected := range want {
		got, ok := scanRunShaBindingForKind[kind]
		if !ok {
			t.Errorf("scanRunShaBindingForKind missing kind %q", kind)
			continue
		}
		if got != expected {
			t.Errorf("scanRunShaBindingForKind[%q] = %q, want %q", kind, got, expected)
		}
	}

	// Cross-check: every kind in `validScanRunKinds` MUST have a
	// binding mapping so the handler cannot fall through to the
	// "no sha_binding mapping" 500 branch on a valid kind.
	for kind := range validScanRunKinds {
		if _, ok := scanRunShaBindingForKind[kind]; !ok {
			t.Errorf("validScanRunKinds includes %q but scanRunShaBindingForKind does not", kind)
		}
	}
}

// TestScanRunShaBindingForKind_PerRowIsExclusive pins that
// `external_per_row` is the ONLY kind that maps to
// sha_binding='per_row'. Per the canonical scan_run sha_binding
// constraint (migration 0001 lines 351-389), per_row binding
// means each emitted MetricSample row carries its own SHA and
// scan_run.to_sha IS NULL. Any other kind mapping to per_row
// would violate the documented sha_binding semantics.
func TestScanRunShaBindingForKind_PerRowIsExclusive(t *testing.T) {
	for kind, binding := range scanRunShaBindingForKind {
		if binding == "per_row" && kind != "external_per_row" {
			t.Errorf("scanRunShaBindingForKind[%q]=per_row violates the canonical "+
				"binding semantics: only external_per_row carries its SHAs on "+
				"emitted samples; %q is single-bound", kind, kind)
		}
	}
}
