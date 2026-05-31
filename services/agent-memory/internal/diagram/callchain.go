package diagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Direction enumerates the BFS walk directions accepted by
// BuildCallChain. The values are matched against the `direction`
// query parameter on `GET /api/diagram/calls` (architecture S5.6)
// and the `codeintel diagram calls --direction` CLI flag.
const (
	// DirectionBoth walks callees via ListEdgesFrom AND callers
	// via ListEdgesTo at every frontier node.
	DirectionBoth = "both"
	// DirectionCallers walks only inbound edges (who calls me).
	DirectionCallers = "callers"
	// DirectionCallees walks only outbound edges (whom do I call).
	DirectionCallees = "callees"
)

// CallChainSeedSeparator is the literal character that splits the
// encoded seed form `<repoID>|<kind>|<canonical-signature>` into
// the triple LookupBySignature needs. The `|` character was
// picked because it does NOT appear in node-kind enum values
// (`repo, package, file, class, method, block`), repository
// UUIDs (hex + dashes), or the canonical-signature grammar used
// by any current language adapter (Go uses `:` and `(`, Java/C#
// use `#` and `(`, Python uses `.` and `(`, TypeScript uses `.`
// and `<>`, Rust uses `::`, PowerShell uses `:`). Anchoring on
// `|` keeps the encoding unambiguous across the polyglot AST set.
const CallChainSeedSeparator = "|"

// SeedNotFoundCode is the machine-readable error code Stage 7.4's
// `GET /api/diagram/calls` handler maps onto HTTP 404. Surfaced
// as both the literal string the handler writes into the
// `{"error":...}` body AND the `Error()` of `ErrSeedNotFound`
// so callers can detect it with either `errors.Is` or a string
// compare on the wire body.
const SeedNotFoundCode = "seed_not_found"

// ErrSeedNotFound is the sentinel returned by BuildCallChain when
// the supplied `seed` does not resolve via either resolution
// path. The Stage 7.4 HTTP handler maps `errors.Is(err,
// ErrSeedNotFound)` onto `404 {"error":"seed_not_found"}`
// (e2e-scenarios.md `calls-handler-seed-not-found-404`).
var ErrSeedNotFound = errors.New(SeedNotFoundCode)

// ErrInvalidDirection is returned when `direction` is not one of
// {both, callers, callees}. The HTTP handler maps this onto a
// 400 response (impl-plan Stage 7.4 validates the closed set).
var ErrInvalidDirection = errors.New("diagram: invalid direction")

// ErrNegativeDepth is returned when `depth < 0`. The HTTP
// handler maps this onto a 400 response. `depth == 0` is
// allowed and yields the seed-only envelope (one node, zero
// edges) -- the natural identity case for a bounded BFS.
var ErrNegativeDepth = errors.New("diagram: negative depth")

// callChainEdgeKinds are the two persisted edge kinds the BFS
// walks: `static_calls` (the AST dispatcher's same-file resolver
// output) and `observed_calls` (runtime spans ingested by the
// span pipeline). Each emitted Edge keeps its underlying `kind`
// so the UI can style `observed_calls` differently from
// `static_calls` per architecture S4.4.1.
var callChainEdgeKinds = []string{"static_calls", "observed_calls"}

