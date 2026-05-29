package metric_ingestor_test

import (
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
	"forge/services/clean-code/internal/metrics/recipes"
)

var canonicalSignatureTestRepoID = uuid.Must(uuid.FromString("aaaaaaaa-bbbb-cccc-dddd-eeeeffff0042"))

// TestBuildCanonicalSignatureForRef_MethodIncludesPath pins
// the iter-4 evaluator item 1 fix: the canonical signature
// for a method MUST embed the file path so two methods with
// the SAME qualified name in DIFFERENT files produce
// DIFFERENT signatures (the iter-3 resolver passed the
// qualified name raw, which collided).
func TestBuildCanonicalSignatureForRef_MethodIncludesPath(t *testing.T) {
	t.Parallel()
	refA := recipes.ScopeRef{Kind: "method", QualifiedName: "pkg.Foo", Path: "a/foo.go"}
	refB := recipes.ScopeRef{Kind: "method", QualifiedName: "pkg.Foo", Path: "b/foo.go"}

	sigA, err := metric_ingestor.BuildCanonicalSignatureForRef(canonicalSignatureTestRepoID, refA)
	if err != nil {
		t.Fatalf("BuildCanonicalSignatureForRef(method,a): err=%v", err)
	}
	sigB, err := metric_ingestor.BuildCanonicalSignatureForRef(canonicalSignatureTestRepoID, refB)
	if err != nil {
		t.Fatalf("BuildCanonicalSignatureForRef(method,b): err=%v", err)
	}
	if sigA == sigB {
		t.Errorf("signatures collided for SAME QN in DIFFERENT files: %q -- iter-4 item 1 must include path in signature", sigA)
	}
	if !strings.Contains(sigA, "a/foo.go") {
		t.Errorf("sigA = %q, want substring %q", sigA, "a/foo.go")
	}
	if !strings.Contains(sigA, "::method::") {
		t.Errorf("sigA = %q, want kind-discriminator %q (per scope.BuildMethod)", sigA, "::method::")
	}
}

// TestBuildCanonicalSignatureForRef_FileShape pins the file-
// kind signature is built via scope.BuildFile -> the result
// has the `::file::` discriminator and embeds the path.
func TestBuildCanonicalSignatureForRef_FileShape(t *testing.T) {
	t.Parallel()
	ref := recipes.ScopeRef{Kind: "file", QualifiedName: "pkg/x.go", Path: "pkg/x.go"}
	sig, err := metric_ingestor.BuildCanonicalSignatureForRef(canonicalSignatureTestRepoID, ref)
	if err != nil {
		t.Fatalf("BuildCanonicalSignatureForRef(file): err=%v", err)
	}
	if !strings.Contains(sig, "::file::") {
		t.Errorf("sig = %q, want substring %q (per scope.BuildFile)", sig, "::file::")
	}
	if !strings.Contains(sig, "pkg/x.go") {
		t.Errorf("sig = %q, want substring %q", sig, "pkg/x.go")
	}
}

// TestBuildCanonicalSignatureForRef_StableAcrossRepoIDOnly
// pins per-repo-stable property: two refs with identical
// per-kind fields but different RepoIDs produce different
// signatures (the synthetic repo stamp depends on repoID).
func TestBuildCanonicalSignatureForRef_StableAcrossRepoIDOnly(t *testing.T) {
	t.Parallel()
	r1 := uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111"))
	r2 := uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222"))
	ref := recipes.ScopeRef{Kind: "method", QualifiedName: "pkg.Foo", Path: "pkg/foo.go"}

	s1, err := metric_ingestor.BuildCanonicalSignatureForRef(r1, ref)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	s2, err := metric_ingestor.BuildCanonicalSignatureForRef(r2, ref)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if s1 == s2 {
		t.Errorf("signatures across distinct repoIDs collided: %q", s1)
	}
	if !strings.Contains(s1, r1.String()) {
		t.Errorf("s1 = %q, want substring %q", s1, r1.String())
	}
}

// TestBuildCanonicalSignatureForRef_RejectsZeroRepoID pins
// the zero-UUID guard.
func TestBuildCanonicalSignatureForRef_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	ref := recipes.ScopeRef{Kind: "method", QualifiedName: "pkg.Foo", Path: "pkg/foo.go"}
	if _, err := metric_ingestor.BuildCanonicalSignatureForRef(uuid.Nil, ref); err == nil {
		t.Error("BuildCanonicalSignatureForRef(uuid.Nil, ...): err=nil, want non-nil")
	}
}

// TestBuildCanonicalSignatureForRef_RejectsUnknownKind pins
// the closed-set guard: a kind outside the canonical seven-
// value set is rejected without minting a colliding sig.
func TestBuildCanonicalSignatureForRef_RejectsUnknownKind(t *testing.T) {
	t.Parallel()
	ref := recipes.ScopeRef{Kind: "garbage", QualifiedName: "pkg.Foo", Path: "pkg/foo.go"}
	if _, err := metric_ingestor.BuildCanonicalSignatureForRef(canonicalSignatureTestRepoID, ref); err == nil {
		t.Error("BuildCanonicalSignatureForRef(unknown kind): err=nil, want non-nil")
	}
}

