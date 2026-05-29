package steward

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"

	"forge/services/clean-code/internal/policy/keys"
)

// envSQLStoreURL is the libpq DSN the SQLStore live tests
// connect to. Matches the same `CLEAN_CODE_PG_URL` the storage
// package and the policy/keys package both consume, so a
// single `export` turns ALL live test paths on at once.
const envSQLStoreURL = "CLEAN_CODE_PG_URL"

// stewardTestSchemaName is the ISOLATED PostgreSQL schema the
// steward SQLStore live tests own. Kept distinct from
// `clean_code` (the production schema owned by the storage-
// package migrate test, which DROP SCHEMA CASCADEs on prep)
// AND from `clean_code_keys_test` (owned by the policy/keys
// SQLStore test) so the three test suites run in parallel
// without racing.
const stewardTestSchemaName = "clean_code_steward_test"

// stewardSchemaPrepTemplate materialises just the tables the
// SQLStore touches -- enough to round-trip a policy_version,
// activation, rule_pack, and rule. We do NOT run the real
// migration so the test stays independent of migration
// ordering (matches the policy/keys SQLStore test pattern).
const stewardSchemaPrepTemplate = `
DROP SCHEMA IF EXISTS %[1]s CASCADE;
CREATE SCHEMA %[1]s;

CREATE TYPE %[1]s.rule_severity AS ENUM ('info', 'warn', 'block');

CREATE TABLE %[1]s.rule_pack (
    pack_id        text         NOT NULL,
    version        integer      NOT NULL,
    display_name   text         NOT NULL,
    description_md text         NOT NULL,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (pack_id, version)
);

CREATE TABLE %[1]s.rule (
    rule_id          text                       NOT NULL,
    version          integer                    NOT NULL,
    pack_id          text                       NOT NULL,
    predicate_dsl    text                       NOT NULL,
    severity_default %[1]s.rule_severity        NOT NULL,
    description_md   text                       NOT NULL,
    created_at       timestamptz                NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, version)
);

CREATE TABLE %[1]s.policy_version (
    policy_version_id  uuid         PRIMARY KEY,
    name               text         NOT NULL,
    rule_refs          jsonb        NOT NULL,
    threshold_refs     jsonb        NOT NULL,
    refactor_weights   jsonb        NOT NULL,
    signature          bytea        NOT NULL,
    created_at         timestamptz  NOT NULL DEFAULT now()
);

CREATE TABLE %[1]s.policy_activation (
    activation_id      uuid         PRIMARY KEY,
    policy_version_id  uuid         NOT NULL
                        REFERENCES %[1]s.policy_version (policy_version_id)
                        ON DELETE RESTRICT,
    activated_by       text         NOT NULL,
    created_at         timestamptz  NOT NULL DEFAULT now()
);

CREATE TABLE %[1]s.threshold (
    threshold_id    uuid                       PRIMARY KEY,
    metric_kind     text                       NOT NULL,
    scope_kind      text                       NOT NULL,
    op              text                       NOT NULL,
    value           double precision           NOT NULL,
    created_at      timestamptz                NOT NULL DEFAULT now()
);

-- override (Stage 5.3) -- mirrors migration 0003 lines 488-546.
-- Append-only mute lifecycle. NO expires_at, NO
-- policy_version_id, NO created_by. actor_id is the OIDC
-- subject; reason is NULL-able on unmute.
--
-- CHECK clause mirrors the PRODUCTION clause exactly:
--   "mute=true implies reason IS NOT NULL".
-- The production schema does NOT enforce non-whitespace
-- reasons -- that is the validator's job (defence in depth,
-- not in the DB). Keeping this test schema in lockstep with
-- migration 0003 lines 526-528 guarantees that the bypass-
-- validator tests below catch exactly what production catches:
-- a NULL reason on a muted row, and nothing more.
CREATE TABLE %[1]s.override (
    override_id    uuid         PRIMARY KEY,
    rule_id        text         NOT NULL,
    scope_filter   jsonb        NOT NULL,
    mute           boolean      NOT NULL,
    reason         text,
    actor_id       text         NOT NULL,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT override_reason_required_when_muted
        CHECK (mute = false OR reason IS NOT NULL)
);

CREATE INDEX override_rule_created_idx
    ON %[1]s.override (rule_id, created_at DESC);
`

const stewardSchemaTeardownTemplate = `
DROP SCHEMA IF EXISTS %[1]s CASCADE;
`

