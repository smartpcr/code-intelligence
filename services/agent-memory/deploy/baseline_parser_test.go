package deploy

// Iter-5 evaluator finding #2: focused unit tests for
// parseGoTestJSONFailures, the helper extracted from
// TestBaselineFailuresAreOnlyDocumentedOnes. These tests
// exercise the parser against SYNTHETIC `go test -json`
// output so we can prove the iter-5 fix actually catches
// package-level (compile/build) failures that the iter-4
// version silently ignored.

import (
	"strings"
	"testing"
)

// TestParseGoTestJSONFailures_DetectsPerTestFailures
// asserts the parser still extracts per-test FAIL events
// the way it did in iter-4. Regression guard for the
// hardening change.
func TestParseGoTestJSONFailures_DetectsPerTestFailures(t *testing.T) {
	t.Parallel()

	// A typical run with one passing test and one failing
	// test in two different packages.
	input := strings.Join([]string{
		`{"Time":"2024-01-01T00:00:00Z","Action":"run","Package":"example.com/p1","Test":"TestA"}`,
		`{"Time":"2024-01-01T00:00:00Z","Action":"fail","Package":"example.com/p1","Test":"TestA","Elapsed":0.01}`,
		`{"Time":"2024-01-01T00:00:00Z","Action":"fail","Package":"example.com/p1","Elapsed":0.02}`,
		`{"Time":"2024-01-01T00:00:00Z","Action":"run","Package":"example.com/p2","Test":"TestB"}`,
		`{"Time":"2024-01-01T00:00:00Z","Action":"pass","Package":"example.com/p2","Test":"TestB","Elapsed":0.01}`,
		`{"Time":"2024-01-01T00:00:00Z","Action":"pass","Package":"example.com/p2","Elapsed":0.02}`,
	}, "\n")

	perTest, pkgOnly := parseGoTestJSONFailures(input)
	if got := len(perTest); got != 1 {
		t.Fatalf("expected 1 package with per-test failures; got %d: %v", got, perTest)
	}
	if !perTest["example.com/p1"]["TestA"] {
		t.Errorf("expected TestA failure in p1; got %v", perTest["example.com/p1"])
	}
	// The package-level fail event for p1 is correctly
	// emitted by go test (because TestA failed). Our
	// parser captures it -- the outer test then
	// suppresses the report because perTest also has p1.
	if !pkgOnly["example.com/p1"] {
		t.Errorf("expected p1 in package-level fails (test failure cascades); got %v", pkgOnly)
	}
	// p2 had no failures, so it should NOT appear.
	if pkgOnly["example.com/p2"] {
		t.Errorf("p2 had no failures but appears in pkgOnly: %v", pkgOnly)
	}
}

// TestParseGoTestJSONFailures_DetectsCompileFailures is the
// iter-5 finding #2 regression test: a synthetic run where
// `example.com/broken` failed to COMPILE. The output for
// such a run has:
//
//   - NO per-test events (no tests ran)
//   - A SINGLE package-level FAIL event with NO Test
//     field
//   - Optional stderr output preceding it.
//
// The iter-4 parser would have skipped this entirely
// because it required a `"Test":"..."` field. The iter-5
// parser MUST surface it via the actualPackageOnlyFailures
// return value.
func TestParseGoTestJSONFailures_DetectsCompileFailures(t *testing.T) {
	t.Parallel()

	// What go test -json emits for a package whose Go
	// source has a syntax error. The "Output" events
	// carry the compiler diagnostic, then a single
	// terminal "fail" event WITHOUT a Test field.
	input := strings.Join([]string{
		`{"Time":"2024-01-01T00:00:00Z","Action":"start","Package":"example.com/broken"}`,
		`{"Time":"2024-01-01T00:00:00Z","Action":"output","Package":"example.com/broken","Output":"FAIL\texample.com/broken [build failed]\n"}`,
		`{"Time":"2024-01-01T00:00:00Z","Action":"fail","Package":"example.com/broken","Elapsed":0}`,
	}, "\n")

	perTest, pkgOnly := parseGoTestJSONFailures(input)
	if got := len(perTest); got != 0 {
		t.Fatalf("expected 0 per-test failures (no tests ran); got %d: %v", got, perTest)
	}
	if !pkgOnly["example.com/broken"] {
		t.Fatalf(
			"ITER-5 FINDING #2 REGRESSION: parser failed to detect package-level FAIL for example.com/broken.\n"+
				"This is exactly the scenario the iter-4 parser silently ignored.\n"+
				"perTest=%v, pkgOnly=%v",
			perTest, pkgOnly,
		)
	}
}

