// Tests for the Stage 10.1 linked-mode adapter
// (architecture Sec 8.7). Every test pins ONE observable
// behaviour of the wire contract OR the aggregator
// adapter's two-axis gating. The whole file MUST stay
// hermetic -- no real agent-memory dependency -- so the
// suite runs deterministically in CI.
package linked

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/management"
)

// --- helpers ---------------------------------------------

// mustUUID parses a UUID literal at test setup; a parse
// failure is a setup bug not a test failure.
func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("mustUUID(%q): %v", s, err)
	}
	return u
}

// newTestServer wires an httptest server with the supplied
// handler and returns the URL + a teardown.
func newTestServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// fakeClient is a hand-rolled stub implementing [Client]; the
// adapter unit tests use it to control the per-call return
// shape without spinning an httptest server.
type fakeClient struct {
	calls   atomic.Int32
	edges   EdgeSet
	err     error
	onCall  func(repoID uuid.UUID, sha string)
	wantSHA string
}

func (f *fakeClient) FetchEdges(ctx context.Context, repoID uuid.UUID, sha string) (EdgeSet, error) {
	f.calls.Add(1)
	if f.onCall != nil {
		f.onCall(repoID, sha)
	}
	if f.wantSHA != "" && sha != f.wantSHA {
		return EdgeSet{}, fmt.Errorf("fakeClient: got sha=%q, want %q", sha, f.wantSHA)
	}
	return f.edges, f.err
}

// fakeModeReader is a hand-rolled stub implementing
// [RepoModeReader].
type fakeModeReader struct {
	mode string
	err  error
	hits atomic.Int32
}

func (f *fakeModeReader) ReadRepoMode(ctx context.Context, repoID uuid.UUID) (string, error) {
	f.hits.Add(1)
	return f.mode, f.err
}

// --- HTTPClient construction -----------------------------

func TestNewHTTPClient_EmptyEndpoint(t *testing.T) {
	t.Parallel()
	_, err := NewHTTPClient("")
	if !errors.Is(err, ErrEndpointEmpty) {
		t.Fatalf("NewHTTPClient(\"\") err = %v, want ErrEndpointEmpty", err)
	}
}

func TestNewHTTPClient_InvalidEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"unsupported_scheme", "ftp://agent-memory.internal/"},
		{"no_scheme", "agent-memory.internal/"},
		{"empty_host", "https:///path"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewHTTPClient(tc.in)
			if !errors.Is(err, ErrInvalidEndpoint) {
				t.Fatalf("NewHTTPClient(%q) err = %v, want ErrInvalidEndpoint", tc.in, err)
			}
		})
	}
}

func TestNewHTTPClient_AcceptsHTTPAndHTTPS(t *testing.T) {
	t.Parallel()
	for _, ep := range []string{"http://am.internal", "https://am.internal/", "http://am.internal:8080/base"} {
		ep := ep
		t.Run(ep, func(t *testing.T) {
			t.Parallel()
			c, err := NewHTTPClient(ep)
			if err != nil {
				t.Fatalf("NewHTTPClient(%q) unexpected err: %v", ep, err)
			}
			if c == nil {
				t.Fatalf("NewHTTPClient(%q) returned nil client", ep)
			}
			if c.timeout != DefaultTimeout {
				t.Errorf("default timeout = %s, want %s", c.timeout, DefaultTimeout)
			}
		})
	}
}

func TestNewHTTPClient_OptionsApply(t *testing.T) {
	t.Parallel()
	custom := &http.Client{Timeout: 17 * time.Second}
	c, err := NewHTTPClient("https://am.internal/", WithHTTPClient(custom), WithTimeout(2*time.Second), WithUserAgent("ua-test/1.0"))
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if c.httpClient != custom {
		t.Errorf("WithHTTPClient did not replace http client")
	}
	if c.timeout != 2*time.Second {
		t.Errorf("timeout = %s, want 2s", c.timeout)
	}
	if c.userAgent != "ua-test/1.0" {
		t.Errorf("userAgent = %q, want %q", c.userAgent, "ua-test/1.0")
	}
}

