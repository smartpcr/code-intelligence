//go:build cgo

// parity_test.go is the cgo-tagged arm of the REPO-SCANNER
// Stage 3.8 backend-parity golden test. It drives the AST
// dispatcher against the SAME fixture the memory arm
// (`parity_shared_test.go`) and the postgres arm
// (`parity_postgres_test.go`) use, but targets the
// `graphsink/sqlite` backend rooted in a per-test temp file.
//
// BUILD TAG (resolves prior-iter evaluator item 2)
//
//   - `//go:build cgo`. The sqlite backend wraps
//     `mattn/go-sqlite3`, which is itself `//go:build cgo`.
//     Inheriting that one constraint -- and nothing more --
//     keeps a CGO=0 build of `go test ./internal/graphsink/...`
//     compiling cleanly (this file vanishes, the memory arm in
//     `parity_shared_test.go` still runs) and lets the
//     integration arm carry the strictly weaker `integration`
//     tag in `parity_postgres_test.go`.
//
// ASSERTIONS
//
// Reads every Node + Edge row back through the sqlite
// backend's `graphsink.Reader` view via `collectFromReader`
// (no recording wrapper: the assertion is on PERSISTED state,
// not on write-call inputs). The Node + Edge tuples MUST be
// byte-identical to the memory backend over the same fixture.
package graphsink_test

import (
	"context"
	"path/filepath"
	"testing"

	sqlitesink "github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
)

// scanSQLite drives `runScan` against a SQLite-backed
// graphsink rooted in a per-test temp file, then reads every
// persisted row back through the sqlite backend's
// `graphsink.Reader` view via `collectFromReader`.
func scanSQLite(t *testing.T) ([]parityNodeRow, []parityEdgeRow) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "parity.db")
	sink, err := sqlitesink.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	repoID := runScan(t, sink)
	return collectFromReader(t, sink, repoID)
}

// TestBackendParity_MemoryAndSQLite is the unit-tier arm of
// the parity gate: the two backends that need NO external
// services (memory + sqlite-via-temp-file) MUST emit
// byte-identical `(repo_id, fingerprint, kind,
// canonical_signature)` Node tuples and `(kind,
// src_fingerprint, dst_fingerprint, fingerprint)` Edge tuples
// when driven over the same fixture.
//
// Failure modes this catches:
//   - A backend mutating `canonical_signature` between accept
//     and persist (e.g. truncating, lower-casing, or
//     normalising the value before INSERT). The persisted-state
//     read in `collectFromReader` -- not the write-call
//     returns -- is what makes this observable.
//   - A backend computing the fingerprint with a different
//     input order than `pkg/fingerprint.NodeFingerprint` /
//     `EdgeFingerprint`.
//   - A backend dropping or extra-emitting Nodes/Edges (e.g.
//     a missing `contains` edge or a duplicate `imports`
//     Node).
//
// Postgres parity is asserted from `parity_postgres_test.go`,
// which is gated behind `//go:build integration`.
func TestBackendParity_MemoryAndSQLite(t *testing.T) {
	memNodes, memEdges := scanMemory(t)
	sqlNodes, sqlEdges := scanSQLite(t)

	if len(memNodes) == 0 {
		t.Fatalf("memory backend persisted 0 node rows; expected the parity fixture to emit at least one Node")
	}
	if len(memEdges) == 0 {
		t.Fatalf("memory backend persisted 0 edge rows; expected the parity fixture to emit at least one Edge")
	}

	assertNodesEqual(t, "memory", "sqlite", memNodes, sqlNodes)
	assertEdgesEqual(t, "memory", "sqlite", memEdges, sqlEdges)
}
