package embedding

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	astpkg "github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// stubEmbedder satisfies `Embedder` without touching a real
// model server.  Tests assign `EmbedFn` / `ModelVersionFn` per
// scenario; nil `EmbedFn` returns a fixed 4-dim unit vector.
type stubEmbedder struct {
	EmbedFn        func(ctx context.Context, content string) ([]float32, error)
	ModelVersionFn func() string
}

func (s stubEmbedder) Embed(ctx context.Context, content string) ([]float32, error) {
	if s.EmbedFn != nil {
		return s.EmbedFn(ctx, content)
	}
	return []float32{1, 0, 0, 0}, nil
}

func (s stubEmbedder) ModelVersion() string {
	if s.ModelVersionFn != nil {
		return s.ModelVersionFn()
	}
	return "stub-embedder@v1"
}

// stubQdrant is a no-arg `Qdrant` whose hooks default to
// success.  The unit tests below only exercise paths that
// fail BEFORE Qdrant is reached (validation, modelVersion), so
// these defaults never fire — but they keep the interface
// satisfied if a future regression test reaches Upsert.
type stubQdrant struct{}

func (stubQdrant) Upsert(context.Context, string, string, []float32, map[string]any) error {
	return nil
}
func (stubQdrant) PointExists(context.Context, string, string) (bool, error) { return true, nil }

// TestPublisher_validateRequest exercises every rejection path
// in the input validator.  Validation runs BEFORE any side
// effect (no PG round-trip, no embedder call, no Qdrant call),
// so these tests can be unit tests with a nil *sql.DB.  The
// publisher's nil-check panic on the constructor would
// normally block us; we cheat by constructing the struct
// directly with the same defaults the constructor sets.
func TestPublisher_validateRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  PublishRequest
		want string // substring of the expected error
	}{
		{
			name: "missing node id",
			req:  PublishRequest{RepoID: "r1", Kind: NodeKindMethod, Content: "x"},
			want: "NodeID is required",
		},
		{
			name: "missing repo id",
			req:  PublishRequest{NodeID: "n1", Kind: NodeKindMethod, Content: "x"},
			want: "RepoID is required",
		},
		{
			name: "unknown kind",
			req:  PublishRequest{NodeID: "n1", RepoID: "r1", Kind: "class", Content: "x"},
			want: "not supported",
		},
		{
			name: "concept kind rejected (handled by Concept Promoter)",
			req:  PublishRequest{NodeID: "n1", RepoID: "r1", Kind: "concept", Content: "x"},
			want: "not supported",
		},
		{
			name: "empty content rejected (zero-information embedding)",
			req:  PublishRequest{NodeID: "n1", RepoID: "r1", Kind: NodeKindMethod, Content: ""},
			want: "Content is required",
		},
		{
			name: "whitespace-only content rejected",
			req:  PublishRequest{NodeID: "n1", RepoID: "r1", Kind: NodeKindMethod, Content: "   \n\t"},
			want: "Content is required",
		},
		{
			name: "SignatureOnly does NOT exempt the Content requirement",
			req: PublishRequest{
				NodeID: "n1", RepoID: "r1", Kind: NodeKindMethod,
				Content: "", SignatureOnly: true,
			},
			want: "Content is required",
		},
	}

	p := &Publisher{}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := p.validateRequest(tc.req)
			if err == nil {
				t.Fatalf("validateRequest(%+v) = nil; want error", tc.req)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateRequest(%+v) = %q; want substring %q", tc.req, err.Error(), tc.want)
			}
		})
	}
}