// --- HTTPClient.FetchEdges -------------------------------

func TestFetchEdges_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	c, err := NewHTTPClient("http://example.invalid/")
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	_, err = c.FetchEdges(context.Background(), uuid.Nil, "abc")
	if !errors.Is(err, ErrZeroRepoID) {
		t.Fatalf("FetchEdges(zero uuid) err = %v, want ErrZeroRepoID", err)
	}
}

func TestFetchEdges_RejectsEmptySHA(t *testing.T) {
	t.Parallel()
	c, err := NewHTTPClient("http://example.invalid/")
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	repoID := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	for _, sha := range []string{"", "   ", "\t"} {
		_, err := c.FetchEdges(context.Background(), repoID, sha)
		if !errors.Is(err, ErrEmptySHA) {
			t.Errorf("FetchEdges(sha=%q) err = %v, want ErrEmptySHA", sha, err)
		}
	}
}

func TestFetchEdges_Happy(t *testing.T) {
	t.Parallel()
	repoID := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	from := mustUUID(t, "22222222-2222-2222-2222-222222222222")
	to := mustUUID(t, "33333333-3333-3333-3333-333333333333")
	fromScope := mustUUID(t, "44444444-4444-4444-4444-444444444444")
	toScope := mustUUID(t, "55555555-5555-5555-5555-555555555555")
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != crossRepoEdgesPath {
			t.Errorf("server got path %q, want %q", r.URL.Path, crossRepoEdgesPath)
		}
		if got := r.URL.Query().Get("repo_id"); got != repoID.String() {
			t.Errorf("repo_id query = %q, want %q", got, repoID.String())
		}
		if got := r.URL.Query().Get("sha"); got != "deadbeef" {
			t.Errorf("sha query = %q, want %q", got, "deadbeef")
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept header = %q, want application/json", got)
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "clean-code-linked-adapter") {
			t.Errorf("User-Agent = %q, want contains 'clean-code-linked-adapter'", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(EdgeSet{
			XRepoEdges:          []XRepoEdge{{FromRepo: from, ToRepo: to}},
			XRepoEdgesAvailable: true,
			CallEdges:           []CallEdge{{FromScope: fromScope, ToScope: toScope}},
			CallEdgesAvailable:  true,
		})
	})
	c, err := NewHTTPClient(url)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	edges, err := c.FetchEdges(context.Background(), repoID, "deadbeef")
	if err != nil {
		t.Fatalf("FetchEdges: %v", err)
	}
	if !edges.XRepoEdgesAvailable || !edges.CallEdgesAvailable {
		t.Fatalf("availability flags both expected true, got xrepo=%v call=%v", edges.XRepoEdgesAvailable, edges.CallEdgesAvailable)
	}
	if len(edges.XRepoEdges) != 1 || edges.XRepoEdges[0].FromRepo != from || edges.XRepoEdges[0].ToRepo != to {
		t.Errorf("XRepoEdges = %+v, want one edge from->to", edges.XRepoEdges)
	}
	if len(edges.CallEdges) != 1 || edges.CallEdges[0].FromScope != fromScope || edges.CallEdges[0].ToScope != toScope {
		t.Errorf("CallEdges = %+v, want one edge from->to", edges.CallEdges)
	}
}