// openStewardSQLStore opens the live PostgreSQL handle for
// the steward SQLStore test suite. On success returns a
// *sql.DB AND a *SQLStore already wired to the isolated test
// schema.
func openStewardSQLStore(t *testing.T) (*sql.DB, *SQLStore, bool) {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(envSQLStoreURL))
	if url == "" {
		t.Skipf("skipping: %s is unset; steward SQLStore live tests require PostgreSQL", envSQLStoreURL)
		return nil, nil, false
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("sql.Open(postgres, %s): %v", envSQLStoreURL, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("db.Ping(%s): %v (set %s to a live PostgreSQL DSN OR unset it to skip)",
			envSQLStoreURL, err, envSQLStoreURL)
	}
	prep := fmt.Sprintf(stewardSchemaPrepTemplate, stewardTestSchemaName)
	if _, err := db.ExecContext(ctx, prep); err != nil {
		_ = db.Close()
		t.Fatalf("schema prep: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		teardown := fmt.Sprintf(stewardSchemaTeardownTemplate, stewardTestSchemaName)
		_, _ = db.ExecContext(ctx, teardown)
		_ = db.Close()
	})
	store, err := NewSQLStoreWithSchema(db, stewardTestSchemaName)
	if err != nil {
		t.Fatalf("NewSQLStoreWithSchema: %v", err)
	}
	return db, store, true
}

func TestSQLStore_NewRejectsNilDB(t *testing.T) {
	t.Parallel()
	_, err := NewSQLStore(nil)
	if err == nil {
		t.Fatal("NewSQLStore(nil): err = nil; want non-nil")
	}
}

func TestSQLStore_NewWithSchemaRejectsEmptySchema(t *testing.T) {
	t.Parallel()
	_, err := NewSQLStoreWithSchema(&sql.DB{}, "")
	if err == nil {
		t.Fatal("NewSQLStoreWithSchema(_, \"\"): err = nil; want non-nil")
	}
}

// TestSQLStore_PolicyVersionRoundTrip pins the most important
// SQLStore invariant: a policy_version row survives the JSONB
// round-trip with bit-identical signed payload, so VerifyAny
// succeeds after reading the row back from PostgreSQL.
//
// Skipped when CLEAN_CODE_PG_URL is unset (developer-laptop
// scenario).
// seedSampleRulesInSQL registers the canonical SRP rulepack
// against the live SQLStore. Required because Steward.Publish
// now enforces the rule_refs FK contract; without seeding, any
// SQL test that calls Publish would fail with ErrUnknownRuleRef.
func seedSampleRulesInSQL(t *testing.T, store *SQLStore) {
	t.Helper()
	now := sampleClockStart()
	pack := RulePack{
		PackID: "solid.srp", Version: 1,
		DisplayName: "Single Responsibility", DescriptionMD: "",
		CreatedAt: now,
	}
	rules := []Rule{
		{RuleID: "solid.srp.lcom4_high", Version: 1, PackID: "solid.srp",
			PredicateDSL: "lcom4 > 0.7", SeverityDefault: SeverityBlock, DescriptionMD: "", CreatedAt: now},
	}
	if err := store.InsertRulePackAndRules(context.Background(), pack, rules); err != nil {
		t.Fatalf("seedSampleRulesInSQL: %v", err)
	}
}

func TestSQLStore_PolicyVersionRoundTrip(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	res, err := keys.Build(ctx, keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	defer res.Close()
	st, err := New(Config{Store: store, Signer: res.Manager})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}

	pv, err := st.Publish(ctx, newSamplePublishRequest())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Read the row back through SQL and verify the signature
	// still validates -- this is the round-trip stability
	// invariant.
	got, err := store.GetPolicyVersion(ctx, pv.PolicyVersionID)
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	if err := st.VerifyPolicyVersionSignature(ctx, got); err != nil {
		t.Fatalf("VerifyPolicyVersionSignature(SQL round-trip): %v -- canonical bytes are NOT round-trip stable through PostgreSQL jsonb", err)
	}
	if got.Name != pv.Name {
		t.Errorf("Name=%q, want %q", got.Name, pv.Name)
	}
	if len(got.RuleRefs) != len(pv.RuleRefs) {
		t.Errorf("RuleRefs len=%d, want %d", len(got.RuleRefs), len(pv.RuleRefs))
	}
}

func TestSQLStore_GetPolicyVersionUnknown(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	_, err := store.GetPolicyVersion(ctx, uuid.Must(uuid.NewV4()))
	if !errors.Is(err, ErrUnknownPolicyVersion) {
		t.Fatalf("GetPolicyVersion(unknown): err=%v, want ErrUnknownPolicyVersion", err)
	}
}

