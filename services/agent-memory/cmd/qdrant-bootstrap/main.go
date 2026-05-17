// Command qdrant-bootstrap idempotently provisions the three
// Qdrant collections agent-memory needs: agent_memory_method,
// agent_memory_block, agent_memory_concept. It is the Qdrant
// counterpart of the SQL migrations under ./migrations -- a one-
// shot, idempotent setup invoked by the deploy/local stack and
// by CI before integration tests run.
//
// Why a separate binary (not a SQL-style migration runner):
//   - Qdrant has no first-class migration tool; collection
//     creation is a REST PUT.
//   - Collection schema changes (vector dim, distance, payload
//     indexes) are *destructive* (Qdrant cannot ALTER a
//     collection's vector size). The right model is "fail loudly
//     if the existing collection disagrees", not "silently
//     reconfigure" -- so the bootstrap validates on re-run
//     rather than no-op'ing or rewriting (rubber-duck #3).
//
// Scope (tech-spec §8.7.5 + §9.6 and implementation-plan.md
// Stage 1.4):
//   - Create three collections with cosine distance and the
//     configured vector size (default 768 -- gte-small).
//   - Create payload indexes on repo_id (uuid) and kind
//     (keyword) so GraphReader's filter pushdown works.
//   - Optionally take a baseline snapshot per collection
//     (--snapshot).
//   - Optionally run as a recurring snapshot scheduler
//     (--snapshot-interval=24h). Qdrant has no first-class
//     scheduled-snapshot API, so the scheduling lives in this
//     binary; deploy/local runs it as a sidecar.
//
// Usage:
//
//	# one-shot bootstrap (CI / first deploy)
//	qdrant-bootstrap --qdrant-url http://qdrant:6333
//
//	# bootstrap + recurring snapshot scheduler (production sidecar)
//	qdrant-bootstrap --qdrant-url http://qdrant:6333 \
//	                 --snapshot-interval 24h
//
//	# bootstrap + immediate baseline snapshot
//	qdrant-bootstrap --snapshot
//
//	# preview without contacting Qdrant
//	qdrant-bootstrap --dry-run
//
// Exit codes:
//
//	0 -- all collections present and conformant; if
//	     --snapshot-interval was set, the loop exited cleanly on
//	     SIGINT/SIGTERM.
//	1 -- one or more collections disagree with the configured
//	     schema OR the Qdrant endpoint was unreachable.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	// defaultQdrantURL is the deploy/local default.
	defaultQdrantURL = "http://localhost:6333"
	// defaultVectorSize matches the embedding model declared in
	// tech-spec §6.4. The project keeps the value tunable
	// because the embedder is selected per deployment; 768 is
	// the historical project default for E5-base-v2-style
	// models, the operator overrides as needed.
	defaultVectorSize  = 768
	defaultHTTPTimeout = 30 * time.Second

	// distanceCosine is the only similarity Qdrant variant
	// agent-memory uses. The Encoder must L2-normalize before
	// upsert; cosine is then equivalent to dot-product and
	// keeps the math identical to FAISS's IVF_FLAT (no surprise
	// when we migrate query traffic between stores in §9.6).
	distanceCosine = "Cosine"

	// httpUserAgent is what the bootstrap presents to Qdrant.
	// It is logged in qdrant's audit trail so operators can
	// tell setup activity apart from query traffic.
	httpUserAgent = "agent-memory-qdrant-bootstrap/1.0"
)

// defaultCollections lists the three collections this binary
// provisions (tech-spec §8.7.5). The names are stable contracts
// with the Encoder, Embedding Writer, and GraphReader; do not
// rename without a coordinated rollout.
var defaultCollections = []string{
	"agent_memory_method",
	"agent_memory_block",
	"agent_memory_concept",
}

// defaultPayloadIndexes is the per-collection payload index
// fixture. Both fields are referenced by every GraphReader
// query path that filters by repo or kind before vector
// rerank; without these indexes Qdrant falls back to a full
// payload scan, which is O(collection size) and unacceptable
// in production.
var defaultPayloadIndexes = []payloadIndex{
	{FieldName: "repo_id", FieldSchema: "uuid"},
	{FieldName: "kind", FieldSchema: "keyword"},
}

type payloadIndex struct {
	FieldName   string
	FieldSchema string
}

