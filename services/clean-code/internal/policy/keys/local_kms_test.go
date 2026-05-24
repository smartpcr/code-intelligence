package keys

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// validMasterKey returns a deterministic 64-char hex master
// key for tests. NEVER use this value in production -- it is
// repository-visible.
func validMasterKey() string {
	// 32 bytes = 64 hex chars. Pin to a fixed pattern so
	// failure messages are reproducible across runs.
	return strings.Repeat("ab", LocalKMSMasterKeyLen)
}

// alternateMasterKey returns a second-but-also-valid master
// key used to prove cross-master sealing fails (one master
// cannot open the other's blob).
func alternateMasterKey() string {
	return strings.Repeat("cd", LocalKMSMasterKeyLen)
}

func TestLocalKMS_NewRejectsMissingMaster(t *testing.T) {
	t.Parallel()
	_, err := NewLocalSealedKMS("")
	if !errors.Is(err, ErrLocalKMSMasterKey) {
		t.Fatalf("NewLocalSealedKMS(\"\"): err=%v; want ErrLocalKMSMasterKey", err)
	}
}

func TestLocalKMS_NewRejectsNonHex(t *testing.T) {
	t.Parallel()
	_, err := NewLocalSealedKMS(strings.Repeat("z", 64))
	if !errors.Is(err, ErrLocalKMSMasterKey) {
		t.Fatalf("NewLocalSealedKMS(non-hex): err=%v; want ErrLocalKMSMasterKey", err)
	}
}

func TestLocalKMS_NewRejectsWrongLength(t *testing.T) {
	t.Parallel()
	_, err := NewLocalSealedKMS(strings.Repeat("ab", LocalKMSMasterKeyLen-1)) // 31 bytes
	if !errors.Is(err, ErrLocalKMSMasterKey) {
		t.Fatalf("NewLocalSealedKMS(31 bytes): err=%v; want ErrLocalKMSMasterKey", err)
	}
}

func TestLocalKMS_RoundTrip(t *testing.T) {
	t.Parallel()
	kms, err := NewLocalSealedKMS(validMasterKey())
	if err != nil {
		t.Fatalf("NewLocalSealedKMS: %v", err)
	}
	ctx := context.Background()
	pub, handle, err := kms.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(pub) != Ed25519PublicKeySize {
		t.Fatalf("pub length=%d, want %d", len(pub), Ed25519PublicKeySize)
	}
	if !strings.HasPrefix(string(handle), localKMSHandlePrefix) {
		t.Fatalf("handle %q lacks prefix %q", string(handle), localKMSHandlePrefix)
	}

	payload := []byte("eval.gate payload v1 required overlap-window-23h-59m")
	sig, err := kms.Sign(ctx, handle, payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != Ed25519SignatureSize {
		t.Fatalf("sig length=%d, want %d", len(sig), Ed25519SignatureSize)
	}
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatal("signature did not verify against the returned public key")
	}
}

func TestLocalKMS_SignTwoKeysHaveDifferentSignatures(t *testing.T) {
	t.Parallel()
	kms, err := NewLocalSealedKMS(validMasterKey())
	if err != nil {
		t.Fatalf("NewLocalSealedKMS: %v", err)
	}
	ctx := context.Background()
	pub1, h1, err := kms.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate 1: %v", err)
	}
	pub2, h2, err := kms.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate 2: %v", err)
	}
	if string(h1) == string(h2) {
		t.Fatalf("two Generate calls returned identical handles")
	}
	if hex.EncodeToString(pub1) == hex.EncodeToString(pub2) {
		t.Fatalf("two Generate calls returned identical public keys")
	}

	payload := []byte("payload")
	s1, err := kms.Sign(ctx, h1, payload)
	if err != nil {
		t.Fatalf("Sign 1: %v", err)
	}
	s2, err := kms.Sign(ctx, h2, payload)
	if err != nil {
		t.Fatalf("Sign 2: %v", err)
	}
	if hex.EncodeToString(s1) == hex.EncodeToString(s2) {
		t.Fatal("two distinct keys produced an identical signature -- the seeds are not distinct")
	}
	if !ed25519.Verify(pub1, payload, s1) {
		t.Fatal("s1 does not verify under pub1")
	}
	if ed25519.Verify(pub2, payload, s1) {
		t.Fatal("s1 verifies under pub2 -- seeds are cross-contaminated")
	}
}