func TestSQLStore_LatestActivationLatestRowWins(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	res, err := keys.Build(ctx, keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	defer res.Close()
	st, err := New(Config{Store: store, Signer: res.Manager, Clock: fixedClock(sampleClockStart())})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}

	pvA, err := st.Publish(ctx, newSamplePublishRequest())
	if err != nil {
		t.Fatalf("Publish A: %v", err)
	}
	req2 := newSamplePublishRequest()
	req2.Name = "default-v2"
	pvB, err := st.Publish(ctx, req2)
	if err != nil {
		t.Fatalf("Publish B: %v", err)
	}

	if _, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: pvA.PolicyVersionID,
		ActivatedBy:     "alice",
	}); err != nil {
		t.Fatalf("Activate A: %v", err)
	}
	paSecond, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: pvB.PolicyVersionID,
		ActivatedBy:     "bob",
	})
	if err != nil {
		t.Fatalf("Activate B: %v", err)
	}

	latest, ok2, err := store.LatestActivation(ctx)
	if err != nil {
		t.Fatalf("LatestActivation: %v", err)
	}
	if !ok2 {
		t.Fatalf("LatestActivation ok=false")
	}
	if latest.ActivationID != paSecond.ActivationID {
		t.Fatalf("LatestActivation=%s, want %s (latest-row-wins)", latest.ActivationID, paSecond.ActivationID)
	}
}

func TestSQLStore_InsertActivationFKViolation(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()

	pa := PolicyActivation{
		ActivationID:    uuid.Must(uuid.NewV4()),
		PolicyVersionID: uuid.Must(uuid.NewV4()), // not persisted
		ActivatedBy:     "alice",
		CreatedAt:       sampleClockStart(),
	}
	err := store.InsertPolicyActivation(ctx, pa)
	if !errors.Is(err, ErrUnknownPolicyVersion) {
		t.Fatalf("InsertPolicyActivation(unknown FK): err=%v, want ErrUnknownPolicyVersion", err)
	}
}

func TestSQLStore_InsertRulePackAndRulesTransactional(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()

	// First publish succeeds.
	now := sampleClockStart()
	pack := RulePack{
		PackID: "solid.srp", Version: 1,
		DisplayName: "Single Responsibility", DescriptionMD: "",
		CreatedAt: now,
	}
	rules := []Rule{
		{RuleID: "solid.srp.lcom4_high", Version: 1, PackID: "solid.srp",
			PredicateDSL: "lcom4 > 0.7", SeverityDefault: SeverityBlock, DescriptionMD: "", CreatedAt: now},
	}
	if err := store.InsertRulePackAndRules(ctx, pack, rules); err != nil {
		t.Fatalf("first InsertRulePackAndRules: %v", err)
	}

	// Second publish with the same `(pack_id, version)` MUST
	// return ErrDuplicateRulePack (the pack insert collides
	// before any rule insert runs).
	err := store.InsertRulePackAndRules(ctx, pack, rules)
	if !errors.Is(err, ErrDuplicateRulePack) {
		t.Fatalf("duplicate pack: err=%v, want ErrDuplicateRulePack", err)
	}

	// A different pack version that re-uses an existing
	// `(rule_id, version)` MUST return ErrDuplicateRule AND
	// roll back the pack insert (transactional).
	pack2 := RulePack{PackID: "solid.srp", Version: 2, DisplayName: "v2", DescriptionMD: "", CreatedAt: now}
	rules2 := []Rule{
		{RuleID: "solid.srp.lcom4_high", Version: 1, PackID: "solid.srp",
			PredicateDSL: "lcom4 > 0.7", SeverityDefault: SeverityBlock, DescriptionMD: "", CreatedAt: now},
	}
	err = store.InsertRulePackAndRules(ctx, pack2, rules2)
	if !errors.Is(err, ErrDuplicateRule) {
		t.Fatalf("duplicate rule: err=%v, want ErrDuplicateRule", err)
	}
	// pack2 must NOT be persisted -- the rule failure should
	// have rolled back the pack insert.
	if _, ok, _ := store.GetRulePack(ctx, "solid.srp", 2); ok {
		t.Errorf("pack version=2 was persisted despite rule failure -- transactional rollback broken")
	}
}

