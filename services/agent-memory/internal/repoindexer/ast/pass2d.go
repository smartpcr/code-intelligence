//go:build canonical_dispatcher

// Stub Pass2dOverrides uses stub MethodDecl field names
// (`m.Name`, `m.ClassName`, `m.LangMeta map[string]string`)
// that do not exist on the canonical types in parser.go.
// Gated behind `canonical_dispatcher` (never enabled) so the
// package builds. The canonical pass-2d implementation will
// land with the Stage 3.2 dispatcher workstream.
package ast

// The Stage 3.2 dispatcher's Pass 2d trait-override emission
// (see dispatcher.go) reads MethodDecl.LangMeta["trait"] and
// emits the matching overrides edge against the rich
// MethodDecl shape declared in parser.go. An earlier story
// stage briefly introduced a stripped-down Pass2dOverrides
// helper in this file that operated on a stub MethodDecl
// type (Name / ClassName / LangMeta map[string]string). The
// helper was a duplicate of the dispatcher's Pass 2d logic
// and conflicted with the production MethodDecl.LangMeta
// shape (map[string]any), so it was removed here to restore
// a compilable package. New Pass 2 helpers belong inside the
// dispatcher in dispatcher.go.