// vectorsConfig is the request shape Qdrant accepts under
// POST/PUT /collections (single named vector layout). Keep
// this in lock-step with the GET-collection response decoder
// below: if either side changes shape, the validation step
// will mis-compare and produce false drift errors.
type vectorsConfig struct {
	Size     int    `json:"size"`
	Distance string `json:"distance"`
}

// createCollectionRequest is the body of PUT /collections/{name}.
type createCollectionRequest struct {
	Vectors vectorsConfig `json:"vectors"`
}

// createPayloadIndexRequest is the body of
// PUT /collections/{name}/index.
type createPayloadIndexRequest struct {
	FieldName   string `json:"field_name"`
	FieldSchema string `json:"field_schema"`
}

// getCollectionResponse is the partial-decoded shape of
// GET /collections/{name}. We only extract the fields we
// validate against; new top-level fields added by future
// Qdrant versions are silently ignored, which is the
// forward-compatible behaviour we want.
type getCollectionResponse struct {
	Result struct {
		Config struct {
			Params struct {
				Vectors vectorsConfig `json:"vectors"`
			} `json:"params"`
		} `json:"config"`
		// PayloadSchema is map[fieldName] -> { data_type: ... }
		// We assert presence/type per field; absence on re-run
		// is recoverable (we just create the missing index).
		PayloadSchema map[string]struct {
			DataType string `json:"data_type"`
		} `json:"payload_schema"`
	} `json:"result"`
}

// Bootstrapper is the unit of work for a single bootstrap run.
// Exported so tests can stub HTTPClient with httptest, and so
// future integration code can call this from inside a test
// harness without spawning the CLI binary.
type Bootstrapper struct {
	BaseURL    string
	VectorSize int
	Distance   string
	HTTPClient *http.Client
	APIKey     string
	DryRun     bool
	// Snapshot, when true, asks Bootstrap() to take a one-off
	// baseline snapshot of every collection after the
	// provisioning step. Independent of SnapshotInterval.
	Snapshot bool
	// SnapshotInterval, when > 0, makes RunWithSchedule loop
	// indefinitely after the bootstrap, snapshotting every
	// collection every interval. Production deployments wire
	// this to 24h via the --snapshot-interval flag and run the
	// binary as a sidecar; the in-binary scheduler is the
	// operative answer to the "snapshot schedule" requirement
	// in implementation-plan.md Stage 1.4 because the Qdrant
	// REST API does not expose a recurring-snapshot endpoint.
	SnapshotInterval time.Duration
	Logger           *log.Logger
}

// NewBootstrapper wires up sensible defaults. Callers may
// override any field afterwards before calling Bootstrap().
func NewBootstrapper(baseURL string, vectorSize int) *Bootstrapper {
	return &Bootstrapper{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		VectorSize: vectorSize,
		Distance:   distanceCosine,
		HTTPClient: &http.Client{Timeout: defaultHTTPTimeout},
		Logger:     log.New(os.Stderr, "[qdrant-bootstrap] ", log.LstdFlags|log.Lmsgprefix),
	}
}

