// Imports a fixture `*_sql_store` package. The analyzer
// MUST fire on the suffix match.
package sqlimport_bad_sqlstore

import (
	"example.com/x/foo_sql_store" // want `no-production-sql-import`
)

var _ = foo_sql_store.Open