func TestSQLStore_ListRulesForPackSortedAndFiltered(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	now := sampleClockStart()

	if err := store.InsertRulePackAndRules(ctx,
		RulePack{PackID: "solid.dip", Version: 1, DisplayName: "DIP", DescriptionMD: "", CreatedAt: now},
		[]Rule{
			{RuleID: "solid.dip.zfanout", Version: 1, PackID: "solid.dip",
				PredicateDSL: "fan_out > 10", SeverityDefault: SeverityWarn, DescriptionMD: "", CreatedAt: now},
			{RuleID: "solid.dip.aabstractness", Version: 1, PackID: "solid.dip",
				PredicateDSL: "abstractness < 0.2", SeverityDefault: SeverityInfo, DescriptionMD: "", CreatedAt: now},
		}); err != nil {
		t.Fatalf("InsertRulePackAndRules: %v", err)
	}
	if err := store.InsertRulePackAndRules(ctx,
		RulePack{PackID: "solid.srp", Version: 1, DisplayName: "SRP", DescriptionMD: "", CreatedAt: now},
		[]Rule{
			{RuleID: "solid.srp.lcom4_high", Version: 1, PackID: "solid.srp",
				PredicateDSL: "lcom4 > 0.7", SeverityDefault: SeverityBlock, DescriptionMD: "", CreatedAt: now},
		}); err != nil {
		t.Fatalf("InsertRulePackAndRules (srp): %v", err)
	}

	got, err := store.ListRulesForPack(ctx, "solid.dip")
	if err != nil {
		t.Fatalf("ListRulesForPack: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2", len(got))
	}
	// Sorted by rule_id ASC: `aabstractness` < `zfanout`.
	if got[0].RuleID != "solid.dip.aabstractness" || got[1].RuleID != "solid.dip.zfanout" {
		t.Errorf("rules not sorted: %+v", got)
	}
	// Filter must exclude the srp rule.
	for _, r := range got {
		if r.PackID != "solid.dip" {
			t.Errorf("rule.PackID=%q, want solid.dip (filter leaked)", r.PackID)
		}
	}
}

// TestSQLStore_RuleExistsAndPublishFK pins the SQL backing for
// the JSON-FK enforcement contract. Steward.Publish queries
// SQLStore.RuleExists, so this test exercises BOTH that the
// SQL query returns the right boolean AND that publish actually
// refuses unknown rule_refs through the SQL backend (not just
// in-memory).
func TestSQLStore_RuleExistsAndPublishFK(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()

	exists, err := store.RuleExists(ctx, "solid.srp.lcom4_high", 1)
	if err != nil {
		t.Fatalf("RuleExists(empty): %v", err)
	}
	if exists {
		t.Fatalf("RuleExists(empty): true, want false")
	}

	res, err := keys.Build(ctx, keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	defer res.Close()
	st, err := New(Config{Store: store, Signer: res.Manager})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}

	// Publish must refuse before the rulepack is registered.
	if _, err := st.Publish(ctx, newSamplePublishRequest()); !errors.Is(err, ErrUnknownRuleRef) {
		t.Fatalf("Publish(unseeded): err=%v, want ErrUnknownRuleRef", err)
	}

	// After seeding, RuleExists flips to true and Publish
	// succeeds.
	seedSampleRulesInSQL(t, store)
	exists, err = store.RuleExists(ctx, "solid.srp.lcom4_high", 1)
	if err != nil {
		t.Fatalf("RuleExists(seeded): %v", err)
	}
	if !exists {
		t.Fatalf("RuleExists(seeded): false, want true")
	}
	if _, err := st.Publish(ctx, newSamplePublishRequest()); err != nil {
		t.Fatalf("Publish(seeded): %v", err)
	}
}

// TestSQLStore_ThresholdExistsAndPublishFK is the analogous
// SQL coverage for the threshold_refs FK contract.
func TestSQLStore_ThresholdExistsAndPublishFK(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	res, err := keys.Build(ctx, keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	defer res.Close()
	st, err := New(Config{Store: store, Signer: res.Manager})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}

	tid := uuid.Must(uuid.NewV4())

	// Unseeded threshold -> Publish must reject.
	req := newSamplePublishRequest()
	req.ThresholdRefs = []ThresholdRef{{ThresholdID: tid}}
	if _, err := st.Publish(ctx, req); !errors.Is(err, ErrUnknownThresholdRef) {
		t.Fatalf("Publish(unseeded threshold): err=%v, want ErrUnknownThresholdRef", err)
	}

	// Seed the threshold + re-publish -> success.
	if err := store.InsertThreshold(ctx, Threshold{
		ThresholdID: tid,
		MetricKind:  "lcom4",
		ScopeKind:   "class",
		Op:          "gt",
		Value:       0.7,
		CreatedAt:   sampleClockStart(),
	}); err != nil {
		t.Fatalf("InsertThreshold: %v", err)
	}
	exists, err := store.ThresholdExists(ctx, tid)
	if err != nil {
		t.Fatalf("ThresholdExists: %v", err)
	}
	if !exists {
		t.Fatalf("ThresholdExists(seeded): false, want true")
	}
	if _, err := st.Publish(ctx, req); err != nil {
		t.Fatalf("Publish(seeded threshold): %v", err)
	}
}