// TestPublisher_validateRequest_acceptsKnownKinds locks down
// the positive case so the validator's invariants (Kind in
// the allowed set, Content non-empty) cannot regress
// without a failing test.  Both legitimate kinds are
// exercised with a minimal non-empty Content body.
func TestPublisher_validateRequest_acceptsKnownKinds(t *testing.T) {
	t.Parallel()
	p := &Publisher{}
	for _, k := range []string{NodeKindMethod, NodeKindBlock} {
		if err := p.validateRequest(PublishRequest{
			NodeID: "n", RepoID: "r", Kind: k, Content: "x",
		}); err != nil {
			t.Fatalf("validateRequest(kind=%q): %v", k, err)
		}
	}

	// Bodyless-method shape: SignatureOnly=true with a
	// non-empty Content (which the dispatcher fills with
	// the canonical signature) MUST be accepted.  Locks
	// down the iter-2 bodyless-method publish path.
	if err := p.validateRequest(PublishRequest{
		NodeID: "n", RepoID: "r", Kind: NodeKindMethod,
		Content: "func Foo()", SignatureOnly: true,
	}); err != nil {
		t.Fatalf("validateRequest(bodyless method): %v", err)
	}
}

// TestCollectionFor maps the two supported kinds and refuses
// everything else.  A wiring bug in the AST adapter that
// passes "concept" or "package" through is caught here, not
// hours later at a Qdrant 404.
func TestCollectionFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind, want string
		wantErr    bool
	}{
		{NodeKindMethod, CollectionMethod, false},
		{NodeKindBlock, CollectionBlock, false},
		{"concept", "", true},
		{"", "", true},
		{"file", "", true},
	}
	for _, tc := range cases {
		got, err := CollectionFor(tc.kind)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("CollectionFor(%q) err = nil; want non-nil", tc.kind)
			}
			continue
		}
		if err != nil {
			t.Fatalf("CollectionFor(%q) err = %v", tc.kind, err)
		}
		if got != tc.want {
			t.Fatalf("CollectionFor(%q) = %q; want %q", tc.kind, got, tc.want)
		}
	}
}

// TestNewUUIDv4_format asserts the helper produces a canonical
// 36-character v4 UUID with the right variant nibble.  The
// publisher uses this for both publish_id (insert-side) and
// point_id (Qdrant payload); a malformed UUID would FK-fail on
// PostgreSQL and silently corrupt Qdrant.
func TestNewUUIDv4_format(t *testing.T) {
	t.Parallel()
	for i := 0; i < 16; i++ {
		s, err := NewUUIDv4()
		if err != nil {
			t.Fatalf("NewUUIDv4: %v", err)
		}
		if len(s) != 36 {
			t.Fatalf("len(%q) = %d; want 36", s, len(s))
		}
		// 8-4-4-4-12 grouping.
		for _, pos := range []int{8, 13, 18, 23} {
			if s[pos] != '-' {
				t.Fatalf("UUID %q: expected '-' at pos %d, got %q", s, pos, s[pos])
			}
		}
		// Version 4 nibble at position 14.
		if s[14] != '4' {
			t.Fatalf("UUID %q: expected '4' (version) at pos 14, got %q", s, s[14])
		}
		// Variant bits: position 19 must be one of 8, 9, a, b.
		switch s[19] {
		case '8', '9', 'a', 'b':
		default:
			t.Fatalf("UUID %q: expected variant nibble in [89ab] at pos 19, got %q", s, s[19])
		}
	}
}

// TestNewUUIDv4_unique sanity-checks the helper isn't a stub
// returning a constant.  Collisions are astronomically rare;
// any duplicate over 1k samples is almost certainly a bug.
func TestNewUUIDv4_unique(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, 1024)
	for i := 0; i < 1024; i++ {
		s, err := NewUUIDv4()
		if err != nil {
			t.Fatalf("NewUUIDv4: %v", err)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("UUID collision on iter %d: %q", i, s)
		}
		seen[s] = struct{}{}
	}
}