func TestFetchEdges_EmptyButAvailable(t *testing.T) {
	t.Parallel()
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"xrepo_edges":[],"xrepo_edges_available":true,"call_edges":[],"call_edges_available":true}`)
	})
	c, _ := NewHTTPClient(url)
	edges, err := c.FetchEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if err != nil {
		t.Fatalf("FetchEdges: %v", err)
	}
	if !edges.XRepoEdgesAvailable || !edges.CallEdgesAvailable {
		t.Errorf("availability flags expected true (empty-but-available shape)")
	}
	if len(edges.XRepoEdges) != 0 || len(edges.CallEdges) != 0 {
		t.Errorf("expected empty slices, got xrepo=%d call=%d", len(edges.XRepoEdges), len(edges.CallEdges))
	}
}

func TestFetchEdges_MissingAvailabilityDefaultsFalse(t *testing.T) {
	t.Parallel()
	// Wire omits the availability flags -- the server's
	// response is parseable but availability silently defaults
	// to FALSE. This matches the "missing != available empty"
	// invariant documented on EdgeSet.
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"xrepo_edges":[],"call_edges":[]}`)
	})
	c, _ := NewHTTPClient(url)
	edges, err := c.FetchEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if err != nil {
		t.Fatalf("FetchEdges: %v", err)
	}
	if edges.XRepoEdgesAvailable || edges.CallEdgesAvailable {
		t.Fatalf("availability flags should default false when wire omits them; got xrepo=%v call=%v",
			edges.XRepoEdgesAvailable, edges.CallEdgesAvailable)
	}
}

func TestFetchEdges_404IsUnavailable(t *testing.T) {
	t.Parallel()
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	c, _ := NewHTTPClient(url)
	_, err := c.FetchEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if !errors.Is(err, ErrEdgesUnavailable) {
		t.Fatalf("FetchEdges err = %v, want ErrEdgesUnavailable", err)
	}
}

func TestFetchEdges_5xxIsUnexpected(t *testing.T) {
	t.Parallel()
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	c, _ := NewHTTPClient(url)
	_, err := c.FetchEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("FetchEdges err = %v, want ErrUnexpectedStatus", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %q, want to contain status code 502", err.Error())
	}
}

func TestFetchEdges_MalformedJSON(t *testing.T) {
	t.Parallel()
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{not json`)
	})
	c, _ := NewHTTPClient(url)
	_, err := c.FetchEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("FetchEdges err = %v, want ErrMalformedResponse", err)
	}
}

func TestFetchEdges_RejectsUnknownJSONFields(t *testing.T) {
	t.Parallel()
	// DisallowUnknownFields ensures wire-shape drift surfaces
	// as a hard error rather than silently dropping data.
	// The production default is tolerant (forward-compat with
	// additive agent-memory wire fields); strict decoding is
	// opt-in via WithStrictDecoding(true) for contract tests
	// and canary deploys.
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"xrepo_edges":[],"unknown_field":1}`)
	})
	c, _ := NewHTTPClient(url, WithStrictDecoding(true))
	_, err := c.FetchEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("FetchEdges err = %v, want ErrMalformedResponse (DisallowUnknownFields should catch unknown_field)", err)
	}
}

func TestFetchEdges_CtxCancelledBeforeRequest(t *testing.T) {
	t.Parallel()
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{}"))
	})
	c, _ := NewHTTPClient(url)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.FetchEdges(ctx, mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchEdges err = %v, want context.Canceled", err)
	}
}

func TestFetchEdges_PerRequestTimeoutFires(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	defer close(block)
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Block forever -- the per-request timeout should
		// abort the client side before the handler returns.
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	c, _ := NewHTTPClient(url, WithTimeout(50*time.Millisecond))
	_, err := c.FetchEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FetchEdges err = %v, want context.DeadlineExceeded", err)
	}
}

// --- AggregatorAdapter -----------------------------------

func TestNewAggregatorAdapter_NilClientPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for nil client")
		}
	}()
	_ = NewAggregatorAdapter(nil, &fakeModeReader{mode: "linked"}, true, nil)
}

func TestNewAggregatorAdapter_NilModeReaderPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for nil mode reader")
		}
	}()
	_ = NewAggregatorAdapter(&fakeClient{}, nil, true, nil)
}