// TestSQLStore_EvaluatorPicksUpActivatedVersion -- live-PG
// version of the evaluator-pickup scenario. Mirrors the
// in-memory test in steward_test.go but exercises the SQL
// backing for `publish -> activate -> evaluator pickup`.
func TestSQLStore_EvaluatorPicksUpActivatedVersion(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	res, err := keys.Build(ctx, keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	defer res.Close()
	st, err := New(Config{Store: store, Signer: res.Manager, Clock: fixedClock(sampleClockStart())})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}

	pvA, err := st.Publish(ctx, newSamplePublishRequest())
	if err != nil {
		t.Fatalf("Publish A: %v", err)
	}
	if _, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: pvA.PolicyVersionID, ActivatedBy: "alice",
	}); err != nil {
		t.Fatalf("Activate A: %v", err)
	}

	active, ok2, err := st.ActivePolicyVersion(ctx)
	if err != nil {
		t.Fatalf("ActivePolicyVersion: %v", err)
	}
	if !ok2 {
		t.Fatalf("ActivePolicyVersion ok=false")
	}
	if active.PolicyVersionID != pvA.PolicyVersionID {
		t.Fatalf("ActivePolicyVersion=%s, want A=%s", active.PolicyVersionID, pvA.PolicyVersionID)
	}
	if err := st.VerifyPolicyVersionSignature(ctx, active); err != nil {
		t.Fatalf("VerifyPolicyVersionSignature(SQL active): %v", err)
	}
}

// TestSQLStore_OverrideRoundTrip pins the most basic SQL round-
// trip for the Stage 5.3 `override` table: an Override row
// inserted via [SQLStore.InsertOverride] reads back via
// [SQLStore.LatestMatchingOverride] with bit-identical
// scope_filter JSON, NULL-able reason, and UTC created_at.
func TestSQLStore_OverrideRoundTrip(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	id := uuid.Must(uuid.NewV4())
	want := Override{
		OverrideID: id,
		RuleID:     "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{
			RepoID:             "repo-a",
			ScopeKind:          ScopeKindClass,
			ScopeSignatureGlob: "com.example.legacy.*",
		},
		Mute:      true,
		Reason:    "legacy code; planned refactor in Q3",
		ActorID:   "alice@example.com",
		CreatedAt: sampleClockStart(),
	}
	if err := store.InsertOverride(ctx, want); err != nil {
		t.Fatalf("InsertOverride: %v", err)
	}
	got, ok2, err := store.LatestMatchingOverride(ctx, want.RuleID, CandidateScope{
		RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.legacy.Foo",
	})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !ok2 {
		t.Fatal("LatestMatchingOverride: row not found after insert")
	}
	if got.OverrideID != want.OverrideID {
		t.Errorf("OverrideID=%s, want %s", got.OverrideID, want.OverrideID)
	}
	if got.RuleID != want.RuleID {
		t.Errorf("RuleID=%q, want %q", got.RuleID, want.RuleID)
	}
	if got.ScopeFilter != want.ScopeFilter {
		t.Errorf("ScopeFilter=%+v, want %+v", got.ScopeFilter, want.ScopeFilter)
	}
	if got.Mute != want.Mute {
		t.Errorf("Mute=%v, want %v", got.Mute, want.Mute)
	}
	if got.Reason != want.Reason {
		t.Errorf("Reason=%q, want %q", got.Reason, want.Reason)
	}
	if got.ActorID != want.ActorID {
		t.Errorf("ActorID=%q, want %q", got.ActorID, want.ActorID)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt=%s, want %s", got.CreatedAt, want.CreatedAt)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt.Location()=%v, want UTC", got.CreatedAt.Location())
	}
}

// TestSQLStore_OverrideUnmuteAllowsNullReason pins the
// `override_reason_required_when_muted` CHECK constraint
// behaviour: when `mute=false` the writer MUST be allowed to
// land a NULL reason. The SQLStore passes NULL (not "") into
// the bind so the CHECK passes.
func TestSQLStore_OverrideUnmuteAllowsNullReason(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	id := uuid.Must(uuid.NewV4())
	o := Override{
		OverrideID: id,
		RuleID:     "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{
			RepoID: "repo-a", ScopeKind: ScopeKindClass,
			ScopeSignatureGlob: "com.example.*",
		},
		Mute:      false,
		Reason:    "",
		ActorID:   "alice@example.com",
		CreatedAt: sampleClockStart(),
	}
	if err := store.InsertOverride(ctx, o); err != nil {
		t.Fatalf("InsertOverride(unmute, empty reason): %v -- the SQLStore must pass NULL for reason on unmute", err)
	}
	got, ok2, err := store.LatestMatchingOverride(ctx, o.RuleID, CandidateScope{
		RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.Foo",
	})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !ok2 || got.Reason != "" {
		t.Errorf("LatestMatchingOverride Reason=%q ok=%v, want empty string + true", got.Reason, ok2)
	}
}