// BuildCallChain projects a left-right call-chain diagram by
// performing a bounded BFS around the resolved `seed` Node up to
// `depth` hops. Seed resolution tries two forms in order:
//
//  1. ENCODED TRIPLE -- `<repoID>|<kind>|<canonical-signature>`,
//     forwarded to `reader.LookupBySignature`. This is the form
//     the `codeintel diagram calls --seed <sig>` CLI and the
//     `GET /api/diagram/calls?seed=...` handler synthesize when
//     the user passes a human-readable signature (architecture
//     S5.6). The `ParseRepoID` strictness rejects any seed where
//     the first segment is not a 36-character canonical UUID,
//     which falls through to form 2.
//
//  2. BARE NODE ID -- forwarded to `reader.GetNode`. This is the
//     form the `<CallChainNav>` React component re-issues when
//     the user clicks a node (architecture S6.5: "re-seed the
//     BFS with the clicked node id"). Works for any backend
//     because Postgres uses UUIDs and the memory/SQLite backends
//     use synthetic IDs -- `GetNode` is opaque to the format.
//
// Both forms map an unresolved seed onto `ErrSeedNotFound` with
// a zero-value Diagram, satisfying e2e-scenarios.md
// `callchain-unresolved-seed-returns-error-envelope`.
//
// BFS semantics:
//   - `direction = "callees"` walks `ListEdgesFrom` only.
//   - `direction = "callers"` walks `ListEdgesTo` only.
//   - `direction = "both"` walks both at every frontier node.
//   - `depth` bounds the number of BFS steps from the seed; a
//     chain `A -> B -> C -> D` with `seed=A, depth=2,
//     direction="callees"` emits `{A, B, C}` and stops before
//     `D` (e2e-scenarios.md `callchain-depth-bounded`).
//   - Visited Nodes and Edges are deduped by id so a cycle
//     `A -> B -> A` terminates and a fan-in `A -> X <- B`
//     surfaces `X` exactly once.
//
// The envelope is built with `KindCallChain` and
// `LayoutHierarchicalLeftRight`. Repo metadata is best-effort:
// if `ListRepos` returns a match for the seed Node's RepoID,
// the `url` and `sha` fields are populated; otherwise only the
// `id` is set. Truncation accounting (Stage 6.4) is deferred to
// `internal/diagram/truncate.go`; this builder leaves
// `Truncated=false` and `Stats.CappedAt=MaxListLimit` (the
// `NewEmpty` default).
func BuildCallChain(
	ctx context.Context,
	reader graphsink.Reader,
	seed string,
	depth int,
	direction string,
) (Diagram, error) {
	if reader == nil {
		return Diagram{}, errors.New("diagram: nil reader")
	}
	if depth < 0 {
		return Diagram{}, fmt.Errorf("%w: %d", ErrNegativeDepth, depth)
	}
	switch direction {
	case DirectionBoth, DirectionCallers, DirectionCallees:
	default:
		return Diagram{}, fmt.Errorf("%w: %q", ErrInvalidDirection, direction)
	}

	seedNode, err := resolveSeed(ctx, reader, seed)
	if err != nil {
		return Diagram{}, err
	}

	repo := lookupRepo(ctx, reader, seedNode.RepoID)
	env := NewEmpty(
		KindCallChain,
		LayoutHierarchicalLeftRight,
		repo,
		time.Now().UTC(),
	)

	visitedNodes := map[string]struct{}{seedNode.NodeID: {}}
	visitedEdges := map[string]struct{}{}
	env.Nodes = append(env.Nodes, toEnvelopeNode(seedNode))

	frontier := []graphreader.Node{seedNode}
	wantCallees := direction == DirectionBoth || direction == DirectionCallees
	wantCallers := direction == DirectionBoth || direction == DirectionCallers

	for step := 0; step < depth && len(frontier) > 0; step++ {
		var next []graphreader.Node

		for _, n := range frontier {
			if wantCallees {
				edges, eerr := reader.ListEdgesFrom(
					ctx, n.NodeID, callChainEdgeKinds,
					graphreader.ReaderOptions{},
				)
				if eerr != nil {
					return Diagram{}, fmt.Errorf(
						"diagram: BuildCallChain: ListEdgesFrom(%s): %w",
						n.NodeID, eerr,
					)
				}
				for _, e := range edges {
					if _, dup := visitedEdges[e.EdgeID]; dup {
						continue
					}
					// Resolve the opposite endpoint FIRST. If
					// it does not exist (retired, missing,
					// dangling reference), skip the entire
					// edge -- emitting an edge whose endpoint
					// is absent would leave the UI's NVL
					// renderer with an orphan relationship and
					// break the single-parser envelope
					// invariant (architecture S4.4).
					if _, seen := visitedNodes[e.DstNodeID]; !seen {
						dst, derr := reader.GetNode(
							ctx, e.DstNodeID,
							graphreader.ReaderOptions{},
						)
						if derr != nil {
							if errors.Is(derr, graphreader.ErrNotFound) {
								// Drop the edge entirely so
								// the envelope never contains
								// a dangling reference.
								continue
							}
							return Diagram{}, fmt.Errorf(
								"diagram: BuildCallChain: GetNode(dst=%s): %w",
								e.DstNodeID, derr,
							)
						}
						visitedNodes[dst.NodeID] = struct{}{}
						env.Nodes = append(env.Nodes, toEnvelopeNode(dst))
						next = append(next, dst)
					}
					visitedEdges[e.EdgeID] = struct{}{}
					env.Edges = append(env.Edges, toEnvelopeEdge(e))
				}
			}

			if wantCallers {
				edges, eerr := reader.ListEdgesTo(
					ctx, n.NodeID, callChainEdgeKinds,
					graphreader.ReaderOptions{},
				)
				if eerr != nil {
					return Diagram{}, fmt.Errorf(
						"diagram: BuildCallChain: ListEdgesTo(%s): %w",
						n.NodeID, eerr,
					)
				}
				for _, e := range edges {
					if _, dup := visitedEdges[e.EdgeID]; dup {
						continue
					}
					if _, seen := visitedNodes[e.SrcNodeID]; !seen {
						src, serr := reader.GetNode(
							ctx, e.SrcNodeID,
							graphreader.ReaderOptions{},
						)
						if serr != nil {
							if errors.Is(serr, graphreader.ErrNotFound) {
								continue
							}
							return Diagram{}, fmt.Errorf(
								"diagram: BuildCallChain: GetNode(src=%s): %w",
								e.SrcNodeID, serr,
							)
						}
						visitedNodes[src.NodeID] = struct{}{}
						env.Nodes = append(env.Nodes, toEnvelopeNode(src))
						next = append(next, src)
					}
					visitedEdges[e.EdgeID] = struct{}{}
					env.Edges = append(env.Edges, toEnvelopeEdge(e))
				}
			}
		}

		frontier = next
	}

	env.Stats.NodeCount = len(env.Nodes)
	env.Stats.EdgeCount = len(env.Edges)
	return env, nil
}

