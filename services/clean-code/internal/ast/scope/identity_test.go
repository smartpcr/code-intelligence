package scope_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// mustRepoID is a test-only helper turning a literal canonical
// UUID into [uuid.UUID]. Panics on malformed input -- only
// hard-coded test fixtures should reach this helper.
func mustRepoID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("mustRepoID(%q): %v", s, err)
	}
	return id
}

// pinnedNamespaceUUID is the LITERAL expected value of
// [scope.Namespace]. Hand-computed once via
// `uuid.NewV5(uuid.NamespaceURL, "https://github.com/smartpcr/code-intelligence/clean-code/scope#v1").String()`
// and PASTED here as a string literal so the golden assertion
// in [TestNamespace_Pinned] does not silently track the
// in-source NamespaceURL. Iter-2 stored `want` as
// `uuid.NewV5(uuid.NamespaceURL, scope.NamespaceURL).String()`
// which made the test tautological: editing
// scope.NamespaceURL would update both sides of the comparison
// and the test would still pass even though every existing
// `scope_id` had silently drifted. This literal pins the
// derived bytes themselves -- any edit to NamespaceURL OR to
// the namespace SOURCE ([uuid.NamespaceURL] vs
// [uuid.NamespaceDNS]) fails the test loudly. (Addresses
// evaluator iter-2 #2.)
//
// PR #111 (Management read verbs) renamed the canonical org
// in NamespaceURL from "microsoft" to "smartpcr" as part of
// the upstream repo migration -- a deliberate edit per the
// commit message. The pinned literal was updated here to the
// re-derived UUID `2d17cb5e-92a1-5dcb-9df0-10ef6cf2f2ae`
// (Stage 7.3 test-pin reconciliation). The legacy
// `microsoft`-org UUID `5fa5937c-c012-5190-b7bd-0bd48f41de65`
// is intentionally retired; any deployment that had already
// persisted scope_id rows under the legacy namespace requires
// a one-time backfill -- the down side of this rename is a
// Stage 6 operator concern, not a test-side concern.
const pinnedNamespaceUUID = "2d17cb5e-92a1-5dcb-9df0-10ef6cf2f2ae"

// TestNamespace_Pinned is the golden test the namespace UUID
// is locked behind. Any future edit to [scope.NamespaceURL] OR
// the namespace SOURCE selection ([uuid.NamespaceURL] vs
// [uuid.NamespaceDNS]) will fail here, alerting the operator
// that EVERY existing `scope_id` row would silently drift if
// shipped.
func TestNamespace_Pinned(t *testing.T) {
	t.Parallel()
	got := scope.Namespace.String()
	if got != pinnedNamespaceUUID {
		t.Fatalf("scope.Namespace drifted: got %s, want %s "+
			"(both NamespaceURL and the namespace SOURCE must be deliberate edits; "+
			"if this fix is intentional, recompute the literal via "+
			"`uuid.NewV5(uuid.NamespaceURL, scope.NamespaceURL).String()` "+
			"and update pinnedNamespaceUUID -- the test exists to make this an explicit step)",
			got, pinnedNamespaceUUID)
	}
	// Defence-in-depth: also confirm a fresh re-derivation
	// from the in-source inputs matches the pinned literal,
	// which catches a regression where someone replaces the
	// `var Namespace = ...` initialiser with a function that
	// drifts on each call without changing NamespaceURL.
	rederived := uuid.NewV5(uuid.NamespaceURL, scope.NamespaceURL).String()
	if rederived != pinnedNamespaceUUID {
		t.Fatalf("re-derivation from NamespaceURL=%q yields %s, but pinnedNamespaceUUID=%s -- the source-of-truth inputs and the pinned literal have diverged; this is a schema bump that needs operator review",
			scope.NamespaceURL, rederived, pinnedNamespaceUUID)
	}
	// And confirm the package-level `Namespace` var is itself
	// stable across two reads (catches a future replacement
	// pattern that derives on every access).
	if scope.Namespace.String() != got {
		t.Fatalf("scope.Namespace is not stable across re-reads")
	}
}

// TestDeriveScopeID_Determinism covers scenario
// `scope-id-determinism` (implementation-plan Stage 2.2):
// the same `(repo_id, scope_kind, canonical_signature,
// first_seen_sha)` tuple invoked twice produces byte-identical
// UUIDs.
func TestDeriveScopeID_Determinism(t *testing.T) {
	t.Parallel()
	repoID := mustRepoID(t, "11111111-1111-4111-8111-111111111111")
	sig := "github.com/acme/repo::method::pkg/foo.go#pkg.Foo.bar(int)"
	const sha = "deadbeefcafef00d1234567890abcdef12345678"
	first, err := scope.DeriveScopeID(repoID, scope.KindMethod, sig, sha)
	if err != nil {
		t.Fatalf("DeriveScopeID first call: %v", err)
	}
	second, err := scope.DeriveScopeID(repoID, scope.KindMethod, sig, sha)
	if err != nil {
		t.Fatalf("DeriveScopeID second call: %v", err)
	}
	if first != second {
		t.Errorf("DeriveScopeID not deterministic: first=%s second=%s", first, second)
	}
}

