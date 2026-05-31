package diagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Module-diagram granularity constants. Architecture S9.5 pins
// `package` as the default with drill-down to `file` / `class`
// via the `--granularity` flag on the CLI and the
// `?granularity=` query parameter on the serve endpoint.
const (
	GranularityPackage = "package"
	GranularityFile    = "file"
	GranularityClass   = "class"
)

// nowFunc is the package-level clock used by BuildModuleDiagram
// for the envelope's `generatedAt` field. Tests override it to
// pin a deterministic timestamp.
var nowFunc = time.Now

// BuildModuleDiagram projects the module / component diagram for
// `repoID` at the requested `granularity`.
//
// Tree shape (architecture S4.4, S6.3):
//
//   - granularity="package": repo -> packages. File and class
//     Nodes are NOT surfaced; `imports` edges are rolled up to
//     pkg -> pkg with `weight = count`.
//
//   - granularity="file": repo -> packages -> files. Per-file
//     `imports` edges are preserved (file -> file), NOT rolled
//     up. Implementation-plan scenario `granularity-file`.
//
//   - granularity="class": repo -> packages -> files -> classes.
//     Per-file `imports` edges are preserved.
//
// Package nodes always use the synthetic id form
// `pkg:<canonical_signature>` per architecture S4.4 rule 2 so
// the same identity is reproducible across the Postgres,
// SQLite, and in-memory backends without requiring each backend
// to expose its surrogate-key UUID. File and class Nodes use
// their persisted NodeID (which is the deterministic synthetic
// id for the memory + SQLite backends and the real UUID for the
// Postgres backend -- both are stable within a single backend).
//
// `layoutHint` is `hierarchical-top-down` for every module
// diagram regardless of granularity (architecture S4.4 +
// implementation-plan stage 6.2).
//
// MaxListLimit clamp visibility (architecture S6 / S7.3): if any
// `ListNodes` / `ListEdgesFrom` call returns at the
// `graphreader.MaxListLimit` cap the envelope's `truncated` flag
// is set to true and `stats.cappedAt` carries the limit so the
// UI can render the truncation badge.
func BuildModuleDiagram(
	ctx context.Context,
	reader graphsink.Reader,
	repoID fingerprint.RepoID,
	granularity string,
) (Diagram, error) {
	if reader == nil {
		return Diagram{}, errors.New("diagram: BuildModuleDiagram: nil reader")
	}
	if repoID.IsZero() {
		return Diagram{}, errors.New("diagram: BuildModuleDiagram: zero RepoID")
	}
	if granularity == "" {
		granularity = GranularityPackage
	}
	switch granularity {
	case GranularityPackage, GranularityFile, GranularityClass:
	default:
		return Diagram{}, fmt.Errorf(
			"diagram: BuildModuleDiagram: invalid granularity %q "+
				"(allowed: package|file|class)", granularity)
	}

	opts := graphreader.ReaderOptions{}
	truncated := false

	// 1. Locate the repo Node so we can anchor the containment
	//    tree on it (architecture S6.3 sequence step 2).
	repoNodes, err := reader.ListNodes(
		ctx, repoID, []string{"repo"},
		graphreader.ListNodesFilter{}, opts,
	)
	if err != nil {
		return Diagram{}, fmt.Errorf("diagram: list repo nodes: %w", err)
	}
	if len(repoNodes) == 0 {
		return Diagram{}, fmt.Errorf(
			"diagram: repo Node not found for RepoID %s", repoID)
	}
	if len(repoNodes) >= graphreader.MaxListLimit {
		truncated = true
	}
	repoNode := repoNodes[0]

	// 2. Resolve the envelope's `repo` block (id, url, sha) from
	//    the per-backend RepoSummary so the envelope's identity
	//    matches what `GET /api/repos` returns. The Postgres
	//    backend populates RepoUUID; the SQLite + memory backends
	//    leave it empty and we fall back to the natural-key
	//    string form.
	repoMeta := Repo{}
	summaries, err := reader.ListRepos(ctx, opts)
	if err != nil {
		return Diagram{}, fmt.Errorf("diagram: list repos: %w", err)
	}
	repoIDStr := repoID.String()
	for _, s := range summaries {
		if s.RepoID != repoIDStr {
			continue
		}
		repoMeta.URL = s.URL
		repoMeta.SHA = s.SHA
		if s.RepoUUID != "" {
			repoMeta.ID = s.RepoUUID
		} else {
			repoMeta.ID = s.RepoID
		}
		break
	}
	if repoMeta.ID == "" {
		// Fall back to the natural-key id when ListRepos did not
		// surface a matching row (e.g. a future test fixture
		// that exercises only the node store).
		repoMeta.ID = repoIDStr
	}

	d := NewEmpty(KindModule, LayoutHierarchicalTopDown, repoMeta, nowFunc().UTC())

	// 3. Anchor: emit the repo Node itself so the containment
	//    tree has a single root the UI can render.
	d.Nodes = append(d.Nodes, Node{
		ID:       repoNode.NodeID,
		Label:    repoLabel(repoMeta.URL, repoNode.CanonicalSignature),
		Kind:     "repo",
		Language: extractLanguage(repoNode.AttrsJSON),
		Group:    "",
		Attrs:    nonEmptyAttrs(repoNode.AttrsJSON),
	})

	// 4. Walk packages under the repo Node.
	pkgs, err := reader.ListNodes(
		ctx, repoID, []string{"package"},
		graphreader.ListNodesFilter{ParentNodeID: repoNode.NodeID}, opts,
	)
	if err != nil {
		return Diagram{}, fmt.Errorf("diagram: list packages: %w", err)
	}
	if len(pkgs) >= graphreader.MaxListLimit {
		truncated = true
	}

	// pkgIDToSyn maps the persisted package NodeID -> synthetic
	// `pkg:<canonical_signature>` id (architecture S4.4 rule 2).
	// Used to wire `contains` edges from the synthetic package
	// id (which is what the diagram uses) and to resolve the
	// owning-package id for the imports roll-up.
	pkgIDToSyn := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		syn := pkgSyntheticID(p.CanonicalSignature)
		pkgIDToSyn[p.NodeID] = syn
		d.Nodes = append(d.Nodes, Node{
			ID:       syn,
			Label:    tailSegment(p.CanonicalSignature),
			Kind:     "package",
			Language: extractLanguage(p.AttrsJSON),
			Group:    repoNode.NodeID,
			Attrs:    nonEmptyAttrs(p.AttrsJSON),
		})
		d.Edges = append(d.Edges, Edge{
			ID:     containsEdgeID(repoNode.NodeID, syn),
			From:   repoNode.NodeID,
			To:     syn,
			Kind:   "contains",
			Weight: 1,
			Label:  "contains",
		})
	}

	// 5. Walk files (needed for the imports roll-up regardless
	//    of granularity, and surfaced as Nodes when granularity
	//    is file or class). The fileToPkgSyn map drives the
	//    roll-up's "resolve src/dst file to owning package" step.
	type fileEntry struct {
		node   graphreader.Node
		pkgSyn string
	}
	var files []fileEntry
	fileToPkgSyn := make(map[string]string)

	for _, p := range pkgs {
		fs, err := reader.ListNodes(
			ctx, repoID, []string{"file"},
			graphreader.ListNodesFilter{ParentNodeID: p.NodeID}, opts,
		)
		if err != nil {
			return Diagram{}, fmt.Errorf(
				"diagram: list files in package %s: %w", p.NodeID, err)
		}
		if len(fs) >= graphreader.MaxListLimit {
			truncated = true
		}
		pkgSyn := pkgIDToSyn[p.NodeID]
		for _, f := range fs {
			files = append(files, fileEntry{node: f, pkgSyn: pkgSyn})
			fileToPkgSyn[f.NodeID] = pkgSyn
			if granularity == GranularityFile || granularity == GranularityClass {
				d.Nodes = append(d.Nodes, Node{
					ID:       f.NodeID,
					Label:    tailSegment(f.CanonicalSignature),
					Kind:     "file",
					Language: extractLanguage(f.AttrsJSON),
					Group:    pkgSyn,
					Attrs:    nonEmptyAttrs(f.AttrsJSON),
				})
				d.Edges = append(d.Edges, Edge{
					ID:     containsEdgeID(pkgSyn, f.NodeID),
					From:   pkgSyn,
					To:     f.NodeID,
					Kind:   "contains",
					Weight: 1,
					Label:  "contains",
				})
			}
		}
	}

	// 6. Drill into classes when granularity=class.
	if granularity == GranularityClass {
		for _, fe := range files {
			cs, err := reader.ListNodes(
				ctx, repoID, []string{"class"},
				graphreader.ListNodesFilter{ParentNodeID: fe.node.NodeID}, opts,
			)
			if err != nil {
				return Diagram{}, fmt.Errorf(
					"diagram: list classes in file %s: %w", fe.node.NodeID, err)
			}
			if len(cs) >= graphreader.MaxListLimit {
				truncated = true
			}
			for _, c := range cs {
				d.Nodes = append(d.Nodes, Node{
					ID:       c.NodeID,
					Label:    tailSegment(c.CanonicalSignature),
					Kind:     "class",
					Language: extractLanguage(c.AttrsJSON),
					Group:    fe.node.NodeID,
					Attrs:    nonEmptyAttrs(c.AttrsJSON),
				})
				d.Edges = append(d.Edges, Edge{
					ID:     containsEdgeID(fe.node.NodeID, c.NodeID),
					From:   fe.node.NodeID,
					To:     c.NodeID,
					Kind:   "contains",
					Weight: 1,
					Label:  "contains",
				})
			}
		}
	}

	// 7. Imports.
	//
	// The AST dispatcher emits one `imports` Edge per import
	// statement with `SrcNodeID = <file>` and
	// `DstNodeID = <package>` -- the package Node is registered
	// under the same repo with canonical_signature
	// `<repoURL>::package::<module>` (see
	// `services/agent-memory/internal/repoindexer/ast/dispatcher.go`
	// imports pass, ~lines 400-428). The roll-up therefore
	// resolves the destination via `pkgIDToSyn` FIRST (the common
	// case), falling back to `fileToPkgSyn` so a hypothetical
	// alternate writer that emits file -> file imports still
	// projects correctly. Targets that resolve to neither are
	// dropped (external / unsurfaced packages -- e.g. an import
	// the dispatcher skipped because it could not register a
	// package row).
	//
	// At granularity=package we aggregate to pkg -> pkg with a
	// weight count and dedupe-by-(src, dst). Intra-package
	// self-loops (a file in pkg A importing another file in
	// pkg A) are suppressed -- they are noise on a component-
	// level diagram.
	//
	// At granularity=file or class we preserve the imports
	// verbatim, rewriting the destination Node id to the
	// synthetic `pkg:<canonical_signature>` when the dispatcher
	// pointed it at a package Node, so every edge endpoint
	// matches a Node id present in the diagram.

	// resolvePkgID returns the synthetic package id for an
	// imports edge destination, honouring the file -> package
	// shape the dispatcher emits AND the file -> file shape an
	// alternate writer might produce. The boolean reports
	// whether the destination resolved to a surfaced package.
	resolvePkgID := func(dstNodeID string) (string, bool) {
		if syn, ok := pkgIDToSyn[dstNodeID]; ok {
			return syn, true
		}
		if syn, ok := fileToPkgSyn[dstNodeID]; ok {
			return syn, true
		}
		return "", false
	}

	if granularity == GranularityPackage {
		type pair struct{ from, to string }
		weights := make(map[pair]int)
		var order []pair
		for _, fe := range files {
			imps, err := reader.ListEdgesFrom(
				ctx, fe.node.NodeID, []string{"imports"}, opts)
			if err != nil {
				return Diagram{}, fmt.Errorf(
					"diagram: list imports for file %s: %w",
					fe.node.NodeID, err)
			}
			if len(imps) >= graphreader.MaxListLimit {
				truncated = true
			}
			for _, e := range imps {
				dstPkg, ok := resolvePkgID(e.DstNodeID)
				if !ok {
					// Target is neither a surfaced package
					// nor a file under one (external /
					// unresolved). Skip rather than emit a
					// dangling edge.
					continue
				}
				if dstPkg == fe.pkgSyn {
					// Intra-package import; suppressed by
					// design (see function doc).
					continue
				}
				p := pair{from: fe.pkgSyn, to: dstPkg}
				if _, seen := weights[p]; !seen {
					order = append(order, p)
				}
				weights[p]++
			}
		}
		sort.Slice(order, func(i, j int) bool {
			if order[i].from != order[j].from {
				return order[i].from < order[j].from
			}
			return order[i].to < order[j].to
		})
		for _, p := range order {
			d.Edges = append(d.Edges, Edge{
				ID:     importsRollupEdgeID(p.from, p.to),
				From:   p.from,
				To:     p.to,
				Kind:   "imports",
				Weight: weights[p],
				Label:  "imports",
			})
		}
	} else {
		for _, fe := range files {
			imps, err := reader.ListEdgesFrom(
				ctx, fe.node.NodeID, []string{"imports"}, opts)
			if err != nil {
				return Diagram{}, fmt.Errorf(
					"diagram: list imports for file %s: %w",
					fe.node.NodeID, err)
			}
			if len(imps) >= graphreader.MaxListLimit {
				truncated = true
			}
			for _, e := range imps {
				toID := e.DstNodeID
				if syn, ok := pkgIDToSyn[e.DstNodeID]; ok {
					// Dispatcher emitted file -> package;
					// rewrite to the synthetic package id
					// already present in the diagram so the
					// UI's edge endpoint resolves.
					toID = syn
				} else if _, known := fileToPkgSyn[e.DstNodeID]; !known {
					// Destination is neither a surfaced
					// package nor a known file Node in this
					// diagram (external / unresolved import
					// the dispatcher could not register).
					// Skip rather than emit a dangling edge
					// the UI cannot resolve -- same policy
					// as the granularity=package roll-up.
					continue
				}
				d.Edges = append(d.Edges, Edge{
					ID:     e.EdgeID,
					From:   e.SrcNodeID,
					To:     toID,
					Kind:   "imports",
					Weight: 1,
					Label:  "imports",
				})
			}
		}
	}

	d.Truncated = truncated
	d.Stats.NodeCount = len(d.Nodes)
	d.Stats.EdgeCount = len(d.Edges)
	return d, nil
}