// callChainSeedKinds is the ordered list of node kinds the bare-
// signature seed-resolution path probes via LookupBySignature.
// Methods/functions are by far the most common call-chain seed
// (architecture S4.4 "BFS rooted at a chosen symbol") so they
// come first; classes/files/packages/blocks/repos follow so
// less-common seeds still resolve. The order also pins the
// "first match wins" deterministic resolution rule -- a
// canonical signature collision across kinds is decided in
// favour of the earliest kind in this slice.
var callChainSeedKinds = []string{
	"method", "class", "file", "package", "block", "repo",
}

// resolveSeed implements the seed-resolution contract documented
// on BuildCallChain. The brief mandates "first via
// LookupBySignature then via GetNode if it parses as a UUID";
// the implementation follows that order exactly:
//
//  1. LookupBySignature path -- accepts EITHER:
//     a) the explicit `<repoID>|<kind>|<signature>` triple the
//        Stage 7.4 HTTP handler synthesizes when it can route
//        the `?repo=<id>` query parameter into the seed, OR
//     b) the bare canonical signature `<signature>` form
//        documented in architecture S5.6 and the
//        `--seed <sig-or-id>` CLI flag, resolved by enumerating
//        `ListRepos` x `callChainSeedKinds` and returning the
//        first match (architecture S4.4 "BFS rooted at a chosen
//        symbol resolved by `canonical_signature`").
//
//  2. GetNode fallback -- ONLY when the seed parses as a
//     canonical 36-character UUID via `fingerprint.ParseRepoID`
//     (the project's standard UUID parser; node ids in the
//     Postgres backend share the UUID namespace with repo ids
//     so the same parser validates both shapes). Synthetic
//     ids from the memory/SQLite backends (e.g. `n-0000001`)
//     do not parse as UUIDs, so the bare-signature path above
//     is the only resolution they support for the CLI form
//     `--seed <sig>` -- which is correct: the memory backend
//     is single-repo by construction, so enumeration is O(1).
//
// Returns `ErrSeedNotFound` (sentinel; `errors.Is` compatible)
// when none of the above resolves -- the Stage 7.4 handler maps
// this onto HTTP 404 with body `{"error":"seed_not_found"}`.
func resolveSeed(
	ctx context.Context,
	reader graphsink.Reader,
	seed string,
) (graphreader.Node, error) {
	if seed == "" {
		return graphreader.Node{}, ErrSeedNotFound
	}

	// Step 1a: explicit `repoID|kind|signature` encoded triple.
	if strings.Contains(seed, CallChainSeedSeparator) {
		parts := strings.SplitN(seed, CallChainSeedSeparator, 3)
		if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
			rid, perr := fingerprint.ParseRepoID(parts[0])
			if perr == nil {
				n, lerr := reader.LookupBySignature(
					ctx, rid, parts[1], parts[2],
					graphreader.ReaderOptions{},
				)
				if lerr == nil {
					return n, nil
				}
				if !errors.Is(lerr, graphreader.ErrNotFound) {
					return graphreader.Node{}, fmt.Errorf(
						"diagram: BuildCallChain: LookupBySignature(encoded): %w",
						lerr,
					)
				}
				// Fall through to the bare-signature path so a
				// signature that accidentally contains the
				// separator can still resolve via enumeration.
			}
		}
	}

	// Step 1b: bare canonical signature -- enumerate the repos
	// we know about, probing each (repo, kind) pair in order.
	// First match wins. The architecture S4.4 contract for
	// `--seed <sig>` does not pass a repo, so enumeration is
	// the only LookupBySignature path that satisfies the
	// documented CLI/API form.
	summaries, lerr := reader.ListRepos(ctx, graphreader.ReaderOptions{})
	if lerr != nil {
		// Surface listing errors; the caller decides whether to
		// treat them as 5xx. Do NOT silently fall through to
		// GetNode because a partial-result fallback would hide
		// the real backend failure.
		return graphreader.Node{}, fmt.Errorf(
			"diagram: BuildCallChain: ListRepos: %w", lerr,
		)
	}
	for _, s := range summaries {
		rid, perr := fingerprint.ParseRepoID(s.RepoID)
		if perr != nil {
			// Best-effort: backends whose RepoID is not a UUID
			// (none today, but the contract leaves it as a
			// natural-key string) skip the enumeration step
			// instead of failing the whole resolve.
			continue
		}
		for _, kind := range callChainSeedKinds {
			n, kerr := reader.LookupBySignature(
				ctx, rid, kind, seed,
				graphreader.ReaderOptions{},
			)
			if kerr == nil {
				return n, nil
			}
			if !errors.Is(kerr, graphreader.ErrNotFound) {
				return graphreader.Node{}, fmt.Errorf(
					"diagram: BuildCallChain: LookupBySignature"+
						"(%s, %s): %w",
					rid.String(), kind, kerr,
				)
			}
		}
	}

	// Step 2: GetNode fallback, gated on canonical-UUID seed.
	// `fingerprint.ParseRepoID` accepts the 36-character form
	// `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` -- the same shape
	// Postgres `gen_random_uuid()` emits for `node.node_id`,
	// per migration 0003. Non-UUID seeds (memory/SQLite
	// synthetic ids like `n-0000001`) short-circuit to
	// seed_not_found instead of issuing a doomed GetNode.
	if _, perr := fingerprint.ParseRepoID(seed); perr != nil {
		return graphreader.Node{}, ErrSeedNotFound
	}
	n, gerr := reader.GetNode(ctx, seed, graphreader.ReaderOptions{})
	if gerr == nil {
		return n, nil
	}
	if errors.Is(gerr, graphreader.ErrNotFound) {
		return graphreader.Node{}, ErrSeedNotFound
	}
	return graphreader.Node{}, fmt.Errorf(
		"diagram: BuildCallChain: GetNode: %w", gerr,
	)
}

