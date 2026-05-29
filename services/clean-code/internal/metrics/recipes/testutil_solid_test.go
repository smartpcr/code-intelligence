package recipes_test

import (
	"forge/services/clean-code/internal/ast/parser"
	astv1 "forge/services/clean-code/internal/ast/v1"
)

// This file augments [testutil_test.go] with helpers that the
// SOLID-pack recipes (`lcom4`, `fan_in`, `fan_out`) need but
// the base-pack recipes did not: classes, fields, call edges,
// field-access edges. Kept in a separate file so the base-pack
// helpers stay focused.

// withCallEdges flips the file-level `call_edges` capability
// stamp -- the canonical [recipes.AttrCallEdges] flag used by
// `fan_in` / `fan_out` AppliesTo gates. Returns the receiver
// for chaining.
func (b *astBuilder) withCallEdges() *astBuilder {
	if b.file.Attrs == nil {
		b.file.Attrs = map[string]string{}
	}
	b.file.Attrs["call_edges"] = "true"
	return b
}

// withFieldAccesses flips the file-level `field_accesses`
// capability stamp -- the canonical [recipes.AttrFieldAccesses]
// flag used by `lcom4` AppliesTo.
func (b *astBuilder) withFieldAccesses() *astBuilder {
	if b.file.Attrs == nil {
		b.file.Attrs = map[string]string{}
	}
	b.file.Attrs["field_accesses"] = "true"
	return b
}

// addClass appends a `SCOPE_KIND_CLASS` scope under `parent`
// (defaults to the file). Used by lcom4 / fan_in / fan_out
// tests where the class is the load-bearing scope.
func (b *astBuilder) addClass(name string, parentID string) *astv1.AstScope {
	if parentID == "" {
		parentID = b.currentFile.GetScopeId()
	}
	c := &astv1.AstScope{
		ScopeKind:     parser.ScopeKindClass,
		Name:          name,
		QualifiedName: b.currentFile.GetQualifiedName() + "." + name,
		ParentScopeId: parentID,
		Range:         freshRange(b.nextOrdinal),
	}
	b.assignID(c)
	b.file.Scopes = append(b.file.Scopes, c)
	b.scopesByName[name] = c
	return c
}

// addInterface appends a `SCOPE_KIND_INTERFACE` scope under
// `parent` (defaults to the file). Used by lcom4 tests to
// model nested interfaces (Java `interface I {}` declared
// inside a class body emits an interface scope whose
// parent is the enclosing class). The lcom4 recipe's
// `methodsOfClass` walk must stop at this boundary so a
// default method on the inner interface is NOT attributed
// to the outer class.
func (b *astBuilder) addInterface(name string, parentID string) *astv1.AstScope {
	if parentID == "" {
		parentID = b.currentFile.GetScopeId()
	}
	c := &astv1.AstScope{
		ScopeKind:     parser.ScopeKindInterface,
		Name:          name,
		QualifiedName: b.currentFile.GetQualifiedName() + "." + name,
		ParentScopeId: parentID,
		Range:         freshRange(b.nextOrdinal),
	}
	b.assignID(c)
	b.file.Scopes = append(b.file.Scopes, c)
	b.scopesByName[name] = c
	return c
}

// addField appends an `AstSymbol(Kind="field")` whose
// `ScopeId` is the OWNING class (NOT a method) -- the canonical
// shape lcom4 recognises as a class-level instance field.
// Returns the synthetic `symbol_id` so a caller can wire a
// field-access edge to it.
func (b *astBuilder) addField(name string, classID string) *astv1.AstSymbol {
	sym := &astv1.AstSymbol{
		SymbolId: "sym:" + name + ":" + itoa(b.nextOrdinal),
		Name:     name,
		Kind:     "field",
		ScopeId:  classID,
		Range:    freshRange(b.nextOrdinal),
	}
	b.nextOrdinal++
	b.file.Symbols = append(b.file.Symbols, sym)
	return sym
}