func TestLocalKMS_TamperedHandleFailsToOpen(t *testing.T) {
	t.Parallel()
	kms, err := NewLocalSealedKMS(validMasterKey())
	if err != nil {
		t.Fatalf("NewLocalSealedKMS: %v", err)
	}
	ctx := context.Background()
	_, handle, err := kms.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := strings.TrimPrefix(string(handle), localKMSHandlePrefix)
	// Flip one base64 character mid-blob. AEAD MUST reject.
	if len(body) < 5 {
		t.Fatalf("handle body too short to tamper: %d chars", len(body))
	}
	idx := len(body) / 2
	tampered := []byte(body)
	if tampered[idx] == 'A' {
		tampered[idx] = 'B'
	} else {
		tampered[idx] = 'A'
	}
	bad := KeyHandle(localKMSHandlePrefix + string(tampered))
	if _, err := kms.Sign(ctx, bad, []byte("payload")); err == nil {
		t.Fatal("tampered handle: Sign returned nil err; want AEAD-open failure")
	}
}

func TestLocalKMS_WrongMasterCannotOpen(t *testing.T) {
	t.Parallel()
	primary, err := NewLocalSealedKMS(validMasterKey())
	if err != nil {
		t.Fatalf("primary: %v", err)
	}
	other, err := NewLocalSealedKMS(alternateMasterKey())
	if err != nil {
		t.Fatalf("other: %v", err)
	}
	ctx := context.Background()
	_, handle, err := primary.Generate(ctx)
	if err != nil {
		t.Fatalf("primary.Generate: %v", err)
	}
	if _, err := other.Sign(ctx, handle, []byte("payload")); err == nil {
		t.Fatal("other master Sign returned nil err; want cross-master rejection")
	}
}

func TestLocalKMS_SignRejectsForeignHandle(t *testing.T) {
	t.Parallel()
	kms, err := NewLocalSealedKMS(validMasterKey())
	if err != nil {
		t.Fatalf("NewLocalSealedKMS: %v", err)
	}
	cases := []struct {
		name   string
		handle KeyHandle
	}{
		{"missing prefix", KeyHandle("not-a-local-handle")},
		{"prefix only", KeyHandle(localKMSHandlePrefix)},
		{"bad base64", KeyHandle(localKMSHandlePrefix + "!!!not-base64!!!")},
		{"empty payload base64", KeyHandle(localKMSHandlePrefix + "AAAA")}, // decodes but too short
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := kms.Sign(context.Background(), tc.handle, []byte("payload")); err == nil {
				t.Fatalf("Sign(%s, %q): err=nil; want non-nil", tc.name, string(tc.handle))
			}
		})
	}
}

func TestLocalKMS_PingFailureHook(t *testing.T) {
	t.Parallel()
	kms, err := NewLocalSealedKMS(validMasterKey())
	if err != nil {
		t.Fatalf("NewLocalSealedKMS: %v", err)
	}
	want := errors.New("simulated KMS outage")
	kms.FailPing(want)
	if err := kms.Ping(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Ping after FailPing: err=%v; want %v", err, want)
	}
	kms.FailPing(nil)
	if err := kms.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after FailPing(nil): err=%v; want nil", err)
	}
}

func TestLocalKMS_SatisfiesKMSInterface(t *testing.T) {
	t.Parallel()
	// Compile-time check is already in local_kms.go; this
	// test exercises the constructor + Ping path so the
	// public API is covered at runtime too.
	kms, err := NewLocalSealedKMS(validMasterKey())
	if err != nil {
		t.Fatalf("NewLocalSealedKMS: %v", err)
	}
	var _ KMS = kms
	if err := kms.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