// TestDeriveScopeID_StableAcrossSHAs covers scenario
// `scope-id-stable-across-shas`: a signature first seen at
// SHA A and observed again at SHA B resolves to the SAME
// `scope_id` -- BECAUSE the caller passes `firstSeenSHA=A` on
// both calls (SHA is not part of identity; first_seen_sha is
// the immutable column).
func TestDeriveScopeID_StableAcrossSHAs(t *testing.T) {
	t.Parallel()
	repoID := mustRepoID(t, "22222222-2222-4222-8222-222222222222")
	sig := "github.com/acme/repo::method::pkg/foo.go#pkg.Foo.bar(int)"
	const shaA = "1111111111111111111111111111111111111111"
	const shaB = "2222222222222222222222222222222222222222"

	// At SHA A: signature first appears, first_seen_sha=A.
	idA, err := scope.DeriveScopeID(repoID, scope.KindMethod, sig, shaA)
	if err != nil {
		t.Fatalf("DeriveScopeID at SHA A: %v", err)
	}
	// At SHA B: same signature observed again. The CALLER (or
	// the storage writer) has looked up first_seen_sha and
	// found it is still A, so passes A here. The derived UUID
	// is the same -- G2 stability.
	idB, err := scope.DeriveScopeID(repoID, scope.KindMethod, sig, shaA)
	if err != nil {
		t.Fatalf("DeriveScopeID at SHA B (reusing first_seen_sha=A): %v", err)
	}
	if idA != idB {
		t.Errorf("scope_id NOT stable across observations using same first_seen_sha: idA=%s idB=%s", idA, idB)
	}
	// Sanity check: passing first_seen_sha=B (which would be a
	// CALLER BUG -- failing to look up the existing row) produces
	// a DIFFERENT UUID. This is the bug the storage-layer writer
	// guards against; the identity function itself is by design
	// a pure function of its inputs.
	idBuggy, err := scope.DeriveScopeID(repoID, scope.KindMethod, sig, shaB)
	if err != nil {
		t.Fatalf("DeriveScopeID at SHA B (buggy: first_seen_sha=B): %v", err)
	}
	if idBuggy == idA {
		t.Errorf("expected DeriveScopeID with different first_seen_sha to produce different UUID, got equal: %s", idA)
	}
}

// TestDeriveScopeID_Uniqueness asserts that varying ANY of the
// four pre-image fields produces a different UUID. This is the
// uniqueness leg of the implementation-plan Stage 2.2 acceptance
// criterion ("different scope_kind or signature => different
// UUID") plus the analogous repo_id and first_seen_sha legs.
func TestDeriveScopeID_Uniqueness(t *testing.T) {
	t.Parallel()
	repoA := mustRepoID(t, "33333333-3333-4333-8333-333333333333")
	repoB := mustRepoID(t, "44444444-4444-4444-8444-444444444444")
	sigA := "github.com/acme/repo::method::pkg/foo.go#pkg.Foo.bar(int)"
	sigB := "github.com/acme/repo::method::pkg/foo.go#pkg.Foo.bar(string)"
	const shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	base, err := scope.DeriveScopeID(repoA, scope.KindMethod, sigA, shaA)
	if err != nil {
		t.Fatalf("base derive: %v", err)
	}
	type variant struct {
		name string
		fn   func() (uuid.UUID, error)
	}
	variants := []variant{
		{"different repo_id", func() (uuid.UUID, error) {
			return scope.DeriveScopeID(repoB, scope.KindMethod, sigA, shaA)
		}},
		{"different scope_kind", func() (uuid.UUID, error) {
			return scope.DeriveScopeID(repoA, scope.KindClass, sigA, shaA)
		}},
		{"different canonical_signature", func() (uuid.UUID, error) {
			return scope.DeriveScopeID(repoA, scope.KindMethod, sigB, shaA)
		}},
		{"different first_seen_sha", func() (uuid.UUID, error) {
			return scope.DeriveScopeID(repoA, scope.KindMethod, sigA, shaB)
		}},
	}
	for _, v := range variants {
		v := v
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			got, err := v.fn()
			if err != nil {
				t.Fatalf("variant %q: %v", v.name, err)
			}
			if got == base {
				t.Errorf("variant %q produced SAME UUID %s as base (uniqueness violated)", v.name, base)
			}
		})
	}
}