// pkgSyntheticID returns the `pkg:<canonical_signature>` form
// architecture S4.4 rule 2 pins for module-diagram package
// roll-up nodes. The synthetic form is identical across all
// three graphsink backends so the diagram's identity is stable
// regardless of which store backed the scan.
func pkgSyntheticID(canonicalSignature string) string {
	return "pkg:" + canonicalSignature
}

// containsEdgeID mints a deterministic id for a containment
// edge. The format is `contains:<from>-><to>` so the id is
// unique within a single envelope and reproducible across
// re-projections of the same input graph (architecture S4.4
// "synthetic ids must be stable across runs").
func containsEdgeID(from, to string) string {
	return "contains:" + from + "->" + to
}

// importsRollupEdgeID mints a deterministic id for a rolled-up
// pkg -> pkg imports edge. Same stable-id rationale as
// containsEdgeID.
func importsRollupEdgeID(from, to string) string {
	return "imports:" + from + "->" + to
}

// tailSegment returns a short display label for a Node by
// stripping every prefix up to and including the final `::` or
// `/`. The canonical_signature form is implementation-specific
// per language (e.g. `<repoURL>::package::<path>`) so this
// best-effort trim keeps the UI caption short without depending
// on per-language signature parsing -- the full
// canonical_signature is always available via the inspector
// panel through `Node.Attrs`.
func tailSegment(sig string) string {
	if sig == "" {
		return ""
	}
	tail := sig
	if i := strings.LastIndex(tail, "::"); i >= 0 {
		tail = tail[i+2:]
	}
	if i := strings.LastIndex(tail, "/"); i >= 0 {
		tail = tail[i+1:]
	}
	if tail == "" {
		return sig
	}
	return tail
}

// repoLabel picks the most descriptive caption available for the
// repo Node: prefer the tail of the URL, fall back to the
// canonical_signature tail, fall back to the raw signature.
func repoLabel(url, sig string) string {
	if url != "" {
		// Strip the scheme prefix and trailing slash so e.g.
		// `https://github.com/owner/name` -> `name`.
		u := strings.TrimRight(url, "/")
		if i := strings.LastIndex(u, "/"); i >= 0 {
			u = u[i+1:]
		}
		if u != "" {
			return u
		}
	}
	return tailSegment(sig)
}

// nonEmptyAttrs returns the supplied attrs verbatim when non-
// empty, and `nil` otherwise so Node.MarshalJSON's nil-guard
// can substitute the `{}` placeholder.
func nonEmptyAttrs(attrs json.RawMessage) json.RawMessage {
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}
