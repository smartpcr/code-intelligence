// Package agentapi: Stage 6.4 PublishedReranker wrapper.
//
// Stage 6.4 (impl-plan §1113-1115) layers a published-row
// lookup over the in-process cold-start reranker so the
// recall response advertises the latest `reranker_model.version`
// AND, when the artifact's URI scheme is recognised by the
// caller-supplied ArtifactDecoder, scores Candidates with
// the trained weight vector instead of the cold-start
// fallback. The wrapper is cache-free: every recall request
// reads the source so a freshly-published row is visible on
// the very next call (impl-plan §1115 acceptance bar).
//
// Composition (per `cmd/agent-api/main.go`):
//
//	pubSrc   := pgPublishedRerankerSource{db: db}
//	decoder  := NewMultiArtifactDecoder(
//	                NewLinearWeightsDecoder(),
//	                NewBertSidecarDecoder(sidecarURL, nil),
//	            )
//	v0       := NewV0ColdStartReranker(nil)
//	reranker := NewPublishedReranker(pubSrc, v0, decoder)
//
// The wrapper deliberately implements the Reranker AND
// AtomicReranker interfaces so the recall handler can prefer
// the atomic (single source-read) path when wiring it through
// `rankWithVersion(...)`. A wrapper-level
// QueryAwareAtomicReranker satisfies the cross-encoder
// (BERT sidecar) recall-time query threading invariant
// without requiring the recall hot-path to special-case the
// concrete scorer type.
//
// Version pinning contract (iter-4 review items 3 / 6):
//   - When the source returns a published row AND the
//     decoder produces a scorer that ranks successfully,
//     `RankWithVersion` returns `(trained_ordering,
//     trained_version)`.
//   - When the decoder declines the URI scheme (e.g. the
//     published artifact is a `s3://` URI but the wired
//     decoder chain only recognises `data:` URIs), the
//     wrapper still ADVERTISES `trained_version` so
//     operators see the publish landed, but falls back to
//     the inner ordering.
//   - When the decoded scorer SIGNALS DEGRADATION via the
//     FallibleReranker / QueryAwareFallibleReranker
//     contract (e.g. a BERT sidecar 500), the wrapper PINS
//     `inner_version` so the response NEVER claims a
//     trained ordering that the trained scorer did not
//     actually produce.
//
// Why a separate file vs reranker.go: keeps the v0 cold-start
// reranker self-contained (zero deps), and keeps the
// Stage 6.4 wrapper alongside the decoder files
// (`linear_weights_decoder.go`, `bert_sidecar_decoder.go`,
// `multi_artifact_decoder.go`) so the Stage 6.4 surface is
// browsable as one unit.
package agentapi

import (
	"context"
	"errors"
	"time"
)

// PublishedArtifact is the row shape the recall path reads
// from `reranker_model` on every request. The full row also
// carries trainer-side bookkeeping fields (loss, sample
// count, hyperparameters); only the three columns the
// recall path actually consumes are exposed here so the
// wrapper does not pretend to own the full schema.
type PublishedArtifact struct {
	// Version is the stable `reranker_model.version` string
	// the recall response advertises on the
	// `reranker_model_version` envelope field. Used by the
	// stale-model freshness watcher (`reranker_model_stale`
	// degraded_reason) and the cross-version A/B logging.
	Version string

	// ArtifactURI points at the trained scorer's weights
	// payload. May be:
	//   * `data:application/json;base64,...` — LinearTrainer
	//     inlines the weight vector for the cold-start
	//     replacement model so the recall path consumes the
	//     trained artifact with zero out-of-process I/O.
	//   * `file:///models/<version>` — the Python BERT
	//     cross-encoder sidecar writes a model directory and
	//     publishes the path; the recall side dispatches
	//     scoring to the sidecar over HTTP.
	//   * Other schemes — declined by every wired decoder,
	//     in which case the wrapper falls back to inner
	//     ordering but still advertises Version.
	ArtifactURI string

	// TrainedAt is the timestamp the trainer recorded the
	// finished publish. The stale-model watcher compares
	// `time.Since(TrainedAt)` to the freshness budget when
	// computing `reranker_model_stale`.
	TrainedAt time.Time
}

