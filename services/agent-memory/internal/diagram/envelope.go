package diagram

import (
	"encoding/json"
	"time"
)

// LayoutHint enumerates the layout presets the projector requests
// from the UI. The UI maps each value to a concrete neo4j-nvl
// layout (see architecture S4.4.1).
type LayoutHint string

const (
	// LayoutHierarchicalTopDown is the default for module diagrams.
	LayoutHierarchicalTopDown LayoutHint = "hierarchical-top-down"
	// LayoutHierarchicalLeftRight is the default for call-chain diagrams.
	LayoutHierarchicalLeftRight LayoutHint = "hierarchical-left-right"
	// LayoutForce is the fallback for dense module-import graphs.
	LayoutForce LayoutHint = "force"
)

// DiagramKind enumerates the two diagram families. Both share the
// envelope, but `Diagram.Diagram` selects the family.
type DiagramKind string

const (
	// KindModule is the module/component containment + imports diagram.
	KindModule DiagramKind = "module"
	// KindCallChain is the BFS call-chain diagram rooted at a seed symbol.
	KindCallChain DiagramKind = "callchain"
)

// MaxListLimit mirrors graphreader.MaxListLimit (10_000). The
// projector populates Stats.CappedAt from this constant so the UI
// can render the truncation badge with the same number the backend
// enforces. Duplicated (rather than imported) to keep this package
// free of any backend dependency -- the envelope must stay a leaf.
const MaxListLimit = 10_000

// Repo identifies the repository the diagram was projected from.
// `id` is the repo's UUID (or a synthetic id for the memory/JSON
// backend, hashed from `(url, sha)`). `url` is the canonical git
// URL or `file://<abs-path>` for local scans. `sha` is the
// 40-char commit SHA or the sentinel "local" for local-dir scans
// without a working git tree (architecture S3.3).
type Repo struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	SHA string `json:"sha"`
}

// Node is one rendered vertex. `id` is the persisted node UUID, a
// deterministic hash for the memory backend, or `pkg:<canonical_signature>`
// for module-diagram roll-up nodes (architecture S4.4 synthetic-id
// rules 1 & 2). `kind` is one of the node-kind enum values; `group`
// is the owning package or file id used for clustering / color.
// `attrs` mirrors the relevant subset of `node.attrs_json` -- typed
// as a generic JSON object so per-language LangMeta keys round-trip
// without a schema change.
type Node struct {
	ID       string          `json:"id"`
	Label    string          `json:"label"`
	Kind     string          `json:"kind"`
	Language string          `json:"language"`
	Group    string          `json:"group"`
	Attrs    json.RawMessage `json:"attrs"`
}

// Edge is one rendered relationship. `kind` is the persisted edge
// kind verbatim (e.g. `contains`, `imports`, `static_calls`,
// `observed_calls`, `extends`, `implements`, `overrides`, `reads`,
// `writes`). `weight` is the rolled-up count for module-diagram
// `imports` edges and defaults to 1 elsewhere; the UI maps it to
// `1 + log2(weight)` line width (architecture S4.4.1). `label` is
// the human-readable edge caption (usually `kind` verbatim).
type Edge struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Weight int    `json:"weight"`
	Label  string `json:"label"`
}

// Stats reports projector counters. `nodeCount` and `edgeCount` are
// the post-truncation sizes that actually shipped in the envelope.
// `cappedAt` is the limit a reader-level clamp hit (always set, even
// when no truncation occurred, so the UI can render "X of 10000"
// affordances). `skipped` is a well-known-keys map of coverage
// degradations from the AST dispatcher; canonical keys are
// `no_parser` and `pwsh_not_available` (architecture S7.3, S7.1).
// The map is always emitted, even when every counter is zero, so
// the UI can rely on its presence rather than guarding for nil.
type Stats struct {
	NodeCount int            `json:"nodeCount"`
	EdgeCount int            `json:"edgeCount"`
	CappedAt  int            `json:"cappedAt"`
	Skipped   map[string]int `json:"skipped"`
}

// Diagram is the single envelope both diagram families return. The
// JSON marshalling preserves field order
//
//	diagram, repo, generatedAt, layoutHint, nodes, edges, truncated, stats
//
// to satisfy the golden-test invariant in envelope_marshal_test.go
// and the UI's single-parser contract (architecture S4.4).
//
// `Nodes` and `Edges` are non-omitempty: an empty diagram MUST emit
// `[]` rather than `null` (the UI maps them straight to NVL arrays).
// `Truncated` is non-omitempty: its absence is meaningful (architecture
// S6 -- truncation MUST be visible).
type Diagram struct {
	Diagram     DiagramKind `json:"diagram"`
	Repo        Repo        `json:"repo"`
	GeneratedAt time.Time   `json:"generatedAt"`
	LayoutHint  LayoutHint  `json:"layoutHint"`
	Nodes       []Node      `json:"nodes"`
	Edges       []Edge      `json:"edges"`
	Truncated   bool        `json:"truncated"`
	Stats       Stats       `json:"stats"`
}

// NewEmpty returns a zero-valued envelope with non-nil slices and a
// non-nil Skipped map so callers can append without nil-guards and
// JSON marshalling emits `[]` / `{}` rather than `null`. The
// `cappedAt` field is pre-populated with MaxListLimit per S7.3.
func NewEmpty(kind DiagramKind, layout LayoutHint, repo Repo, generatedAt time.Time) Diagram {
	return Diagram{
		Diagram:     kind,
		Repo:        repo,
		GeneratedAt: generatedAt,
		LayoutHint:  layout,
		Nodes:       []Node{},
		Edges:       []Edge{},
		Truncated:   false,
		Stats: Stats{
			CappedAt: MaxListLimit,
			Skipped:  map[string]int{},
		},
	}
}
