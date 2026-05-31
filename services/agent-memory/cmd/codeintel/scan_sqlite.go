//go:build cgo

package main

import (
	"context"
	"fmt"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
)

// openSqliteSink opens (or creates) the SQLite database at `path`
// and returns the graphsink.Sink plus a close func the scan
// subcommand defers. Wired only on CGO=1 builds because the
// SQLite backend depends on `mattn/go-sqlite3` (CGO driver); the
// CGO=0 fallback in `scan_nocgo.go` returns a clear error so the
// operator does not get a build that links but cannot persist.
func openSqliteSink(ctx context.Context, path string) (graphsink.Sink, func() error, error) {
	s, err := sqlite.Open(ctx, path)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	return s, s.Close, nil
}
