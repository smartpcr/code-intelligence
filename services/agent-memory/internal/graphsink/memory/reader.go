// reader.go is the read-half of the memory graphsink backend.
// It implements `graphsink.Reader` by linear-scanning the
// in-process `nodes` / `edges` slices that the writer half in
// `sink.go` appends to, applying the same per-field filters the
// SQLite reader's WHERE clauses encode so a diagram-projector
// run against a `--store=memory` scan produces byte-identical
// envelopes to one served by `--store=sqlite`.
//
// One non-obvious shape lives here: `LookupBySignature` uses a
// `map[sigKey]nodeID` fast-path (the `sigIndex` field on
// `*Sink`, populated by `InsertNode` / `LoadExport` in
// `sink.go`) so resolving a `--seed <canonical_signature>` CLI
// argument runs in O(1) instead of scanning the nodes slice.
// The brief calls this out by name: every other Reader method
// is a linear scan (matching the SQLite WHERE-clause shape),
// only LookupBySignature is indexed because it is the single
// hot lookup the call-chain BFS performs once per BFS root.

package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// sigKey is the composite key for the LookupBySignature fast-
// path. RepoID is intentionally omitted: the memory backend is
// single-repo by construction (architecture S3.2.4 -- a Sink
// holds exactly one repo per process), and LookupBySignature
// rejects mismatching repoIDs against `s.repo.record.ID`
// before touching the index. Keeping the key narrow keeps the
// map literal-equal to the workstream brief's `map[sigKey]nodeID`
// shape.
type sigKey struct {
	Kind string
	Sig  string
}

// ListRepos returns the single repo this sink wraps (or an
// empty slice when EnsureRepo has not been called). The memory
// backend stores exactly one repo per process (architecture
// S3.2.4) so the result has at most one element. The SHA falls
// back to the most-recent `EnsureCommit`-supplied value when
// the `RepoInput.CurrentHeadSHA` field was empty -- the CLI's
// scan path registers the repo before resolving the scanned
// commit, so the commit slice is the authoritative SHA source.
func (s *Sink) ListRepos(ctx context.Context, opts graphreader.ReaderOptions) ([]graphreader.RepoSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.repo == nil {
		return nil, nil
	}
	sha := s.repo.input.CurrentHeadSHA
	if sha == "" {
		for i := len(s.commits) - 1; i >= 0; i-- {
			if s.commits[i].record.RepoID == s.repo.record.RepoID {
				sha = s.commits[i].record.SHA
				break
			}
		}
	}
	return []graphreader.RepoSummary{{
		RepoID:      s.repo.record.RepoID,
		URL:         s.repo.input.URL,
		SHA:         sha,
		GeneratedAt: s.repo.generatedAt,
	}}, nil
}