func TestAggregatorAdapter_DisabledShortCircuits(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{}
	fm := &fakeModeReader{mode: "linked"}
	a := NewAggregatorAdapter(fc, fm, false, nil)
	got, err := a.ResolveLinkedEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Applicable {
		t.Errorf("Applicable = true, want false when disabled")
	}
	if fc.calls.Load() != 0 {
		t.Errorf("client.calls = %d, want 0 (must not fire when disabled)", fc.calls.Load())
	}
	if fm.hits.Load() != 0 {
		t.Errorf("modeReader.hits = %d, want 0 (must not fire when disabled)", fm.hits.Load())
	}
}

func TestAggregatorAdapter_EmbeddedModeShortCircuits(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{}
	fm := &fakeModeReader{mode: management.RepoModeEmbedded}
	a := NewAggregatorAdapter(fc, fm, true, nil)
	got, err := a.ResolveLinkedEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Applicable {
		t.Errorf("Applicable = true, want false when repo mode embedded")
	}
	if fc.calls.Load() != 0 {
		t.Errorf("client.calls = %d, want 0 (must not fetch in embedded mode)", fc.calls.Load())
	}
	if fm.hits.Load() != 1 {
		t.Errorf("modeReader.hits = %d, want 1 (one mode-read per call)", fm.hits.Load())
	}
}

func TestAggregatorAdapter_LinkedModeHappy(t *testing.T) {
	t.Parallel()
	from := mustUUID(t, "22222222-2222-2222-2222-222222222222")
	to := mustUUID(t, "33333333-3333-3333-3333-333333333333")
	fc := &fakeClient{
		edges: EdgeSet{
			XRepoEdges:          []XRepoEdge{{FromRepo: from, ToRepo: to}},
			XRepoEdgesAvailable: true,
			CallEdges:           nil,
			CallEdgesAvailable:  false,
		},
		wantSHA: "deadbeef",
	}
	fm := &fakeModeReader{mode: management.RepoModeLinked}
	a := NewAggregatorAdapter(fc, fm, true, nil)
	got, err := a.ResolveLinkedEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got.Applicable {
		t.Fatalf("Applicable = false, want true")
	}
	if !got.XRepoEdgesAvailable || got.CallEdgesAvailable {
		t.Errorf("availability flags = (xrepo=%v, call=%v), want (true, false)", got.XRepoEdgesAvailable, got.CallEdgesAvailable)
	}
	if len(got.XRepoEdges) != 1 || got.XRepoEdges[0].FromRepo != from || got.XRepoEdges[0].ToRepo != to {
		t.Errorf("XRepoEdges = %+v, want one edge from->to", got.XRepoEdges)
	}
	if len(got.CallEdges) != 0 {
		t.Errorf("CallEdges = %+v, want empty (CallEdgesAvailable was false)", got.CallEdges)
	}
}

func TestAggregatorAdapter_ModeReadErrorPropagates(t *testing.T) {
	t.Parallel()
	storeErr := errors.New("pg: connection refused")
	fc := &fakeClient{}
	fm := &fakeModeReader{err: storeErr}
	a := NewAggregatorAdapter(fc, fm, true, nil)
	_, err := a.ResolveLinkedEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if err == nil {
		t.Fatalf("want propagated error, got nil")
	}
	if !errors.Is(err, storeErr) {
		t.Errorf("err = %v, want errors.Is(storeErr) (raw cause must remain on chain for operator log detail)", err)
	}
	if !errors.Is(err, aggregator.ErrLinkedModeStore) {
		t.Errorf("err = %v, want errors.Is(aggregator.ErrLinkedModeStore) (sentinel must be wrapped so aggregator can classify as FATAL)", err)
	}
	if fc.calls.Load() != 0 {
		t.Errorf("client.calls = %d, want 0 when mode read fails", fc.calls.Load())
	}
}

