// Stage 5.5 -- `--with-embeddings` opt-in wiring for `codeintel scan`.
//
// Contract (tech-spec C8): when `--with-embeddings` is ABSENT
// the CLI MUST run without Qdrant and MUST NOT read any
// embedder-related environment variable. The runScan loop in
// scan.go honours that by calling the factory in this file ONLY
// when `root.withEmbeddings` is true; therefore every os.Getenv
// here is gated behind that condition.
//
// When the flag IS set, the default factory below mirrors the
// `cmd/repoindexer/main.go` wiring:
//
//	embedder + qdrant + db -> embedding.NewPublisher
//	Publisher              -> embedding.AsASTPublisher
//	AsASTPublisher         -> ast.WithEmbeddingPublisher
//
// The publisher needs a real `*sql.DB` to append durable
// `embedding_publish_event` rows, so the default factory
// requires `--store=postgres` with a valid DSN on `--db`. A
// future workstream may add a sqlite-backed event log; until
// then this guard prevents a silent no-op.
//
// Embedder selection currently supports only the in-process
// stub (mirrors `cmd/repoindexer.selectEmbedder`). The stub is
// gated by `AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true` so an
// operator cannot accidentally publish zero-vectors into a
// production Qdrant collection without opting in.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// cliStubEmbedder is the local-dev placeholder embedder used by
// `codeintel scan --with-embeddings`. It mirrors
// `cmd/repoindexer.stubEmbedder` exactly so the publisher's
// model_version cascade is byte-compatible between the CLI and
// the production worker.
//
// Returns a 768-dim all-zeros vector to match the
// `cmd/qdrant-bootstrap` default `vector_size`. NOT fit for
// production recall.
type cliStubEmbedder struct{}

// stubEmbedderDims is the vector dimensionality the stub emits;
// kept as a named constant so a future tightening of the
// `embedding_publish` CHECK at vector_size != 768 fails at this
// one site instead of every caller.
const stubEmbedderDims = 768

// stubEmbedderModelVersion is the model-version sentinel the
// stub publishes. The literal MUST match
// `cmd/repoindexer.stubEmbedder.ModelVersion` so a row published
// by either binary is treated as the same model by
// `Publisher.Retry`'s model-mismatch gate.
const stubEmbedderModelVersion = "stub-zero-vector@v0"

// Embed satisfies `embedding.Embedder`.
func (cliStubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return make([]float32, stubEmbedderDims), nil
}

// ModelVersion satisfies `embedding.Embedder`.
func (cliStubEmbedder) ModelVersion() string { return stubEmbedderModelVersion }

// Environment variable names this file reads. Centralised so
// `grep -F` finds every coupling and so tests can verify the
// exact spellings without re-typing them.
const (
	envQdrantURL        = "AGENT_MEMORY_QDRANT_URL"
	envQdrantAPIKey     = "AGENT_MEMORY_QDRANT_API_KEY"
	envAllowStubEmbedder = "AGENT_MEMORY_ALLOW_STUB_EMBEDDER"
)

// envReader is the indirection seam unit tests use to feed a
// fake env to `selectCLIEmbedder` without touching the real
// process environment. Production wiring passes `os.Getenv`.
type envReader func(string) string

// selectCLIEmbedder picks the embedding-model client for the
// CLI. Today the only supported client is the in-process stub,
// gated by `AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true`. The function
// MUST NOT be called when `--with-embeddings` is absent (the
// runScan loop enforces that gate).
//
// Returns:
//   - cliStubEmbedder{}, nil       when the stub env is true
//   - nil, error                   when the stub env is unset,
//                                  false, or unparseable
//
// The error wording deliberately surfaces the env var name so
// the operator can fix the misconfiguration without grepping
// source.
func selectCLIEmbedder(env envReader) (embedding.Embedder, error) {
	if env == nil {
		env = os.Getenv
	}
	v := env(envAllowStubEmbedder)
	if v == "" {
		return nil, fmt.Errorf(
			"--with-embeddings requires %s=true (no real embedder is configured yet; "+
				"the stub returns a fixed zero-vector and is for local development only)",
			envAllowStubEmbedder)
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envAllowStubEmbedder, err)
	}
	if !b {
		return nil, fmt.Errorf("--with-embeddings requires %s=true (got %q)", envAllowStubEmbedder, v)
	}
	slog.Warn("scan.embedder_stub",
		slog.String("warning", "stub embedder returns a fixed zero-vector; NOT fit for production recall"),
		slog.String("model_version", stubEmbedderModelVersion))
	return cliStubEmbedder{}, nil
}

// defaultEmbeddingPublisherFactory is the production factory
// the runScan loop installs when no test seam is provided. It
// requires:
//
//   - --store=postgres  (the Publisher needs *sql.DB for the
//     durable event log; sqlite/memory cannot host it today)
//   - --db <DSN>        (the postgres DSN; reused by the
//     graphsink/postgres opener so we don't open a second
//     connection-string source)
//   - AGENT_MEMORY_QDRANT_URL
//   - AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true (until a real
//     embedder client lands)
//
// Returns the AST-side publisher (already wrapped via
// `embedding.AsASTPublisher`) and a closer the runScan loop
// defers. The closer disposes the dedicated *sql.DB this
// factory opened; the graph-sink's own *sql.DB lifecycle is
// independent.
func defaultEmbeddingPublisherFactory(ctx context.Context, root *rootFlags) (ast.NodeEmbeddingPublisher, func() error, error) {
	if root == nil {
		return nil, nil, errors.New("nil root flags")
	}
	if root.store != "postgres" {
		return nil, nil, fmt.Errorf(
			"--with-embeddings requires --store=postgres (got %q); the embedding "+
				"publisher writes durable events to the agent-memory database",
			root.store)
	}
	if root.db == "" {
		return nil, nil, errors.New("--with-embeddings requires --db <postgres DSN>")
	}
	qdrantURL := os.Getenv(envQdrantURL)
	if qdrantURL == "" {
		return nil, nil, fmt.Errorf("--with-embeddings requires %s", envQdrantURL)
	}
	embedder, err := selectCLIEmbedder(os.Getenv)
	if err != nil {
		return nil, nil, err
	}

	db, err := sql.Open("postgres", root.db)
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres for embedding publisher: %w", err)
	}
	// Match cmd/repoindexer's pool sizes so the CLI doesn't
	// monopolise the database during a large scan.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if perr := db.PingContext(pingCtx); perr != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("ping postgres for embedding publisher: %w", perr)
	}

	qdrant := embedding.NewHTTPQdrant(qdrantURL)
	if apiKey := os.Getenv(envQdrantAPIKey); apiKey != "" {
		qdrant.Client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &qdrantAPIKeyTransport{key: apiKey, base: http.DefaultTransport},
		}
	}

	publisher := embedding.NewPublisher(db, embedder, qdrant,
		embedding.WithLogger(slog.Default()))

	slog.Info("scan.embedding_publisher_wired",
		"store", root.store,
		"qdrant_url", qdrantURL,
		"model_version", publisher.ModelVersion(),
	)

	return embedding.AsASTPublisher(publisher), db.Close, nil
}

// qdrantAPIKeyTransport injects the Qdrant `api-key` header on
// every outbound request. Mirrors the helper in
// `cmd/repoindexer/main.go` so the secret never leaks into
// query strings or UserAgent.
type qdrantAPIKeyTransport struct {
	key  string
	base http.RoundTripper
}

func (t *qdrantAPIKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone so we never mutate the caller's request headers.
	clone := req.Clone(req.Context())
	clone.Header.Set("api-key", t.key)
	rt := t.base
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(clone)
}
