// Stage 5.5 -- tests for the `--with-embeddings` opt-in flag.
//
// Covers two contractual requirements:
//
//   (a) Flag absent  => no embedder is constructed; no embedder
//       env var is read. Per tech-spec C8 the CLI must run
//       without Qdrant by default.
//   (b) Flag present + AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true
//       => the stub embedder is constructed and the dispatcher
//       actually invokes the resulting publisher.
//
// The factory seam on `scanRunner` is used in test (b) so the
// test does not require a live Postgres or Qdrant. The
// `selectCLIEmbedder` helper -- the function the production
// factory calls to pick an embedder -- is exercised directly so
// the env-read path itself is locked in.

package main

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// recordingASTPublisher counts PublishNodeEmbedding invocations
// so tests can assert the dispatcher actually consumed the
// publisher the factory returned.
type recordingASTPublisher struct {
	calls atomic.Int32
}

func (r *recordingASTPublisher) PublishNodeEmbedding(_ context.Context, _ ast.NodeEmbedRequest) (ast.NodeEmbedResult, error) {
	r.calls.Add(1)
	return ast.NodeEmbedResult{PublishID: "p-test", LastEventKind: "published"}, nil
}

var _ ast.NodeEmbeddingPublisher = (*recordingASTPublisher)(nil)

// recordingEnvReader captures every env key the unit under test
// looks up, so a test can prove that an env var was (or was
// NOT) consulted.
type recordingEnvReader struct {
	values  map[string]string
	lookups []string
}

func (r *recordingEnvReader) get(key string) string {
	r.lookups = append(r.lookups, key)
	if r.values == nil {
		return ""
	}
	return r.values[key]
}

func (r *recordingEnvReader) looked(key string) bool {
	for _, k := range r.lookups {
		if k == key {
			return true
		}
	}
	return false
}

// TestScanFlagAbsentDoesNotConstructEmbedder locks requirement
// (a): when `--with-embeddings` is NOT set, the factory seam is
// NEVER invoked and therefore no embedder env var is read. We
// set sentinel env values so a buggy implementation that
// short-circuited through os.Getenv would still be detectable
// (the factory would have to be called for that to matter).
func TestScanFlagAbsentDoesNotConstructEmbedder(t *testing.T) {
	t.Setenv(envQdrantURL, "http://should-never-be-read.invalid")
	t.Setenv(envAllowStubEmbedder, "true")
	t.Setenv(envQdrantAPIKey, "should-never-be-read")

	var factoryCalls atomic.Int32
	runner := scanRunner{
		stdout: &bytes.Buffer{},
		newEmbeddingPublisher: func(_ context.Context, _ *rootFlags) (ast.NodeEmbeddingPublisher, func() error, error) {
			factoryCalls.Add(1)
			t.Errorf("embedding factory must not be invoked when --with-embeddings is absent")
			return nil, nil, nil
		},
	}

	dir := writeFixtureRepo(t)
	root := defaultRootFlags()
	root.store = "memory"
	// Explicit: flag is OFF.
	root.withEmbeddings = false

	if err := runScan(context.Background(), &root, &scanFlags{}, dir, runner); err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if n := factoryCalls.Load(); n != 0 {
		t.Fatalf("embedding factory must not be called when --with-embeddings is absent; called %d times", n)
	}
}

// TestScanFlagPresentConstructsStubEmbedderAndDispatches locks
// requirement (b): when `--with-embeddings` IS set and
// AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true, the runScan loop
// invokes the factory, the factory's env-read path returns the
// cliStubEmbedder, and the dispatcher actually publishes
// (proven by the recording publisher seeing at least one call
// for the fixture's method).
//
// The factory seam here invokes the production
// `selectCLIEmbedder` so the env-read path itself is exercised.
// We then return a recording publisher (instead of building a
// real `embedding.NewPublisher` -- that would need a live
// *sql.DB) so the dispatcher's invocation can be observed
// without postgres/qdrant infra.
func TestScanFlagPresentConstructsStubEmbedderAndDispatches(t *testing.T) {
	t.Setenv(envAllowStubEmbedder, "true")

	rp := &recordingASTPublisher{}
	var (
		factoryCalls atomic.Int32
		sawStub      atomic.Bool
	)
	factory := func(_ context.Context, _ *rootFlags) (ast.NodeEmbeddingPublisher, func() error, error) {
		factoryCalls.Add(1)
		emb, err := selectCLIEmbedder(nil /* defaults to os.Getenv, which sees t.Setenv */)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := emb.(cliStubEmbedder); !ok {
			t.Errorf("selectCLIEmbedder returned %T, want cliStubEmbedder", emb)
		} else {
			sawStub.Store(true)
		}
		// Surface the production adapter symbol so the import is pinned
		// and a future refactor that drops it from this test still
		// compiles only if the wiring it represents survives.
		_ = embedding.AsASTPublisher
		return rp, func() error { return nil }, nil
	}

	mat := &repoindexer.InMemoryMaterializer{
		Files: []repoindexer.InMemoryFile{{
			RelPath: "lib.go",
			Content: []byte("package lib\n\ntype T struct{}\n\nfunc (t *T) A() string { return t.b() }\nfunc (t *T) b() string { return \"hi\" }\n"),
		}},
	}
	root := defaultRootFlags()
	root.store = "memory"
	root.withEmbeddings = true
	flags := &scanFlags{sha: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}
	var buf bytes.Buffer
	runner := scanRunner{
		stdout:                &buf,
		newGitMat:             func() repoindexer.Materializer { return mat },
		newEmbeddingPublisher: factory,
	}

	if err := runScan(context.Background(), &root, flags, "https://example.com/repo.git", runner); err != nil {
		t.Fatalf("runScan: %v\noutput=%s", err, buf.String())
	}
	if n := factoryCalls.Load(); n != 1 {
		t.Fatalf("embedding factory should be invoked exactly once when --with-embeddings is set; got %d", n)
	}
	if !sawStub.Load() {
		t.Fatalf("selectCLIEmbedder should return cliStubEmbedder when %s=true", envAllowStubEmbedder)
	}
	if n := rp.calls.Load(); n == 0 {
		t.Fatalf("dispatcher should invoke the embedding publisher at least once for the fixture's method node; got 0 calls\noutput=%s", buf.String())
	}
}