func TestAggregatorAdapter_ClientErrorPropagates(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{err: ErrEdgesUnavailable}
	fm := &fakeModeReader{mode: management.RepoModeLinked}
	a := NewAggregatorAdapter(fc, fm, true, nil)
	_, err := a.ResolveLinkedEdges(context.Background(), mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if !errors.Is(err, ErrEdgesUnavailable) {
		t.Fatalf("err = %v, want ErrEdgesUnavailable preserved through adapter", err)
	}
}

func TestAggregatorAdapter_CtxErrorPropagates(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{err: context.Canceled}
	fm := &fakeModeReader{mode: management.RepoModeLinked}
	a := NewAggregatorAdapter(fc, fm, true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.ResolveLinkedEdges(ctx, mustUUID(t, "11111111-1111-1111-1111-111111111111"), "deadbeef")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled propagated", err)
	}
}

func TestAggregatorAdapter_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	fc := &fakeClient{}
	fm := &fakeModeReader{mode: management.RepoModeLinked}
	a := NewAggregatorAdapter(fc, fm, true, nil)
	_, err := a.ResolveLinkedEdges(context.Background(), uuid.Nil, "deadbeef")
	if !errors.Is(err, ErrZeroRepoID) {
		t.Fatalf("err = %v, want ErrZeroRepoID", err)
	}
}

func TestAggregatorAdapter_EnabledFlag(t *testing.T) {
	t.Parallel()
	off := NewAggregatorAdapter(&fakeClient{}, &fakeModeReader{}, false, nil)
	on := NewAggregatorAdapter(&fakeClient{}, &fakeModeReader{}, true, nil)
	if off.Enabled() {
		t.Errorf("disabled adapter reports Enabled() = true")
	}
	if !on.Enabled() {
		t.Errorf("enabled adapter reports Enabled() = false")
	}
}

// TestAggregatorAdapter_ModeLiteral pins the canonical mode
// string the adapter compares against. The linked package
// duplicates the string rather than importing
// `management.RepoModeLinked` to avoid a coupling cycle;
// this test ensures a rename in management surfaces as a
// unit-test failure here.
func TestAggregatorAdapter_ModeLiteral(t *testing.T) {
	t.Parallel()
	if linkedModeValue != management.RepoModeLinked {
		t.Fatalf("linkedModeValue = %q, want management.RepoModeLinked = %q (the two literals MUST stay in sync)",
			linkedModeValue, management.RepoModeLinked)
	}
}

// TestAggregatorAdapter_SatisfiesAggregatorInterface is a
// compile-time pin via the `var _ ...` in client.go; the
// run-time variant here lets a `go test ./...` failure name
// the seam explicitly.
func TestAggregatorAdapter_SatisfiesAggregatorInterface(t *testing.T) {
	t.Parallel()
	var _ aggregator.LinkedEdgeReader = (*AggregatorAdapter)(nil)
}

// --- end-to-end: HTTPClient wired into adapter -----------

func TestHTTPClient_WiredThroughAdapter(t *testing.T) {
	t.Parallel()
	repoID := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	from := mustUUID(t, "22222222-2222-2222-2222-222222222222")
	to := mustUUID(t, "33333333-3333-3333-3333-333333333333")
	url := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(EdgeSet{
			XRepoEdges:          []XRepoEdge{{FromRepo: from, ToRepo: to}},
			XRepoEdgesAvailable: true,
			CallEdges:           []CallEdge{},
			CallEdgesAvailable:  true,
		})
	})
	client, err := NewHTTPClient(url)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	a := NewAggregatorAdapter(client, &fakeModeReader{mode: management.RepoModeLinked}, true, nil)
	got, err := a.ResolveLinkedEdges(context.Background(), repoID, "deadbeef")
	if err != nil {
		t.Fatalf("ResolveLinkedEdges: %v", err)
	}
	if !got.Applicable {
		t.Fatalf("Applicable = false, want true")
	}
	if !got.XRepoEdgesAvailable || !got.CallEdgesAvailable {
		t.Errorf("availability flags = (xrepo=%v, call=%v), want both true", got.XRepoEdgesAvailable, got.CallEdgesAvailable)
	}
	if len(got.XRepoEdges) != 1 {
		t.Errorf("XRepoEdges length = %d, want 1", len(got.XRepoEdges))
	}
}
