// Blank import of stdlib SQL package. forbidigo cannot
// see this (no symbol use); the analyzer MUST fire.
// Regression guard for evaluator iter 1, item #2.
package sqlimport_bad_blank

import (
	_ "database/sql" // want `no-production-sql-import`
)
