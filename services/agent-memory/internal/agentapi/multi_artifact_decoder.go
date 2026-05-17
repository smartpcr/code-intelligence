// Package agentapi: artifact-decoder composition helpers.
//
// Stage 6.4 (impl-plan §1113-1115) lets multiple trainers
// publish to `reranker_model` over the lifetime of a
// deployment: the in-process LinearTrainer emits inlined
// `data:application/json;base64,…` URIs; the Python BERT
// cross-encoder sidecar emits `file://...` URIs. The recall
// path must accept whichever scheme the latest published row
// carries — so a single ArtifactDecoder is insufficient.
//
// This file ships two composition helpers:
//
//   - MultiArtifactDecoder: tries each child decoder in
//     order; the first to return `recognised=true` wins.
//   - DisabledArtifactDecoder: a no-op decoder that returns
//     `(_, false, nil)` for every URI. Used as a sentinel
//     when the operator has not yet wired the inference
//     side of a trainer; the chain skips it cleanly and
//     PublishedReranker falls back to the inner V0 Reranker.
//
// Both are deliberately small and self-contained so the
// agent-api binary can wire `Linear + Bert sidecar + future
// decoders` in one call without bespoke fan-out logic.
package agentapi

// MultiArtifactDecoder is the recall-side decoder chain. It
// tries each child in declaration order and returns the
// first that reports `recognised=true`. The chain is the
// load-bearing seam for "publish-time the trainer chooses
// the artifact format; recall-time the decoder chain finds
// the matching consumer" — without it, the recall path can
// only consume one artifact scheme at a time.
//
// On a child returning `(_, false, err)` (it recognised the
// scheme but the payload is corrupt), MultiArtifactDecoder
// propagates the error AND does NOT try later children: a
// payload that *was* recognised by an upstream decoder but
// failed to decode is a real corruption signal the operator
// needs to see, not a hint to fall through to a fallback.
//
// On EVERY child returning `(_, false, nil)` (no child
// recognised the scheme), MultiArtifactDecoder also returns
// `(_, false, nil)` — the PublishedReranker treats this as
// "this artifact format is not yet consumed by the recall
// path" and falls back to the inner V0 Reranker.
type MultiArtifactDecoder struct {
	children []ArtifactDecoder
}

// NewMultiArtifactDecoder constructs a decoder chain. Order
// matters: each URI is offered to children in argument
// order. Nil children are silently skipped so callers can
// pass conditionally-constructed decoders without
// scaffolding pre-filtering logic.
func NewMultiArtifactDecoder(children ...ArtifactDecoder) ArtifactDecoder {
	live := make([]ArtifactDecoder, 0, len(children))
	for _, c := range children {
		if c != nil {
			live = append(live, c)
		}
	}
	return &MultiArtifactDecoder{children: live}
}

// Decode implements ArtifactDecoder. See the type
// doc-comment for the chain semantics.
func (m *MultiArtifactDecoder) Decode(uri string) (Reranker, bool, error) {
	for _, child := range m.children {
		scorer, recognised, err := child.Decode(uri)
		if recognised || err != nil {
			return scorer, recognised, err
		}
	}
	return nil, false, nil
}

// DisabledArtifactDecoder is the sentinel decoder used when
// the operator has not wired a particular artifact backend
// (e.g. the BERT sidecar inference URL is unset). Returns
// `(_, false, nil)` for every URI so the chain skips past
// it cleanly.
//
// Concrete benefit over passing `nil`: callers can wire the
// decoder slot unconditionally and use this sentinel as the
// "off" position, which keeps the deployment topology
// stable across environments — dev runs with the disabled
// sentinel, prod runs with a real BERT sidecar decoder, and
// the chain composition code at cmd/agent-api/main.go does
// not branch on the env.
type DisabledArtifactDecoder struct{}

// NewDisabledArtifactDecoder constructs the sentinel.
func NewDisabledArtifactDecoder() ArtifactDecoder { return &DisabledArtifactDecoder{} }

// Decode implements ArtifactDecoder.
func (DisabledArtifactDecoder) Decode(uri string) (Reranker, bool, error) {
	return nil, false, nil
}