// Bootstrap runs the full provisioning pipeline:
//  1. For each default collection: ensure it exists with the
//     configured vector size / distance (validate on re-run).
//  2. For each (collection, index) pair: ensure the payload
//     index exists with the right schema.
//  3. If --snapshot was passed: take a baseline snapshot.
//
// Bootstrap is the single entrypoint exercised by the
// integration test and by main(); we keep main() trivial so
// the test can drive the same code path the CLI does.
func (b *Bootstrapper) Bootstrap(ctx context.Context) error {
	if b.BaseURL == "" {
		return errors.New("qdrant base URL is required")
	}
	if b.VectorSize <= 0 {
		return fmt.Errorf("invalid vector size %d (must be > 0)", b.VectorSize)
	}
	if b.Distance == "" {
		b.Distance = distanceCosine
	}
	if b.HTTPClient == nil {
		b.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if b.Logger == nil {
		b.Logger = log.New(io.Discard, "", 0)
	}

	for _, name := range defaultCollections {
		if err := b.EnsureCollection(ctx, name); err != nil {
			return fmt.Errorf("ensure collection %s: %w", name, err)
		}
		for _, idx := range defaultPayloadIndexes {
			if err := b.EnsurePayloadIndex(ctx, name, idx); err != nil {
				return fmt.Errorf("ensure payload index %s.%s: %w",
					name, idx.FieldName, err)
			}
		}
		if b.Snapshot {
			if err := b.CreateBaselineSnapshot(ctx, name); err != nil {
				// Snapshot failure is non-fatal for the bootstrap
				// proper -- the collection is provisioned even if
				// the snapshot step trips. Log loud and move on.
				b.Logger.Printf("WARN snapshot for %s failed: %v", name, err)
			}
		}
	}
	return nil
}

// EnsureCollection is idempotent. If the collection is absent
// (404 from GET), it is PUT-created with the configured params.
// If it is present, the existing vector size + distance are
// validated against the configured params; mismatches return
// an error rather than silently rewriting (Qdrant cannot
// ALTER vectors.size on an existing collection -- a "fix" here
// would be a data-loss event).
func (b *Bootstrapper) EnsureCollection(ctx context.Context, name string) error {
	existing, status, err := b.getCollection(ctx, name)
	switch {
	case err != nil && status != http.StatusNotFound:
		return err
	case status == http.StatusOK:
		got := existing.Result.Config.Params.Vectors
		if got.Size != b.VectorSize {
			return fmt.Errorf(
				"collection %s exists with vector size %d, "+
					"configured %d -- Qdrant cannot ALTER this; "+
					"either drop the collection (data loss!) or "+
					"reconcile --vector-size",
				name, got.Size, b.VectorSize)
		}
		if !strings.EqualFold(got.Distance, b.Distance) {
			return fmt.Errorf(
				"collection %s exists with distance %q, "+
					"configured %q",
				name, got.Distance, b.Distance)
		}
		b.Logger.Printf("collection %s already conforms (size=%d, distance=%s)",
			name, got.Size, got.Distance)
		return nil
	}

	// status == 404 -- create.
	if b.DryRun {
		b.Logger.Printf("DRY-RUN would CREATE collection %s (size=%d, distance=%s)",
			name, b.VectorSize, b.Distance)
		return nil
	}
	body, err := json.Marshal(createCollectionRequest{
		Vectors: vectorsConfig{Size: b.VectorSize, Distance: b.Distance},
	})
	if err != nil {
		return fmt.Errorf("marshal create-collection body: %w", err)
	}
	if _, err := b.do(ctx, http.MethodPut, "/collections/"+name, body); err != nil {
		return err
	}
	b.Logger.Printf("collection %s created (size=%d, distance=%s)",
		name, b.VectorSize, b.Distance)
	return nil
}

// EnsurePayloadIndex creates the payload index if missing. If
// the index exists with the configured schema, it is a no-op.
// If the existing index has a different schema, we error
// rather than rewriting (same rationale as the collection
// validator -- payload index reshape is non-atomic).
func (b *Bootstrapper) EnsurePayloadIndex(
	ctx context.Context, collection string, idx payloadIndex,
) error {
	// In dry-run mode the collection may not actually exist
	// (EnsureCollection only logged what it WOULD create), so
	// we cannot re-read the collection schema -- short-circuit
	// to the would-create log line instead.
	if b.DryRun {
		b.Logger.Printf("DRY-RUN would CREATE payload index %s.%s (%s)",
			collection, idx.FieldName, idx.FieldSchema)
		return nil
	}
	resp, status, err := b.getCollection(ctx, collection)
	if err != nil {
		return fmt.Errorf("re-read collection %s: %w", collection, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("collection %s missing during payload-index "+
			"ensure (status=%d)", collection, status)
	}
	if cur, ok := resp.Result.PayloadSchema[idx.FieldName]; ok {
		if !strings.EqualFold(cur.DataType, idx.FieldSchema) {
			return fmt.Errorf(
				"payload index %s.%s exists with data_type %q, "+
					"configured %q",
				collection, idx.FieldName, cur.DataType, idx.FieldSchema)
		}
		b.Logger.Printf("payload index %s.%s already conforms (%s)",
			collection, idx.FieldName, cur.DataType)
		return nil
	}

	body, err := json.Marshal(createPayloadIndexRequest{
		FieldName:   idx.FieldName,
		FieldSchema: idx.FieldSchema,
	})
	if err != nil {
		return fmt.Errorf("marshal create-index body: %w", err)
	}
	if _, err := b.do(ctx, http.MethodPut,
		"/collections/"+collection+"/index", body); err != nil {
		return err
	}
	b.Logger.Printf("payload index %s.%s created (%s)",
		collection, idx.FieldName, idx.FieldSchema)
	return nil
}

// CreateBaselineSnapshot is best-effort. The operator cron
// owns the recurring schedule; this entry exists so the very
// first bootstrap leaves a known-good restore point.
func (b *Bootstrapper) CreateBaselineSnapshot(
	ctx context.Context, collection string,
) error {
	if b.DryRun {
		b.Logger.Printf("DRY-RUN would CREATE baseline snapshot for %s",
			collection)
		return nil
	}
	if _, err := b.do(ctx, http.MethodPost,
		"/collections/"+collection+"/snapshots", nil); err != nil {
		return err
	}
	b.Logger.Printf("baseline snapshot requested for %s", collection)
	return nil
}

// listCollectionsResponse is the partial-decoded shape of
// GET /collections (no name suffix).
type listCollectionsResponse struct {
	Result struct {
		Collections []struct {
			Name string `json:"name"`
		} `json:"collections"`
	} `json:"result"`
}

// ListCollections issues GET /collections and returns the names
// Qdrant reports. Used by the post-bootstrap acceptance test
// (implementation-plan.md Stage 1.4 scenario "Qdrant collections
// exist": "When `GET /collections` is issued against Qdrant,
// Then all three collections are present"). Callers that need
// the per-collection distance should follow up with
// EnsureCollection (which already validates) or call
// getCollection directly.
func (b *Bootstrapper) ListCollections(ctx context.Context) ([]string, error) {
	raw, err := b.do(ctx, http.MethodGet, "/collections", nil)
	if err != nil {
		return nil, err
	}
	var out listCollectionsResponse
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return nil, fmt.Errorf("decode GET /collections: %w", uerr)
	}
	names := make([]string, 0, len(out.Result.Collections))
	for _, c := range out.Result.Collections {
		names = append(names, c.Name)
	}
	return names, nil
}

// CollectionDistance returns the configured similarity for the
// named collection. Distinct from EnsureCollection (which
// also creates / validates size); used by the live test to
// implement the §1.4 scenario "all three collections are
// present with distance: cosine".
func (b *Bootstrapper) CollectionDistance(
	ctx context.Context, name string,
) (string, error) {
	resp, status, err := b.getCollection(ctx, name)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET /collections/%s: status %d",
			name, status)
	}
	return resp.Result.Config.Params.Vectors.Distance, nil
}

