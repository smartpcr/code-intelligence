package steward

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// canonicalSignedPayload is the in-memory shape that gets
// canonical-JSON-encoded for the [PolicyVersion.Signature]
// computation. The architecture Sec 5.3.3 line 1131 pins the
// signed inputs as `(rule_refs, threshold_refs,
// refactor_weights)`; we group them into a single object so
// the signature covers all three atomically.
//
// CRITICAL: the field order in this struct is IRRELEVANT --
// [canonicalJSON] re-encodes through a sorted-key map below,
// so a future field re-order here cannot change the signed
// bytes. The rubber-duck #2 concern (PostgreSQL `jsonb` round-
// trip) is addressed by NEVER signing the inbound bytes
// directly; we always round-trip through typed structs first.
type canonicalSignedPayload struct {
	RuleRefs        []RuleRef       `json:"rule_refs"`
	ThresholdRefs   []ThresholdRef  `json:"threshold_refs"`
	RefactorWeights RefactorWeights `json:"refactor_weights"`
}

// canonicalJSON returns the deterministic byte serialisation
// of payload used as the signing input. The contract:
//
//  1. Top-level keys appear in lexicographic order
//     (`refactor_weights`, `rule_refs`, `threshold_refs`).
//
//  2. Every nested object has lexicographically-sorted keys
//     -- we render through `map[string]any` via a recursive
//     walk that sorts keys at every depth.
//
//  3. Arrays preserve element order (operator-meaningful for
//     `rule_refs` / `threshold_refs`).
//
//  4. Nil slices and empty slices BOTH render as `[]` (never
//     `null`). Required because a typed-struct round-trip
//     through PostgreSQL `jsonb` (or through a deep-copy in
//     the InMemoryStore) can collapse `[]RuleRef{}` to nil;
//     the canonical bytes must remain identical so the
//     signature still verifies.
//
//  5. No trailing newline, no extra whitespace, no HTML escapes.
//
// The function is deterministic: equal `payload` inputs always
// produce the same `[]byte`. A round-trip through PostgreSQL
// `jsonb` (which normalises whitespace + key order on its own)
// followed by re-canonicalisation is bit-identical to the
// first canonicalisation.
func canonicalJSON(payload canonicalSignedPayload) ([]byte, error) {
	// Normalise nil to empty so the canonical bytes do not
	// depend on whether a caller passes nil or []RuleRef{}.
	// Without this, a deep-copied PolicyVersion whose
	// ThresholdRefs round-trips from []ThresholdRef{} to nil
	// would produce a different signed payload (`null` vs
	// `[]`) and fail VerifyAny.
	if payload.RuleRefs == nil {
		payload.RuleRefs = []RuleRef{}
	}
	if payload.ThresholdRefs == nil {
		payload.ThresholdRefs = []ThresholdRef{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("steward: canonicalJSON: marshal payload: %w", err)
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("steward: canonicalJSON: re-decode: %w", err)
	}
	return canonicalEncode(generic)
}

// canonicalEncode walks v (which must be the output of
// `json.Unmarshal` into a generic `any`) and emits the
// canonical-JSON byte stream described in [canonicalJSON].
//
// The recursion ensures EVERY nested object's keys are sorted,
// not just the top level. This matters because [PolicyVersion]
// inputs include `RefactorWeights` (a nested object) and
// `RuleRef` / `ThresholdRef` (objects inside arrays).
func canonicalEncode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		return encodeString(buf, val)
	case json.Number:
		// `Decoder.UseNumber()` keeps every number as
		// `json.Number` so integer-valued inputs (like
		// `window_days=90`) render without a `.0` tail
		// even when the column round-trips through
		// PostgreSQL `jsonb` as a numeric literal.
		buf.WriteString(string(val))
		return nil
	case float64:
		// Defensive: hit only when a future caller passes
		// a raw float64 instead of decoding via
		// `Decoder.UseNumber`. Render via `json.Marshal`
		// so the formatting matches Go's standard float
		// encoding.
		raw, err := json.Marshal(val)
		if err != nil {
			return fmt.Errorf("steward: canonicalJSON: encode float %v: %w", val, err)
		}
		buf.Write(raw)
		return nil
	case []any:
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	default:
		return fmt.Errorf("steward: canonicalJSON: unsupported type %T", v)
	}
}

// encodeString writes a JSON-encoded string into buf using
// `encoding/json.Encoder` with SetEscapeHTML(false) so the
// canonical bytes match a hand-rolled curl payload (where `<`,
// `>`, and `&` are not escaped).
func encodeString(buf *bytes.Buffer, s string) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("steward: canonicalJSON: encode string: %w", err)
	}
	out := tmp.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	buf.Write(out)
	return nil
}
