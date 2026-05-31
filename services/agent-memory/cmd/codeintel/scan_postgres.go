// Postgres-backed sink opener for `codeintel scan --store=postgres`.
//
// Wires `--db <DSN>` through `database/sql` (the lib/pq driver
// already used by `cmd/repoindexer/main.go`) to a
// `*graphwriter.Writer`, then wraps the writer with the
// `graphsink/postgres` adapter so the scan path targets the
// production store with zero behavioural drift from the worker.
//
// The DSN is the standard `postgres://user:pass@host:port/db?...`
// connection string; migrations are NOT applied here -- the
// operator is expected to have run them ahead of time (same
// contract as the worker, which exits 3 on a bad ping).

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	postgressink "github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/postgres"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

func openPostgresSink(ctx context.Context, dsn string) (graphsink.Sink, func() error, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("ping postgres: %w", err)
	}
	writer := graphwriter.New(db, slog.Default())
	sink := postgressink.NewSink(writer)
	closer := func() error {
		cerr := sink.Close()
		if derr := db.Close(); derr != nil && cerr == nil {
			cerr = derr
		}
		return cerr
	}
	return sink, closer, nil
}
