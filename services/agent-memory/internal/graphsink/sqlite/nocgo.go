//go:build !cgo

// Package sqlite intentionally fails to compile when CGO is
// disabled. The SQLite Sink backend depends on
// `github.com/mattn/go-sqlite3`, a CGO driver -- a CGO=0 build
// would link but every Sink method would fail at runtime the
// first time it tried to open a database file. Failing fast at
// compile time, with a clear error message identifying the
// missing toolchain requirement, is much friendlier than the
// runtime "sql: unknown driver" surprise.
//
// Tech-spec C7 / §4.3 already mandates CGO=1 for the tree-sitter
// parsers in `internal/repoindexer/ast/parsers_cgo.go`, so the
// codeintel binary already requires a working C toolchain. This
// file exists only to make the requirement enforceable at
// `internal/graphsink/sqlite`'s import boundary.
package sqlite

// The empty const reference below triggers a compile-time
// "undefined" error under CGO=0 with the message text the
// operator needs to see in their build log. The error from the
// Go compiler will read roughly:
//
//   internal/graphsink/sqlite/nocgo.go:NN:14: undefined:
//   graphsinkSqliteRequiresCgoEnabled
//
// which is searchable and self-explanatory.
var _ = graphsinkSqliteRequiresCgoEnabled
