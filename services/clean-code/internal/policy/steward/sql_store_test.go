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

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
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
