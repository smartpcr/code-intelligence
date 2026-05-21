package scenarios

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// GraphIngestScenario drives `mgmt.ingest_spans` at the §8.3
// sustained rate (50 batches/min → 0.8333 RPS in the nominal
// envelope). This is the **graph-ingest workload** called out
// in the AGENT-MEMORY story description: the path that feeds
// OTel-resolved Method/Block observations into the
// `static_calls` / `observed_calls` edges that the
// CallChainQueryScenario then walks.
//
// The scenario synthesises a small OTel batch (one
// ResourceSpan with N spans) per tick. The wire envelope is
// the §C14 / tech-spec.md §8.3 cap of ≤ 1000 spans/batch — the
// calibration default is 50 spans/batch so the harness exercises
// the batched path without saturating the ingestor on every
// tick. Operators that want a heavier soak can raise SpansPerBatch
// on the cmd line.
//
// Wire contract (mirrors `internal/mgmtapi/spans.go`):
//   - The batch carries a `mgmt.repo_id` resource attribute
//     (NOT `agent_memory.repo_id`) so the mgmt-api's
//     [MgmtRepoIDResourceAttr] resolver branch accepts it
//     even when the [MgmtRepoIDHeader] header is absent.
//   - Every span's `traceId` / `spanId` is a per-tick / per-
//     index nonzero 32 / 16 hex-char value. The mgmt-api's
//     `normalizeOTelID` rejects all-zero IDs (the W3C
//     "invalid" sentinel); using `tick`/`tick*N+i` as raw
//     numbers would produce a zero on the first tick / first
//     index and crash the whole first batch with
//     `invalid_span`. We add 1 + use a non-zero prefix.
type GraphIngestScenario struct {
	Client         ManagementClient
	RepoID         string
	SpansPerBatch  int    // default 50; cap 1000 (§8.3)
	BatchEncoder   func(repoID string, spans int, tick uint64) []byte
	RequestTimeout time.Duration

	next atomic.Uint64
}

// Verb returns the canonical verb identifier.
func (s *GraphIngestScenario) Verb() string { return reliability.VerbMgmtIngestSpans }

// Execute fires one IngestSpans batch.
func (s *GraphIngestScenario) Execute(ctx context.Context, _ RNG) Sample {
	sample := Sample{Verb: s.Verb(), Started: time.Now()}
	if s.Client == nil {
		// Scenario contract (scenario.go §"MUST NOT panic on
		// a nil or returned-error client"): nil client is a
		// failed sample, not a panic.
		sample.Finished = sample.Started
		sample.Err = errNilClient("mgmt.ingest_spans")
		return sample
	}

	tick := s.next.Add(1) - 1
	spans := s.SpansPerBatch
	if spans <= 0 {
		spans = 50
	}
	if spans > 1000 {
		spans = 1000
	}
	enc := s.BatchEncoder
	if enc == nil {
		enc = defaultBatchEncoder
	}
	req := IngestSpansRequest{
		RepoID:    s.RepoID,
		BatchJSON: enc(s.RepoID, spans, tick),
	}

	if s.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.RequestTimeout)
		defer cancel()
	}

	sample.Started = time.Now()
	resp, err := s.Client.IngestSpans(ctx, req)
	sample.Finished = time.Now()
	if err != nil {
		sample.Err = err
		return sample
	}
	// Thread the mgmt-api's wire `degraded` flag onto the
	// sample so the artifact's degraded-responses note
	// captures elevated mgmt backpressure. The flag mirrors
	// `internal/mgmtapi.SpanIngestResponse.Degraded`; the
	// cmd binary decodes it from the 202 envelope in
	// `loadtestMgmtClient.IngestSpans`. The accompanying
	// reason string lets the harness aggregator render
	// per-reason counts on the artifact's note line so an
	// operator can spot the dominant backpressure mode
	// without trawling raw mgmt-api logs.
	sample.Degraded = resp.Degraded
	sample.DegradedReason = resp.DegradedReason
	return sample
}

// defaultBatchEncoder produces a minimal but well-formed
// synthetic batch the mgmt-api accepts. Wire shape requirements
// (see `internal/mgmtapi/spans.go`):
//   - One resourceSpan with a `mgmt.repo_id` resource attribute
//     carrying the operator-supplied (UUID-format) repo id, so
//     the mgmt-api's body-or-attr repo_id resolver accepts the
//     batch even when the operator did not set the
//     X-Mgmt-Repo-ID header.
//   - Every span carries a 32-hex-char `traceId` and a
//     16-hex-char `spanId`. Neither may be all-zero (W3C
//     invalid-id sentinel) — we offset by +1 and use a
//     non-zero prefix so tick=0 / i=0 still produces valid
//     IDs.
func defaultBatchEncoder(repoID string, spans int, tick uint64) []byte {
	// Hand-crafted JSON keeps the harness free of any JSON
	// encoder import surface; the body is bounded to a few KB
	// per tick.
	var b []byte
	b = append(b, []byte(fmt.Sprintf(
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"agent-memory-calibration"}},{"key":"mgmt.repo_id","value":{"stringValue":%q}}]},"scopeSpans":[{"scope":{"name":"calibration"},"spans":[`,
		repoID))...)
	// stride must be > 0 and large enough that (tick, i) maps
	// uniquely; spans is bounded by 1000 per §8.3, +1 keeps
	// the stride strictly above the i range so per-tick spans
	// don't collide.
	stride := uint64(spans) + 1
	for i := 0; i < spans; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		// seed is strictly positive (offset +1) which makes
		// both traceId and spanId non-zero hex strings — the
		// mgmt-api's normalizeOTelID rejects the all-zero
		// W3C "invalid" sentinel.
		seed := tick*stride + uint64(i) + 1
		// 32-hex-char traceId. The "ca11" prefix
		// (mnemonic: "call") is a non-zero anchor so the
		// upper 16 chars are never all-zero even when seed
		// is small.
		b = append(b, []byte(fmt.Sprintf(
			`{"traceId":"ca110000ca110000%016x","spanId":"%016x","name":"calibration.span","kind":"SPAN_KIND_INTERNAL","startTimeUnixNano":"1000000000","endTimeUnixNano":"1001000000","attributes":[{"key":"code.namespace","value":{"stringValue":"pkg.cal"}},{"key":"code.function","value":{"stringValue":"fn%d"}},{"key":"mgmt.repo_id","value":{"stringValue":%q}}]}`,
			seed, seed, i, repoID,
		))...)
	}
	b = append(b, []byte(`]}]}]}`)...)
	return b
}

// errNilClient is the canned error every scenario returns when
// the operator (or a test) passes a nil client. Surfaces a
// stable string so callers can `errors.Is` / substring-match
// without depending on a typed error.
func errNilClient(verb string) error {
	return fmt.Errorf("scenarios.%s: nil client", verb)
}

// _ ensures we explicitly use the time package elsewhere; some
// linters complain when a package is imported but only via
// receiver-method references.
var _ = time.Now
