// Package memory provides the in-process implementation of the
// `graphsink.Sink` + `graphsink.Reader` pair used by the
// `codeintel scan --store=memory [--export <file>]` one-shot
// path (architecture S3.2.4). Two append-only slices back the
// node + edge stores; a `map[fingerprint.Sum]string` keyed on
// the G2 fingerprint is the idempotent re-emit cache so a
// re-scan that re-derives the same Node / Edge fingerprints
// returns the original synthetic IDs without growing the
// slices.
//
// The backend is single-repo by construction: `EnsureRepo`
// rejects a zero `RepoInput.RepoID` (the CLI is expected to
// have called `fingerprint.RepoIDFromURL` before reaching the
// sink) and rejects a second EnsureRepo whose RepoID disagrees
// with the first. This matches the architecture's "every
// invocation is a fresh scan" rule for the memory backend
// (architecture §7.2) and keeps the JSON export schema's
// single `repo` object honest.
//
// `Close` writes the JSON export when the sink was constructed
// with a non-empty export path. The exporter emits keys in the
// exact order architecture S3.2.4 pins -- `repo`, `nodes`,
// `edges` -- by declaring them in that order on the top-level
// `Export` struct (Go's `encoding/json` marshals struct fields
// in source order). `LoadExport` is the round-trip helper the
// `codeintel diagram --from-export <file>` path uses to read a
// previously written export without re-scanning the source
// tree: it returns the same value as both a `graphsink.Sink`
// (so the diagram projector's wiring doesn't have to special-
// case the rehydrated path) and a `graphsink.Reader` (the
// surface the projector actually consumes).
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ErrClosed is returned by every public Sink / Reader method
// after `Close` has run. Mirrors the closed-pool sentinel the
// Postgres adapter wraps and the `sql.ErrConnDone` value the
// SQLite adapter surfaces so callers can treat all three
// backends uniformly with `errors.Is`.
var ErrClosed = errors.New("graphsink/memory: sink is closed")

// ErrRepoMismatch is returned by `EnsureRepo` when a second
// call supplies a `RepoInput.RepoID` that disagrees with the
// one this sink already committed to. The memory backend is
// single-repo (architecture §7.2) so cross-repo writes through
// the same sink would silently corrupt the JSON export.
var ErrRepoMismatch = errors.New(
	"graphsink/memory: RepoID disagrees with previously ensured repo",
)

// Sink is the in-process backend. It satisfies both
// `graphsink.Sink` (writer side) and `graphsink.Reader`
// (reader side) so the rehydrator helper can return a single
// value typed as either interface.
//
// A zero-value Sink is NOT usable; construct with `New`.
type Sink struct {
	mu sync.Mutex

	exportPath string
	now        func() time.Time

	closed bool

	repo       *repoState
	commits    []commitEntry
	commitIdx  map[commitKey]int
	nodes      []nodeEntry
	nodesByFP  map[fingerprint.Sum]int // fingerprint -> index in `nodes`
	nodeFPByID map[string]fingerprint.Sum
	edges      []edgeEntry
	edgesByFP  map[fingerprint.Sum]int // fingerprint -> index in `edges`

	// nextNodeID / nextEdgeID are the monotonic counters
	// behind the synthetic ids the memory backend mints.
	// Postgres uses `gen_random_uuid()`; SQLite uses an
	// autoincrement column. The memory backend prefixes "n-" /
	// "e-" so a synthetic id is visually distinct from a real
	// UUID in debug dumps.
	nextNodeID int
	nextEdgeID int
}

// Options configures a new Sink. ExportPath is the file the
// JSON export is written to on `Close`; the empty string
// disables export (and `Close` becomes a no-op for IO). Now is
// the clock the EnsureRepo `GeneratedAt` reads; `nil` selects
// `time.Now`.
type Options struct {
	ExportPath string
	Now        func() time.Time
}

// New constructs a Sink. The returned value satisfies BOTH
// `graphsink.Sink` and `graphsink.Reader`.
func New(opts Options) *Sink {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Sink{
		exportPath: opts.ExportPath,
		now:        now,
		commitIdx:  make(map[commitKey]int),
		nodesByFP:  make(map[fingerprint.Sum]int),
		nodeFPByID: make(map[string]fingerprint.Sum),
		edgesByFP:  make(map[fingerprint.Sum]int),
	}
}

