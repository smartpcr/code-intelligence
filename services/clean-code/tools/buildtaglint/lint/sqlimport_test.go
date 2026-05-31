// -----------------------------------------------------------------------
// <copyright file="sqlimport_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package lint

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestSQLImportAnalyzer_Fixtures drives SQLImportAnalyzer
// against the per-scenario packages under
// `testdata/src/sqlimport_*`. Each fixture file is
// annotated with `// want "..."` comments where a
// diagnostic is expected; analysistest verifies an exact
// match.
//
// Scenarios:
//
//   - `sqlimport_bad_stdlib`   -- imports `database/sql`.
//     Expect: diagnostic.
//   - `sqlimport_bad_blank`    -- imports `database/sql`
//     as a blank import (`import _ "database/sql"`).
//     Expect: diagnostic. This is the regression guard
//     for evaluator iter 1, item #2 (forbidigo cannot
//     see blank imports).
//   - `sqlimport_bad_sqlstore` -- imports a fake
//     `repo/foo_sql_store` package. Expect: diagnostic.
//   - `sqlimport_good`         -- imports `fmt` and
//     `strings` only. Expect: no diagnostic.
func TestSQLImportAnalyzer_Fixtures(t *testing.T) {
	testdata := analysistest.TestData()
	cases := []string{
		"sqlimport_bad_stdlib",
		"sqlimport_bad_blank",
		"sqlimport_bad_sqlstore",
		"sqlimport_good",
	}
	for _, pkg := range cases {
		pkg := pkg
		t.Run(pkg, func(t *testing.T) {
			analysistest.Run(t, testdata, SQLImportAnalyzer, pkg)
		})
	}
}

// TestIsForbiddenSQLImport pins the exact path-pattern
// match set. End-to-end coverage is in the fixtures
// above; this unit test guards the path predicate.
func TestIsForbiddenSQLImport(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"database/sql", true},
		{"github.com/x/y/foo_sql_store", true},
		{"github.com/x/y/sql_store", true},
		{"github.com/x/y/internal/policy/steward/sql_store", true},
		// Negatives.
		{"database/sql/driver", false}, // sub-package; not the stdlib root
		{"github.com/x/y/sql_storage", false},
		{"github.com/x/y/_sql_store_test", false}, // suffix is _test
		{"fmt", false},
		{"", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.path, func(t *testing.T) {
			got := isForbiddenSQLImport(c.path)
			if got != c.want {
				t.Fatalf("isForbiddenSQLImport(%q) = %v; want %v",
					c.path, got, c.want)
			}
		})
	}
}