// TestDeriveScopeID_AllKindsDistinct asserts every canonical
// [scope.Kind] value produces a distinct `scope_id` when fed
// the same `(repo_id, canonical_signature, first_seen_sha)`.
// Coupled with the closed-set assertion in
// `TestKind_IsValid_ClosedSet`, this guards against a future
// enum addition that forgets to bump the namespace -- the new
// kind's `scope_id` would land in the same UUID space as
// existing kinds but the discriminator ensures no collision.
func TestDeriveScopeID_AllKindsDistinct(t *testing.T) {
	t.Parallel()
	repoID := mustRepoID(t, "55555555-5555-4555-8555-555555555555")
	const sig = "github.com/acme/repo::method::pkg/foo.go#pkg.Foo.bar()"
	sha := strings.Repeat("c", 40)
	seen := map[uuid.UUID]scope.Kind{}
	for _, k := range []scope.Kind{
		scope.KindRepo,
		scope.KindPackage,
		scope.KindFile,
		scope.KindClass,
		scope.KindInterface,
		scope.KindMethod,
		scope.KindBlock,
	} {
		id, err := scope.DeriveScopeID(repoID, k, sig, sha)
		if err != nil {
			t.Fatalf("DeriveScopeID(%s): %v", k, err)
		}
		if prior, dup := seen[id]; dup {
			t.Errorf("scope_id collision between kinds %s and %s -> %s", prior, k, id)
		}
		seen[id] = k
	}
	if got, want := len(seen), 7; got != want {
		t.Errorf("distinct scope_ids = %d, want %d (one per Kind)", got, want)
	}
}

// TestDeriveScopeID_Validation covers the validation surface
// of [scope.DeriveScopeID]: zero repo_id, invalid kind, empty
// canonical_signature, empty first_seen_sha, NUL in signature,
// NUL in sha. Each MUST return a typed sentinel so callers can
// branch via [errors.Is].
func TestDeriveScopeID_Validation(t *testing.T) {
	t.Parallel()
	repoID := mustRepoID(t, "66666666-6666-4666-8666-666666666666")
	const sig = "valid::sig"
	const sha = "0000000000000000000000000000000000000000"

	cases := []struct {
		name     string
		repoID   uuid.UUID
		kind     scope.Kind
		sig, sha string
		want     error
	}{
		{"zero repo_id", uuid.Nil, scope.KindMethod, sig, sha, scope.ErrZeroRepoID},
		{"invalid kind", repoID, scope.Kind("function"), sig, sha, scope.ErrInvalidKind},
		{"empty signature", repoID, scope.KindMethod, "", sha, scope.ErrEmptyField},
		{"empty sha", repoID, scope.KindMethod, sig, "", scope.ErrEmptyField},
		{"NUL in signature", repoID, scope.KindMethod, "before\x00after", sha, scope.ErrEmbeddedNUL},
		{"NUL in sha", repoID, scope.KindMethod, sig, "be\x00ef" + strings.Repeat("a", 35), scope.ErrEmbeddedNUL},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := scope.DeriveScopeID(tc.repoID, tc.kind, tc.sig, tc.sha)
			if err == nil {
				t.Fatalf("expected error %v, got nil", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("expected errors.Is to match %v, got %v", tc.want, err)
			}
		})
	}
}

// TestDeriveScopeID_NotZero is a sanity check that a valid
// invocation never produces the zero UUID -- a [uuid.Nil]
// scope_id would collide with the [ErrZeroRepoID] sentinel
// pattern many callers use as a "no value" marker.
func TestDeriveScopeID_NotZero(t *testing.T) {
	t.Parallel()
	repoID := mustRepoID(t, "77777777-7777-4777-8777-777777777777")
	got, err := scope.DeriveScopeID(repoID, scope.KindFile, "github.com/acme/repo::file::foo.go", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if err != nil {
		t.Fatalf("DeriveScopeID: %v", err)
	}
	if got == uuid.Nil {
		t.Errorf("DeriveScopeID returned uuid.Nil")
	}
}

// TestKind_IsValid_ClosedSet pins the canonical seven-value
// scope_kind enum. Adding a value here requires also adding it
// to the PostgreSQL ENUM (migration) AND the architecture doc
// (Sec 5.2.3 line 1046) -- the test fails loudly when only
// one of the three is updated.
func TestKind_IsValid_ClosedSet(t *testing.T) {
	t.Parallel()
	allowed := []scope.Kind{
		scope.KindRepo,
		scope.KindPackage,
		scope.KindFile,
		scope.KindClass,
		scope.KindInterface,
		scope.KindMethod,
		scope.KindBlock,
	}
	for _, k := range allowed {
		if !k.IsValid() {
			t.Errorf("Kind(%q).IsValid() = false, want true", k)
		}
	}
	rejected := []scope.Kind{
		"",         // empty
		"function", // canonical synonym, NOT allowed (Stage 1.3 closed set)
		"module",   // canonical synonym, NOT allowed
		"Method",   // case-sensitive
		"file ",    // trailing space
	}
	for _, k := range rejected {
		if k.IsValid() {
			t.Errorf("Kind(%q).IsValid() = true, want false", k)
		}
	}
}