// Compile-time assertions: *Sink satisfies both halves of the
// graphsink contract. If a future graphsink edit breaks the
// shape these fail at build time.
var (
	_ graphsink.Sink   = (*Sink)(nil)
	_ graphsink.Reader = (*Sink)(nil)
)

// ----- internal record shapes ------------------------------------

type repoState struct {
	record      graphwriter.RepoRecord
	input       graphwriter.RepoInput
	generatedAt time.Time
}

type commitKey struct {
	RepoID fingerprint.RepoID
	SHA    string
}

type commitEntry struct {
	record graphwriter.CommitRecord
	input  graphwriter.CommitInput
}

type nodeEntry struct {
	record graphwriter.NodeRecord
	input  graphwriter.NodeInput
}

type edgeEntry struct {
	record graphwriter.EdgeRecord
	input  graphwriter.EdgeInput
}

// ----- Sink: writer side -----------------------------------------

// EnsureRepo upserts the per-sink repo state. Requires
// `in.RepoID` to be non-zero -- the memory backend's identity
// rule mirrors the Postgres `EnsureRepoWithID` precondition.
func (s *Sink) EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	if err := ctx.Err(); err != nil {
		return graphwriter.RepoRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return graphwriter.RepoRecord{}, ErrClosed
	}
	if in.URL == "" {
		return graphwriter.RepoRecord{}, errors.New(
			"graphsink/memory: EnsureRepo: empty url",
		)
	}
	if in.RepoID.IsZero() {
		return graphwriter.RepoRecord{}, errors.New(
			"graphsink/memory: EnsureRepo: zero RepoID " +
				"(precompute via fingerprint.RepoIDFromURL)",
		)
	}
	if s.repo != nil && s.repo.record.ID != in.RepoID {
		return graphwriter.RepoRecord{}, fmt.Errorf(
			"%w: have %s, got %s",
			ErrRepoMismatch, s.repo.record.ID, in.RepoID,
		)
	}
	if s.repo != nil {
		// Idempotent re-emit: overwrite mutable fields on the
		// stored input so a follow-up scan that updated the
		// default branch / head SHA is reflected in the export,
		// but report Inserted=false the second time around.
		s.repo.input = in
		rec := s.repo.record
		rec.Inserted = false
		s.repo.record = rec
		return rec, nil
	}
	rec := graphwriter.RepoRecord{
		RepoID:   in.RepoID.String(),
		ID:       in.RepoID,
		Inserted: true,
	}
	s.repo = &repoState{
		record:      rec,
		input:       in,
		generatedAt: s.now().UTC(),
	}
	return rec, nil
}

// EnsureCommit idempotently appends the (RepoID, SHA) row.
func (s *Sink) EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	if err := ctx.Err(); err != nil {
		return graphwriter.CommitRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return graphwriter.CommitRecord{}, ErrClosed
	}
	if in.RepoID.IsZero() {
		return graphwriter.CommitRecord{}, errors.New(
			"graphsink/memory: EnsureCommit: zero RepoID",
		)
	}
	if in.SHA == "" {
		return graphwriter.CommitRecord{}, errors.New(
			"graphsink/memory: EnsureCommit: empty sha",
		)
	}
	key := commitKey{RepoID: in.RepoID, SHA: in.SHA}
	if idx, ok := s.commitIdx[key]; ok {
		rec := s.commits[idx].record
		rec.Inserted = false
		return rec, nil
	}
	rec := graphwriter.CommitRecord{
		RepoID:   in.RepoID.String(),
		SHA:      in.SHA,
		Inserted: true,
	}
	s.commits = append(s.commits, commitEntry{record: rec, input: in})
	s.commitIdx[key] = len(s.commits) - 1
	return rec, nil
}

