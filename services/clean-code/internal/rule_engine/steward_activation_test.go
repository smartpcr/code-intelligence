package rule_engine

import (
	"context"
	"errors"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// stewardReaderFake is a minimal [StewardPolicyReader] stub so
// the activation-adapter tests do not need to stand up a real
// [steward.Steward].
type stewardReaderFake struct {
	pv  steward.PolicyVersion
	ok  bool
	err error
}

func (f *stewardReaderFake) ActivePolicyVersion(ctx context.Context) (steward.PolicyVersion, bool, error) {
	return f.pv, f.ok, f.err
}

func TestStewardActivationReader_HappyPath(t *testing.T) {
	t.Parallel()

	want := uuid.Must(uuid.NewV4())
	fake := &stewardReaderFake{pv: steward.PolicyVersion{PolicyVersionID: want}, ok: true}
	adapter := NewStewardActivation(fake)

	got, ok, err := adapter.ActivePolicyVersionID(context.Background())
	if err != nil {
		t.Fatalf("ActivePolicyVersionID: unexpected err: %v", err)
	}
	if !ok {
		t.Fatalf("ActivePolicyVersionID: ok=false; want true")
	}
	if got != want {
		t.Fatalf("ActivePolicyVersionID: got=%s want=%s", got, want)
	}
}

func TestStewardActivationReader_NoActivation_OkFalse(t *testing.T) {
	t.Parallel()

	fake := &stewardReaderFake{ok: false}
	adapter := NewStewardActivation(fake)

	got, ok, err := adapter.ActivePolicyVersionID(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatalf("ok=true; want false (no activation)")
	}
	if got != uuid.Nil {
		t.Fatalf("got=%s; want zero uuid on ok=false", got)
	}
}

func TestStewardActivationReader_StewardError_Propagated(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("steward unavailable")
	fake := &stewardReaderFake{err: sentinel}
	adapter := NewStewardActivation(fake)

	_, ok, err := adapter.ActivePolicyVersionID(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v; want sentinel %v", err, sentinel)
	}
	if ok {
		t.Fatalf("ok=true on error; want false")
	}
}

// TestStewardActivationReader_ZeroUUID_LoudInvariantViolation
// pins the contract that a steward reply of (ok=true, pv with
// zero uuid) is treated as a loud error, NOT silently as
// ok=false. The latter would hide a steward bug that allows
// an unsigned policy to become "active".
func TestStewardActivationReader_ZeroUUID_LoudInvariantViolation(t *testing.T) {
	t.Parallel()

	fake := &stewardReaderFake{pv: steward.PolicyVersion{PolicyVersionID: uuid.Nil}, ok: true}
	adapter := NewStewardActivation(fake)

	_, ok, err := adapter.ActivePolicyVersionID(context.Background())
	if err == nil {
		t.Fatalf("expected loud error on zero PolicyVersionID with ok=true; got nil")
	}
	if ok {
		t.Fatalf("ok=true on invariant violation; want false")
	}
}

func TestStewardActivationReader_NilReader_OkFalse(t *testing.T) {
	t.Parallel()

	adapter := NewStewardActivation(nil)
	got, ok, err := adapter.ActivePolicyVersionID(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatalf("ok=true on nil reader; want false")
	}
	if got != uuid.Nil {
		t.Fatalf("got=%s; want zero uuid", got)
	}
}

func TestStewardActivationReader_CtxCanceled(t *testing.T) {
	t.Parallel()

	fake := &stewardReaderFake{pv: steward.PolicyVersion{PolicyVersionID: uuid.Must(uuid.NewV4())}, ok: true}
	adapter := NewStewardActivation(fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, ok, err := adapter.ActivePolicyVersionID(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v; want context.Canceled", err)
	}
	if ok {
		t.Fatalf("ok=true on cancelled ctx; want false")
	}
}
