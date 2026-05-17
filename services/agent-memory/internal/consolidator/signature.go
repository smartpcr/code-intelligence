package consolidator

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// observationKey is a (role, fingerprint) pair extracted from
// one Observation row. The role string is the schema's
// observation_role enum literal as text. The fingerprint is the
// 32-byte canonical hash of the targeted Node / Edge / Concept
// row (G2: every Node/Edge/Concept carries a `fingerprint bytea`
// per migrations 0003 / 0011 with octet_length=32). Using the
// fingerprint -- NOT the per-repo uuid id of the target row --
// is what makes two Episodes from different repos that touch
// the same canonical Node element produce the same signature,
// which is the architectural pre-condition for the G6 cross-
// repo Concept (architecture.md §3.4 step 5).
//
// For the rare `degraded_recall_context` role, the target is a
// recall_context_log row which has no fingerprint column. The
// scanEpisodes loop falls back to hashing the recall_context_id
// uuid bytes; cross-repo collision is not meaningful for
// audit-only recall-context references so this fallback does
// not weaken G6.
//
// The signature pre-image deliberately omits Observation.weight
// (the per-Episode contribution scalar) and Observation.created_at:
//
//   - `weight` is a continuous-valued contribution metric (arch
//     §5.3.3); folding it into the signature would split every
//     signature into weight-noise-cardinality-many buckets and
//     starve Concept crystallisation.
//
//   - `created_at` is the partition-key wall-clock; including it
//     would make every Episode trivially distinct (G4 append-
//     only timestamps are by definition unique per row) and
//     degenerate the grouping to "one Concept per Episode".
type observationKey struct {
	role        string
	fingerprint []byte
}

// computeSignature returns the deterministic 32-byte hash of the
// observation set for one Episode. The pre-image is
// `sort_unique([role || ':' || hex(fingerprint) for obs in episode]).joined("\n")`
// -- sorted (order-independent) and deduplicated (a duplicate
// observation does not change the signature). The hex encoding
// of the fingerprint avoids the raw-byte-concatenation ambiguity
// the rubber-duck reviewer flagged at iter 2 (different (role,
// fingerprint) splits could otherwise collide if the role-byte
// boundary fell inside the fingerprint bytes).
//
// Returns the all-zero hash AND `nonEmpty=false` when keys is
// empty (or every key has an empty fingerprint). Callers MUST
// treat that as "no signature; do not emit" because the
// schema's concept_fingerprint_octet_length_chk constraint
// admits any 32-byte sequence -- the all-zero hash is a legal
// byte string and we do NOT want N observation-less Episodes
// from across the cluster colliding into one bogus "empty"
// Concept (G6 says cross-repo collisions on fingerprint are
// CORRECT; an all-zero fingerprint shared by every observation-
// less Episode is the pathological degenerate case the §3.4
// step-5 architectural intent is NOT asking for).
func computeSignature(keys []observationKey) (sig [32]byte, nonEmpty bool) {
	if len(keys) == 0 {
		return sig, false
	}

	encoded := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if len(k.fingerprint) == 0 {
			// Defensive: an observation that didn't resolve
			// to a fingerprint contributes nothing. Skipping
			// keeps the signature stable across schema
			// evolutions that might add an opaque-target role
			// without a fingerprint column yet.
			continue
		}
		joined := k.role + ":" + hex.EncodeToString(k.fingerprint)
		if _, ok := seen[joined]; ok {
			continue
		}
		seen[joined] = struct{}{}
		encoded = append(encoded, joined)
	}
	if len(encoded) == 0 {
		return sig, false
	}
	sort.Strings(encoded)

	h := sha256.New()
	for i, s := range encoded {
		if i > 0 {
			_, _ = h.Write([]byte{'\n'})
		}
		_, _ = h.Write([]byte(s))
	}
	copy(sig[:], h.Sum(nil))
	return sig, true
}