// IsZero reports whether the PublishedArtifact is the zero
// value. Callers reading from snapshot inspectors or test
// fixtures use this to detect the "no published row"
// fallback path without comparing every field by hand.
func (p PublishedArtifact) IsZero() bool {
	return p.Version == "" && p.ArtifactURI == "" && p.TrainedAt.IsZero()
}

// PublishedRerankerSource reads the latest published
// `reranker_model` row. The (`ok=false`, `err=nil`) return
// signals "no row exists yet" — i.e. cold-start mode; the
// (`ok=true`, `err=nil`) return is the steady-state happy
// path; (`_, _, err`) signals a source-side outage that the
// recall handler degrades through.
type PublishedRerankerSource interface {
	Latest(ctx context.Context) (PublishedArtifact, bool, error)
}

// PublishedRerankerSourceFunc adapts a plain function to
// `PublishedRerankerSource` so tests and inline DB-readers
// can avoid scaffolding a one-method struct per call site.
type PublishedRerankerSourceFunc func(ctx context.Context) (PublishedArtifact, bool, error)

// Latest implements `PublishedRerankerSource`.
func (f PublishedRerankerSourceFunc) Latest(ctx context.Context) (PublishedArtifact, bool, error) {
	return f(ctx)
}

// ArtifactDecoder decodes a published `reranker_model.artifact_uri`
// into a request-scoped `Reranker`. The (`_, false, nil`)
// return signals "I don't recognise this URI scheme" — the
// chain proceeds to the next child. The (`_, false, err`)
// return signals "I recognised the scheme but the payload is
// corrupt" — the chain stops and surfaces the error to the
// recall handler (which degrades to inner ordering).
type ArtifactDecoder interface {
	Decode(uri string) (Reranker, bool, error)
}

// ArtifactDecoderFunc adapts a plain function to
// `ArtifactDecoder`. Used by the inline LinearWeights
// decoder (`linear_weights_decoder.go`) and the
// PublishedReranker test stubs.
type ArtifactDecoderFunc func(uri string) (Reranker, bool, error)

// Decode implements `ArtifactDecoder`.
func (f ArtifactDecoderFunc) Decode(uri string) (Reranker, bool, error) { return f(uri) }

// AtomicReranker is the recall-side optimisation: implement
// this and a single `RankWithVersion` call atomically pins
// the ordering and the advertised version against a single
// `PublishedRerankerSource.Latest` read. Without it, the
// recall handler must call `Rank()` and `ModelVersion()`
// separately which (for a cache-free source) can race
// across a publish boundary and produce an envelope whose
// ordering is from version A but whose advertised version
// is B.
type AtomicReranker interface {
	RankWithVersion(ctx context.Context, candidates []Candidate) ([]Candidate, string)
}

// QueryAwareAtomicReranker extends `AtomicReranker` with
// the natural-language `query` the recall request carries.
// Required by the BERT cross-encoder sidecar so the
// scoring surface matches the trainer's training
// distribution (`[CLS] query [SEP] candidate [SEP]`);
// without this threading the cross-encoder scores on
// candidate-only text which silently regresses NDCG.
type QueryAwareAtomicReranker interface {
	AtomicReranker
	RankWithQuery(ctx context.Context, query string, candidates []Candidate) ([]Candidate, string)
}

// FallibleReranker is the per-request degradation contract.
// A trained scorer that talks to an out-of-process backend
// (e.g. a BERT sidecar) MUST implement this so the
// PublishedReranker can detect a backend outage and pin the
// envelope's reranker_model_version to the inner version —
// otherwise a transient sidecar failure would surface as
// "we ranked with v-trained-001" while the candidates were
// actually returned untouched by the sidecar.
type FallibleReranker interface {
	RankErr(candidates []Candidate) ([]Candidate, error)
}

