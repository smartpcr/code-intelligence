package linked

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
)

// XRepoEdge is one directed cross-repo dependency edge as
// reported by the agent-memory linked-mode endpoint. The
// wire-side counterpart of [aggregator.XRepoEdge]; the
// [AggregatorAdapter] converts between the two so the linked
// package owns its own wire shape independent of the
// aggregator's in-memory composer types.
//
// Edges are directed: `FromRepo` depends on `ToRepo`.
type XRepoEdge struct {
	FromRepo uuid.UUID `json:"from_repo"`
	ToRepo   uuid.UUID `json:"to_repo"`
}

// CallEdge is one directed downstream call-graph edge.
// Edges are directed: `FromScope` is called by `ToScope`
// (`ToScope` would break if `FromScope` regresses).
type CallEdge struct {
	FromScope uuid.UUID `json:"from_scope"`
	ToScope   uuid.UUID `json:"to_scope"`
}

// EdgeSet is the parsed body of one successful FetchEdges
// call. Both availability flags are EXPLICIT booleans on the
// wire so the aggregator can degrade `xrepo_dep_depth` and
// `blast_radius` independently. An UNSPECIFIED flag on the
// wire defaults to FALSE (Go zero) -- "missing" is never
// treated as "available empty"; the rubber-duck iter-1
// review flagged the silent-non-degradation hazard.
type EdgeSet struct {
	// XRepoEdges is the directed cross-repo dependency edge
	// set the `xrepo_dep_depth` composer reads. May be
	// non-nil but empty when the repo has no cross-repo
	// dependencies AND XRepoEdgesAvailable is true (a
	// legitimate `xrepo_dep_depth = 0` shape).
	XRepoEdges []XRepoEdge `json:"xrepo_edges"`
	// XRepoEdgesAvailable signals the agent-memory subsystem
	// successfully indexed cross-repo edges for the pair.
	// MUST be set true for the empty-but-available shape to
	// be distinguishable from the unindexed-pair shape.
	XRepoEdgesAvailable bool `json:"xrepo_edges_available"`
	// CallEdges is the directed call-graph edge set the
	// `blast_radius` composer reads. Same empty-but-available
	// rules apply.
	CallEdges []CallEdge `json:"call_edges"`
	// CallEdgesAvailable signals the agent-memory subsystem
	// successfully indexed call-graph edges for the pair.
	CallEdgesAvailable bool `json:"call_edges_available"`
}

// Client is the narrow seam the [AggregatorAdapter] consumes.
// Implementations MUST be safe for concurrent invocation.
// Production callers wire [HTTPClient]; tests substitute the
// in-memory [FakeClient] (see `client_test.go`).
type Client interface {
	// FetchEdges fetches the cross-repo and call-graph edge
	// sets for `(repoID, sha)`. The implementation MUST
	// honour `ctx` (HTTP requests are cancelled on
	// `ctx.Done`). On HTTP non-2xx the implementation
	// returns the appropriate sentinel ([ErrEdgesUnavailable]
	// for 404, [ErrUnexpectedStatus] for other 4xx/5xx); on
	// malformed body it returns [ErrMalformedResponse].
	FetchEdges(ctx context.Context, repoID uuid.UUID, sha string) (EdgeSet, error)
}