// TestBuildCanonicalSignatureForRef_BlockKindUnsupported
// pins the explicit "not yet supported" surface for block
// kinds: recipes.ScopeRef does not carry the enclosing
// method signature today, so the helper refuses rather than
// minting a colliding sig.
func TestBuildCanonicalSignatureForRef_BlockKindUnsupported(t *testing.T) {
	t.Parallel()
	ref := recipes.ScopeRef{Kind: "block", QualifiedName: "blk0", Path: "pkg/foo.go"}
	if _, err := metric_ingestor.BuildCanonicalSignatureForRef(canonicalSignatureTestRepoID, ref); err == nil {
		t.Error("BuildCanonicalSignatureForRef(block kind): err=nil, want non-nil (block requires enclosing method sig)")
	}
}

// TestBuildCanonicalSignatureForRef_RepoKindNoPath pins
// that the repo-kind signature does NOT require Path or
// QualifiedName -- the synthetic stamp alone disambiguates.
func TestBuildCanonicalSignatureForRef_RepoKindNoPath(t *testing.T) {
	t.Parallel()
	ref := recipes.ScopeRef{Kind: "repo"}
	sig, err := metric_ingestor.BuildCanonicalSignatureForRef(canonicalSignatureTestRepoID, ref)
	if err != nil {
		t.Fatalf("BuildCanonicalSignatureForRef(repo, no path): err=%v", err)
	}
	if !strings.Contains(sig, canonicalSignatureTestRepoID.String()) {
		t.Errorf("sig = %q, want substring %q", sig, canonicalSignatureTestRepoID.String())
	}
}

// TestBuildCanonicalSignatureForRefURL_MethodOverloadsDistinct
// pins the iter-5 evaluator item 3 fix: two methods with
// the SAME (path, qualifiedName) but DIFFERENT parameter
// lists MUST produce DIFFERENT canonical signatures. The
// iter-4 resolver called `scope.BuildMethod(..., nil)`,
// which collided overloaded methods into a single signature.
func TestBuildCanonicalSignatureForRefURL_MethodOverloadsDistinct(t *testing.T) {
	t.Parallel()
	repoURL := "https://example.com/org/repo"
	refNoArgs := recipes.ScopeRef{
		Kind: "method", QualifiedName: "pkg.Foo", Path: "foo.go",
		Params: nil,
	}
	refOneArg := recipes.ScopeRef{
		Kind: "method", QualifiedName: "pkg.Foo", Path: "foo.go",
		Params: []string{"int"},
	}
	refTwoArgs := recipes.ScopeRef{
		Kind: "method", QualifiedName: "pkg.Foo", Path: "foo.go",
		Params: []string{"int", "string"},
	}
	sig0, err := metric_ingestor.BuildCanonicalSignatureForRefURL(repoURL, refNoArgs)
	if err != nil {
		t.Fatalf("sig0: err=%v", err)
	}
	sig1, err := metric_ingestor.BuildCanonicalSignatureForRefURL(repoURL, refOneArg)
	if err != nil {
		t.Fatalf("sig1: err=%v", err)
	}
	sig2, err := metric_ingestor.BuildCanonicalSignatureForRefURL(repoURL, refTwoArgs)
	if err != nil {
		t.Fatalf("sig2: err=%v", err)
	}
	if sig0 == sig1 || sig0 == sig2 || sig1 == sig2 {
		t.Errorf(
			"overloaded methods collided: noArgs=%q, 1arg=%q, 2args=%q -- iter-5 item 3 requires Params to disambiguate",
			sig0, sig1, sig2,
		)
	}
	// Every signature must still embed the path + qualified
	// name so the iter-4 path-aware fix is preserved.
	for _, sig := range []string{sig0, sig1, sig2} {
		if !strings.Contains(sig, "foo.go") {
			t.Errorf("sig %q missing path", sig)
		}
		if !strings.Contains(sig, "::method::") {
			t.Errorf("sig %q missing ::method:: discriminator", sig)
		}
	}
}

// TestBuildCanonicalSignatureForRefURL_RealURLDiffersFromSynthetic
// pins the iter-5 evaluator item 2 fix: when a real repo
// URL is supplied (via [RepoURLLookup]), the canonical
// signature MUST differ from the synthetic
// `clean-code-repo:<repoID>` fallback. This proves
// signatures are stable across logical repo re-creates
// (real URL is persistent; the synthetic stamp is not).
func TestBuildCanonicalSignatureForRefURL_RealURLDiffersFromSynthetic(t *testing.T) {
	t.Parallel()
	ref := recipes.ScopeRef{Kind: "method", QualifiedName: "pkg.Foo", Path: "foo.go"}
	synthetic, err := metric_ingestor.BuildCanonicalSignatureForRef(canonicalSignatureTestRepoID, ref)
	if err != nil {
		t.Fatalf("synthetic: err=%v", err)
	}
	real, err := metric_ingestor.BuildCanonicalSignatureForRefURL("https://example.com/org/repo", ref)
	if err != nil {
		t.Fatalf("real: err=%v", err)
	}
	if synthetic == real {
		t.Errorf("synthetic and real-URL signatures collided: %q -- the iter-5 item 2 fix requires the real URL to flow into BuildMethod", synthetic)
	}
	if !strings.Contains(synthetic, "clean-code-repo:") {
		t.Errorf("synthetic = %q, want substring %q", synthetic, "clean-code-repo:")
	}
	if strings.Contains(real, "clean-code-repo:") {
		t.Errorf("real = %q, must NOT embed synthetic stamp", real)
	}
}