// QueryAwareFallibleReranker is the joint
// FallibleReranker + query-aware contract. Implemented by
// the BERT sidecar scorer so the recall query threads
// through AND a sidecar 500 surfaces as a degradation
// signal that pins the envelope version to inner.
type QueryAwareFallibleReranker interface {
	FallibleReranker
	RankWithQueryErr(ctx context.Context, query string, candidates []Candidate) ([]Candidate, error)
}

// PublishedReranker is the Stage 6.4 wrapper. See the
// file header for the contract; see the test file
// (`published_reranker_test.go`) for executable specs.
type PublishedReranker struct {
	src     PublishedRerankerSource
	inner   Reranker
	decoder ArtifactDecoder
}

// NewPublishedReranker constructs the wrapper. `src` and
// `inner` are required (a nil panics so a mis-wired binary
// fails at construction rather than at the first recall
// request); `decoder` is optional — a nil decoder advertises
// the trained version without scoring with the trained
// artifact (early-adoption mode where the publish-side has
// landed but the recall-side consumer is still being wired).
func NewPublishedReranker(src PublishedRerankerSource, inner Reranker, decoder ArtifactDecoder) *PublishedReranker {
	if src == nil {
		panic("agentapi: NewPublishedReranker: source MUST NOT be nil")
	}
	if inner == nil {
		panic("agentapi: NewPublishedReranker: inner Reranker MUST NOT be nil")
	}
	return &PublishedReranker{src: src, inner: inner, decoder: decoder}
}

// ModelVersion advertises the latest published version, or
// the inner cold-start version when no row exists / the
// source errors out. Reads the source EVERY call so a fresh
// publish is visible on the next request (impl-plan §1115).
func (p *PublishedReranker) ModelVersion() string {
	art, ok, err := p.src.Latest(context.Background())
	if err != nil || !ok || art.Version == "" {
		return p.inner.ModelVersion()
	}
	return art.Version
}

// Rank scores the candidate set. When the source reports a
// published row AND the decoder produces a successful
// scorer, the trained scorer ranks; otherwise the inner
// cold-start reranker ranks. Errors from either the source
// or the decoder fall through to inner — a recall response
// is NEVER hard-failed by an out-of-band publication path
// outage.
func (p *PublishedReranker) Rank(candidates []Candidate) []Candidate {
	ranked, _ := p.RankWithVersion(context.Background(), candidates)
	return ranked
}

// RankWithVersion implements `AtomicReranker`. The single
// source read pins both the ordering and the advertised
// version so a publish landing between two separate calls
// cannot produce a mixed (orderingA, versionB) envelope.
//
// Pinning rules (iter-4 items 3 + 6):
//   - source error / no row / nil decoder -> (inner.Rank, inner.ModelVersion)
//   - decoder declines URI                -> (inner.Rank, trained.Version)
//   - decoder corrupt payload (err != nil) -> (inner.Rank, inner.ModelVersion)
//   - scorer FallibleReranker error       -> (inner.Rank, inner.ModelVersion)
//   - happy path                          -> (trained.Rank, trained.Version)
func (p *PublishedReranker) RankWithVersion(ctx context.Context, candidates []Candidate) ([]Candidate, string) {
	innerRank := func() []Candidate { return p.inner.Rank(candidates) }
	innerVersion := p.inner.ModelVersion()

	art, ok, err := p.src.Latest(ctx)
	if err != nil || !ok || art.Version == "" {
		return innerRank(), innerVersion
	}

	if p.decoder == nil {
		return innerRank(), art.Version
	}

	scorer, recognised, decErr := p.decoder.Decode(art.ArtifactURI)
	if decErr != nil {
		// Recognised scheme but corrupt payload -- pin
		// inner; the response advertises the cold-start
		// version because the trained scorer did NOT
		// contribute to the ordering.
		return innerRank(), innerVersion
	}
	if !recognised || scorer == nil {
		// Trained row exists but the wired decoder chain
		// cannot consume it (e.g. an `s3://` BERT artifact
		// behind a decoder chain that only knows the inline
		// `data:` LinearTrainer scheme). Advertise the
		// trained version so operators see the publish
		// landed; rank with inner because no trained scorer
		// is available.
		return innerRank(), art.Version
	}

	ranked, scorerErr := rankWithScorer(scorer, candidates)
	if scorerErr != nil {
		return innerRank(), innerVersion
	}
	return ranked, art.Version
}

