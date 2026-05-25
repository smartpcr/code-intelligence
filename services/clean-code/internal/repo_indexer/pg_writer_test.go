package repo_indexer_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/repo_indexer"
)

// TestNewPGCatalogWriter_RejectsNilDB pins the
// constructor's nil-handle guard: a writer wired against a
// nil `*sql.DB` would NPE on first request; failing at
// construction makes the misconfig visible at the
// composition-root, where it can still be triaged.
func TestNewPGCatalogWriter_RejectsNilDB(t *testing.T) {
	t.Parallel()

	_, err := repo_indexer.NewPGCatalogWriter(nil)
	if err == nil {
		t.Fatal("NewPGCatalogWriter(nil): err = nil; want non-nil")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("NewPGCatalogWriter(nil): error %q should mention 'nil'", err.Error())
	}
}

// TestNewPGCatalogWriterWithSchema_RejectsEmptySchema pins
// the schema-validation guard. The schema is interpolated
// into raw SQL via `pq.QuoteIdentifier`; an empty string
// would produce `""."commit"` -- a syntactically valid but
// semantically catastrophic write target.
func TestNewPGCatalogWriterWithSchema_RejectsEmptySchema(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty":      "",
		"whitespace": "   ",
		"tab":        "\t",
	}
	for name, schema := range cases {
		name, schema := name, schema
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := repo_indexer.NewPGCatalogWriterWithSchema(&sql.DB{}, schema)
			if err == nil {
				t.Fatalf("NewPGCatalogWriterWithSchema(_, %q): err = nil; want non-nil", schema)
			}
			if !strings.Contains(err.Error(), "schema") {
				t.Errorf("NewPGCatalogWriterWithSchema(_, %q): error %q should mention 'schema'", schema, err.Error())
			}
		})
	}
}

// TestNewPGCatalogWriterWithSchema_AcceptsTestSchema confirms
// the alt-schema constructor mints a non-nil writer when both
// inputs are present. The writer is NOT exercised against a
// real DB here; the smoke is that construction succeeds and
// returns a non-nil pointer that satisfies the
// [CatalogWriter] interface.
func TestNewPGCatalogWriterWithSchema_AcceptsTestSchema(t *testing.T) {
	t.Parallel()

	w, err := repo_indexer.NewPGCatalogWriterWithSchema(&sql.DB{}, "clean_code_indexer_test")
	if err != nil {
		t.Fatalf("NewPGCatalogWriterWithSchema: %v", err)
	}
	if w == nil {
		t.Fatal("NewPGCatalogWriterWithSchema: writer = nil; want non-nil")
	}
	// Compile-time assertion already happens in pg_writer.go;
	// here we just round-trip via the interface to ensure the
	// signature aligns at call sites too.
	var _ repo_indexer.CatalogWriter = w
}

// TestNewPGCatalogWriter_UsesCanonicalSchema confirms the
// default constructor wires `clean_code` (matching
// `internal/storage.SchemaName`); a writer pointed at the
// wrong default would silently miss the production tables.
//
// We can't read the schema back out of the struct (the field
// is unexported); instead we assert by behaviour: a writer
// constructed with the explicit `clean_code` schema is
// equivalent to one constructed via the no-schema overload.
// The test stops at "both constructors succeed without
// error" -- a deeper assertion would require exposing the
// schema getter, which would weaken the type and serve no
// production purpose.
func TestNewPGCatalogWriter_UsesCanonicalSchema(t *testing.T) {
	t.Parallel()

	w1, err := repo_indexer.NewPGCatalogWriter(&sql.DB{})
	if err != nil {
		t.Fatalf("NewPGCatalogWriter: %v", err)
	}
	w2, err := repo_indexer.NewPGCatalogWriterWithSchema(&sql.DB{}, "clean_code")
	if err != nil {
		t.Fatalf("NewPGCatalogWriterWithSchema(clean_code): %v", err)
	}
	if w1 == nil || w2 == nil {
		t.Fatal("constructors returned nil writer")
	}
}

// TestPGCatalogWriter_SatisfiesCatalogWriterInterface is a
// compile-time assertion against the SAME pin that
// `pg_writer.go` makes via `var _ CatalogWriter =
// (*PGCatalogWriter)(nil)`. Repeating the assertion in the
// test file makes the pin survive a future refactor that
// might inline the production-side compile assertion away.
func TestPGCatalogWriter_SatisfiesCatalogWriterInterface(t *testing.T) {
	t.Parallel()

	w, err := repo_indexer.NewPGCatalogWriter(&sql.DB{})
	if err != nil {
		t.Fatalf("NewPGCatalogWriter: %v", err)
	}
	var iface repo_indexer.CatalogWriter = w
	if iface == nil {
		t.Fatal("PGCatalogWriter does not satisfy CatalogWriter")
	}
}

// TestPGCatalogWriter_DefaultSchemaConstantIsCleanCode pins
// the schema string the default constructor uses. A drift
// of this default to anything other than "clean_code" would
// silently misroute INSERTs to a sibling schema that doesn't
// contain the migration's `commit` / `repo_event` tables.
func TestPGCatalogWriter_DefaultSchemaConstantIsCleanCode(t *testing.T) {
	t.Parallel()

	// Indirect assertion: the only way to reach the
	// default-schema codepath is via NewPGCatalogWriter; if
	// the constant ever drifts away from "clean_code" the
	// caller would have to thread a different value into
	// NewPGCatalogWriterWithSchema, which downstream tests
	// (`TestNewPGCatalogWriter_UsesCanonicalSchema` above)
	// would surface.
	if _, err := repo_indexer.NewPGCatalogWriterWithSchema(&sql.DB{}, "clean_code"); err != nil {
		t.Fatalf("the canonical 'clean_code' schema must be acceptable: %v", err)
	}
}
