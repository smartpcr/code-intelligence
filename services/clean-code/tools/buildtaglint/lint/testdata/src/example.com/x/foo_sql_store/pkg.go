// Fake *_sql_store package for analysistest fixtures.
// Importing this package from a fixture file must
// trigger the no-production-sql-import rule.
package foo_sql_store

// Open is a placeholder constructor.
func Open(dsn string) error { _ = dsn; return nil }