// ListNodes enumerates Nodes matching the supplied filters.
// Stable order: `kind, canonical_signature, node_id` -- matches
// the Postgres reader's ORDER BY so the diagram projector sees
// the same row order regardless of backend.
func (s *Sink) ListNodes(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kinds []string,
	f graphreader.ListNodesFilter,
	opts graphreader.ReaderOptions,
) ([]graphreader.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if repoID.IsZero() {
		return nil, errors.New("graphsink/memory: ListNodes: zero repo_id")
	}
	if err := validateNodeKinds(kinds); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.repo == nil || s.repo.record.ID != repoID {
		return nil, nil
	}
	kindSet := stringSet(kinds)
	var out []graphreader.Node
	limit := normaliseLimit(f.Limit)
	for _, n := range s.nodes {
		if len(kindSet) > 0 && !kindSet[n.input.Kind] {
			continue
		}
		if f.ParentNodeID != "" && n.input.ParentNodeID != f.ParentNodeID {
			continue
		}
		if f.FromSHA != "" && n.input.FromSHA != f.FromSHA {
			continue
		}
		if f.CanonicalSignature != "" && n.input.CanonicalSignature != f.CanonicalSignature {
			continue
		}
		out = append(out, nodeToReader(n, repoID))
	}
	sortNodes(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListEdgesFrom returns every outbound Edge from srcNodeID.
func (s *Sink) ListEdgesFrom(
	ctx context.Context, srcNodeID string, kinds []string, opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	if srcNodeID == "" {
		return nil, errors.New("graphsink/memory: ListEdgesFrom: empty src_node_id")
	}
	return s.listEdges(ctx, srcNodeID, "", kinds, opts)
}

// ListEdgesTo returns every inbound Edge to dstNodeID.
func (s *Sink) ListEdgesTo(
	ctx context.Context, dstNodeID string, kinds []string, opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	if dstNodeID == "" {
		return nil, errors.New("graphsink/memory: ListEdgesTo: empty dst_node_id")
	}
	return s.listEdges(ctx, "", dstNodeID, kinds, opts)
}

func (s *Sink) listEdges(
	ctx context.Context, src, dst string, kinds []string, opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateEdgeKinds(kinds); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.repo == nil {
		return nil, nil
	}
	kindSet := stringSet(kinds)
	limit := normaliseLimit(opts.Limit)
	var out []graphreader.Edge
	for _, e := range s.edges {
		if src != "" && e.input.SrcNodeID != src {
			continue
		}
		if dst != "" && e.input.DstNodeID != dst {
			continue
		}
		if len(kindSet) > 0 && !kindSet[e.input.Kind] {
			continue
		}
		out = append(out, edgeToReader(e, s.repo.record.ID))
	}
	sortEdges(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetNode fetches a single Node by id. Returns
// `graphreader.ErrNotFound` when the id is unknown -- matches
// the Postgres reader's behaviour so callers can use
// `errors.Is(err, graphreader.ErrNotFound)` portably.
func (s *Sink) GetNode(ctx context.Context, nodeID string, opts graphreader.ReaderOptions) (graphreader.Node, error) {
	if err := ctx.Err(); err != nil {
		return graphreader.Node{}, err
	}
	if nodeID == "" {
		return graphreader.Node{}, errors.New("graphsink/memory: GetNode: empty node_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return graphreader.Node{}, ErrClosed
	}
	idx, ok := s.nodeIdxByID[nodeID]
	if !ok || s.repo == nil {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	return nodeToReader(s.nodes[idx], s.repo.record.ID), nil
}

// LookupBySignature resolves (repoID, kind, canonicalSignature)
// to its Node via the `sigIndex` fast-path. The index is
// populated on `InsertNode` (and on `LoadExport`'s rehydration
// pass), so this method is O(1) instead of the O(n) linear
// scan the other Reader methods perform. The workstream brief
// names this fast-path literally: `map[sigKey]nodeID`.
func (s *Sink) LookupBySignature(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kind string,
	canonicalSignature string,
	opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	if err := ctx.Err(); err != nil {
		return graphreader.Node{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return graphreader.Node{}, ErrClosed
	}
	if s.repo == nil || s.repo.record.ID != repoID {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	nodeID, ok := s.sigIndex[sigKey{Kind: kind, Sig: canonicalSignature}]
	if !ok {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	idx, ok := s.nodeIdxByID[nodeID]
	if !ok {
		// The index pointed at a node that is no longer in
		// `nodes`. This should never happen with the current
		// append-only writer (the memory backend never deletes),
		// but treat as ErrNotFound rather than panic so a future
		// retraction-aware writer degrades gracefully.
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	return nodeToReader(s.nodes[idx], repoID), nil
}

// ----- reader-only helpers ---------------------------------------

// stringSet collapses a kinds slice to a presence-set so the
// filter loop is O(1) per row instead of O(|kinds|).
func stringSet(in []string) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[s] = true
	}
	return out
}

// nodeToReader projects a nodeEntry to the wire-shape Node the
// graphreader / diagram projector consumes. RepoID is passed
// in (rather than re-read from the entry) because every node
// in a memory Sink shares the same repo by construction.
func nodeToReader(n nodeEntry, repoID fingerprint.RepoID) graphreader.Node {
	return graphreader.Node{
		NodeID:             n.record.NodeID,
		RepoID:             repoID.String(),
		Fingerprint:        n.record.Fingerprint,
		Kind:               n.input.Kind,
		CanonicalSignature: n.input.CanonicalSignature,
		ParentNodeID:       n.input.ParentNodeID,
		FromSHA:            n.input.FromSHA,
		AttrsJSON:          cloneRaw(n.input.AttrsJSON),
	}
}

// edgeToReader is the edge analogue of nodeToReader.
func edgeToReader(e edgeEntry, repoID fingerprint.RepoID) graphreader.Edge {
	return graphreader.Edge{
		EdgeID:      e.record.EdgeID,
		RepoID:      repoID.String(),
		Fingerprint: e.record.Fingerprint,
		Kind:        e.input.Kind,
		SrcNodeID:   e.input.SrcNodeID,
		DstNodeID:   e.input.DstNodeID,
		FromSHA:     e.input.FromSHA,
		AttrsJSON:   cloneRaw(e.input.AttrsJSON),
	}
}

// sortNodes orders rows by `kind, canonical_signature, node_id`,
// matching the Postgres reader's ORDER BY so diagram envelopes
// are byte-identical across backends.
func sortNodes(in []graphreader.Node) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Kind != in[j].Kind {
			return in[i].Kind < in[j].Kind
		}
		if in[i].CanonicalSignature != in[j].CanonicalSignature {
			return in[i].CanonicalSignature < in[j].CanonicalSignature
		}
		return in[i].NodeID < in[j].NodeID
	})
}

// sortEdges orders rows by `kind, edge_id`, matching the
// Postgres reader's ORDER BY.
func sortEdges(in []graphreader.Edge) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Kind != in[j].Kind {
			return in[i].Kind < in[j].Kind
		}
		return in[i].EdgeID < in[j].EdgeID
	})
}

// normaliseLimit applies the standard ReaderOptions / filter
// limit policy: <=0 or > MaxListLimit clamps to MaxListLimit;
// every other value is used as-is.
func normaliseLimit(requested int) int {
	if requested <= 0 || requested > graphreader.MaxListLimit {
		return graphreader.MaxListLimit
	}
	return requested
}

// nodeKindsAllowed mirrors graphreader's closed node_kind set
// (architecture.md §5.2.1 / migration 0001). Memory backend
// keeps its own copy because graphreader's set is unexported;
// when a future migration appends a kind, update BOTH lists.
var nodeKindsAllowed = map[string]struct{}{
	"repo":    {},
	"package": {},
	"file":    {},
	"class":   {},
	"method":  {},
	"block":   {},
}

// edgeKindsAllowed mirrors graphreader's closed edge_kind set
// (migration 0001 + 0022 overrides).
var edgeKindsAllowed = map[string]struct{}{
	"contains":       {},
	"imports":        {},
	"static_calls":   {},
	"observed_calls": {},
	"extends":        {},
	"implements":     {},
	"reads":          {},
	"writes":         {},
	"renamed_to":     {},
	"overrides":      {},
}

// validateNodeKinds returns an error when any element of kinds
// is not a member of the node_kind ENUM. An empty slice passes
// -- callers use that to mean "all kinds". Same contract as
// `graphreader.validateNodeKinds` so the memory backend rejects
// the same set of invalid inputs the Postgres backend would
// (otherwise a `--store=memory` scan could swallow a typo that
// `--store=postgres` would reject).
func validateNodeKinds(kinds []string) error {
	for _, k := range kinds {
		if _, ok := nodeKindsAllowed[k]; !ok {
			return fmt.Errorf("graphsink/memory: invalid node kind %q "+
				"(allowed: repo/package/file/class/method/block)", k)
		}
	}
	return nil
}

// validateEdgeKinds is the edge analogue of validateNodeKinds.
func validateEdgeKinds(kinds []string) error {
	for _, k := range kinds {
		if _, ok := edgeKindsAllowed[k]; !ok {
			return fmt.Errorf("graphsink/memory: invalid edge kind %q "+
				"(allowed: contains/imports/static_calls/observed_calls/"+
				"extends/implements/reads/writes/renamed_to/overrides)", k)
		}
	}
	return nil
}
