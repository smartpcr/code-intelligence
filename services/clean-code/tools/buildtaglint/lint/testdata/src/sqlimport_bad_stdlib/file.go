// Direct stdlib SQL import. The analyzer MUST fire.
package sqlimport_bad_stdlib

import (
	"database/sql" // want `no-production-sql-import`
)

var _ = sql.ErrNoRows
