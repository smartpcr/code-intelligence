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