// addCallEdge appends an `AstEdge(Kind="calls")` from one
// scope to another. Used by fan_in / fan_out tests where the
// edge IS the load-bearing input. `fromKind` / `toKind` let
// the test build either SCOPE refs (caller is a method scope)
// or external SYMBOL refs.
func (b *astBuilder) addCallEdge(fromScopeID, toScopeID string) *astv1.AstEdge {
	e := &astv1.AstEdge{
		EdgeId: "edge:" + itoa(b.nextOrdinal),
		Kind:   "calls",
		From:   &astv1.AstRef{Kind: parser.RefKindScope, Id: fromScopeID},
		To:     &astv1.AstRef{Kind: parser.RefKindScope, Id: toScopeID},
	}
	b.nextOrdinal++
	b.file.Edges = append(b.file.Edges, e)
	return e
}

// addCallEdgeToExternal appends a `calls` edge whose target
// is a SYMBOL ref pointing at a symbol NOT in this file --
// the canonical shape for "this method calls something
// declared in another file". Used by fan_out tests to verify
// external targets count as outbound coupling.
func (b *astBuilder) addCallEdgeToExternal(fromScopeID, externalSymbolID string) *astv1.AstEdge {
	e := &astv1.AstEdge{
		EdgeId: "edge:" + itoa(b.nextOrdinal),
		Kind:   "calls",
		From:   &astv1.AstRef{Kind: parser.RefKindScope, Id: fromScopeID},
		To:     &astv1.AstRef{Kind: parser.RefKindSymbol, Id: externalSymbolID},
	}
	b.nextOrdinal++
	b.file.Edges = append(b.file.Edges, e)
	return e
}

// addCallEdgeFromExternal appends a `calls` edge whose
// SOURCE is a SYMBOL ref pointing at a symbol NOT in this
// file -- the canonical shape for "something declared in
// another file calls a scope in this file". Used by fan_in
// tests to verify external callers count as inbound coupling
// at every emitted scope (method, class, file).
//
// Mirror of [astBuilder.addCallEdgeToExternal]: `from`
// external, `to` local. The producer-side contract here is
// that a parser advertising the `call_edges` capability may
// surface cross-file inbound calls it resolved (e.g. when
// the parser has multi-file context) in THIS file's edge
// list; fan_in counts them as the authoritative value of
// the computed-tier row at this SHA per architecture
// Sec 5.2.1 lines 909-921 (G3: computed rows are immutable
// once written).
func (b *astBuilder) addCallEdgeFromExternal(externalSymbolID, toScopeID string) *astv1.AstEdge {
	e := &astv1.AstEdge{
		EdgeId: "edge:" + itoa(b.nextOrdinal),
		Kind:   "calls",
		From:   &astv1.AstRef{Kind: parser.RefKindSymbol, Id: externalSymbolID},
		To:     &astv1.AstRef{Kind: parser.RefKindScope, Id: toScopeID},
	}
	b.nextOrdinal++
	b.file.Edges = append(b.file.Edges, e)
	return e
}

// addFieldAccessEdge appends an `AstEdge(Kind=kind)` from a
// caller scope to a field symbol -- the canonical shape for
// a method-reads-a-class-field event. `kind` MUST be one of
// `reads_field`, `writes_field`, `uses_field` for the lcom4
// recipe to count it.
func (b *astBuilder) addFieldAccessEdge(kind string, fromScopeID, fieldSymbolID string) *astv1.AstEdge {
	e := &astv1.AstEdge{
		EdgeId: "edge:" + itoa(b.nextOrdinal),
		Kind:   kind,
		From:   &astv1.AstRef{Kind: parser.RefKindScope, Id: fromScopeID},
		To:     &astv1.AstRef{Kind: parser.RefKindSymbol, Id: fieldSymbolID},
	}
	b.nextOrdinal++
	b.file.Edges = append(b.file.Edges, e)
	return e
}
