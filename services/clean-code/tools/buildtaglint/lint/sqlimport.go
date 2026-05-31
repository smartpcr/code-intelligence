// -----------------------------------------------------------------------
// <copyright file="sqlimport.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package lint

import (
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// SQLImportAnalyzer implements `no-production-sql-import`
// (tech-spec REFACTOR-GUIDE Sec 8.10).
//
// For every file in the analyzed package, the analyzer
// walks the import list and reports a diagnostic when an
// import path is either:
//
//   - exactly `database/sql`, or
//   - ends in `_sql_store` (the repo's convention for
//     PostgreSQL-backed reader/writer adapters such as
//     `<...>/internal/rule_engine/sql_store.go`), or
//   - ends in `/sql_store` (defensive: catches a future
//     refactor that moves the adapter into a dedicated
//     sub-package named `sql_store`).
//
// SCOPE -- the analyzer itself does NOT gate by package
// path. Scope is enforced by the `make lint-cli` target,
// which only passes `./cmd/cleanc/...` and `./internal/cli/...`
// to `go vet`. This separation keeps the analyzer's
// behaviour predictable when invoked directly (e.g.
// `go vet -vettool=...` from an editor integration) and
// allows the same analyzer to be re-used by future scopes
// without recompilation.
//
// Why a custom analyzer (and not just `forbidigo`):
//
// `forbidigo` matches identifier USES, which means a blank
// import (`import _ "database/sql"`) or a side-effect-only
// import slips past it. The evaluator (iter 1, item #2)
// called out this gap explicitly. This analyzer walks the
// AST's import list directly, so blank and side-effect
// imports fire the rule.
//
// Diagnostic format always contains the literal substring
// `no-production-sql-import` so `make lint-cli` greps and
// CI log searches both surface the rule name (constraint
// shared with `BuildTagAnalyzer`).
var SQLImportAnalyzer = &analysis.Analyzer{
	Name: "nocliproductionsqlimport",
	Doc: "Asserts that no file in the analyzed packages imports " +
		"database/sql or any *_sql_store adapter (tech-spec " +
		"REFACTOR-GUIDE Sec 8.10, rule no-production-sql-import). " +
		"Scope is enforced by the caller (e.g. `make lint-cli` " +
		"runs the vettool against ./cmd/cleanc/... ./internal/cli/...).",
	Run: runSQLImport,
}

const (
	// stdlibSQLImport is the stdlib SQL package whose
	// presence in the CLI tree would smuggle a SQL
	// dependency into the dev-only composition root.
	stdlibSQLImport = "database/sql"

	// sqlStoreSuffix matches the `_sql_store` package
	// naming convention used throughout the repo for the
	// PostgreSQL adapters (see
	// `internal/rule_engine/sql_store.go` etc).
	sqlStoreSuffix = "_sql_store"

	// sqlStorePathSegment matches a future refactor that
	// promotes `sql_store` to its own sub-package.
	sqlStorePathSegment = "/sql_store"
)

// runSQLImport drives the SQLImportAnalyzer.
func runSQLImport(pass *analysis.Pass) (interface{}, error) {
	for _, file := range pass.Files {
		for _, imp := range file.Imports {
			if imp.Path == nil {
				continue
			}
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if !isForbiddenSQLImport(path) {
				continue
			}
			pass.ReportRangef(imp,
				"no-production-sql-import: import %q is forbidden "+
					"in the CLI composition root; use the in-memory "+
					"readers/writers from internal/rule_engine and "+
					"internal/refactor instead (tech-spec REFACTOR-GUIDE "+
					"Sec 8.10)", path)
		}
	}
	return nil, nil
}

// isForbiddenSQLImport returns true when `path` matches one
// of the forbidden import-path patterns.
func isForbiddenSQLImport(path string) bool {
	if path == stdlibSQLImport {
		return true
	}
	if strings.HasSuffix(path, sqlStoreSuffix) {
		return true
	}
	if strings.HasSuffix(path, sqlStorePathSegment) {
		return true
	}
	return false
}
