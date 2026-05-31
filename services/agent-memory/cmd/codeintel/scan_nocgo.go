//go:build !cgo

package main

import (
	"context"
	"errors"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
)

// openSqliteSink on CGO=0 returns a clear error -- the SQLite
// driver (`mattn/go-sqlite3`) is CGO-only. Operators on a
// CGO=0 build should pass `--store=memory` instead.
func openSqliteSink(_ context.Context, _ string) (graphsink.Sink, func() error, error) {
	return nil, nil, errors.New("--store=sqlite requires a CGO build (rebuild with CGO_ENABLED=1, or use --store=memory)")
}