// Sentinel errors returned by [HTTPClient.FetchEdges]. Tests
// pin against these via [errors.Is]; the [AggregatorAdapter]
// also distinguishes them from [context.Canceled] /
// [context.DeadlineExceeded] (which are propagated upward to
// abort the tick rather than degrade in place).
var (
	// ErrEndpointEmpty is returned by [NewHTTPClient] when
	// the operator-supplied endpoint string is empty. The
	// composition root MUST refuse to wire the adapter
	// without an endpoint -- the `config` package's
	// `EnableLinkedModeAdapter` interlock catches this at
	// startup so a production binary never reaches this
	// sentinel.
	ErrEndpointEmpty = errors.New("linked: NewHTTPClient: endpoint is empty")

	// ErrInvalidEndpoint is returned by [NewHTTPClient] when
	// the endpoint string fails [url.Parse] or is not an
	// `http`/`https` URL.
	ErrInvalidEndpoint = errors.New("linked: NewHTTPClient: endpoint is not a valid http(s) URL")

	// ErrZeroRepoID is returned by [HTTPClient.FetchEdges]
	// when the caller passes the zero UUID -- a defensive
	// guard against a wiring bug that would otherwise emit
	// `repo_id=00000000-0000-0000-0000-000000000000` to
	// agent-memory.
	ErrZeroRepoID = errors.New("linked: FetchEdges: repo_id is the zero UUID")

	// ErrEmptySHA is returned by [HTTPClient.FetchEdges]
	// when the caller passes an empty SHA. Same defensive
	// rationale as [ErrZeroRepoID].
	ErrEmptySHA = errors.New("linked: FetchEdges: sha is empty")

	// ErrEdgesUnavailable is returned by
	// [HTTPClient.FetchEdges] when the agent-memory endpoint
	// responds with 404. Per the package wire contract this
	// signals "agent-memory has NOT indexed this repo+sha
	// pair OR the endpoint route is wrong" -- explicitly
	// NOT "this repo has zero edges" (that case is
	// expressed as 200 with empty arrays + availability
	// flags TRUE). The [AggregatorAdapter] propagates this
	// as a remote-failure so the aggregator degrades the
	// affected `xrepo_dep_depth` / `blast_radius` rows with
	// `xrepo_edges_unavailable`.
	ErrEdgesUnavailable = errors.New("linked: FetchEdges: agent-memory has not indexed this repo+sha pair (HTTP 404)")

	// ErrUnexpectedStatus is returned by
	// [HTTPClient.FetchEdges] on any non-2xx, non-404 HTTP
	// status code. The error message includes the status
	// for operator triage.
	ErrUnexpectedStatus = errors.New("linked: FetchEdges: unexpected HTTP status from agent-memory")

	// ErrMalformedResponse is returned when the 200 body
	// fails JSON decode against the [EdgeSet] schema.
	ErrMalformedResponse = errors.New("linked: FetchEdges: malformed JSON response body")
)

// DefaultTimeout is the per-request HTTP timeout the
// [HTTPClient] applies when no [WithTimeout] option is
// supplied. 5 seconds is short enough to keep the aggregator
// tick latency bounded on a single agent-memory outage and
// long enough for the cross-repo index lookup at p99 (the
// agent-memory team's published SLO is sub-second for the
// graph-reader read verbs per their architecture Sec 6.2.3).
const DefaultTimeout = 5 * time.Second

// HTTPClient is the production [Client]: a thin wrapper around
// an [http.Client] that issues one GET per FetchEdges call.
//
// # Concurrency
//
// Safe for concurrent invocation; the underlying
// [http.Client] is concurrency-safe and the wrapper holds no
// mutable state past construction.
//
// # Timeout layering
//
// FetchEdges derives a child context with [DefaultTimeout]
// (or the [WithTimeout] override) so a slow agent-memory
// response is bounded EVEN IF the caller's `ctx` has no
// deadline. The caller's `ctx` is still honoured via
// [context.WithCancel] composition -- whichever fires first
// wins.
type HTTPClient struct {
	endpoint   *url.URL
	httpClient *http.Client
	timeout    time.Duration
	userAgent  string
	logger     *slog.Logger
}

// Option configures an [HTTPClient].
type Option func(*HTTPClient)

