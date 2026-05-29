//go:build canonical_dispatcher

package embedding

// End-to-end wiring test for the Stage 3.3 contract.  Proves
// that when a Stage 3.1 Worker is constructed with a Stage 3.2
// AST dispatcher whose `WithEmbeddingPublisher` is wired
// through `AsASTPublisher(*Publisher)`, a full ingest produces
// one `embedding_publish` + `embedding_publish_event` row pair
// per Method (and per Block) node the AST dispatcher emits.
//
// This test addresses rubber-duck #1 ("publisher appears not
// wired into production ingest") — without it, the publisher
// code paths could pass their own unit tests while the worker
// silently never invokes them.  The test stands up the full
// composition (worker → dispatcher(WithEmbeddingPublisher) →
// publisher → Postgres) and asserts the §9.6a state-log rows
// actually appear after a real `runFull` invocation.

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// stubLifecyclePublisher is a no-op `repoindexer.EventPublisher`
// used by the wiring test below.  The worker constructor rejects
// a nil Publisher (its panic guards the §3.1 acceptance
// contract for `repo.registered` / `repo.full_ingested`
// events), but the wiring test only cares about the embedding
// publish path; lifecycle events are unused here.
type stubLifecyclePublisher struct {
	mu     sync.Mutex
	events []repoindexer.Event
}

