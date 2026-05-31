//go:build cgo

package sqlite

import "database/sql"

// MergeDefaultPragmasForTest exposes the package-private
// mergeDefaultPragmas helper to the external _test package so
// the soft-default merge behaviour can be unit-tested in
// isolation. Note: this helper does NOT include the mandatory
// `_foreign_keys=on` enforcement -- that lives in
// `BuildDSNForTest`. Test code asserting end-to-end DSN shape
// should call `BuildDSNForTest`.
func MergeDefaultPragmasForTest(dsn string) string {
	return mergeDefaultPragmas(dsn, [][2]string{
		{"_journal_mode", "WAL"},
		{"_busy_timeout", "5000"},
	})
}

// BuildDSNForTest exposes the package-private buildDSN helper
// so the external _test package can assert the iter-3 pragma
// policy: `_foreign_keys=on` is non-negotiable; soft defaults
// merge in; caller-supplied `_foreign_keys=off` is stripped.
func BuildDSNForTest(dsn string) string { return buildDSN(dsn) }

// DBForTest exposes the underlying *sql.DB so external tests
// can run direct verification queries (e.g. confirming a
// deterministic repo_id landed unchanged in the `node.repo_id`
// column). Production callers go through the Sink methods; this
// is strictly a test helper.
func DBForTest(s *Sink) *sql.DB { return s.db }