// WithHTTPClient overrides the underlying [http.Client]. The
// composition root uses this to inject a shared client with
// connection pooling / OTel instrumentation; tests inject
// `httptest.Server.Client()` for the right TLS chain.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *HTTPClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTimeout overrides [DefaultTimeout] for each FetchEdges
// call. A non-positive value is ignored (defaults stand).
func WithTimeout(d time.Duration) Option {
	return func(c *HTTPClient) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithUserAgent sets the `User-Agent` header sent to
// agent-memory. Defaults to `clean-code-linked-adapter/v1`.
func WithUserAgent(ua string) Option {
	return func(c *HTTPClient) {
		ua = strings.TrimSpace(ua)
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// WithLogger overrides the slog logger the client uses for
// debug-level wire failures. Nil leaves the client silent
// (slog.Default() is NOT consulted automatically -- pass an
// explicit logger if you want output).
func WithLogger(log *slog.Logger) Option {
	return func(c *HTTPClient) {
		c.logger = log
	}
}

// NewHTTPClient constructs an [HTTPClient]. The `endpoint`
// is the agent-memory base URL (e.g.
// `https://agent-memory.internal/`). The HTTP path
// `/v1/cross-repo/edges` is appended at request time so the
// operator does not need to know the path structure.
func NewHTTPClient(endpoint string, opts ...Option) (*HTTPClient, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, ErrEndpointEmpty
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidEndpoint, err.Error())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme=%q (expected http|https)", ErrInvalidEndpoint, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: empty host", ErrInvalidEndpoint)
	}
	c := &HTTPClient{
		endpoint:   u,
		httpClient: &http.Client{},
		timeout:    DefaultTimeout,
		userAgent:  "clean-code-linked-adapter/v1",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// crossRepoEdgesPath is the canonical sub-path appended to
// the endpoint base URL. Pinned as a const so a future verb
// rename is a one-line change AND a `grep -nF
// "/v1/cross-repo/edges"` surfaces every reference.
const crossRepoEdgesPath = "/v1/cross-repo/edges"

// FetchEdges implements [Client]. See the package doc for the
// wire contract.
func (c *HTTPClient) FetchEdges(ctx context.Context, repoID uuid.UUID, sha string) (EdgeSet, error) {
	if repoID == uuid.Nil {
		return EdgeSet{}, ErrZeroRepoID
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return EdgeSet{}, ErrEmptySHA
	}

	reqURL := *c.endpoint // shallow copy so we do not mutate the configured base
	reqURL.Path = singleJoinPath(c.endpoint.Path, crossRepoEdgesPath)
	q := reqURL.Query()
	q.Set("repo_id", repoID.String())
	q.Set("sha", sha)
	reqURL.RawQuery = q.Encode()

	// Layer the per-request timeout on top of the caller's
	// ctx so a slow agent-memory response is bounded EVEN
	// when the caller passed context.Background(). The
	// child cancel fires when ctx is done OR the timeout
	// elapses, whichever comes first.
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return EdgeSet{}, fmt.Errorf("linked: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Surface ctx errors verbatim so callers can use
		// [errors.Is] to distinguish cancellation /
		// deadline from remote-network failure.
		if ctxErr := contextErr(reqCtx, err); ctxErr != nil {
			return EdgeSet{}, ctxErr
		}
		return EdgeSet{}, fmt.Errorf("linked: GET %s: %w", reqURL.String(), err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return EdgeSet{}, fmt.Errorf("%w (repo_id=%s, sha=%s)", ErrEdgesUnavailable, repoID, sha)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// Drain a bounded body slice for the error message
		// but do not let an over-eager 1 MB error body
		// chew memory.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return EdgeSet{}, fmt.Errorf("%w: status=%d body=%q (repo_id=%s, sha=%s)",
			ErrUnexpectedStatus, resp.StatusCode, string(body), repoID, sha)
	}

	var out EdgeSet
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64*1024*1024)) // 64 MiB ceiling guards against pathological responses
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return EdgeSet{}, fmt.Errorf("%w: %s (repo_id=%s, sha=%s)",
			ErrMalformedResponse, err.Error(), repoID, sha)
	}
	if c.logger != nil {
		c.logger.DebugContext(ctx, "linked.HTTPClient.FetchEdges",
			"repo_id", repoID.String(),
			"sha", sha,
			"xrepo_edges_count", len(out.XRepoEdges),
			"xrepo_edges_available", out.XRepoEdgesAvailable,
			"call_edges_count", len(out.CallEdges),
			"call_edges_available", out.CallEdgesAvailable,
		)
	}
	return out, nil
}