// InsertNode idempotently writes a Node. The fingerprint is
// computed via `pkg/fingerprint.NodeFingerprint` so the
// resulting `NodeRecord.Fingerprint` is byte-identical to what
// the Postgres / SQLite backends would produce for the same
// inputs (backend-parity ID).
func (s *Sink) InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	if err := ctx.Err(); err != nil {
		return graphwriter.NodeRecord{}, err
	}
	fp, err := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	if err != nil {
		return graphwriter.NodeRecord{}, fmt.Errorf(
			"graphsink/memory: InsertNode fingerprint: %w", err,
		)
	}
	attrs, err := normaliseAttrs(in.AttrsJSON)
	if err != nil {
		return graphwriter.NodeRecord{}, fmt.Errorf(
			"graphsink/memory: InsertNode attrs_json: %w", err,
		)
	}
	in.AttrsJSON = attrs

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return graphwriter.NodeRecord{}, ErrClosed
	}
	if s.repo == nil {
		return graphwriter.NodeRecord{}, errors.New(
			"graphsink/memory: InsertNode: EnsureRepo has not been called",
		)
	}
	if in.RepoID != s.repo.record.ID {
		return graphwriter.NodeRecord{}, fmt.Errorf(
			"%w: node RepoID %s != sink RepoID %s",
			ErrRepoMismatch, in.RepoID, s.repo.record.ID,
		)
	}
	if idx, ok := s.nodesByFP[fp]; ok {
		rec := s.nodes[idx].record
		rec.Inserted = false
		return rec, nil
	}
	if in.ParentNodeID != "" {
		if _, ok := s.nodeFPByID[in.ParentNodeID]; !ok {
			return graphwriter.NodeRecord{}, fmt.Errorf(
				"graphsink/memory: InsertNode: parent_node_id %s not found",
				in.ParentNodeID,
			)
		}
	}
	s.nextNodeID++
	nodeID := fmt.Sprintf("n-%010d", s.nextNodeID)
	rec := graphwriter.NodeRecord{
		NodeID:      nodeID,
		Fingerprint: fp,
		Inserted:    true,
	}
	s.nodes = append(s.nodes, nodeEntry{record: rec, input: in})
	s.nodesByFP[fp] = len(s.nodes) - 1
	s.nodeFPByID[nodeID] = fp
	return rec, nil
}

// InsertEdge idempotently writes an Edge. Both endpoints MUST
// be Nodes previously inserted into this sink so the writer
// can re-derive their fingerprints for the G2 EdgeFingerprint
// pre-image (matching the Postgres writer's behaviour of
// reading endpoint fingerprints inside the insert tx).
func (s *Sink) InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	if err := ctx.Err(); err != nil {
		return graphwriter.EdgeRecord{}, err
	}
	attrs, err := normaliseAttrs(in.AttrsJSON)
	if err != nil {
		return graphwriter.EdgeRecord{}, fmt.Errorf(
			"graphsink/memory: InsertEdge attrs_json: %w", err,
		)
	}
	in.AttrsJSON = attrs

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return graphwriter.EdgeRecord{}, ErrClosed
	}
	if s.repo == nil {
		return graphwriter.EdgeRecord{}, errors.New(
			"graphsink/memory: InsertEdge: EnsureRepo has not been called",
		)
	}
	if in.RepoID != s.repo.record.ID {
		return graphwriter.EdgeRecord{}, fmt.Errorf(
			"%w: edge RepoID %s != sink RepoID %s",
			ErrRepoMismatch, in.RepoID, s.repo.record.ID,
		)
	}
	srcFP, ok := s.nodeFPByID[in.SrcNodeID]
	if !ok {
		return graphwriter.EdgeRecord{}, fmt.Errorf(
			"graphsink/memory: InsertEdge: src_node_id %s not found",
			in.SrcNodeID,
		)
	}
	dstFP, ok := s.nodeFPByID[in.DstNodeID]
	if !ok {
		return graphwriter.EdgeRecord{}, fmt.Errorf(
			"graphsink/memory: InsertEdge: dst_node_id %s not found",
			in.DstNodeID,
		)
	}
	fp, err := fingerprint.EdgeFingerprint(in.RepoID, in.Kind, srcFP, dstFP, in.FromSHA)
	if err != nil {
		return graphwriter.EdgeRecord{}, fmt.Errorf(
			"graphsink/memory: InsertEdge fingerprint: %w", err,
		)
	}
	if idx, ok := s.edgesByFP[fp]; ok {
		rec := s.edges[idx].record
		rec.Inserted = false
		return rec, nil
	}
	s.nextEdgeID++
	edgeID := fmt.Sprintf("e-%010d", s.nextEdgeID)
	rec := graphwriter.EdgeRecord{
		EdgeID:      edgeID,
		Fingerprint: fp,
		SrcFP:       srcFP,
		DstFP:       dstFP,
		Inserted:    true,
	}
	s.edges = append(s.edges, edgeEntry{record: rec, input: in})
	s.edgesByFP[fp] = len(s.edges) - 1
	return rec, nil
}

// Flush is a no-op on the memory backend: every insert is
// already in-process, so there is nothing to drain. Kept on the
// interface so callers do not have to special-case backends.
func (s *Sink) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}

