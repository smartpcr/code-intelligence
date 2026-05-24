package steward

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
)

// newOverrideTestSteward returns a Steward backed by an
// InMemoryStore PRE-SEEDED with the sample SOLID-SRP rule (the
// Override row's logical FK target). Tests that exercise the
// FK-miss negative case use [buildEmptyOverrideSteward] which
// skips the seeding.
//
// The returned clock advances 1s per tick so two consecutive
// Override appends land with distinct, monotonically-increasing
// `created_at` values -- required for the latest-row-wins
// assertion to be unambiguous.
func newOverrideTestSteward(t *testing.T) (*Steward, *InMemoryStore) {
	t.Helper()
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	mgr := newKeysManagerWithMintedKey(t)
	st, err := New(Config{
		Store:  store,
		Signer: mgr,
		Clock:  fixedClock(sampleClockStart()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st, store
}

func buildEmptyOverrideSteward(t *testing.T) (*Steward, *InMemoryStore) {
	t.Helper()
	store := NewInMemoryStore()
	mgr := newKeysManagerWithMintedKey(t)
	st, err := New(Config{
		Store:  store,
		Signer: mgr,
		Clock:  fixedClock(sampleClockStart()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st, store
}

func sampleOverrideRequest() OverrideRequest {
	return OverrideRequest{
		RuleID: "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{
			RepoID:             "repo-a",
			ScopeKind:          ScopeKindClass,
			ScopeSignatureGlob: "com.example.legacy.*",
		},
		Mute:    true,
		Reason:  "legacy code; planned refactor in Q3",
		ActorID: "alice@example.com",
	}
}

// sampleCandidateScope returns a CONCRETE class signature
// inside the `com.example.legacy.*` glob (so the
// sample-mute filter matches it via [scopeGlobMatches]).
func sampleCandidateScope() CandidateScope {
	return CandidateScope{
		RepoID:    "repo-a",
		ScopeKind: ScopeKindClass,
		Signature: "com.example.legacy.OrderProcessor",
	}
}

// ---- happy paths --------------------------------------------------

func TestSteward_Override_MuteHappyPath(t *testing.T) {
	t.Parallel()
	st, store := newOverrideTestSteward(t)
	ctx := context.Background()

	o, err := st.Override(ctx, sampleOverrideRequest())
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if o.OverrideID == uuid.Nil {
		t.Error("OverrideID is the zero uuid")
	}
	if o.RuleID != "solid.srp.lcom4_high" {
		t.Errorf("RuleID=%q, want solid.srp.lcom4_high", o.RuleID)
	}
	if !o.Mute {
		t.Error("Mute=false, want true")
	}
	if o.Reason != "legacy code; planned refactor in Q3" {
		t.Errorf("Reason=%q, want %q", o.Reason, "legacy code; planned refactor in Q3")
	}
	if o.ActorID != "alice@example.com" {
		t.Errorf("ActorID=%q, want alice@example.com", o.ActorID)
	}
	if o.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if o.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt.Location()=%v, want UTC", o.CreatedAt.Location())
	}
	// Verify the row landed and the gate-time reader finds
	// it via the glob-matching candidate-scope lookup.
	got, ok, err := store.LatestMatchingOverride(ctx, o.RuleID, sampleCandidateScope())
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !ok {
		t.Fatal("LatestMatchingOverride: row not found after insert")
	}
	if got.OverrideID != o.OverrideID {
		t.Errorf("LatestMatchingOverride OverrideID=%s, want %s", got.OverrideID, o.OverrideID)
	}
}

func TestSteward_Override_UnmuteAppendsNewRow(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	ctx := context.Background()

	muteReq := sampleOverrideRequest()
	mute, err := st.Override(ctx, muteReq)
	if err != nil {
		t.Fatalf("mute Override: %v", err)
	}

	unmuteReq := muteReq
	unmuteReq.Mute = false
	unmuteReq.Reason = "" // reason optional on unmute
	unmute, err := st.Override(ctx, unmuteReq)
	if err != nil {
		t.Fatalf("unmute Override: %v", err)
	}
	if unmute.OverrideID == mute.OverrideID {
		t.Error("unmute reused the prior OverrideID; want a fresh uuid (append-only)")
	}
	if unmute.Mute {
		t.Error("unmute.Mute=true, want false")
	}
	if !unmute.CreatedAt.After(mute.CreatedAt) {
		t.Errorf("unmute.CreatedAt=%s not after mute.CreatedAt=%s -- clock did not advance",
			unmute.CreatedAt, mute.CreatedAt)
	}
}

// TestSteward_Override_LatestRowWins is the scenario from the
// implementation-plan Stage 5.3 test scenarios:
// "latest-row-wins: Given two override rows for the same
// scope/rule with `mute=true` then `mute=false`, When the
// evaluator reads the active mute via a CANDIDATE scope, Then
// it sees `mute=false`."
func TestSteward_Override_LatestRowWins(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	ctx := context.Background()

	req := sampleOverrideRequest()
	if _, err := st.Override(ctx, req); err != nil {
		t.Fatalf("mute Override: %v", err)
	}
	req.Mute = false
	req.Reason = ""
	if _, err := st.Override(ctx, req); err != nil {
		t.Fatalf("unmute Override: %v", err)
	}
	latest, ok, err := st.LatestMatchingOverride(ctx, req.RuleID, sampleCandidateScope())
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !ok {
		t.Fatal("LatestMatchingOverride: ok=false; want a row")
	}
	if latest.Mute {
		t.Errorf("latest.Mute=true, want false (unmute is the newer row)")
	}
}

// TestSteward_Override_OldRowRemainsActiveWithoutTTL pins the
// "no-ttl-enforcement" scenario from the implementation-plan
// Stage 5.3 test scenarios + tech-spec Sec 10A "mute lifecycle"
// pin. An override row older than any reasonable TTL (365
// days) remains the active state when no fresher row exists --
// proving the absence of a scheduled "expire old overrides"
// job.
func TestSteward_Override_OldRowRemainsActiveWithoutTTL(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	mgr := newKeysManagerWithMintedKey(t)
	start := sampleClockStart()
	plantedAt := start.Add(-400 * 24 * time.Hour)
	clockReturns := plantedAt
	st, err := New(Config{
		Store:  store,
		Signer: mgr,
		Clock:  func() time.Time { return clockReturns },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o, err := st.Override(context.Background(), sampleOverrideRequest())
	if err != nil {
		t.Fatalf("Override: %v", err)
	}
	if !o.CreatedAt.Equal(plantedAt.UTC()) {
		t.Fatalf("CreatedAt=%s, want %s", o.CreatedAt, plantedAt.UTC())
	}
	// Advance the clock 400 days; with no expiry timer, the
	// row remains the active mute when the evaluator asks
	// for the candidate scope.
	clockReturns = start
	latest, ok, err := st.LatestMatchingOverride(context.Background(), o.RuleID, sampleCandidateScope())
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !ok {
		t.Fatal("LatestMatchingOverride: ok=false; aged row was implicitly expired (TTL leaked into v1)")
	}
	if !latest.Mute {
		t.Errorf("latest.Mute=false; the aged row was implicitly demoted")
	}
}

// ---- glob matching (the load-bearing read semantic) -------------

// TestSteward_LatestMatchingOverride_GlobMatchesSubScope pins
// the architecture Sec 5.3.6 read contract end-to-end: an
// override registered with glob `com.example.legacy.*` is
// reported for ANY concrete class signature INSIDE that
// package, not just an exact equality match.
func TestSteward_LatestMatchingOverride_GlobMatchesSubScope(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	ctx := context.Background()
	req := sampleOverrideRequest() // glob: com.example.legacy.*
	if _, err := st.Override(ctx, req); err != nil {
		t.Fatalf("Override: %v", err)
	}
	// The candidate signature `com.example.legacy.Foo` is
	// INSIDE the registered glob -- the gate MUST find the
	// mute.
	cases := []string{
		"com.example.legacy.Foo",
		"com.example.legacy.Bar",
		"com.example.legacy.sub.Pkg",   // `*` crosses dots
		"com.example.legacy.",          // trailing dot still matches `.*`
		"com.example.legacy.with/path", // `*` crosses slashes
	}
	for _, sig := range cases {
		got, ok, err := st.LatestMatchingOverride(ctx, req.RuleID, CandidateScope{
			RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: sig,
		})
		if err != nil {
			t.Errorf("Signature=%q: err=%v, want nil", sig, err)
			continue
		}
		if !ok {
			t.Errorf("Signature=%q: ok=false; want true (glob com.example.legacy.* should match)", sig)
			continue
		}
		if !got.Mute {
			t.Errorf("Signature=%q: Mute=false; want true", sig)
		}
	}
}

// TestSteward_LatestMatchingOverride_GlobDoesNotMatchOtherScopes
// pins the negative side of the glob contract: a candidate
// OUTSIDE the registered glob must not match.
func TestSteward_LatestMatchingOverride_GlobDoesNotMatchOtherScopes(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	ctx := context.Background()
	req := sampleOverrideRequest() // glob: com.example.legacy.*
	if _, err := st.Override(ctx, req); err != nil {
		t.Fatalf("Override: %v", err)
	}
	cases := []struct {
		desc string
		cs   CandidateScope
	}{
		{
			"different package",
			CandidateScope{RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.other.legacy.Foo"},
		},
		{
			"package prefix is shorter than the glob",
			CandidateScope{RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.Foo"},
		},
		{
			"different repo",
			CandidateScope{RepoID: "repo-b", ScopeKind: ScopeKindClass, Signature: "com.example.legacy.Foo"},
		},
		{
			"different scope_kind",
			CandidateScope{RepoID: "repo-a", ScopeKind: ScopeKindMethod, Signature: "com.example.legacy.Foo"},
		},
	}
	for _, tc := range cases {
		_, ok, err := st.LatestMatchingOverride(ctx, req.RuleID, tc.cs)
		if err != nil {
			t.Errorf("%s: err=%v, want nil", tc.desc, err)
			continue
		}
		if ok {
			t.Errorf("%s: ok=true; want false", tc.desc)
		}
	}
}

// TestSteward_LatestMatchingOverride_StarMatchesEverything pins
// the wildcard glob `*` -- operator's "every scope of this
// kind in this repo".
func TestSteward_LatestMatchingOverride_StarMatchesEverything(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	ctx := context.Background()
	req := sampleOverrideRequest()
	req.ScopeFilter.ScopeSignatureGlob = "*"
	if _, err := st.Override(ctx, req); err != nil {
		t.Fatalf("Override: %v", err)
	}
	for _, sig := range []string{"a", "com.example.X", "x/y/z.go", "any-thing.at-all"} {
		_, ok, err := st.LatestMatchingOverride(ctx, req.RuleID, CandidateScope{
			RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: sig,
		})
		if err != nil {
			t.Errorf("Signature=%q: err=%v", sig, err)
			continue
		}
		if !ok {
			t.Errorf("Signature=%q: ok=false; the `*` glob should match every non-empty signature", sig)
		}
	}
}

// TestSteward_LatestMatchingOverride_QuestionMarkMatchesOneChar
// pins the `?` glob -- exactly one character.
func TestSteward_LatestMatchingOverride_QuestionMarkMatchesOneChar(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	ctx := context.Background()
	req := sampleOverrideRequest()
	req.ScopeFilter.ScopeSignatureGlob = "Foo?"
	if _, err := st.Override(ctx, req); err != nil {
		t.Fatalf("Override: %v", err)
	}
	hits := []string{"FooA", "Foo1", "Foo!"}
	misses := []string{"Foo", "FooAA", "fooA"}
	for _, sig := range hits {
		_, ok, _ := st.LatestMatchingOverride(ctx, req.RuleID, CandidateScope{
			RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: sig,
		})
		if !ok {
			t.Errorf("Signature=%q: ok=false; want true (`Foo?` matches one char)", sig)
		}
	}
	for _, sig := range misses {
		_, ok, _ := st.LatestMatchingOverride(ctx, req.RuleID, CandidateScope{
			RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: sig,
		})
		if ok {
			t.Errorf("Signature=%q: ok=true; want false", sig)
		}
	}
}

// TestSteward_LatestMatchingOverride_NewerBroadOverridesOlderLiteral
// pins the rubber-duck "tie-break semantics" critique: when an
// older LITERAL-match override (`com.example.legacy.Foo`) is
// followed by a newer BROADER override (`com.example.legacy.*`)
// that ALSO matches the candidate, the newer row wins per
// MAX(created_at). The mute reason / actor lineage matters
// because operator readers see the latest decision, not the
// most specific historical pattern.
func TestSteward_LatestMatchingOverride_NewerBroadOverridesOlderLiteral(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	ctx := context.Background()

	// First (older): literal-match mute.
	literal := sampleOverrideRequest()
	literal.ScopeFilter.ScopeSignatureGlob = "com.example.legacy.Foo"
	literal.Reason = "literal mute"
	if _, err := st.Override(ctx, literal); err != nil {
		t.Fatalf("literal mute: %v", err)
	}
	// Second (newer): broad-glob unmute.
	broad := sampleOverrideRequest()
	broad.ScopeFilter.ScopeSignatureGlob = "com.example.legacy.*"
	broad.Mute = false
	broad.Reason = ""
	broad.ActorID = "bob@example.com"
	if _, err := st.Override(ctx, broad); err != nil {
		t.Fatalf("broad unmute: %v", err)
	}
	latest, ok, err := st.LatestMatchingOverride(ctx, literal.RuleID, CandidateScope{
		RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.legacy.Foo",
	})
	if err != nil || !ok {
		t.Fatalf("LatestMatchingOverride: err=%v ok=%v", err, ok)
	}
	if latest.Mute {
		t.Errorf("Mute=true; want false (newer broad unmute wins over older literal mute)")
	}
	if latest.ActorID != "bob@example.com" {
		t.Errorf("ActorID=%q, want bob@example.com (the newer row's actor)", latest.ActorID)
	}
}

// ---- candidate validation ---------------------------------------

func TestSteward_LatestMatchingOverride_RejectsInvalidCandidate(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	ctx := context.Background()

	cases := []struct {
		desc string
		cs   CandidateScope
	}{
		{"empty repo_id", CandidateScope{RepoID: "", ScopeKind: ScopeKindClass, Signature: "X"}},
		{"empty signature", CandidateScope{RepoID: "r", ScopeKind: ScopeKindClass, Signature: ""}},
		{"whitespace signature", CandidateScope{RepoID: "r", ScopeKind: ScopeKindClass, Signature: "  "}},
		{"empty scope_kind", CandidateScope{RepoID: "r", ScopeKind: "", Signature: "X"}},
		{"unknown scope_kind", CandidateScope{RepoID: "r", ScopeKind: "module", Signature: "X"}},
	}
	for _, tc := range cases {
		_, _, err := st.LatestMatchingOverride(ctx, "solid.srp.lcom4_high", tc.cs)
		if !errors.Is(err, ErrInvalidCandidateScope) {
			t.Errorf("%s: err=%v, want ErrInvalidCandidateScope", tc.desc, err)
		}
	}
}

func TestSteward_LatestMatchingOverride_RejectsEmptyRuleID(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	_, _, err := st.LatestMatchingOverride(context.Background(), "  ", sampleCandidateScope())
	if !errors.Is(err, ErrInvalidCandidateScope) {
		t.Fatalf("empty rule_id: err=%v, want ErrInvalidCandidateScope", err)
	}
}

// ---- validation failures on write -------------------------------

func TestSteward_Override_RejectsEmptyRuleID(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	req := sampleOverrideRequest()
	req.RuleID = ""
	if _, err := st.Override(context.Background(), req); !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("Override empty rule_id: err=%v, want ErrInvalidOverride", err)
	}
}

func TestSteward_Override_RejectsEmptyActorID(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	req := sampleOverrideRequest()
	req.ActorID = ""
	if _, err := st.Override(context.Background(), req); !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("Override empty actor_id: err=%v, want ErrInvalidOverride", err)
	}
}

func TestSteward_Override_RejectsMuteWithoutReason(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	for _, reason := range []string{"", "   ", "\t\n"} {
		req := sampleOverrideRequest()
		req.Mute = true
		req.Reason = reason
		if _, err := st.Override(context.Background(), req); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("mute=true reason=%q: err=%v, want ErrInvalidOverride", reason, err)
		}
	}
}

func TestSteward_Override_UnmuteWithoutReasonAllowed(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	req := sampleOverrideRequest()
	req.Mute = false
	req.Reason = ""
	if _, err := st.Override(context.Background(), req); err != nil {
		t.Fatalf("unmute reason=\"\": err=%v, want nil (architecture Sec 5.3.6 line 1169 only requires reason when mute=true)", err)
	}
}

func TestSteward_Override_RejectsInvalidScopeKind(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	for _, bad := range []ScopeKind{"", "module", "namespace", "function", "Class"} {
		req := sampleOverrideRequest()
		req.ScopeFilter.ScopeKind = bad
		if _, err := st.Override(context.Background(), req); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("scope_kind=%q: err=%v, want ErrInvalidOverride", bad, err)
		}
	}
}

func TestSteward_Override_RejectsEmptyRepoID(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	req := sampleOverrideRequest()
	req.ScopeFilter.RepoID = ""
	if _, err := st.Override(context.Background(), req); !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("empty repo_id: err=%v, want ErrInvalidOverride", err)
	}
}

func TestSteward_Override_RejectsEmptyScopeSignatureGlob(t *testing.T) {
	t.Parallel()
	st, _ := newOverrideTestSteward(t)
	req := sampleOverrideRequest()
	req.ScopeFilter.ScopeSignatureGlob = ""
	if _, err := st.Override(context.Background(), req); !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("empty scope_signature_glob: err=%v, want ErrInvalidOverride", err)
	}
}

func TestSteward_Override_AcceptsAllSevenScopeKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []ScopeKind{
		ScopeKindRepo, ScopeKindPackage, ScopeKindFile,
		ScopeKindClass, ScopeKindInterface, ScopeKindMethod, ScopeKindBlock,
	} {
		st, _ := newOverrideTestSteward(t)
		req := sampleOverrideRequest()
		req.ScopeFilter.ScopeKind = kind
		if _, err := st.Override(context.Background(), req); err != nil {
			t.Errorf("scope_kind=%q: err=%v, want nil", kind, err)
		}
	}
}

// ---- FK enforcement ----------------------------------------------

func TestSteward_Override_RejectsUnknownRule(t *testing.T) {
	t.Parallel()
	st, _ := buildEmptyOverrideSteward(t)
	if _, err := st.Override(context.Background(), sampleOverrideRequest()); !errors.Is(err, ErrUnknownRule) {
		t.Fatalf("unknown rule_id: err=%v, want ErrUnknownRule (logical FK from architecture Sec 5.3.6 line 1166)", err)
	}
}

// ---- no signing key precondition --------------------------------

// TestSteward_Override_NoSigningKeyAccepted pins the Stage 5.3
// design decision: `mgmt.override` is the operator kill switch
// and MUST work during a signing-key outage (architecture Sec
// 4.6 + rubber-duck #2 critique). Unlike Publish/Activate/
// PublishRulepack, Override does NOT call checkSigningKey.
func TestSteward_Override_NoSigningKeyAccepted(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	mgr := newKeysManagerEmpty(t)
	st, err := New(Config{
		Store:  store,
		Signer: mgr,
		Clock:  fixedClock(sampleClockStart()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := st.Override(context.Background(), sampleOverrideRequest()); err != nil {
		t.Fatalf("Override without active signing key: err=%v, want nil (override is the kill switch -- must work during signing-key outage)", err)
	}
}

// ---- append-only invariants -------------------------------------

// TestStore_OverrideAppendOnlyInterfaceShape pins the architecture
// G3 contract at the interface level: the Store interface has no
// UpdateOverride or DeleteOverride method, so attempting to mutate
// an existing row trips a compile error rather than a runtime
// permission check.
func TestStore_OverrideAppendOnlyInterfaceShape(t *testing.T) {
	t.Parallel()
	var s Store = NewInMemoryStore()
	// The Store interface MUST provide InsertOverride and
	// LatestMatchingOverride (append-only + read).
	type appendOnly interface {
		InsertOverride(ctx context.Context, o Override) error
		LatestMatchingOverride(ctx context.Context, ruleID string, candidate CandidateScope) (Override, bool, error)
	}
	if _, ok := s.(appendOnly); !ok {
		t.Fatal("Store does not satisfy the append-only override surface")
	}
	type forbiddenMutation interface {
		UpdateOverride(ctx context.Context, o Override) error
	}
	type forbiddenDelete interface {
		DeleteOverride(ctx context.Context, id uuid.UUID) error
	}
	if _, ok := s.(forbiddenMutation); ok {
		t.Fatal("Store implements UpdateOverride -- violates the architecture G3 append-only contract")
	}
	if _, ok := s.(forbiddenDelete); ok {
		t.Fatal("Store implements DeleteOverride -- violates the architecture G3 append-only contract")
	}
}

// TestStore_InsertOverrideRejectsDuplicateID pins the PK
// uniqueness contract -- two inserts with the same OverrideID
// MUST surface a wrapped error so a caller's idempotency story
// works.
func TestStore_InsertOverrideRejectsDuplicateID(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	id := uuid.Must(uuid.NewV4())
	o := Override{
		OverrideID: id,
		RuleID:     "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{
			RepoID:             "repo-a",
			ScopeKind:          ScopeKindClass,
			ScopeSignatureGlob: "com.example.*",
		},
		Mute:      true,
		Reason:    "test",
		ActorID:   "alice",
		CreatedAt: sampleClockStart(),
	}
	if err := store.InsertOverride(context.Background(), o); err != nil {
		t.Fatalf("first InsertOverride: %v", err)
	}
	err := store.InsertOverride(context.Background(), o)
	if err == nil {
		t.Fatal("second InsertOverride with same OverrideID: err=nil, want a wrapped error")
	}
	if !strings.Contains(err.Error(), id.String()) {
		t.Errorf("duplicate error %q does not name the offending id %s", err.Error(), id)
	}
}

// TestStore_LatestMatchingOverrideMissingReturnsOK -- empty
// store returns ok=false (not an error).
func TestStore_LatestMatchingOverrideMissingReturnsOK(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	_, ok, err := store.LatestMatchingOverride(context.Background(), "anything",
		CandidateScope{RepoID: "r", ScopeKind: ScopeKindRepo, Signature: "x"})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if ok {
		t.Error("ok=true on empty store; want false")
	}
}

// TestStore_LatestMatchingOverrideTieBreakOnOverrideID pins the
// secondary ordering for two rows that share `created_at`:
// the row with the lexicographically-larger uuid wins.
// Mirrors the SQLStore's `ORDER BY created_at DESC,
// override_id DESC` clause.
func TestStore_LatestMatchingOverrideTieBreakOnOverrideID(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	at := sampleClockStart()
	filter := ScopeFilter{
		RepoID:             "repo-a",
		ScopeKind:          ScopeKindClass,
		ScopeSignatureGlob: "com.example.*",
	}
	// Force a deterministic ordering: build two uuids and
	// pick the larger one explicitly.
	a := uuid.Must(uuid.NewV4())
	b := uuid.Must(uuid.NewV4())
	if uuidCompare(a, b) < 0 {
		a, b = b, a // ensure a > b
	}
	for _, id := range []uuid.UUID{b, a} {
		if err := store.InsertOverride(context.Background(), Override{
			OverrideID:  id,
			RuleID:      "solid.srp.lcom4_high",
			ScopeFilter: filter,
			Mute:        true,
			Reason:      "tie-break test",
			ActorID:     "alice",
			CreatedAt:   at,
		}); err != nil {
			t.Fatalf("InsertOverride %s: %v", id, err)
		}
	}
	got, ok, err := store.LatestMatchingOverride(context.Background(), "solid.srp.lcom4_high",
		CandidateScope{RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.Foo"})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !ok {
		t.Fatal("LatestMatchingOverride: ok=false")
	}
	if got.OverrideID != a {
		t.Errorf("tie-break OverrideID=%s, want larger uuid %s (got smaller %s)", got.OverrideID, a, b)
	}
}

// TestStore_LatestMatchingOverrideIgnoresOtherRulesAndRepos
// pins the partition filter: a row for a different rule_id OR
// a different repo_id MUST not be returned.
func TestStore_LatestMatchingOverrideIgnoresOtherRulesAndRepos(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	ctx := context.Background()
	scopeA := ScopeFilter{RepoID: "repo-a", ScopeKind: ScopeKindClass, ScopeSignatureGlob: "com.example.*"}
	// Insert a row under a different repo.
	if err := store.InsertOverride(ctx, Override{
		OverrideID:  uuid.Must(uuid.NewV4()),
		RuleID:      "solid.srp.lcom4_high",
		ScopeFilter: scopeA,
		Mute:        true,
		Reason:      "different repo",
		ActorID:     "alice",
		CreatedAt:   sampleClockStart(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Look up under repo-b -- must miss.
	_, ok, err := store.LatestMatchingOverride(ctx, "solid.srp.lcom4_high",
		CandidateScope{RepoID: "repo-b", ScopeKind: ScopeKindClass, Signature: "com.example.Foo"})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if ok {
		t.Error("ok=true for non-matching repo_id; the store leaked a row across repos")
	}
	// Different rule -- must miss.
	_, ok, err = store.LatestMatchingOverride(ctx, "no.such.rule",
		CandidateScope{RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.Foo"})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if ok {
		t.Error("ok=true for non-matching rule_id; the store leaked a row across rules")
	}
}

// TestSteward_Override_DoesNotShareSigningPathContract pins that
// errors.Is(ErrInvalidOverride, ErrInvalidRequest) is FALSE --
// the two sentinels are distinct and the HTTP layer relies on
// the distinction for separate logging tags.
func TestSteward_Override_DoesNotShareSigningPathContract(t *testing.T) {
	t.Parallel()
	if errors.Is(ErrInvalidOverride, ErrInvalidRequest) {
		t.Fatal("ErrInvalidOverride is identical to ErrInvalidRequest; they should be distinct sentinels")
	}
	if errors.Is(ErrUnknownRule, ErrUnknownRuleRef) {
		t.Fatal("ErrUnknownRule is identical to ErrUnknownRuleRef; they should be distinct sentinels")
	}
	if errors.Is(ErrInvalidCandidateScope, ErrInvalidOverride) {
		t.Fatal("ErrInvalidCandidateScope aliases ErrInvalidOverride; they should be distinct sentinels")
	}
}

// ---- scope_glob translation pins -------------------------------

// TestScopeGlobToRegex pins the translation rules character-by-
// character so a future change that "tightens" the glob (e.g.
// introducing a path separator semantic) trips this test.
func TestScopeGlobToRegex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"*", "^.*$"},
		{"?", "^.$"},
		{"foo", "^foo$"},
		{"foo.bar", `^foo\.bar$`}, // `.` quoted
		{"foo*", `^foo.*$`},       //
		{"a*b?c", `^a.*b.c$`},     //
		{"a+b", `^a\+b$`},         // regex meta quoted
		{"[x]", `^\[x\]$`},        // brackets are literal
		{`a\b`, `^a\\b$`},         // backslash quoted
		{"com.example.legacy.*", `^com\.example\.legacy\..*$`},
	}
	for _, tc := range cases {
		got := scopeGlobToRegex(tc.in)
		if got != tc.want {
			t.Errorf("scopeGlobToRegex(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestScopeGlobMatchesCacheHit hits the same pattern twice to
// exercise the compiled-regex cache; the assertion is purely
// "no error, idempotent answer".
func TestScopeGlobMatchesCacheHit(t *testing.T) {
	t.Parallel()
	for i := 0; i < 5; i++ {
		hit, err := scopeGlobMatches("com.example.*", "com.example.Foo")
		if err != nil {
			t.Fatalf("iter %d err=%v", i, err)
		}
		if !hit {
			t.Errorf("iter %d hit=false; want true", i)
		}
	}
}
