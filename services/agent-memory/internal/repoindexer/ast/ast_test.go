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