// Close finalises the sink. When constructed with a non-empty
// `ExportPath`, the JSON export is written here. Idempotent:
// the second and subsequent calls return nil.
func (s *Sink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	path := s.exportPath
	var (
		data []byte
		err  error
	)
	if path != "" {
		data, err = s.encodeExportLocked()
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if path == "" {
		return nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("graphsink/memory: write export: %w", err)
	}
	return nil
}

// ----- Reader: read side -----------------------------------------

// ListRepos returns the single repo this sink wraps (or an
// empty slice when EnsureRepo has not been called).
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
	// Prefer the most recent EnsureCommit-supplied SHA when the
	// repo row's `current_head_sha` is empty (the CLI registers
	// the repo before scanning a particular commit).
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
// the Postgres reader's ORDER BY.
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

// GetNode fetches a single Node by id.
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
	fp, ok := s.nodeFPByID[nodeID]
	if !ok {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	idx, ok := s.nodesByFP[fp]
	if !ok || s.repo == nil {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	return nodeToReader(s.nodes[idx], s.repo.record.ID), nil
}

// LookupBySignature resolves (repoID, kind, canonicalSignature)
// to its Node.
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
	for _, n := range s.nodes {
		if n.input.Kind == kind && n.input.CanonicalSignature == canonicalSignature {
			return nodeToReader(n, repoID), nil
		}
	}
	return graphreader.Node{}, graphreader.ErrNotFound
}

// ----- JSON export --------------------------------------------------

// Export is the on-wire shape `Close` writes (and `LoadExport`
// reads). Field ordering is significant: Go's `encoding/json`
// marshals struct fields in source order, and architecture
// S3.2.4 pins the top-level keys to `repo`, `nodes`, `edges`
// in that order.
type Export struct {
	Repo  ExportRepo   `json:"repo"`
	Nodes []ExportNode `json:"nodes"`
	Edges []ExportEdge `json:"edges"`
}

// ExportRepo is the `repo` object in the export. Architecture
// S3.2.4 pins the inner keys to `id`, `url`, `sha`.
type ExportRepo struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	SHA string `json:"sha"`
	// GeneratedAt is the scan timestamp. Not part of the
	// architecture-pinned shape but carried so `LoadExport` can
	// rehydrate `RepoSummary.GeneratedAt`. RFC3339 in UTC.
	GeneratedAt time.Time `json:"generated_at"`
	// DefaultBranch / LanguageHints are best-effort metadata
	// from the original RepoInput; omitted when empty so a
	// minimal repo still produces a tight envelope.
	DefaultBranch string   `json:"default_branch,omitempty"`
	LanguageHints []string `json:"language_hints,omitempty"`
}

// ExportNode is one Node row: every column the diagram
// projector consumes plus enough provenance for round-trip.
type ExportNode struct {
	NodeID             string          `json:"node_id"`
	Fingerprint        string          `json:"fingerprint"`
	RepoID             string          `json:"repo_id"`
	Kind               string          `json:"kind"`
	CanonicalSignature string          `json:"canonical_signature"`
	ParentNodeID       string          `json:"parent_node_id,omitempty"`
	FromSHA            string          `json:"from_sha"`
	AttrsJSON          json.RawMessage `json:"attrs_json"`
}

// ExportEdge is one Edge row. The src / dst fingerprints are
// carried in addition to the node ids so a rehydrator that
// wants to verify the G2 fingerprint of an edge does not have
// to re-walk the nodes slice.
type ExportEdge struct {
	EdgeID         string          `json:"edge_id"`
	Fingerprint    string          `json:"fingerprint"`
	RepoID         string          `json:"repo_id"`
	Kind           string          `json:"kind"`
	SrcNodeID      string          `json:"src_node_id"`
	DstNodeID      string          `json:"dst_node_id"`
	SrcFingerprint string          `json:"src_fingerprint"`
	DstFingerprint string          `json:"dst_fingerprint"`
	FromSHA        string          `json:"from_sha"`
	AttrsJSON      json.RawMessage `json:"attrs_json"`
}