// TestSelectCLIEmbedderEnforcesStubGate exhaustively locks the
// env-read contract used by the production factory.
func TestSelectCLIEmbedderEnforcesStubGate(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		wantErr   bool
		wantStub  bool
		wantMatch string // substring expected in the error (only checked when wantErr)
	}{
		{"unset", map[string]string{}, true, false, envAllowStubEmbedder + "=true"},
		{"false", map[string]string{envAllowStubEmbedder: "false"}, true, false, envAllowStubEmbedder + "=true"},
		{"garbage", map[string]string{envAllowStubEmbedder: "yes"}, true, false, envAllowStubEmbedder},
		{"true", map[string]string{envAllowStubEmbedder: "true"}, false, true, ""},
		{"true-1", map[string]string{envAllowStubEmbedder: "1"}, false, true, ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			reader := &recordingEnvReader{values: c.env}
			emb, err := selectCLIEmbedder(reader.get)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (embedder=%T)", emb)
				}
				if c.wantMatch != "" && !strings.Contains(err.Error(), c.wantMatch) {
					t.Errorf("error %q missing substring %q", err.Error(), c.wantMatch)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantStub {
				if _, ok := emb.(cliStubEmbedder); !ok {
					t.Fatalf("expected cliStubEmbedder, got %T", emb)
				}
				// Sanity: stub returns a 768-dim zero vector and
				// the agreed model_version sentinel.
				v, _ := emb.Embed(context.Background(), "anything")
				if len(v) != stubEmbedderDims {
					t.Errorf("stub vector length: want %d, got %d", stubEmbedderDims, len(v))
				}
				for i, f := range v {
					if f != 0 {
						t.Errorf("stub vector[%d] = %v, want 0", i, f)
						break
					}
				}
				if mv := emb.ModelVersion(); mv != stubEmbedderModelVersion {
					t.Errorf("stub model version: want %q, got %q", stubEmbedderModelVersion, mv)
				}
			}
			// The stub gate should have read AGENT_MEMORY_ALLOW_STUB_EMBEDDER.
			if !reader.looked(envAllowStubEmbedder) {
				t.Errorf("selectCLIEmbedder must read %s; lookups were %v", envAllowStubEmbedder, reader.lookups)
			}
		})
	}
}

// TestDefaultEmbeddingPublisherFactoryRequiresPostgres locks
// the default factory's preconditions so a misconfigured opt-in
// fails with an actionable message instead of a panic from
// embedding.NewPublisher (nil *sql.DB) or a Qdrant timeout.
func TestDefaultEmbeddingPublisherFactoryRequiresPostgres(t *testing.T) {
	// AGENT_MEMORY_QDRANT_URL is set so the missing-postgres
	// branch is the first failure surfaced.
	t.Setenv(envQdrantURL, "http://localhost:6333")
	t.Setenv(envAllowStubEmbedder, "true")

	root := defaultRootFlags()
	root.store = "sqlite"
	root.db = "ignored.db"
	pub, closer, err := defaultEmbeddingPublisherFactory(context.Background(), &root)
	if err == nil {
		if closer != nil {
			_ = closer()
		}
		t.Fatalf("expected an error for --store=sqlite, got publisher=%v", pub)
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("error should mention postgres requirement, got %v", err)
	}
}

// TestDefaultEmbeddingPublisherFactoryRequiresQdrantURL locks
// the env-read precondition: postgres is set but
// AGENT_MEMORY_QDRANT_URL is not.
func TestDefaultEmbeddingPublisherFactoryRequiresQdrantURL(t *testing.T) {
	// Explicitly unset by setting empty -- t.Setenv with "" still
	// records the override for restoration.
	t.Setenv(envQdrantURL, "")
	t.Setenv(envAllowStubEmbedder, "true")

	root := defaultRootFlags()
	root.store = "postgres"
	root.db = "postgres://u:p@127.0.0.1:1/db?sslmode=disable"
	pub, closer, err := defaultEmbeddingPublisherFactory(context.Background(), &root)
	if err == nil {
		if closer != nil {
			_ = closer()
		}
		t.Fatalf("expected error when %s is unset, got publisher=%v", envQdrantURL, pub)
	}
	if !strings.Contains(err.Error(), envQdrantURL) {
		t.Fatalf("error should mention %s, got %v", envQdrantURL, err)
	}
}
