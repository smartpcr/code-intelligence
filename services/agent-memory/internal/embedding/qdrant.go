package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPQdrant is the production `Qdrant` implementation: a
// minimal REST client over `net/http` that talks to a Qdrant
// server provisioned by `cmd/qdrant-bootstrap`.  The bootstrap
// is the schema-owner; this client only writes / reads point
// bodies, so it is intentionally narrow (Upsert + PointExists)
// and has zero knowledge of collection lifecycles.
//
// Concurrency-safe: every `Upsert` / `PointExists` call opens
// its own `*http.Request`, so a single `HTTPQdrant` value can
// be shared across goroutines (the embedded `*http.Client`
// already promises that).
type HTTPQdrant struct {
	// BaseURL is the Qdrant server's root (e.g.
	// "http://qdrant:6333") with NO trailing slash; the client
	// strips one if present.
	BaseURL string
	// Client is the HTTP client used for every request.  Nil
	// means a default `http.Client{Timeout: 30s}` is used.
	Client *http.Client
	// UserAgent is propagated on every request.  Empty means
	// "agent-memory-embedding-publisher/1.0" — operators can
	// distinguish publisher traffic from bootstrap traffic in
	// the Qdrant access log via this header.
	UserAgent string
}

// NewHTTPQdrant constructs a Qdrant client pointed at
// `baseURL`.  Panics on empty `baseURL` because the production
// wiring cannot operate without a target server (and the
// alternative — a "default to localhost" fallback — would mask
// a configuration bug).
func NewHTTPQdrant(baseURL string) *HTTPQdrant {
	if strings.TrimSpace(baseURL) == "" {
		panic("embedding: NewHTTPQdrant: empty baseURL")
	}
	return &HTTPQdrant{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Client:    &http.Client{Timeout: 30 * time.Second},
		UserAgent: "agent-memory-embedding-publisher/1.0",
	}
}

// upsertPointsBody is the payload shape Qdrant's `/points`
// upsert API accepts.  Single-point bodies use the same shape
// as multi-point bodies (the Qdrant REST API does not
// differentiate at the URL).
type upsertPointsBody struct {
	Points []upsertPoint `json:"points"`
}

type upsertPoint struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Upsert satisfies `Qdrant.Upsert`.  Issues
// `PUT /collections/{collection}/points?wait=true`; the
// `wait=true` is load-bearing — without it the §9.6a step-5
// read-after-write fetch races against eventual consistency
// and the publisher records spurious `failed` events.
func (q *HTTPQdrant) Upsert(
	ctx context.Context,
	collection, pointID string,
	vector []float32,
	payload map[string]any,
) error {
	if collection == "" {
		return errors.New("embedding: HTTPQdrant.Upsert: empty collection")
	}
	if pointID == "" {
		return errors.New("embedding: HTTPQdrant.Upsert: empty pointID")
	}
	body, err := json.Marshal(upsertPointsBody{
		Points: []upsertPoint{{ID: pointID, Vector: vector, Payload: payload}},
	})
	if err != nil {
		return fmt.Errorf("embedding: marshal upsert body: %w", err)
	}
	endpoint := fmt.Sprintf("%s/collections/%s/points?wait=true",
		q.BaseURL, url.PathEscape(collection))
	resp, err := q.do(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("embedding: qdrant upsert %s/%s: status %d: %s",
			collection, pointID, resp.StatusCode, readBodySnippet(resp.Body))
	}
	return nil
}

// pointReadResponse models the partial decode of Qdrant's
// `GET /collections/{collection}/points/{id}` response.  We
// only need the `result.id` field to confirm presence.
type pointReadResponse struct {
	Result *struct {
		ID any `json:"id"`
	} `json:"result"`
}