// Snapshot builds and returns the export view of the current
// sink contents WITHOUT writing it to disk. Useful for tests
// and for callers who want to ship the JSON over an HTTP
// response without an intermediate file.
func (s *Sink) Snapshot() (Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Sink) snapshotLocked() (Export, error) {
	if s.repo == nil {
		return Export{}, errors.New(
			"graphsink/memory: Snapshot: EnsureRepo has not been called",
		)
	}
	repoOut := ExportRepo{
		ID:            s.repo.record.RepoID,
		URL:           s.repo.input.URL,
		SHA:           s.repo.input.CurrentHeadSHA,
		GeneratedAt:   s.repo.generatedAt,
		DefaultBranch: s.repo.input.DefaultBranch,
		LanguageHints: append([]string(nil), s.repo.input.LanguageHints...),
	}
	if repoOut.SHA == "" {
		for i := len(s.commits) - 1; i >= 0; i-- {
			if s.commits[i].record.RepoID == s.repo.record.RepoID {
				repoOut.SHA = s.commits[i].record.SHA
				break
			}
		}
	}
	nodes := make([]ExportNode, len(s.nodes))
	for i, n := range s.nodes {
		nodes[i] = ExportNode{
			NodeID:             n.record.NodeID,
			Fingerprint:        n.record.Fingerprint.Hex(),
			RepoID:             n.input.RepoID.String(),
			Kind:               n.input.Kind,
			CanonicalSignature: n.input.CanonicalSignature,
			ParentNodeID:       n.input.ParentNodeID,
			FromSHA:            n.input.FromSHA,
			AttrsJSON:          cloneRaw(n.input.AttrsJSON),
		}
	}
	edges := make([]ExportEdge, len(s.edges))
	for i, e := range s.edges {
		edges[i] = ExportEdge{
			EdgeID:         e.record.EdgeID,
			Fingerprint:    e.record.Fingerprint.Hex(),
			RepoID:         e.input.RepoID.String(),
			Kind:           e.input.Kind,
			SrcNodeID:      e.input.SrcNodeID,
			DstNodeID:      e.input.DstNodeID,
			SrcFingerprint: e.record.SrcFP.Hex(),
			DstFingerprint: e.record.DstFP.Hex(),
			FromSHA:        e.input.FromSHA,
			AttrsJSON:      cloneRaw(e.input.AttrsJSON),
		}
	}
	return Export{Repo: repoOut, Nodes: nodes, Edges: edges}, nil
}

func (s *Sink) encodeExportLocked() ([]byte, error) {
	exp, err := s.snapshotLocked()
	if err != nil {
		return nil, fmt.Errorf("graphsink/memory: snapshot: %w", err)
	}
	data, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("graphsink/memory: marshal export: %w", err)
	}
	data = append(data, '\n')
	return data, nil
}