// contextErr returns ctx.Err() iff the supplied `err` looks
// like it was caused by ctx cancellation / deadline. We do
// the [errors.Is] check first AND fall back to inspecting
// ctx.Err() directly because some HTTP transports report
// ctx errors via wrapped sentinels that don't satisfy
// errors.Is for [context.Canceled] directly.
func contextErr(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// singleJoinPath joins `base` and `sub` with exactly one
// slash between them so the operator can supply either
// `https://host` or `https://host/` for the endpoint.
func singleJoinPath(base, sub string) string {
	base = strings.TrimRight(base, "/")
	sub = "/" + strings.TrimLeft(sub, "/")
	return base + sub
}

// RepoModeReader is the narrow read-only interface the
// [AggregatorAdapter] consumes to look up a repo's persisted
// mode. Duck-typed onto `management.RepoModeReader` so the
// linked package does NOT have to import management
// (avoiding a coupling that would re-introduce the cycle the
// architecture's package layout avoids).
//
// Implementations MUST return one of the canonical mode
// strings (`"embedded"` | `"linked"`). Any other value is
// treated as embedded by the adapter (defensive default).
type RepoModeReader interface {
	ReadRepoMode(ctx context.Context, repoID uuid.UUID) (string, error)
}

// AggregatorAdapter wires a [Client] + [RepoModeReader] +
// global-enable flag into the seam the aggregator consumes
// (`aggregator.LinkedEdgeReader`). It is the single place
// where the TWO-AXIS gate is enforced -- both axes default
// closed; both must be open before any HTTP call fires.
//
// # Concurrency
//
// Safe for concurrent invocation. The adapter holds no
// mutable state past construction; per-call state lives on
// the stack of [ResolveLinkedEdges].
type AggregatorAdapter struct {
	client     Client
	modeReader RepoModeReader
	enabled    bool
	logger     *slog.Logger
}

// NewAggregatorAdapter constructs the adapter. Passing
// `client == nil` or `modeReader == nil` panics at startup
// (composition-root wiring bug -- the aggregator MUST NOT
// silently no-op the linked-mode pass). When `enabled=false`
// the adapter is wired but every ResolveLinkedEdges call
// returns Applicable=false -- the operator can flip the
// global config flag at the next deploy without re-wiring
// the binary.
//
// `logger` may be nil; nil logger silences the adapter's
// debug output (`slog.Default()` is NOT consulted).
func NewAggregatorAdapter(client Client, modeReader RepoModeReader, enabled bool, logger *slog.Logger) *AggregatorAdapter {
	if client == nil {
		panic("linked: NewAggregatorAdapter: client is nil")
	}
	if modeReader == nil {
		panic("linked: NewAggregatorAdapter: modeReader is nil")
	}
	return &AggregatorAdapter{
		client:     client,
		modeReader: modeReader,
		enabled:    enabled,
		logger:     logger,
	}
}

// Enabled reports whether the global gate is open. The
// aggregator's health/exporter can probe this to surface
// "adapter wired but disabled" deployments.
func (a *AggregatorAdapter) Enabled() bool {
	return a != nil && a.enabled
}

// ResolveLinkedEdges satisfies [aggregator.LinkedEdgeReader].
// See the package doc's "Two-axis gating" + "Fail-safe
// contract" sections for the semantics.
//
// Return shape:
//
//   - global flag closed OR repo mode != "linked":
//     `{Applicable: false}` + nil error -- aggregator keeps
//     the embedded shape (composer degrades the row).
//   - mode-store error: `{}` + wrapped error -- aggregator
//     PROPAGATES (tick aborts; broken catalog is not a
//     fail-safe condition).
//   - linked + agent-memory remote error: `{}` + wrapped
//     remote error -- aggregator distinguishes ctx errors
//     (propagate) from remote errors (log + leave embedded).
//   - linked + happy path: `{Applicable: true, ...}` with
//     per-family availability flags echoing the wire.
func (a *AggregatorAdapter) ResolveLinkedEdges(ctx context.Context, repoID uuid.UUID, sha string) (aggregator.LinkedEdges, error) {
	if !a.enabled {
		return aggregator.LinkedEdges{Applicable: false}, nil
	}
	if err := ctx.Err(); err != nil {
		return aggregator.LinkedEdges{}, err
	}
	if repoID == uuid.Nil {
		// Defensive: this would otherwise emit
		// `repo_id=00000000-...` to agent-memory. The
		// aggregator's PG source filters zero UUIDs so we
		// should never reach this; the guard is
		// belt-and-braces.
		return aggregator.LinkedEdges{}, ErrZeroRepoID
	}

	mode, err := a.modeReader.ReadRepoMode(ctx, repoID)
	if err != nil {
		// Mode-store errors are FATAL (architecture pin:
		// the catalog read is not a fail-safe-to-degraded
		// condition). Propagate so the aggregator's outer
		// loop surfaces the operator-visible error.
		return aggregator.LinkedEdges{}, fmt.Errorf("linked.AggregatorAdapter: read repo mode (repo_id=%s): %w", repoID, err)
	}
	if mode != linkedModeValue {
		return aggregator.LinkedEdges{Applicable: false}, nil
	}

	edges, err := a.client.FetchEdges(ctx, repoID, sha)
	if err != nil {
		// Preserve the sentinel so the aggregator's caller
		// can [errors.Is] check ctx errors and remote
		// errors separately.
		return aggregator.LinkedEdges{}, err
	}

	if a.logger != nil {
		a.logger.DebugContext(ctx, "linked.AggregatorAdapter.ResolveLinkedEdges",
			"repo_id", repoID.String(),
			"sha", sha,
			"xrepo_edges_available", edges.XRepoEdgesAvailable,
			"call_edges_available", edges.CallEdgesAvailable,
		)
	}

	return aggregator.LinkedEdges{
		Applicable:          true,
		XRepoEdges:          toAggregatorXRepoEdges(edges.XRepoEdges),
		XRepoEdgesAvailable: edges.XRepoEdgesAvailable,
		CallEdges:           toAggregatorCallEdges(edges.CallEdges),
		CallEdgesAvailable:  edges.CallEdgesAvailable,
	}, nil
}

// linkedModeValue is the canonical mode string the adapter
// compares against. Pinned as a const here (rather than
// importing `management.RepoModeLinked`) to keep the linked
// package free of any management import. The two literals
// MUST stay in sync; the [TestAggregatorAdapter_ModeLiteral]
// test in `client_test.go` asserts the equality so a
// rename in management surfaces as a unit-test failure here.
const linkedModeValue = "linked"

// toAggregatorXRepoEdges maps the wire-side XRepoEdge slice
// into the aggregator-side XRepoEdge slice. Allocates a
// fresh slice (never returns the caller's backing array) so
// the adapter cannot leak its decode buffer into the
// composer.
func toAggregatorXRepoEdges(in []XRepoEdge) []aggregator.XRepoEdge {
	if len(in) == 0 {
		return nil
	}
	out := make([]aggregator.XRepoEdge, len(in))
	for i, e := range in {
		out[i] = aggregator.XRepoEdge{FromRepo: e.FromRepo, ToRepo: e.ToRepo}
	}
	return out
}

// toAggregatorCallEdges maps the wire-side CallEdge slice
// into the aggregator-side CallEdge slice. Same fresh-slice
// rationale as [toAggregatorXRepoEdges].
func toAggregatorCallEdges(in []CallEdge) []aggregator.CallEdge {
	if len(in) == 0 {
		return nil
	}
	out := make([]aggregator.CallEdge, len(in))
	for i, e := range in {
		out[i] = aggregator.CallEdge{FromScope: e.FromScope, ToScope: e.ToScope}
	}
	return out
}

// Compile-time interface guard: the adapter MUST satisfy the
// aggregator's seam. A future rename of either side
// surfaces here as a compile error.
var _ aggregator.LinkedEdgeReader = (*AggregatorAdapter)(nil)
