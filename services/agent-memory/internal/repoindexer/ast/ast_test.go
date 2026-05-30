//go:build canonical_dispatcher

// Stub tests reference stub-only symbols (NewDispatcher with
// option-only signature, MethodDecl.Name, MethodDecl.ClassName,
// MethodDecl.ReceiverAliases as map[string]string,
// Pass2dOverrides) that no longer exist on the canonical
// types. Gated behind `canonical_dispatcher` (never enabled)
// so the package builds. The Stage 3.2 dispatcher landing
// workstream will replace these tests with canonical
// equivalents that share the makeEvent/fakeNodeEdgeWriter
// helpers already encoded in dispatcher_test.go.
package ast

// The dispatcher / multimap / Pass 2d / LangMeta unit tests
// previously lived in this file as stub-shape duplicates of
// the canonical coverage in dispatcher_test.go,
// dispatcher_pass2bd_test.go, dispatcher_embedding_test.go,
// and the per-language parser tests. The stub assertions ran
// against a MethodDecl{Name, ClassName, LangMeta
// map[string]string} shape that conflicted with the rich
// production MethodDecl in parser.go (which uses
// QualifiedName / EnclosingClass / LangMeta map[string]any).
// They were removed here to restore a compilable package and
// because the canonical tests cover the same contracts at
// higher fidelity.