// TestSQLStore_OverrideMutedReasonNullIsRejectedByCheck pins
// what the production `override_reason_required_when_muted`
// CHECK actually catches: muted rows whose `reason` column is
// SQL NULL. The SQLStore writer translates an empty-string
// `Reason` to a SQL NULL bind (see [SQLStore.InsertOverride]
// lines 423-428), so a muted row with `Reason: ""` reaches PG
// as `reason = NULL` and is rejected. This is the schema-side
// defense in depth; the Steward validator is the primary
// defense and runs first.
//
// What this CHECK does NOT catch is whitespace-only reasons
// on muted rows -- those are `reason IS NOT NULL` and pass.
// See [TestSQLStore_OverrideMutedWhitespaceReasonAcceptedByCheck]
// for the matching "production allows this" pin.
func TestSQLStore_OverrideMutedReasonNullIsRejectedByCheck(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	err := store.InsertOverride(ctx, Override{
		OverrideID: uuid.Must(uuid.NewV4()),
		RuleID:     "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{
			RepoID: "repo-a", ScopeKind: ScopeKindClass,
			ScopeSignatureGlob: "*",
		},
		Mute:      true,
		Reason:    "", // writer maps "" -> NULL
		ActorID:   "alice@example.com",
		CreatedAt: sampleClockStart(),
	})
	if err == nil {
		t.Fatal("InsertOverride(mute=true, reason=\"\"): err=nil -- override_reason_required_when_muted CHECK did not catch the NULL reason a muted row")
	}
}

// TestSQLStore_OverrideMutedWhitespaceReasonAcceptedByCheck
// pins the *boundary* of the production CHECK: a muted row
// whose `reason` column is the string `"   "` is NOT NULL, so
// the CHECK passes and PG accepts the row. This is the
// rubber-duck "false confidence" pin: the database does NOT
// enforce a non-whitespace reason; the Steward validator does.
//
// If a future maintainer assumes the CHECK guarantees a non-
// whitespace reason and removes the validator's whitespace
// check, this test still passes -- and the bypass-validator
// case would silently land whitespace mute reasons in
// production. The matching validator test that pins the
// primary defense is
// [TestSteward_Override_RejectsMuteWithoutReason] in
// override_test.go.
//
// Together these two tests document the exact division of
// labour between the validator (catches "" / whitespace) and
// the CHECK (catches NULL only).
func TestSQLStore_OverrideMutedWhitespaceReasonAcceptedByCheck(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	err := store.InsertOverride(ctx, Override{
		OverrideID: uuid.Must(uuid.NewV4()),
		RuleID:     "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{
			RepoID: "repo-a", ScopeKind: ScopeKindClass,
			ScopeSignatureGlob: "*",
		},
		Mute:      true,
		Reason:    "   ", // not "" -- writer passes literally
		ActorID:   "alice@example.com",
		CreatedAt: sampleClockStart(),
	})
	if err != nil {
		t.Fatalf("InsertOverride(mute=true, reason=\"   \"): err=%v -- production CHECK only blocks NULL, not whitespace; the validator is the defense for whitespace", err)
	}
}

// TestSQLStore_OverrideLatestRowWins pins the SQL ORDER BY
// clause: two override rows for the same (rule_id,
// scope_filter), latest `created_at` wins.
func TestSQLStore_OverrideLatestRowWins(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	filter := ScopeFilter{
		RepoID: "repo-a", ScopeKind: ScopeKindClass,
		ScopeSignatureGlob: "com.example.*",
	}
	t0 := sampleClockStart()
	first := Override{
		OverrideID: uuid.Must(uuid.NewV4()), RuleID: "solid.srp.lcom4_high",
		ScopeFilter: filter, Mute: true, Reason: "noisy", ActorID: "alice",
		CreatedAt: t0,
	}
	second := Override{
		OverrideID: uuid.Must(uuid.NewV4()), RuleID: "solid.srp.lcom4_high",
		ScopeFilter: filter, Mute: false, Reason: "",
		ActorID:   "alice",
		CreatedAt: t0.Add(time.Second),
	}
	if err := store.InsertOverride(ctx, first); err != nil {
		t.Fatalf("InsertOverride first: %v", err)
	}
	if err := store.InsertOverride(ctx, second); err != nil {
		t.Fatalf("InsertOverride second: %v", err)
	}
	got, ok2, err := store.LatestMatchingOverride(ctx, "solid.srp.lcom4_high", CandidateScope{
		RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.Foo",
	})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !ok2 {
		t.Fatal("LatestMatchingOverride ok=false")
	}
	if got.OverrideID != second.OverrideID {
		t.Errorf("LatestMatchingOverride OverrideID=%s, want %s (second/newer row)",
			got.OverrideID, second.OverrideID)
	}
	if got.Mute {
		t.Errorf("LatestMatchingOverride Mute=true; want false (unmute wins)")
	}
}

