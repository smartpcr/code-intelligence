package rule_engine

import (
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
)

// newTestWALWriter returns a fresh [wal.Writer] rooted at
// `t.TempDir()` with [wal.NoopSigner]. Used by every
// SQLStore test so the Stage 9.1 atomicity contract --
// every successful Audit INSERT is paired with a WAL frame
// fsynced before SQL commit -- is exercised on the same
// code path production runs.
//
// The tempdir is cleaned up automatically by `testing.T` so
// each test starts with an empty partition root.
func newTestWALWriter(t *testing.T) *wal.Writer {
	t.Helper()
	w, err := wal.NewWriter(wal.WriterConfig{
		Dir:    t.TempDir(),
		Signer: wal.NoopSigner{},
	})
	if err != nil {
		t.Fatalf("wal.NewWriter: %v", err)
	}
	return w
}