// PointExists satisfies `Qdrant.PointExists`.  Returns
// `(true, nil)` when the GET succeeded and the response body
// carried a non-empty result object; `(false, nil)` on 404 or
// on a 2xx with a null result.  Transport errors propagate.
func (q *HTTPQdrant) PointExists(ctx context.Context, collection, pointID string) (bool, error) {
	if collection == "" {
		return false, errors.New("embedding: HTTPQdrant.PointExists: empty collection")
	}
	if pointID == "" {
		return false, errors.New("embedding: HTTPQdrant.PointExists: empty pointID")
	}
	endpoint := fmt.Sprintf("%s/collections/%s/points/%s",
		q.BaseURL, url.PathEscape(collection), url.PathEscape(pointID))
	resp, err := q.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("embedding: qdrant get %s/%s: status %d: %s",
			collection, pointID, resp.StatusCode, readBodySnippet(resp.Body))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return false, fmt.Errorf("embedding: read qdrant body: %w", err)
	}
	var decoded pointReadResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false, fmt.Errorf("embedding: decode qdrant body: %w", err)
	}
	return decoded.Result != nil && decoded.Result.ID != nil, nil
}

func (q *HTTPQdrant) do(ctx context.Context, method, endpoint string, body io.Reader) (*http.Response, error) {
	client := q.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("embedding: build qdrant request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if q.UserAgent != "" {
		req.Header.Set("User-Agent", q.UserAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: qdrant %s %s: %w", method, endpoint, err)
	}
	return resp, nil
}

// readBodySnippet returns up to 1 KiB of the response body for
// inclusion in an error message.  Used only on non-2xx
// responses so the cost is negligible.
func readBodySnippet(r io.Reader) string {
	const cap = 1024
	buf, _ := io.ReadAll(io.LimitReader(r, cap))
	return strings.TrimSpace(string(buf))
}

// SearchRequest carries the §6.4 recall-pushdown shape: the
// query vector, the over-fetch limit, and the SERVER-side
// payload filters Qdrant applies BEFORE the k-NN scan.
// `RepoIDFilter` and `KindFilter` are both optional but
// strongly recommended — the publisher writes both fields
// onto every Qdrant point's payload precisely so the
// recall path can scope a search to a single repository or
// node kind without dragging unrelated vectors through the
// scan budget.
type SearchRequest struct {
	// Vector is the query embedding the caller wants to
	// search against.  Length must match the collection's
	// vector dimension (`cmd/qdrant-bootstrap` default: 768).
	Vector []float32
	// Limit is the over-fetch budget — Qdrant returns at
	// most this many candidates.  The recall layer is
	// expected to filter out unpublished hits AFTER the
	// k-NN, so callers commonly request `k * 3` or larger
	// to leave headroom for filtering.  Capped at 1024 by
	// the server-side default Qdrant config.
	Limit int
	// RepoIDFilter, when non-empty, restricts the scan to
	// points whose payload `repo_id` equals this value.
	// The publisher writes `req.RepoID` onto every point's
	// payload (see `publisher.go.buildPayload`) so this
	// filter is honoured for any vector published through
	// `embedding.Publisher`.
	RepoIDFilter string
	// KindFilter, when non-empty, restricts the scan to
	// points whose payload `kind` equals this value
	// (i.e. "method" or "block").  Production callers
	// usually leave this blank to fan out across both
	// kinds inside a single collection.
	KindFilter string
}

// SearchHit is the per-point shape Qdrant returns from a
// search.  Carries the `point_id` (so `RecallFilter` can
// dereference to a publish row), the `score`, and the
// payload Qdrant stored at upsert time.
type SearchHit struct {
	// PointID is the Qdrant point identifier — equal to
	// the `embedding_publish.qdrant_point_id` the
	// publisher minted at step 1 of §9.6a.
	PointID string
	// Score is Qdrant's similarity score for this hit
	// against the query vector.  Higher is better
	// (collection uses cosine distance per §8.1).
	Score float32
	// Payload is the raw payload map Qdrant stored on
	// the point at upsert time.  Same shape as
	// `publisher.go.buildPayload` returns.
	Payload map[string]any
}

// searchRequestBody is the Qdrant `/points/search` body.
type searchRequestBody struct {
	Vector      []float32         `json:"vector"`
	Limit       int               `json:"limit"`
	WithPayload bool              `json:"with_payload"`
	Filter      *searchBodyFilter `json:"filter,omitempty"`
}

type searchBodyFilter struct {
	Must []searchBodyMatch `json:"must,omitempty"`
}

type searchBodyMatch struct {
	Key   string                  `json:"key"`
	Match searchBodyMatchOperator `json:"match"`
}

type searchBodyMatchOperator struct {
	Value any `json:"value"`
}

type searchResponse struct {
	Result []struct {
		ID      any            `json:"id"`
		Score   float32        `json:"score"`
		Payload map[string]any `json:"payload"`
	} `json:"result"`
}

// Search runs a §6.4 k-NN scan against `collection` with the
// supplied request body.  Returns the hits in descending
// score order; an empty result is `([]SearchHit{}, nil)`.
// Hits are NOT filtered by §9.6a publish state — that is the
// recall layer's job via `RecallFilter.FilterPublishedPoints`.
//
// The endpoint used is `POST /collections/{collection}/points/search`
// — the canonical Qdrant search shape.  `with_payload=true`
// is set unconditionally so the caller can read back the
// `repo_id` / `kind` / `node_id` / `publish_id` /
// `canonical_signature` payload fields the publisher wrote
// at upsert.
func (q *HTTPQdrant) Search(ctx context.Context, collection string, req SearchRequest) ([]SearchHit, error) {
	if collection == "" {
		return nil, errors.New("embedding: HTTPQdrant.Search: empty collection")
	}
	if len(req.Vector) == 0 {
		return nil, errors.New("embedding: HTTPQdrant.Search: empty vector")
	}
	if req.Limit <= 0 {
		return nil, fmt.Errorf("embedding: HTTPQdrant.Search: invalid limit %d", req.Limit)
	}

	body := searchRequestBody{
		Vector:      req.Vector,
		Limit:       req.Limit,
		WithPayload: true,
	}
	var must []searchBodyMatch
	if req.RepoIDFilter != "" {
		must = append(must, searchBodyMatch{
			Key:   "repo_id",
			Match: searchBodyMatchOperator{Value: req.RepoIDFilter},
		})
	}
	if req.KindFilter != "" {
		must = append(must, searchBodyMatch{
			Key:   "kind",
			Match: searchBodyMatchOperator{Value: req.KindFilter},
		})
	}
	if len(must) > 0 {
		body.Filter = &searchBodyFilter{Must: must}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal search body: %w", err)
	}
	endpoint := fmt.Sprintf("%s/collections/%s/points/search",
		q.BaseURL, url.PathEscape(collection))
	resp, err := q.do(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding: qdrant search %s: status %d: %s",
			collection, resp.StatusCode, readBodySnippet(resp.Body))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("embedding: read qdrant search body: %w", err)
	}
	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("embedding: decode qdrant search body: %w", err)
	}

	out := make([]SearchHit, 0, len(decoded.Result))
	for _, r := range decoded.Result {
		id, ok := r.ID.(string)
		if !ok {
			// Numeric Qdrant ids are valid per the API,
			// but the publisher only mints UUIDs (per
			// `embedding_publish.qdrant_point_id` type);
			// surface non-string ids as a structural
			// wiring error rather than silently
			// stringifying them, so a misconfigured
			// upsert is caught here, not at the
			// `RecallFilter` join.
			return nil, fmt.Errorf(
				"embedding: qdrant search returned non-string id %T %v", r.ID, r.ID)
		}
		out = append(out, SearchHit{
			PointID: id,
			Score:   r.Score,
			Payload: r.Payload,
		})
	}
	return out, nil
}