// SnapshotLoop runs CreateBaselineSnapshot for every
// defaultCollection on `interval` until ctx is done. The first
// round fires at t=0 (so the loop doubles as the immediate
// baseline when --snapshot was not separately requested);
// subsequent rounds fire every `interval`. Returns ctx.Err()
// on cancellation; failures of individual snapshots are
// logged but never abort the loop -- a transient Qdrant blip
// must not stall the schedule.
//
// In production this is the daemon path: deploy/local runs
// `qdrant-bootstrap --snapshot-interval=24h` as a sidecar.
func (b *Bootstrapper) SnapshotLoop(
	ctx context.Context, interval time.Duration,
) error {
	if interval <= 0 {
		return fmt.Errorf("snapshot interval %v must be > 0", interval)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	b.Logger.Printf("snapshot scheduler running every %s for %d collections",
		interval, len(defaultCollections))
	for {
		for _, name := range defaultCollections {
			if err := b.CreateBaselineSnapshot(ctx, name); err != nil {
				// Transient failure; log and keep the schedule alive.
				b.Logger.Printf("WARN scheduled snapshot %s failed: %v",
					name, err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RunWithSchedule combines Bootstrap + SnapshotLoop. It is
// the entrypoint main() calls when --snapshot-interval > 0;
// keeping it on the type makes the schedule path testable
// without going through os.Exit.
func (b *Bootstrapper) RunWithSchedule(ctx context.Context) error {
	if err := b.Bootstrap(ctx); err != nil {
		return err
	}
	if b.SnapshotInterval <= 0 {
		return nil
	}
	return b.SnapshotLoop(ctx, b.SnapshotInterval)
}

// getCollection returns the decoded response plus the raw HTTP
// status. Both are returned so the caller can distinguish
// "404 -- missing, create it" from "transport error". A 200
// always yields a non-nil decoded response.
func (b *Bootstrapper) getCollection(
	ctx context.Context, name string,
) (*getCollectionResponse, int, error) {
	raw, status, err := b.doRaw(ctx, http.MethodGet, "/collections/"+name, nil)
	if status == http.StatusNotFound {
		return nil, status, nil
	}
	if err != nil {
		return nil, status, err
	}
	var out getCollectionResponse
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return nil, status, fmt.Errorf("decode GET /collections/%s: %w",
			name, uerr)
	}
	return &out, status, nil
}

// do is a thin wrapper over doRaw that discards the status when
// the caller does not need it. It exists so the call sites
// stay terse for the (common) fire-and-forget mutations.
func (b *Bootstrapper) do(
	ctx context.Context, method, path string, body []byte,
) ([]byte, error) {
	out, _, err := b.doRaw(ctx, method, path, body)
	return out, err
}

// doRaw issues a single HTTP request. Auth, content-type and
// the user-agent header are set centrally so individual call
// sites stay short. Non-2xx responses (except 404, which we
// expose verbatim via the returned status) are turned into
// Go errors with the response body included for triage.
func (b *Bootstrapper) doRaw(
	ctx context.Context, method, path string, body []byte,
) ([]byte, int, error) {
	url := b.BaseURL + path
	var req *http.Request
	var err error
	if body == nil {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url,
			bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return nil, 0, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	req.Header.Set("User-Agent", httpUserAgent)
	if b.APIKey != "" {
		req.Header.Set("api-key", b.APIKey)
	}

	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, resp.StatusCode, fmt.Errorf(
			"read response body for %s %s: %w", method, url, rerr)
	}
	if resp.StatusCode == http.StatusNotFound {
		return raw, resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, resp.StatusCode, fmt.Errorf(
			"%s %s: status %d, body=%s",
			method, url, resp.StatusCode, truncate(string(raw), 512))
	}
	return raw, resp.StatusCode, nil
}

// truncate keeps log lines short -- Qdrant error bodies can be
// multi-KB JSON, which clutters CI logs.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func main() {
	var (
		qdrantURL = flag.String("qdrant-url", "",
			"Qdrant base URL (default: $AGENT_MEMORY_QDRANT_URL or "+
				defaultQdrantURL+")")
		vectorSize = flag.Int("vector-size", defaultVectorSize,
			"vector dimensionality for new collections")
		dryRun = flag.Bool("dry-run", false,
			"log every action without contacting Qdrant for writes")
		snapshot = flag.Bool("snapshot", false,
			"take a baseline snapshot of every collection after "+
				"provisioning")
		snapshotInterval = flag.Duration("snapshot-interval", 0,
			"if > 0, after the bootstrap, daemonize and snapshot every "+
				"collection at this interval (e.g. 24h). The Qdrant REST API "+
				"has no recurring-snapshot endpoint, so this binary is the "+
				"snapshot scheduler; deploy/local runs it as a sidecar.")
		apiKey = flag.String("api-key", "",
			"optional Qdrant api-key (default: $AGENT_MEMORY_QDRANT_API_KEY)")
	)
	flag.Parse()

	if *qdrantURL == "" {
		if env := os.Getenv("AGENT_MEMORY_QDRANT_URL"); env != "" {
			*qdrantURL = env
		} else {
			*qdrantURL = defaultQdrantURL
		}
	}
	if *apiKey == "" {
		*apiKey = os.Getenv("AGENT_MEMORY_QDRANT_API_KEY")
	}

	b := NewBootstrapper(*qdrantURL, *vectorSize)
	b.DryRun = *dryRun
	b.Snapshot = *snapshot
	b.SnapshotInterval = *snapshotInterval
	b.APIKey = *apiKey

	ctx, cancel := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := b.RunWithSchedule(ctx)
	// SnapshotLoop returns ctx.Err() (Canceled / DeadlineExceeded)
	// on a clean SIGINT/SIGTERM; that is success, not failure.
	if err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "qdrant-bootstrap: %v\n", err)
		os.Exit(1)
	}
	b.Logger.Printf("done: %d collections conformant",
		len(defaultCollections))
}
