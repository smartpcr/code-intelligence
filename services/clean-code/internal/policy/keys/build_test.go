package keys

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestBuild_RejectsEmptyProvider(t *testing.T) {
	t.Parallel()
	_, err := Build(context.Background(), BuildConfig{})
	if err == nil || !strings.Contains(err.Error(), "KMSProvider") {
		t.Fatalf("Build(empty provider): err=%v; want KMSProvider error", err)
	}
}

func TestBuild_RejectsUnknownProvider(t *testing.T) {
	t.Parallel()
	_, err := Build(context.Background(), BuildConfig{KMSProvider: "azure-key-vault"})
	if err == nil || !strings.Contains(err.Error(), "canonical closed set") {
		t.Fatalf("Build(azure-key-vault): err=%v; want closed-set error", err)
	}
}

func TestBuild_LocalRequiresMasterKey(t *testing.T) {
	t.Parallel()
	_, err := Build(context.Background(), BuildConfig{
		KMSProvider: KMSProviderLocal,
		DB:          &sql.DB{}, // non-nil so we don't trip the DB check first
	})
	if err == nil || !strings.Contains(err.Error(), "KMSMasterKeyHex") {
		t.Fatalf("Build(local without master): err=%v; want master-key error", err)
	}
}

func TestBuild_LocalRequiresDB(t *testing.T) {
	t.Parallel()
	_, err := Build(context.Background(), BuildConfig{
		KMSProvider:     KMSProviderLocal,
		KMSMasterKeyHex: validMasterKey(),
		DB:              nil,
	})
	if err == nil || !strings.Contains(err.Error(), "requires a *sql.DB") {
		t.Fatalf("Build(local without DB): err=%v; want DB-required error", err)
	}
}

func TestBuild_InMemoryRejectsDB(t *testing.T) {
	t.Parallel()
	_, err := Build(context.Background(), BuildConfig{
		KMSProvider: KMSProviderInMemory,
		DB:          &sql.DB{},
	})
	if err == nil || !strings.Contains(err.Error(), "must NOT be paired") {
		t.Fatalf("Build(in-memory with DB): err=%v; want pairing error", err)
	}
}

func TestBuild_InMemoryRejectsMasterKey(t *testing.T) {
	t.Parallel()
	_, err := Build(context.Background(), BuildConfig{
		KMSProvider:     KMSProviderInMemory,
		KMSMasterKeyHex: validMasterKey(),
	})
	if err == nil || !strings.Contains(err.Error(), "must NOT be paired with") {
		t.Fatalf("Build(in-memory with master key): err=%v; want pairing error", err)
	}
}

// TestBuild_InMemoryHappyPath proves the scaffold-mode wiring
// (no PG, no master key) works end-to-end: Bootstrap mints a
// first key, the health check returns nil, the manager can
// Sign and Verify.
func TestBuild_InMemoryHappyPath(t *testing.T) {
	t.Parallel()
	res, err := Build(context.Background(), BuildConfig{
		KMSProvider:         KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("Build(in-memory): %v", err)
	}
	defer res.Close()
	if res.Manager == nil {
		t.Fatal("Manager is nil")
	}
	if res.HealthCheck == nil {
		t.Fatal("HealthCheck is nil")
	}
	if err := res.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	views, err := res.Manager.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("ListActive after MintFirstKey: len=%d, want 1", len(views))
	}
	// Sign + Verify smoke test.
	payload := []byte("payload")
	id, sig, err := res.Manager.Sign(context.Background(), payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := res.Manager.Verify(context.Background(), id, payload, sig); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestBuild_LocalKMSValidatesMasterKey proves the local
// provider rejects a malformed master key (forwarding the
// LocalSealedKMS error). DB nil is allowed for this check
// because the master-key validation fires FIRST -- but we
// inject a non-nil DB to confirm.
func TestBuild_LocalKMSValidatesMasterKey(t *testing.T) {
	t.Parallel()
	_, err := Build(context.Background(), BuildConfig{
		KMSProvider:     KMSProviderLocal,
		KMSMasterKeyHex: "deadbeef", // 8 chars -- wrong length
		DB:              &sql.DB{},  // satisfy DB-required check
	})
	if err == nil {
		t.Fatal("Build(local with bad master): err=nil; want master-key length error")
	}
	if !errors.Is(err, ErrLocalKMSMasterKey) {
		t.Fatalf("Build: err=%v; want errors.Is ErrLocalKMSMasterKey", err)
	}
}