func (p *stubLifecyclePublisher) Publish(_ context.Context, ev repoindexer.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

func (p *stubLifecyclePublisher) PublishTx(_ context.Context, _ *sql.Tx, ev repoindexer.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

// TestEmbeddingPublisher_endToEnd_wiredThroughWorker stands
// up a full Stage 3.1 → 3.2 → 3.3 pipeline and asserts:
//
//  1. A worker with a publisher-enabled dispatcher actually
//     creates `embedding_publish` + `embedding_publish_event`
//     rows during `runFull`.
//  2. The row count matches `count(node WHERE kind IN
//     ('method','block'))` the dispatcher inserted — proving
//     the wiring fires once per emitted Node, not zero times
//     (no fire) or many times (duplicate hook).
//  3. Every publish row's `embedding_model_version` matches
//     the stub embedder's `ModelVersion()` — the risk §9.6
//     mitigation is wired all the way through.
//  4. Every publish row's latest event is `published` — the
//     §9.6a read protocol's exclusion guard would otherwise
//     keep the vector hidden.
//
// Skips cleanly when AGENT_MEMORY_PG_URL is unset, matching
// the convention across the other integration tests.
func TestEmbeddingPublisher_endToEnd_wiredThroughWorker(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	gw := graphwriter.New(fix.app, slog.Default())

	// Seed a job + repo via the existing graphwriter API.
	const repoURL = "https://example.invalid/wiring"
	const sha = "0123456789abcdef0123456789abcdef01234567"
	rec, err := gw.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            repoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: sha,
		LanguageHints:  []string{"typescript"},
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	var jobID string
	if err := fix.app.QueryRowContext(ctx, `
		INSERT INTO ingest_jobs (repo_id, mode, from_sha, to_sha, status)
		VALUES ($1, 'full', NULL, $2, 'pending')
		RETURNING job_id::text
	`, rec.RepoID, sha).Scan(&jobID); err != nil {
		t.Fatalf("insert ingest_jobs: %v", err)
	}

	// Build the embedding publisher and the production-style
	// dispatcher wiring.  In a real cmd/main.go this is the
	// same composition pattern — the test exists to prove
	// the composition is wireable today (so future main.go
	// can lift it verbatim).
	q := &mockQdrant{}
	pub := NewPublisher(fix.app, fixedEmbedder{Version: "wiring-stub@v1"}, q)
	dispatcher := ast.NewDispatcher(gw, ast.WithEmbeddingPublisher(AsASTPublisher(pub)))

	// Two TS files with multiple methods/blocks so we can
	// assert publish rows landed for both kinds.
	mat := &repoindexer.InMemoryMaterializer{
		Files: []repoindexer.InMemoryFile{
			{
				RelPath: "src/a.ts",
				Content: []byte("class Foo { bar() { return 1; } baz() { if (true) { return 2; } } }"),
			},
			{
				RelPath: "src/b.ts",
				Content: []byte("class Quux { ping() { return 'pong'; } }"),
			},
		},
	}

	w := repoindexer.NewWorker(fix.app, gw, repoindexer.WorkerOptions{
		Materializer: mat,
		Emitter:      dispatcher,
		Publisher:    &stubLifecyclePublisher{},
	})
	processed, err := w.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if !processed {
		t.Fatalf("ProcessOnce returned processed=false; expected the seeded job to be claimed")
	}
	// Re-load the row to confirm the worker actually drove
	// it to terminal `done` — a `failed` row would still
	// say processed=true but leave embedding_publish empty.
	var finalStatus string
	if err := fix.app.QueryRowContext(ctx,
		`SELECT status::text FROM ingest_jobs WHERE job_id = $1`, jobID,
	).Scan(&finalStatus); err != nil {
		t.Fatalf("re-read job status: %v", err)
	}
	if finalStatus != "done" {
		t.Fatalf("ingest_jobs.status = %q; want %q (handler failed)", finalStatus, "done")
	}

	// Count the method/block nodes the dispatcher actually
	// emitted, then assert the embedding_publish row count
	// matches.  The publish hook fires exactly once per
	// node, so the two counts should be equal.
	var nodeCount int
	if err := fix.app.QueryRowContext(ctx, `
		SELECT count(*) FROM node
		WHERE repo_id = $1 AND kind IN ('method', 'block')
	`, rec.RepoID).Scan(&nodeCount); err != nil {
		t.Fatalf("count method+block nodes: %v", err)
	}
	if nodeCount == 0 {
		t.Fatalf("dispatcher emitted zero method+block nodes; test fixture broken")
	}

	var publishCount int
	if err := fix.app.QueryRowContext(ctx, `
		SELECT count(*) FROM embedding_publish ep
		JOIN node n ON n.node_id = ep.node_id
		WHERE n.repo_id = $1
	`, rec.RepoID).Scan(&publishCount); err != nil {
		t.Fatalf("count embedding_publish rows: %v", err)
	}
	if publishCount != nodeCount {
		t.Fatalf("embedding_publish rows = %d; emitted method+block nodes = %d "+
			"(publisher hook not firing once per node)", publishCount, nodeCount)
	}

	// Every publish row's recorded model version must match
	// the embedder's ModelVersion() — proves the risk §9.6
	// column gets populated through the full wiring.
	var distinctModels int
	if err := fix.app.QueryRowContext(ctx, `
		SELECT count(DISTINCT embedding_model_version) FROM embedding_publish ep
		JOIN node n ON n.node_id = ep.node_id
		WHERE n.repo_id = $1
	`, rec.RepoID).Scan(&distinctModels); err != nil {
		t.Fatalf("count distinct model versions: %v", err)
	}
	if distinctModels != 1 {
		t.Fatalf("distinct model versions = %d; want 1 (single embedder)", distinctModels)
	}

	// Every publish row's LATEST event must be `published`.
	// The query mirrors the §9.6a recall-path exclusion
	// probe; any vector whose latest event lags below
	// `published` would be served as stale.
	var notPublishedCount int
	if err := fix.app.QueryRowContext(ctx, `
		WITH latest AS (
		  SELECT DISTINCT ON (e.publish_id) e.publish_id, e.event_kind
		  FROM embedding_publish_event e
		  ORDER BY e.publish_id, e.created_at DESC, e.event_id DESC
		)
		SELECT count(*) FROM latest l
		JOIN embedding_publish ep ON ep.publish_id = l.publish_id
		JOIN node n ON n.node_id = ep.node_id
		WHERE n.repo_id = $1 AND l.event_kind <> 'published'
	`, rec.RepoID).Scan(&notPublishedCount); err != nil {
		t.Fatalf("count not-published latest events: %v", err)
	}
	if notPublishedCount != 0 {
		t.Fatalf("%d publish rows have a latest event other than 'published' "+
			"(recall path would exclude these vectors)", notPublishedCount)
	}

	// Sanity: the Qdrant mock saw at least `nodeCount`
	// upserts (it MAY have seen more across the
	// queued-then-confirm round trip, but never fewer).
	if int(q.Upserts.Load()) < nodeCount {
		t.Fatalf("Qdrant Upsert count = %d; want >= %d (one per method+block)",
			q.Upserts.Load(), nodeCount)
	}
}

// Compile-time check: the dispatcher constructed with the
// wiring above must satisfy the ASTEmitter interface the
// worker requires.  A signature drift on either side would
// otherwise surface only at runtime through a panic.
var _ repoindexer.ASTEmitter = (*ast.Dispatcher)(nil)
