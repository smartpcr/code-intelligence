package ast

import "encoding/json"

// MergeLangMeta folds parser-supplied LangMeta into the attrs map.
// First-class keys (language, start_line, end_line, params_raw,
// calls_raw, enclosing_class, modifiers) are never overwritten —
// the dispatcher's authoritative values always win (architecture
// Section 4.4.2 + C11).
func MergeLangMeta(attrs map[string]any, langMeta map[string]any) {
	if langMeta == nil {
		return
	}
	for k, v := range langMeta {
		if _, exists := attrs[k]; exists {
			continue // first-class or previously-set key — do NOT override
		}
		attrs[k] = v
	}
}

// MergeCallsDeduped merges Calls and ReceiverCalls into a single
// deduped slice preserving insertion order of first occurrence.
func MergeCallsDeduped(calls, receiverCalls []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, c := range calls {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, c := range receiverCalls {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// BuildMethodAttrs produces the attrs_json that the graph-writer
// persists for a MethodDecl node.  It:
//  1. Sets first-class keys (language, start_line, end_line, etc.)
//  2. Merges Calls + ReceiverCalls into calls_raw (deduped)
//  3. Calls MergeLangMeta to fold parser-specific metadata
//
// The result is returned as json.RawMessage ready for persistence.
func BuildMethodAttrs(language string, m MethodDecl) json.RawMessage {
	attrs := map[string]any{
		"language":   language,
		"start_line": m.StartLine,
		"end_line":   m.EndLine,
		"params_raw": m.ParamSignature,
	}
	if m.EnclosingClass != "" {
		attrs["enclosing_class"] = m.EnclosingClass
	}
	if len(m.Modifiers) > 0 {
		mods := make([]string, len(m.Modifiers))
		copy(mods, m.Modifiers)
		attrs["modifiers"] = mods
	}

	// Merge Calls + ReceiverCalls into calls_raw.  CRITICAL: must NOT
	// gate on len(m.Calls) > 0 — ReceiverCalls alone must still emit
	// calls_raw.
	merged := MergeCallsDeduped(m.Calls, m.ReceiverCalls)
	if len(merged) > 0 {
		attrs["calls_raw"] = merged
	}

	// Fold parser-supplied LangMeta; first-class keys win.
	MergeLangMeta(attrs, m.LangMeta)

	b, _ := json.Marshal(attrs)
	return json.RawMessage(b)
}
