package webhook_test

import (
	"errors"
	"strings"
	"testing"

	"forge/services/clean-code/internal/ingest/webhook"
)

// TestStaticSecretResolver_RoundTrip pins the happy-path
// resolution: a registered key id returns the canonical
// secret.
func TestStaticSecretResolver_RoundTrip(t *testing.T) {
	t.Parallel()
	secret := []byte("clean-coded-test-hmac-secret-32-bytes!")
	r := webhook.NewStaticSecretResolver(map[string][]byte{
		"kv-prod-2026-q1": secret,
	})
	got, err := r.Resolve("kv-prod-2026-q1")
	if err != nil {
		t.Fatalf("Resolve: unexpected err %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("Resolve: got %q, want %q", got, secret)
	}
}

// TestStaticSecretResolver_UnknownKeyIDReturnsSentinel pins
// the rubber-duck iter-1 #4 contract: a SecretResolver MUST
// distinguish "id not registered" from "lookup error" so the
// Router can map the former to 401 and the latter to 500.
func TestStaticSecretResolver_UnknownKeyIDReturnsSentinel(t *testing.T) {
	t.Parallel()
	r := webhook.NewStaticSecretResolver(map[string][]byte{
		"kv-prod-2026-q1": []byte("secret"),
	})
	_, err := r.Resolve("kv-typo-2026-q1")
	if err == nil {
		t.Fatalf("Resolve: want error, got nil")
	}
	if !errors.Is(err, webhook.ErrUnknownSigningKeyID) {
		t.Fatalf("Resolve: want ErrUnknownSigningKeyID, got %v", err)
	}
}

// TestStaticSecretResolver_RotationAddAndRemove pins the
// rotation primitives the operator runbook drives during
// the tech-spec Sec 8.2 row 6 24h overlap. Both keys
// resolve successfully during the overlap; after Remove the
// old key returns the unknown sentinel.
func TestStaticSecretResolver_RotationAddAndRemove(t *testing.T) {
	t.Parallel()
	r := webhook.NewStaticSecretResolver(map[string][]byte{
		"kv-2025-q4": []byte("old-secret"),
	})
	r.Add("kv-2026-q1", []byte("new-secret"))

	for _, id := range []string{"kv-2025-q4", "kv-2026-q1"} {
		if _, err := r.Resolve(id); err != nil {
			t.Errorf("Resolve %s: want nil err during overlap, got %v", id, err)
		}
	}

	if !r.Remove("kv-2025-q4") {
		t.Errorf("Remove: want true (existing key), got false")
	}
	if _, err := r.Resolve("kv-2025-q4"); !errors.Is(err, webhook.ErrUnknownSigningKeyID) {
		t.Errorf("Resolve after Remove: want ErrUnknownSigningKeyID, got %v", err)
	}
	if r.Remove("kv-2025-q4") {
		t.Errorf("Remove (second): want false (already removed), got true")
	}
}

// TestStaticSecretResolver_DefensiveCopyOnRead asserts the
// returned secret bytes are a copy: a caller mutating the
// returned slice MUST NOT alter the stored secret.
func TestStaticSecretResolver_DefensiveCopyOnRead(t *testing.T) {
	t.Parallel()
	r := webhook.NewStaticSecretResolver(map[string][]byte{
		"k1": []byte("original-secret"),
	})
	got1, _ := r.Resolve("k1")
	for i := range got1 {
		got1[i] = 0x00
	}
	got2, _ := r.Resolve("k1")
	if string(got2) != "original-secret" {
		t.Fatalf("Resolve after caller mutation: want %q, got %q", "original-secret", got2)
	}
}

// TestStaticSecretResolver_DefensiveCopyOnConstruct asserts
// that mutating the seed map's secret slice after
// construction does NOT poison the resolver.
func TestStaticSecretResolver_DefensiveCopyOnConstruct(t *testing.T) {
	t.Parallel()
	seed := []byte("original-secret")
	r := webhook.NewStaticSecretResolver(map[string][]byte{"k1": seed})
	for i := range seed {
		seed[i] = 0x00
	}
	got, _ := r.Resolve("k1")
	if string(got) != "original-secret" {
		t.Fatalf("Resolve after seed mutation: want %q, got %q", "original-secret", got)
	}
}

// TestNewStaticSecretResolver_PanicsOnEmptyInputs pins the
// "fail loudly" wiring guard.
func TestNewStaticSecretResolver_PanicsOnEmptyInputs(t *testing.T) {
	t.Parallel()
	t.Run("empty key id", func(t *testing.T) {
		defer expectPanic(t, "empty signing_key_id")
		_ = webhook.NewStaticSecretResolver(map[string][]byte{"": []byte("x")})
	})
	t.Run("empty secret", func(t *testing.T) {
		defer expectPanic(t, "empty secret")
		_ = webhook.NewStaticSecretResolver(map[string][]byte{"k1": nil})
	})
}

// TestValidateSigningKeyID_ClosedSet pins the shape contract.
func TestValidateSigningKeyID_ClosedSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want error
	}{
		{name: "happy ascii", in: "kv-prod-2026-q1", want: nil},
		{name: "happy uuid", in: "5cef9af1-93f1-4cca-9b6a-0f9e9c4ac7c2", want: nil},
		{name: "empty", in: "", want: webhook.ErrSigningKeyIDMissing},
		{name: "too long", in: strings.Repeat("a", webhook.MaxSigningKeyIDLength+1), want: webhook.ErrSigningKeyIDMalformed},
		{name: "control byte", in: "kv\x01prod", want: webhook.ErrSigningKeyIDMalformed},
		{name: "newline injection", in: "kv\nX-Injected: 1", want: webhook.ErrSigningKeyIDMalformed},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := webhook.ValidateSigningKeyID(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Errorf("got %v; want nil", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Errorf("got %v; want %v", got, tc.want)
			}
		})
	}
}

// TestStaticSecretResolver_Len pins the inspector helper.
func TestStaticSecretResolver_Len(t *testing.T) {
	t.Parallel()
	r := webhook.NewStaticSecretResolver(nil)
	if r.Len() != 0 {
		t.Errorf("Len on empty: want 0, got %d", r.Len())
	}
	r.Add("k1", []byte("s1"))
	r.Add("k2", []byte("s2"))
	if r.Len() != 2 {
		t.Errorf("Len after 2 Add: want 2, got %d", r.Len())
	}
	r.Remove("k1")
	if r.Len() != 1 {
		t.Errorf("Len after 1 Remove: want 1, got %d", r.Len())
	}
}

func expectPanic(t *testing.T, msgFragment string) {
	t.Helper()
	r := recover()
	if r == nil {
		t.Fatalf("expected panic containing %q, got none", msgFragment)
	}
	if msg, ok := r.(string); ok {
		if !strings.Contains(msg, msgFragment) {
			t.Errorf("panic message %q does not contain %q", msg, msgFragment)
		}
		return
	}
	if err, ok := r.(error); ok {
		if !strings.Contains(err.Error(), msgFragment) {
			t.Errorf("panic error %q does not contain %q", err.Error(), msgFragment)
		}
		return
	}
	t.Errorf("panic with unexpected type %T: %v", r, r)
}