// lookupRepo best-effort enriches the envelope's Repo block with
// the URL + SHA from ListRepos. If the lookup fails (transient
// reader error, backend that does not back the metadata) we
// fall back to a Repo with only the id populated -- the diagram
// is still useful and the UI's repo-picker drives the URL/SHA
// labels independently.
func lookupRepo(
	ctx context.Context,
	reader graphsink.Reader,
	repoID string,
) Repo {
	if repoID == "" {
		return Repo{}
	}
	summaries, err := reader.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		return Repo{ID: repoID}
	}
	for _, s := range summaries {
		if s.RepoID == repoID || s.RepoUUID == repoID {
			return Repo{ID: s.RepoID, URL: s.URL, SHA: s.SHA}
		}
	}
	return Repo{ID: repoID}
}

// toEnvelopeNode maps a persisted graphreader.Node onto the
// envelope's Node shape. `group` defaults to the ParentNodeID so
// siblings cluster under the same parent in the UI; for the
// repo Node (no parent) it falls back to the RepoID.
func toEnvelopeNode(n graphreader.Node) Node {
	group := n.ParentNodeID
	if group == "" {
		group = n.RepoID
	}
	return Node{
		ID:       n.NodeID,
		Label:    deriveLabel(n.CanonicalSignature),
		Kind:     n.Kind,
		Language: extractLanguage(n.AttrsJSON),
		Group:    group,
		Attrs:    n.AttrsJSON,
	}
}

