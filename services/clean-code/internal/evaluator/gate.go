package evaluator

import (
	"context"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
)

// ErrPolicySignatureInvalid is the canonical sentinel the gate
// returns whenever a signed bundle fails verification. The
// underlying reason (unknown key id, expired window, bad
// signature) is wrapped via fmt.Errorf so callers can inspect
// it with errors.Is for triage but the sentinel is what the
// blast-radius-aware caller (the rule pipeline) keys off.
var ErrPolicySignatureInvalid = errors.New("evaluator: policy signature invalid")

// ErrGateUnwired signals a composition-root mistake: the gate
// was constructed without a [keys.Manager]. Returned by every
// verification entry point so a wiring bug is caught at the
// first signed bundle rather than via a nil pointer panic.
var ErrGateUnwired = errors.New("evaluator: gate has no signing key manager")

// PolicySignature is the canonical envelope the Policy Steward
// publishes alongside a rulepack bundle. Fields:
//
//   - KeyID: the signing key the steward used; verified against
//     the active set. Stage 5.1 brief Sec 8.4 requires this be
//     persisted into the audit trail so a post-hoc compromise
//     investigation can identify exactly which key signed each
//     bundle.
//   - Payload: the canonical bytes the steward signed. Callers
//     supply the same bytes here that they hashed pre-publish;
//     the gate does NOT re-canonicalize, that's the steward's
//     contract.
//   - Signature: the Ed25519 signature over Payload. 64 bytes.
type PolicySignature struct {
	KeyID     uuid.UUID
	Payload   []byte
	Signature []byte
}

// Gate is the verification surface for signed policy bundles.
// Constructed once at startup by the composition root and
// invoked by every consumer that receives a policy bundle.
type Gate struct {
	keys *keys.Manager

	// Evaluate-path dependencies. All nil on a gate built
	// via [NewGate]; populated by [NewGateWithEngine].
	engine        RuleEngine
	readiness     SampleReadinessReader
	policyReader  PolicyVersionReader
	sigVerify     PolicySignatureVerifier
	degradedStore DegradedRunStore
	activation    PolicyActivationReader
	newID         IDMinter
	now           func() int64
}

// NewGate wires the gate to a [keys.Manager]. mgr MAY be nil
// for scaffold-mode bring-ups; in that case every verification
// method returns [ErrGateUnwired].
func NewGate(mgr *keys.Manager) *Gate {
	return &Gate{keys: mgr}
}

// VerifyPolicy verifies a [PolicySignature] against the signing
// key identified by `sig.KeyID`. Returns nil on success;
// [ErrPolicySignatureInvalid] wrapping the underlying error
// otherwise.
//
// The 24h overlap window from tech-spec Sec 8.2 is enforced
// implicitly: [keys.Manager.Verify] only accepts a key while
// `now ∈ [valid_from, valid_until)`. During the overlap both
// the new and the old key are inside their windows
// simultaneously so both signatures verify -- that's the whole
// point of the overlap.
//
// Pre-condition: sig.KeyID, sig.Payload, sig.Signature all
// non-zero. The gate fails closed on any of those being empty
// (rather than passing through to a crypto library that might
// be lenient with a 0-byte input).
func (g *Gate) VerifyPolicy(ctx context.Context, sig PolicySignature) error {
	if g == nil || g.keys == nil {
		return ErrGateUnwired
	}
	if sig.KeyID.IsNil() {
		return fmt.Errorf("%w: key_id is zero uuid", ErrPolicySignatureInvalid)
	}
	if len(sig.Payload) == 0 {
		return fmt.Errorf("%w: payload is empty", ErrPolicySignatureInvalid)
	}
	if len(sig.Signature) == 0 {
		return fmt.Errorf("%w: signature is empty", ErrPolicySignatureInvalid)
	}
	if err := g.keys.Verify(ctx, sig.KeyID, sig.Payload, sig.Signature); err != nil {
		return fmt.Errorf("%w: %v", ErrPolicySignatureInvalid, err)
	}
	return nil
}

// VerifyAnyPolicySignature attempts verification against every
// active key in the cache and returns the matching key_id on
// success. Useful for legacy / unsigned-KID callers that did
// not record the signing key with the payload; new producers
// SHOULD use [VerifyPolicy] with an explicit key_id so the
// audit trail can attribute the signature.
func (g *Gate) VerifyAnyPolicySignature(ctx context.Context, payload, signature []byte) (uuid.UUID, error) {
	if g == nil || g.keys == nil {
		return uuid.Nil, ErrGateUnwired
	}
	if len(payload) == 0 {
		return uuid.Nil, fmt.Errorf("%w: payload is empty", ErrPolicySignatureInvalid)
	}
	if len(signature) == 0 {
		return uuid.Nil, fmt.Errorf("%w: signature is empty", ErrPolicySignatureInvalid)
	}
	keyID, err := g.keys.VerifyAny(ctx, payload, signature)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %v", ErrPolicySignatureInvalid, err)
	}
	return keyID, nil
}