// TestSQLStore_OverrideIgnoresOtherRulesAndScopes pins the
// JSONB partition predicate of the SQLStore reader -- a row
// with the same scope_filter but DIFFERENT rule_id, OR same
// rule_id but DIFFERENT repo_id, MUST NOT match.
func TestSQLStore_OverrideIgnoresOtherRulesAndScopes(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	other := ScopeFilter{
		RepoID: "repo-b", ScopeKind: ScopeKindClass,
		ScopeSignatureGlob: "com.example.*",
	}
	if err := store.InsertOverride(ctx, Override{
		OverrideID:  uuid.Must(uuid.NewV4()),
		RuleID:      "solid.srp.lcom4_high",
		ScopeFilter: other, Mute: true, Reason: "different repo",
		ActorID:   "alice",
		CreatedAt: sampleClockStart(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Look up the candidate under repo-a -- no row exists
	// under that repo so we must miss.
	_, ok2, err := store.LatestMatchingOverride(ctx, "solid.srp.lcom4_high", CandidateScope{
		RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.Foo",
	})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if ok2 {
		t.Error("LatestMatchingOverride matched across repo_id; SQL should filter by scope_filter->>'repo_id'")
	}
}

// TestSQLStore_OverrideGlobMatchesSubScope pins the
// production glob semantic at the SQL layer: an override
// registered with glob `com.example.legacy.*` is found for any
// concrete signature INSIDE that package. The SQL pre-filter
// narrows to (rule_id, repo_id, scope_kind), then Go applies
// [scopeGlobMatches] over the streamed rows.
func TestSQLStore_OverrideGlobMatchesSubScope(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	if err := store.InsertOverride(ctx, Override{
		OverrideID:  uuid.Must(uuid.NewV4()),
		RuleID:      "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{RepoID: "repo-a", ScopeKind: ScopeKindClass, ScopeSignatureGlob: "com.example.legacy.*"},
		Mute:        true,
		Reason:      "glob mute",
		ActorID:     "alice",
		CreatedAt:   sampleClockStart(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, found, err := store.LatestMatchingOverride(ctx, "solid.srp.lcom4_high", CandidateScope{
		RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.legacy.OrderProcessor",
	})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !found {
		t.Fatal("LatestMatchingOverride found=false; glob com.example.legacy.* should match com.example.legacy.OrderProcessor")
	}
	if !got.Mute {
		t.Errorf("Mute=false, want true")
	}
}

// TestSQLStore_OverrideGlobSkipsNonMatchingRow pins the
// rubber-duck #2 "no LIMIT" critique: when an older row's glob
// matches but a NEWER row's glob does NOT match, the reader
// MUST keep walking and find the older match (rather than
// stopping at the newest scanned row).
func TestSQLStore_OverrideGlobSkipsNonMatchingRow(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	t0 := sampleClockStart()
	// Older: glob matches the candidate.
	if err := store.InsertOverride(ctx, Override{
		OverrideID:  uuid.Must(uuid.NewV4()),
		RuleID:      "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{RepoID: "repo-a", ScopeKind: ScopeKindClass, ScopeSignatureGlob: "com.example.legacy.*"},
		Mute:        true,
		Reason:      "older mute",
		ActorID:     "alice",
		CreatedAt:   t0,
	}); err != nil {
		t.Fatalf("seed older: %v", err)
	}
	// Newer (same partition, different glob): does NOT
	// match the target candidate.
	if err := store.InsertOverride(ctx, Override{
		OverrideID:  uuid.Must(uuid.NewV4()),
		RuleID:      "solid.srp.lcom4_high",
		ScopeFilter: ScopeFilter{RepoID: "repo-a", ScopeKind: ScopeKindClass, ScopeSignatureGlob: "com.example.other.*"},
		Mute:        false,
		Reason:      "",
		ActorID:     "bob",
		CreatedAt:   t0.Add(time.Second),
	}); err != nil {
		t.Fatalf("seed newer: %v", err)
	}
	got, found, err := store.LatestMatchingOverride(ctx, "solid.srp.lcom4_high", CandidateScope{
		RepoID: "repo-a", ScopeKind: ScopeKindClass, Signature: "com.example.legacy.Foo",
	})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !found {
		t.Fatal("found=false; the older matching row was hidden behind a newer non-matching row (LIMIT 1 regression)")
	}
	if got.Reason != "older mute" {
		t.Errorf("Reason=%q, want %q (older row should win after newer is skipped)", got.Reason, "older mute")
	}
}

// TestSQLStore_RuleExistsByID exercises the logical-FK helper
// added in Stage 5.3.
func TestSQLStore_RuleExistsByID(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	hit, err := store.RuleExistsByID(ctx, "solid.srp.lcom4_high")
	if err != nil {
		t.Fatalf("RuleExistsByID: %v", err)
	}
	if !hit {
		t.Error("RuleExistsByID(solid.srp.lcom4_high)=false, want true after seeding")
	}
	miss, err := store.RuleExistsByID(ctx, "no.such.rule")
	if err != nil {
		t.Fatalf("RuleExistsByID(no.such.rule): %v", err)
	}
	if miss {
		t.Error("RuleExistsByID(no.such.rule)=true, want false")
	}
}

// TestSQLStore_ListAllOverrides_RoundTripPreservesEveryField
// pins the Stage 10.2 substrate read at the SQL surface:
// after inserting two mute + one unmute row the table-scan
// SELECT MUST return all three rows ordered
// `(created_at ASC, override_id ASC)` with bit-identical
// scope_filter JSON, NULL-vs-empty Reason handling, and UTC
// CreatedAt. This is the only test that pins the SQL
// implementation of ListAllOverrides; the InMemoryStore test
// in override_test.go pins the semantics.
func TestSQLStore_ListAllOverrides_RoundTripPreservesEveryField(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	ctx := context.Background()
	seedSampleRulesInSQL(t, store)

	base := sampleClockStart()
	rows := []Override{
		{
			OverrideID: uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111")),
			RuleID:     "solid.srp.lcom4_high",
			ScopeFilter: ScopeFilter{
				RepoID: "repo-a", ScopeKind: ScopeKindClass, ScopeSignatureGlob: "com.example.Foo",
			},
			Mute:      true,
			Reason:    "legacy class",
			ActorID:   "alice@example.com",
			CreatedAt: base.Add(-100 * 24 * time.Hour),
		},
		{
			OverrideID: uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
			RuleID:     "solid.srp.lcom4_high",
			ScopeFilter: ScopeFilter{
				RepoID: "repo-a", ScopeKind: ScopeKindClass, ScopeSignatureGlob: "com.example.Foo",
			},
			Mute:      false,
			Reason:    "", // unmute -> SQL NULL bind path
			ActorID:   "bob@example.com",
			CreatedAt: base.Add(-1 * time.Hour),
		},
		{
			OverrideID: uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
			RuleID:     "solid.srp.lcom4_high",
			ScopeFilter: ScopeFilter{
				RepoID: "repo-b", ScopeKind: ScopeKindClass, ScopeSignatureGlob: "com.example.Bar",
			},
			Mute:      true,
			Reason:    "different repo",
			ActorID:   "carol@example.com",
			CreatedAt: base.Add(-50 * 24 * time.Hour),
		},
	}
	// Insert in REVERSE order to verify the SQL ORDER BY is
	// what produces the sort, not the natural-insertion order.
	for i := len(rows) - 1; i >= 0; i-- {
		if err := store.InsertOverride(ctx, rows[i]); err != nil {
			t.Fatalf("InsertOverride[%d]: %v", i, err)
		}
	}

	got, err := store.ListAllOverrides(ctx)
	if err != nil {
		t.Fatalf("ListAllOverrides: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got)=%d, want 3", len(got))
	}
	// Expected order: 100d (id11) -> 50d (id33) -> 1h (id22).
	wantOrder := []string{
		"11111111-1111-1111-1111-111111111111",
		"33333333-3333-3333-3333-333333333333",
		"22222222-2222-2222-2222-222222222222",
	}
	for i, want := range wantOrder {
		if got[i].OverrideID.String() != want {
			t.Errorf("got[%d].OverrideID=%s, want %s", i, got[i].OverrideID, want)
		}
	}
	// Unmute row had empty Reason on insert -> SQL NULL bind ->
	// scan back as empty string (not "null").
	unmute := got[2]
	if unmute.Mute {
		t.Errorf("unmute row Mute=true, want false")
	}
	if unmute.Reason != "" {
		t.Errorf("unmute row Reason=%q, want empty (NULL-bind round-trip)", unmute.Reason)
	}
	// Spot-check the scope_filter JSON round-trip on the
	// cross-repo row.
	cross := got[1]
	if cross.ScopeFilter.RepoID != "repo-b" {
		t.Errorf("cross-repo row RepoID=%q", cross.ScopeFilter.RepoID)
	}
}

// TestSQLStore_ListAllOverrides_EmptyTableReturnsEmptyNonNilSlice
// pins the JSON-stability contract at the SQL surface --
// even with zero rows, the projection MUST get a non-nil
// empty slice so the encoded JSON is `[]`.
func TestSQLStore_ListAllOverrides_EmptyTableReturnsEmptyNonNilSlice(t *testing.T) {
	_, store, ok := openStewardSQLStore(t)
	if !ok {
		return
	}
	got, err := store.ListAllOverrides(context.Background())
	if err != nil {
		t.Fatalf("ListAllOverrides: %v", err)
	}
	if got == nil {
		t.Fatal("got=nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len(got)=%d, want 0", len(got))
	}
}