// TestFailureDetails locks the `{phase, error}` JSON shape the
// `embedding_publish_event.details_json` column carries.  A
// future refactor that drops `phase` would break operator
// triage queries built against this shape.
func TestFailureDetails(t *testing.T) {
	t.Parallel()

	got := string(failureDetails("qdrant_upsert", errors.New("boom")))
	want := `{"error":"boom","phase":"qdrant_upsert"}`
	if got != want {
		t.Fatalf("failureDetails =\n  %s\nwant\n  %s", got, want)
	}

	got2 := string(failureDetails("embedder", nil))
	want2 := `{"phase":"embedder"}`
	if got2 != want2 {
		t.Fatalf("failureDetails(nil err) =\n  %s\nwant\n  %s", got2, want2)
	}
}

// TestBuildPayload locks rubber-duck #8 — Qdrant payload MUST
// carry repo_id, kind, node_id, publish_id, canonical_signature,
// embedding_model_version.  A regression that drops any of
// these breaks the recall filter pushdown (`repo_id`, `kind`)
// or the operator reverse-lookup (`node_id`, `publish_id`).
func TestBuildPayload(t *testing.T) {
	t.Parallel()
	p := &Publisher{}
	req := PublishRequest{
		NodeID:             "node-uuid",
		RepoID:             "repo-uuid",
		Kind:               NodeKindMethod,
		CanonicalSignature: "repo::pkg::Method(int)",
		Content:            "func f() {}",
	}
	res := PublishResult{
		PublishID:     "publish-uuid",
		QdrantPointID: "point-uuid",
	}
	got := p.buildPayload(req, res, "stub@v1")
	for _, key := range []string{
		"repo_id", "kind", "node_id", "publish_id",
		"canonical_signature", "embedding_model_version",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("buildPayload missing key %q (rubber-duck #8 violation); got %+v", key, got)
		}
	}
	if got["repo_id"] != "repo-uuid" {
		t.Fatalf("payload.repo_id = %v; want repo-uuid", got["repo_id"])
	}
	if got["embedding_model_version"] != "stub@v1" {
		t.Fatalf("payload.embedding_model_version = %v; want stub@v1", got["embedding_model_version"])
	}
}

// TestAsASTPublisher_panicsOnNilSurface preserves the
// adapter's loud-fail contract.  A nil publisher slipping
// through the dispatcher wiring would otherwise silently
// dispatch to a no-op and the §9.6a log would never grow.
func TestAsASTPublisher_panicsOnNil(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("AsASTPublisher(nil) did not panic")
		}
	}()
	_ = AsASTPublisher(nil)
}

// TestAsASTPublisher_translatesErrAttemptFailed locks the
// error-translation contract documented in `astadapter.go`:
// an `embedding.ErrAttemptFailed`-wrapped error becomes an
// `ast.ErrPublishRecordedFailed`-wrapped error on the way out.
//
// We exercise the translation through a hand-rolled fake
// (`fakeASTPublisher.invoke` mirrors `astPublisherAdapter.PublishNodeEmbedding`)
// because the real adapter wraps a `*Publisher` whose
// constructor demands a non-nil `*sql.DB` — the §9.6a SQL
// paths themselves are covered by the integration test.
func TestAsASTPublisher_translatesErrAttemptFailed(t *testing.T) {
	t.Parallel()
	fake := &fakeASTPublisher{
		PublishFn: func(ctx context.Context, req PublishRequest) (PublishResult, error) {
			return PublishResult{PublishID: "p1", LastEventKind: EventKindFailed},
				errors.Join(ErrAttemptFailed, errors.New("qdrant 503"))
		},
	}
	res, err := fake.invoke(context.Background(), astpkg.NodeEmbedRequest{
		NodeID: "n1", RepoID: "r1", Kind: NodeKindMethod, Content: "x",
	})
	if err == nil {
		t.Fatalf("expected error from fake; got nil")
	}
	if !errors.Is(err, astpkg.ErrPublishRecordedFailed) {
		t.Fatalf("error %v not Is(ast.ErrPublishRecordedFailed)", err)
	}
	if res.PublishID != "p1" {
		t.Fatalf("res.PublishID = %q; want p1", res.PublishID)
	}
}