// RankWithQuery implements `QueryAwareAtomicReranker`. When
// the decoded scorer implements `QueryAwareFallibleReranker`,
// the recall query threads through; otherwise the call
// degrades to `RankWithVersion` so non-query-aware scorers
// (e.g. the LinearWeights reranker) still work.
func (p *PublishedReranker) RankWithQuery(ctx context.Context, query string, candidates []Candidate) ([]Candidate, string) {
	innerRank := func() []Candidate { return p.inner.Rank(candidates) }
	innerVersion := p.inner.ModelVersion()

	art, ok, err := p.src.Latest(ctx)
	if err != nil || !ok || art.Version == "" {
		return innerRank(), innerVersion
	}
	if p.decoder == nil {
		return innerRank(), art.Version
	}

	scorer, recognised, decErr := p.decoder.Decode(art.ArtifactURI)
	if decErr != nil {
		return innerRank(), innerVersion
	}
	if !recognised || scorer == nil {
		return innerRank(), art.Version
	}

	if qaf, qok := scorer.(QueryAwareFallibleReranker); qok {
		ranked, qerr := qaf.RankWithQueryErr(ctx, query, candidates)
		if qerr != nil {
			return innerRank(), innerVersion
		}
		return ranked, art.Version
	}

	ranked, scorerErr := rankWithScorer(scorer, candidates)
	if scorerErr != nil {
		return innerRank(), innerVersion
	}
	return ranked, art.Version
}

// rankWithScorer invokes the scorer's strongest available
// ranking method: prefer `FallibleReranker.RankErr` so a
// scorer-side outage surfaces as an error the wrapper can
// pin against; fall back to the plain `Reranker.Rank` when
// the scorer offers no fallible signal (e.g. an in-process
// linear scorer that cannot fail).
func rankWithScorer(scorer Reranker, candidates []Candidate) ([]Candidate, error) {
	if fr, ok := scorer.(FallibleReranker); ok {
		return fr.RankErr(candidates)
	}
	return scorer.Rank(candidates), nil
}

// rankWithVersion is the recall-side shim: prefer the
// AtomicReranker single-read path when the reranker
// supports it, otherwise fall back to Rank+ModelVersion
// separately. Threads a query through when the reranker
// is `QueryAwareAtomicReranker`. Kept package-private; the
// recall handler calls it directly via the same package.
func rankWithVersion(ctx context.Context, reranker Reranker, query string, candidates []Candidate) ([]Candidate, string) {
	if qa, ok := reranker.(QueryAwareAtomicReranker); ok {
		return qa.RankWithQuery(ctx, query, candidates)
	}
	if ar, ok := reranker.(AtomicReranker); ok {
		return ar.RankWithVersion(ctx, candidates)
	}
	return reranker.Rank(candidates), reranker.ModelVersion()
}

// errUnrecognisedArtifactURI is the sentinel a custom
// ArtifactDecoder may return from `Decode` to indicate
// "recognised the scheme prefix but the URI is otherwise
// malformed". Not used internally; exported for downstream
// decoder implementations that want to share the wrapper's
// pin-to-inner classifier.
var errUnrecognisedArtifactURI = errors.New("agentapi: artifact URI not recognised by any wired decoder")