// toEnvelopeEdge maps a persisted graphreader.Edge onto the
// envelope's Edge shape. The persisted `Kind` is preserved
// verbatim so the UI can style `observed_calls` differently
// from `static_calls` (architecture S4.4.1).
func toEnvelopeEdge(e graphreader.Edge) Edge {
	return Edge{
		ID:     e.EdgeID,
		From:   e.SrcNodeID,
		To:     e.DstNodeID,
		Kind:   e.Kind,
		Weight: 1,
		Label:  e.Kind,
	}
}

// deriveLabel returns a short human-readable caption for the
// envelope's `label` field. It strips a leading `file://` URI
// scheme and returns the rightmost token split on `/`, `.`,
// `#`, or `::`, then trims any trailing parameter list. The
// fallback for a signature with no separators is the signature
// itself.
func deriveLabel(sig string) string {
	if sig == "" {
		return ""
	}
	s := strings.TrimPrefix(sig, "file://")
	// Trim parameter list: `foo(int,int)` -> `foo`.
	if idx := strings.IndexByte(s, '('); idx >= 0 {
		s = s[:idx]
	}
	// Rightmost segment after any of the common separators.
	for _, sep := range []string{"::", "/", "#", "."} {
		if idx := strings.LastIndex(s, sep); idx >= 0 {
			s = s[idx+len(sep):]
		}
	}
	if s == "" {
		return sig
	}
	return s
}

// extractLanguage returns the value of a `language` or `lang`
// key in the attrs JSON object, or the empty string when the
// attrs are absent, malformed, or do not name a language. The
// UI uses this to color-code nodes per the architecture S4.4.1
// "group by language" rule.
func extractLanguage(attrs json.RawMessage) string {
	if len(attrs) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(attrs, &m); err != nil {
		return ""
	}
	for _, k := range []string{"language", "lang"} {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}