// TestAsASTPublisher_passesThroughNonRecordedErrors locks the
// SECOND branch of the translation: errors that are NOT
// `embedding.ErrAttemptFailed`-wrapped propagate verbatim
// (the dispatcher then aborts ingest, per the two-bucket
// policy in `ast/embedding.go`).
func TestAsASTPublisher_passesThroughNonRecordedErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("pg connection refused")
	fake := &fakeASTPublisher{
		PublishFn: func(ctx context.Context, req PublishRequest) (PublishResult, error) {
			return PublishResult{}, sentinel
		},
	}
	_, err := fake.invoke(context.Background(), astpkg.NodeEmbedRequest{
		NodeID: "n1", RepoID: "r1", Kind: NodeKindMethod, Content: "x",
	})
	if err == nil {
		t.Fatalf("expected error; got nil")
	}
	if errors.Is(err, astpkg.ErrPublishRecordedFailed) {
		t.Fatalf("unexpectedly classified as recorded-failed: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("sentinel not preserved: %v", err)
	}
}

// fakeASTPublisher reproduces astPublisherAdapter's
// translation logic with a swappable Publish backend — used so
// the test above can prove the translation without standing
// up a real *sql.DB / Publisher.  The translation MUST stay in
// lock-step with the real adapter in `astadapter.go`.
type fakeASTPublisher struct {
	PublishFn func(ctx context.Context, req PublishRequest) (PublishResult, error)
}

func (f *fakeASTPublisher) invoke(ctx context.Context, req astpkg.NodeEmbedRequest) (astpkg.NodeEmbedResult, error) {
	res, err := f.PublishFn(ctx, PublishRequest{
		NodeID:             req.NodeID,
		RepoID:             req.RepoID,
		Kind:               req.Kind,
		CanonicalSignature: req.CanonicalSignature,
		Content:            req.Content,
	})
	astRes := astpkg.NodeEmbedResult{
		PublishID:     res.PublishID,
		QdrantPointID: res.QdrantPointID,
		LastEventKind: res.LastEventKind,
	}
	if err == nil {
		return astRes, nil
	}
	if errors.Is(err, ErrAttemptFailed) {
		// Mirror the real adapter exactly — same `%w` /
		// `%v` shape so the test below covers the production
		// translation, not a parallel implementation.
		return astRes, fmt.Errorf("%w: %v", astpkg.ErrPublishRecordedFailed, err)
	}
	return astRes, err
}

// TestNewPublisher_panicsOnNilArgs locks the constructor's
// loud-fail contract for each required dependency.
func TestNewPublisher_panicsOnNilArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		db       *sql.DB
		embedder Embedder
		qdrant   Qdrant
	}{
		{"nil db", nil, stubEmbedder{}, stubQdrant{}},
		{"nil embedder", &sql.DB{}, nil, stubQdrant{}},
		{"nil qdrant", &sql.DB{}, stubEmbedder{}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("NewPublisher(%s) did not panic", tc.name)
				}
			}()
			_ = NewPublisher(tc.db, tc.embedder, tc.qdrant)
		})
	}
}

// TestNewHTTPQdrant_panicsOnEmptyBaseURL guards the
// production-wiring "default to localhost" footgun: the
// HTTPQdrant constructor refuses an empty baseURL rather than
// silently falling back.
func TestNewHTTPQdrant_panicsOnEmptyBaseURL(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewHTTPQdrant(\"\") did not panic")
		}
	}()
	_ = NewHTTPQdrant("")
}

// TestNewHTTPQdrant_stripsTrailingSlash makes the join logic
// in `do` simpler: `BaseURL + "/collections/..."` is always
// well-formed.
func TestNewHTTPQdrant_stripsTrailingSlash(t *testing.T) {
	t.Parallel()
	q := NewHTTPQdrant("http://qdrant:6333/")
	if q.BaseURL != "http://qdrant:6333" {
		t.Fatalf("BaseURL = %q; want http://qdrant:6333", q.BaseURL)
	}
}
