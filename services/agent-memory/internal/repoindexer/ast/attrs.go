//go:build canonical_dispatcher

// Stub MergeLangMeta declaration; the canonical implementation lives in
// method_attrs.go and is unconditionally compiled. This stub is gated behind
// the never-enabled `canonical_dispatcher` build tag so the package builds
// without symbol-collision errors. See dispatcher.go / method_attrs.go for the
// canonical contract.
package ast