// LoadExport reads a memory-backend JSON export previously
// written by `*Sink.Close` and returns a rehydrated *Sink. The
// returned value satisfies BOTH `graphsink.Sink` (write side
// -- though writes after load are uncommon) and
// `graphsink.Reader` (the surface the `codeintel diagram
// --from-export <file>` path consumes).
//
// The rehydrated sink preserves the original synthetic node /
// edge ids and re-populates the fingerprint maps. Repo / node /
// edge records carry `Inserted = false` because nothing was
// newly inserted by the load.
func LoadExport(path string) (*Sink, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("graphsink/memory: read export %s: %w", path, err)
	}
	var exp Export
	if err := json.Unmarshal(data, &exp); err != nil {
		return nil, fmt.Errorf("graphsink/memory: decode export %s: %w", path, err)
	}

	if exp.Repo.ID == "" {
		return nil, fmt.Errorf("graphsink/memory: export %s missing repo.id", path)
	}
	repoID, err := fingerprint.ParseRepoID(exp.Repo.ID)
	if err != nil {
		return nil, fmt.Errorf("graphsink/memory: parse repo id %q: %w", exp.Repo.ID, err)
	}

	s := New(Options{})
	s.repo = &repoState{
		record: graphwriter.RepoRecord{
			RepoID:   exp.Repo.ID,
			ID:       repoID,
			Inserted: false,
		},
		input: graphwriter.RepoInput{
			URL:            exp.Repo.URL,
			DefaultBranch:  exp.Repo.DefaultBranch,
			CurrentHeadSHA: exp.Repo.SHA,
			LanguageHints:  append([]string(nil), exp.Repo.LanguageHints...),
			RepoID:         repoID,
		},
		generatedAt: exp.Repo.GeneratedAt,
	}
	// Record the SHA on a synthetic commit row so ListRepos
	// surfaces it when the original CurrentHeadSHA was empty.
	if exp.Repo.SHA != "" {
		key := commitKey{RepoID: repoID, SHA: exp.Repo.SHA}
		s.commits = append(s.commits, commitEntry{
			record: graphwriter.CommitRecord{
				RepoID:   exp.Repo.ID,
				SHA:      exp.Repo.SHA,
				Inserted: false,
			},
			input: graphwriter.CommitInput{
				RepoID: repoID,
				SHA:    exp.Repo.SHA,
			},
		})
		s.commitIdx[key] = 0
	}

	maxNodeID := 0
	for _, n := range exp.Nodes {
		fp, err := fingerprint.SumFromHex(n.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf(
				"graphsink/memory: decode node fingerprint %s: %w", n.Fingerprint, err)
		}
		input := graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               n.Kind,
			CanonicalSignature: n.CanonicalSignature,
			ParentNodeID:       n.ParentNodeID,
			FromSHA:            n.FromSHA,
			AttrsJSON:          cloneRaw(n.AttrsJSON),
		}
		rec := graphwriter.NodeRecord{
			NodeID:      n.NodeID,
			Fingerprint: fp,
			Inserted:    false,
		}
		s.nodes = append(s.nodes, nodeEntry{record: rec, input: input})
		s.nodesByFP[fp] = len(s.nodes) - 1
		s.nodeFPByID[n.NodeID] = fp
		if seq := parseSyntheticID(n.NodeID, "n-"); seq > maxNodeID {
			maxNodeID = seq
		}
	}
	s.nextNodeID = maxNodeID

	maxEdgeID := 0
	for _, e := range exp.Edges {
		fp, err := fingerprint.SumFromHex(e.Fingerprint)
		if err != nil {
			return nil, fmt.Errorf(
				"graphsink/memory: decode edge fingerprint %s: %w", e.Fingerprint, err)
		}
		srcFP, err := fingerprint.SumFromHex(e.SrcFingerprint)
		if err != nil {
			return nil, fmt.Errorf(
				"graphsink/memory: decode edge src fingerprint %s: %w", e.SrcFingerprint, err)
		}
		dstFP, err := fingerprint.SumFromHex(e.DstFingerprint)
		if err != nil {
			return nil, fmt.Errorf(
				"graphsink/memory: decode edge dst fingerprint %s: %w", e.DstFingerprint, err)
		}
		input := graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      e.Kind,
			SrcNodeID: e.SrcNodeID,
			DstNodeID: e.DstNodeID,
			FromSHA:   e.FromSHA,
			AttrsJSON: cloneRaw(e.AttrsJSON),
		}
		rec := graphwriter.EdgeRecord{
			EdgeID:      e.EdgeID,
			Fingerprint: fp,
			SrcFP:       srcFP,
			DstFP:       dstFP,
			Inserted:    false,
		}
		s.edges = append(s.edges, edgeEntry{record: rec, input: input})
		s.edgesByFP[fp] = len(s.edges) - 1
		if seq := parseSyntheticID(e.EdgeID, "e-"); seq > maxEdgeID {
			maxEdgeID = seq
		}
	}
	s.nextEdgeID = maxEdgeID

	return s, nil
}

// ----- helpers ----------------------------------------------------

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

func cloneRaw(in json.RawMessage) json.RawMessage {
	if len(in) == 0 {
		return json.RawMessage("{}")
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}

// normaliseAttrs mirrors graphwriter's attrs normalisation:
// nil / empty becomes `{}` and the payload must be a JSON
// object (not an array or scalar).
func normaliseAttrs(in json.RawMessage) (json.RawMessage, error) {
	if len(in) == 0 {
		return json.RawMessage("{}"), nil
	}
	trimmed := strings.TrimSpace(string(in))
	if trimmed == "" {
		return json.RawMessage("{}"), nil
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("attrs_json must be a JSON object, got %q", trimmed[:1])
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, fmt.Errorf("attrs_json is not valid JSON: %w", err)
	}
	return json.RawMessage(trimmed), nil
}

func parseSyntheticID(id, prefix string) int {
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(id[len(prefix):], "%d", &n); err != nil {
		return 0
	}
	return n
}

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

func sortEdges(in []graphreader.Edge) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Kind != in[j].Kind {
			return in[i].Kind < in[j].Kind
		}
		return in[i].EdgeID < in[j].EdgeID
	})
}

func normaliseLimit(requested int) int {
	if requested <= 0 || requested > graphreader.MaxListLimit {
		return graphreader.MaxListLimit
	}
	return requested
}