// TestParseGoTestJSONFailures_DistinguishesCompileFromTestFailures
// asserts the parser correctly classifies a mixed run: one
// package with per-test failures (where the package-level
// fail is a cascade, NOT a new signal) and one package
// with a build failure (the new signal iter-5 must catch).
//
// The downstream consumer in
// TestBaselineFailuresAreOnlyDocumentedOnes uses this
// distinction to suppress the package-level report when
// per-test failures are already attributed.
func TestParseGoTestJSONFailures_DistinguishesCompileFromTestFailures(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		// Package with per-test failure -- the
		// package-level fail is a CASCADE, not new info.
		`{"Action":"fail","Package":"example.com/cascade","Test":"TestX","Elapsed":0.01}`,
		`{"Action":"fail","Package":"example.com/cascade","Elapsed":0.01}`,
		// Package with build failure -- NEW SIGNAL the
		// iter-4 parser missed.
		`{"Action":"fail","Package":"example.com/buildbreak","Elapsed":0}`,
	}, "\n")

	perTest, pkgOnly := parseGoTestJSONFailures(input)
	if !perTest["example.com/cascade"]["TestX"] {
		t.Errorf("cascade package: expected TestX in perTest; got %v", perTest)
	}
	if !pkgOnly["example.com/cascade"] {
		t.Errorf("cascade package: expected entry in pkgOnly; got %v", pkgOnly)
	}
	if !pkgOnly["example.com/buildbreak"] {
		t.Errorf("BUILD FAILURE NOT DETECTED: buildbreak missing from pkgOnly: %v", pkgOnly)
	}
	if _, present := perTest["example.com/buildbreak"]; present {
		t.Errorf("buildbreak had no per-test events; should not appear in perTest: %v", perTest)
	}

	// The classification logic in
	// TestBaselineFailuresAreOnlyDocumentedOnes derives
	// "compile-only failures" as: package-level fails MINUS
	// packages with per-test fails. Verify that math on the
	// synthetic input.
	compileOnly := map[string]bool{}
	for pkg := range pkgOnly {
		if _, hasTest := perTest[pkg]; hasTest {
			continue
		}
		compileOnly[pkg] = true
	}
	if len(compileOnly) != 1 || !compileOnly["example.com/buildbreak"] {
		t.Errorf(
			"compile-only classification wrong: expected exactly {buildbreak}, got %v",
			compileOnly,
		)
	}
}

// TestParseGoTestJSONFailures_IgnoresSubtests asserts that
// subtests (e.g. `TestX/sub_case`) are not counted as
// distinct test-function failures -- they roll up to their
// parent. This preserves the waiver list's grain: the
// .baseline-test-failures.md doc lists parent test
// functions, not every subtest.
func TestParseGoTestJSONFailures_IgnoresSubtests(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		`{"Action":"fail","Package":"example.com/p","Test":"TestX","Elapsed":0.01}`,
		`{"Action":"fail","Package":"example.com/p","Test":"TestX/sub_a","Elapsed":0.01}`,
		`{"Action":"fail","Package":"example.com/p","Test":"TestX/sub_b","Elapsed":0.01}`,
	}, "\n")

	perTest, _ := parseGoTestJSONFailures(input)
	if !perTest["example.com/p"]["TestX"] {
		t.Errorf("expected TestX in perTest; got %v", perTest)
	}
	for name := range perTest["example.com/p"] {
		if strings.Contains(name, "/") {
			t.Errorf("subtest %q should have been filtered out", name)
		}
	}
}
