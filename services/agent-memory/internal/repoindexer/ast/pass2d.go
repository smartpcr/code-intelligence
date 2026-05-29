package ast

import (
	"encoding/json"
)

// Pass2dOverrides inserts "overrides" edges when an impl method's LangMeta
// contains a "trait" key and the corresponding trait method exists in the
// same file's methodNodeID map.
//
// Returns:
//   - edges: overrides edges for resolved trait→impl pairs.
//   - attrsJSON: for unresolved pairs (cross-file miss), the impl method's
//     LangMeta is serialised as JSON and keyed by "ClassName.MethodName".
//     The trait name is preserved on attrs_json so downstream passes or
//     queries can still discover the relationship.
func Pass2dOverrides(pr ParseResult, methodNodeID map[string]string) ([]Edge, map[string]string) {
	var edges []Edge
	attrsJSON := make(map[string]string)

	for _, m := range pr.Methods {
		if m.LangMeta == nil {
			continue
		}
		traitNameRaw, ok := m.LangMeta["trait"]
		if !ok {
			continue
		}
		traitName, _ := traitNameRaw.(string)
		if traitName == "" {
			continue
		}

		// m.QualifiedName is already dotted (e.g. "MyStruct.bar"),
		// so extract the simple method name before composing the
		// trait-side and impl-side lookup keys — otherwise we get
		// double-prefixed keys like "MyTrait.MyStruct.bar" and
		// "MyStruct.MyStruct.bar" that never resolve. Matches the
		// dispatcher's inline Pass 2d (dispatcher.go ~line 800).
		simpleName := lastDottedSegment(m.QualifiedName)
		traitMethodKey := traitName + "." + simpleName
		implMethodKey := m.EnclosingClass + "." + simpleName

		traitNodeID, traitFound := methodNodeID[traitMethodKey]
		implNodeID, implFound := methodNodeID[implMethodKey]

		if traitFound && implFound {
			edges = append(edges, Edge{
				Kind:   "overrides",
				Source: implNodeID,
				Target: traitNodeID,
			})
		} else {
			// Cross-file miss: preserve trait on attrs_json, emit no edge.
			raw, _ := json.Marshal(m.LangMeta)
			attrsJSON[implMethodKey] = string(raw)
		}
	}

	return edges, attrsJSON
}
