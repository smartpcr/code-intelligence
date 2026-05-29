package ast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

// LogEntry represents a single structured log event captured by the dispatcher.
type LogEntry struct {
	Message string
	Attrs   map[string]string
}

// Logger receives structured log events from the dispatcher.
type Logger interface {
	Log(msg string, attrs map[string]string)
}

// NodeEdgeWriter is the canonical writer surface the dispatcher
// targets in production: a strict subset of `*graphwriter.Writer`
// covering the Node + Edge inserts the Stage 5.3 dispatcher needs.
//
// `*graphwriter.Writer` satisfies this interface natively, so the
// production wiring in `cmd/repoindexer/main.go` can pass a real
// graph writer in without an adapter. Tests can provide a minimal
// fake whose method bodies record inputs instead of writing to
// Postgres.
type NodeEdgeWriter interface {
	InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error)
	InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error)
}

// Node is the legacy lightweight node descriptor exposed via the
// `ast.Writer` interface in parser.go. Kept for compile-compat with
// e2e tests that target that surface; the canonical dispatcher path
// uses `graphwriter.NodeInput` via `NodeEdgeWriter`.
type Node struct {
	Kind string
	Name string
}

// Edge is the legacy lightweight edge descriptor exposed via the
// `ast.Writer` interface in parser.go. Carries an optional Symbols
// slice so the legacy surface can still convey the imports-edge
// symbol-list that Stage 5.3 requires; canonical production paths
// thread Symbols through `graphwriter.EdgeInput.AttrsJSON`.
type Edge struct {
	Kind    string
	Source  string
	Target  string
	Symbols []string
}

// EmitResult is the legacy dispatcher-local result struct that the
// e2e test suite continues to reference. The canonical EmitFile
// path returns `repoindexer.EmitResult`; this type exists purely
// for compile compatibility with the e2e build tag.
type EmitResult struct {
	NodeCount int
	EdgeCount int
}

// DispatcherOption configures a *Dispatcher at construction time.
// Options are applied left-to-right in `NewDispatcher`; later
// options overwrite earlier ones for single-valued fields.
type DispatcherOption func(*Dispatcher)

// WithEmbeddingPublisher attaches an embedding publisher the
// dispatcher will fan out Node insertions to once the embedding
// wiring is fully landed. The publisher is stored unconditionally
// so callers compile against the canonical option surface; the
// dispatcher body invokes it best-effort.
func WithEmbeddingPublisher(p NodeEmbeddingPublisher) DispatcherOption {
	return func(d *Dispatcher) { d.embedPub = p }
}

// WithLogger attaches a structured-log sink the dispatcher uses
// for skip/parse-error events. Nil silently disables logging.
func WithLogger(l Logger) DispatcherOption {
	return func(d *Dispatcher) { d.logger = l }
}

// WithParsers REPLACES the dispatcher's default parser set with
// the supplied parsers. When no `WithParsers` option is passed,
// `NewDispatcher` falls back to `defaultParsers()` so the
// production wiring picks up the build-appropriate (CGO vs
// no-CGO) parser list automatically.
//
// Variadic shape mirrors the canonical surface the
// `canonical_dispatcher`-tagged tests expect:
//
//	NewDispatcher(fw, WithParsers(NewTreeSitterRustParser()))
//	NewDispatcher(fw, WithParsers(p1, p2, p3))
func WithParsers(parsers ...Parser) DispatcherOption {
	return func(d *Dispatcher) {
		d.parsers = parsers
		d.parsersExplicit = true
	}
}

// WithLanguageHints supplies a dispatcher-global hint list used
// by extension routing when a file's suffix is ambiguous. The
// production worker also passes per-event hints via
// `EmitFileEvent.LanguageHints`; the dispatcher prefers
// per-event hints over the dispatcher-global list.
func WithLanguageHints(hints []string) DispatcherOption {
	return func(d *Dispatcher) { d.languageHints = append([]string(nil), hints...) }
}

// Dispatcher routes source files to registered parsers and writes
// the resulting Nodes/Edges through a NodeEdgeWriter, satisfying
// the `repoindexer.ASTEmitter` contract.
type Dispatcher struct {
	parsers         []Parser
	parsersExplicit bool
	extMap          map[string]Parser
	writer          NodeEdgeWriter
	embedPub        NodeEmbeddingPublisher
	logger          Logger
	languageHints   []string
}

// NewDispatcher constructs a Dispatcher with the supplied writer
// and option list. When `WithParsers` is omitted, the dispatcher
// uses the build-tagged `defaultParsers()` set so the production
// `main.go` wiring inherits the correct parser list automatically.
func NewDispatcher(w NodeEdgeWriter, opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{writer: w}
	for _, opt := range opts {
		if opt != nil {
			opt(d)
		}
	}
	if !d.parsersExplicit {
		for _, p := range defaultParsers() {
			d.parsers = append(d.parsers, p)
		}
	}
	d.extMap = make(map[string]Parser, len(d.parsers)*2)
	for _, p := range d.parsers {
		for _, ext := range p.Extensions() {
			d.extMap[ext] = p
		}
	}
	return d
}

// dispatcherParsersForTest exposes the dispatcher's extension→parser
// routing map for the cgo-tagged routing tests. Test-only accessor;
// production callers MUST use `EmitFile`.
func (d *Dispatcher) dispatcherParsersForTest() map[string]Parser {
	out := make(map[string]Parser, len(d.extMap))
	for k, v := range d.extMap {
		out[k] = v
	}
	return out
}

// EmitFile parses one file event and writes the resulting Nodes /
// Edges through the dispatcher's NodeEdgeWriter. The returned
// `repoindexer.EmitResult.TouchedNodes` lists every Node the
// dispatcher ensured (newly inserted or idempotently re-confirmed)
// so the Stage 3.4 delta handler can compute the retire-set.
//
// Pass contract (architecture Section 4 + parser_treesitter_rust.go):
//   - Pass 0  (imports):       one `package` Node + one `imports`
//     Edge per non-relative ParseResult.Import. The imports Edge
//     carries `attrs_json.symbols` so downstream consumers can see
//     which specific names were pulled in (e.g. Rust
//     `use std::fmt::Display` ⇒ `["Display"]`).
//   - Pass 1a (classes):       one `class` Node per ClassDecl.
//   - Pass 1b (methods):       one `method` Node per MethodDecl,
//     plus a simple-name multimap used by Pass 2b's ambiguity-aware
//     bare-name resolver. ReceiverAliases are registered ONLY into
//     the scoped QN lookup map (used by Pass 2b's receiver-qualified
//     path and Pass 2d), NOT into the bare-name multimap.
//   - Pass 2a (extends + implements): one edge per
//     ClassDecl.Extends/Implements entry whose target is in the
//     file's local class set (cross-file targets are dropped per
//     architecture A4 silent-drop rule).
//   - Pass 2b (static_calls):  AMBIGUITY-AWARE — a bare call
//     target is emitted as an edge ONLY when exactly one local
//     method has a matching simple name. Receiver-qualified
//     calls are scoped to `<EnclosingClass>.<name>` and emitted
//     when the scoped target exists locally.
//   - Pass 2d (overrides):     one `overrides` edge per impl
//     method (LangMeta["trait"] set) whose trait-side method
//     exists locally AND carries LangMeta["trait_default"]=true.
//
// When no parser is registered for the file's extension the dispatcher
// logs an `ast.dispatch.skip` event with reason `no_parser` and
// returns an empty `EmitResult` with nil error.
//
// When the writer is nil (tests that exercise the routing surface
// only) the dispatcher walks the ParseResult per the Pass contract
// but skips every `InsertNode` / `InsertEdge` call.
func (d *Dispatcher) EmitFile(ctx context.Context, ev repoindexer.EmitFileEvent) (repoindexer.EmitResult, error) {
	ext := strings.ToLower(filepath.Ext(ev.RelPath))

	p, ok := d.extMap[ext]
	if !ok {
		if d.logger != nil {
			d.logger.Log("ast.dispatch.skip", map[string]string{
				"file":   ev.RelPath,
				"reason": "no_parser",
			})
		}
		return repoindexer.EmitResult{}, nil
	}

	if ev.Open == nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast: dispatcher: EmitFileEvent.Open is nil for %s", ev.RelPath)
	}
	rdr, err := ev.Open()
	if err != nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast: dispatcher: open %s: %w", ev.RelPath, err)
	}
	src, readErr := io.ReadAll(rdr)
	closeErr := rdr.Close()
	if readErr != nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast: dispatcher: read %s: %w", ev.RelPath, readErr)
	}
	if closeErr != nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast: dispatcher: close %s: %w", ev.RelPath, closeErr)
	}

	result, err := p.Parse(ev.RelPath, src)
	if err != nil {
		if d.logger != nil {
			d.logger.Log("ast.parse.error", map[string]string{
				"file":  ev.RelPath,
				"error": err.Error(),
			})
		}
		return repoindexer.EmitResult{}, err
	}

	var touched []repoindexer.TouchedNode

	insertNode := func(in graphwriter.NodeInput) (string, error) {
		if d.writer == nil {
			return "", nil
		}
		rec, ierr := d.writer.InsertNode(ctx, in)
		if ierr != nil {
			return "", ierr
		}
		touched = append(touched, repoindexer.TouchedNode{
			NodeID:             rec.NodeID,
			Kind:               in.Kind,
			CanonicalSignature: in.CanonicalSignature,
			ParentNodeID:       in.ParentNodeID,
			Inserted:           rec.Inserted,
		})
		return rec.NodeID, nil
	}
	insertEdge := func(in graphwriter.EdgeInput) error {
		if d.writer == nil {
			return nil
		}
		_, eerr := d.writer.InsertEdge(ctx, in)
		return eerr
	}

	// Pass 0: imports → package nodes + imports edges
	// (skip workspace-relative module specifiers).
	for _, imp := range result.Imports {
		if isRelativeImportSpecifier(imp.Module) {
			continue
		}
		pkgSig := fmt.Sprintf("%s::package::%s", ev.RepoURL, imp.Module)
		// Package nodes are keyed by module alone; per-import
		// symbol/alias detail belongs on the edge, not the node
		// (multiple imports from the same module share one node).
		pkgID, perr := insertNode(graphwriter.NodeInput{
			RepoID:             ev.RepoID,
			Kind:               "package",
			CanonicalSignature: pkgSig,
			ParentNodeID:       ev.RepoNodeID,
			FromSHA:            ev.SHA,
			AttrsJSON:          packageAttrsJSON(imp),
		})
		if perr != nil {
			return repoindexer.EmitResult{TouchedNodes: touched}, perr
		}
		if eerr := insertEdge(graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "imports",
			SrcNodeID: ev.FileNodeID,
			DstNodeID: pkgID,
			FromSHA:   ev.SHA,
			AttrsJSON: importsEdgeAttrsJSON(imp),
		}); eerr != nil {
			return repoindexer.EmitResult{TouchedNodes: touched}, eerr
		}
	}

	// Pass 1a: classes (build local-class set + sig→NodeID map).
	classSigToNodeID := make(map[string]string, len(result.Classes))
	for _, c := range result.Classes {
		sig := fmt.Sprintf("%s::class::%s#%s", ev.RepoURL, ev.RelPath, c.QualifiedName)
		id, cerr := insertNode(graphwriter.NodeInput{
			RepoID:             ev.RepoID,
			Kind:               "class",
			CanonicalSignature: sig,
			ParentNodeID:       ev.FileNodeID,
			FromSHA:            ev.SHA,
		})
		if cerr != nil {
			return repoindexer.EmitResult{TouchedNodes: touched}, cerr
		}
		classSigToNodeID[c.QualifiedName] = id
	}

	// Pass 1b: methods (build scoped-QN map for receiver-qualified
	// resolution AND simple-name multimap for ambiguity-aware
	// bare-name resolution).
	methodSigToNodeID := make(map[string]string, len(result.Methods))
	methodDeclsByQN := make(map[string]MethodDecl, len(result.Methods))
	simpleNameToQNs := make(map[string]map[string]struct{}, len(result.Methods))
	registerSimpleName := func(simple, qn string) {
		set, ok := simpleNameToQNs[simple]
		if !ok {
			set = make(map[string]struct{}, 1)
			simpleNameToQNs[simple] = set
		}
		set[qn] = struct{}{}
	}
	for _, m := range result.Methods {
		methodSig := fmt.Sprintf("%s::method::%s#%s(%s)", ev.RepoURL, ev.RelPath, m.QualifiedName, m.ParamSignature)
		parentID := ev.FileNodeID
		if m.EnclosingClass != "" {
			if pid, ok := classSigToNodeID[m.EnclosingClass]; ok {
				parentID = pid
			}
		}
		id, merr := insertNode(graphwriter.NodeInput{
			RepoID:             ev.RepoID,
			Kind:               "method",
			CanonicalSignature: methodSig,
			ParentNodeID:       parentID,
			FromSHA:            ev.SHA,
		})
		if merr != nil {
			return repoindexer.EmitResult{TouchedNodes: touched}, merr
		}
		methodSigToNodeID[m.QualifiedName] = id
		methodDeclsByQN[m.QualifiedName] = m
		registerSimpleName(lastDottedSegment(m.QualifiedName), m.QualifiedName)
		// Register receiver aliases ONLY in the scoped QN map
		// (avoids creating artificial bare-name ambiguity for
		// Go's pointer-receiver `*Foo.Bar` → `Foo.Bar` alias).
		for _, alias := range m.ReceiverAliases {
			methodSigToNodeID[alias] = id
		}
	}

	// Pass 2a: extends + implements edges (local targets only;
	// cross-file targets silently dropped per architecture A4).
	for _, c := range result.Classes {
		srcID, ok := classSigToNodeID[c.QualifiedName]
		if !ok {
			continue
		}
		for _, parent := range c.Extends {
			dstID, ok := classSigToNodeID[parent]
			if !ok {
				continue
			}
			if eerr := insertEdge(graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "extends",
				SrcNodeID: srcID,
				DstNodeID: dstID,
				FromSHA:   ev.SHA,
			}); eerr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, eerr
			}
		}
		for _, iface := range c.Implements {
			dstID, ok := classSigToNodeID[iface]
			if !ok {
				continue
			}
			if eerr := insertEdge(graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "implements",
				SrcNodeID: srcID,
				DstNodeID: dstID,
				FromSHA:   ev.SHA,
			}); eerr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, eerr
			}
		}
	}

	// Pass 2b: static_calls — ambiguity-aware bare-name + scoped
	// receiver-qualified resolution.
	for _, m := range result.Methods {
		srcID, ok := methodSigToNodeID[m.QualifiedName]
		if !ok {
			continue
		}
		for _, callee := range m.Calls {
			simple := lastDottedSegment(callee)
			cands := simpleNameToQNs[simple]
			if len(cands) != 1 {
				continue
			}
			var targetQN string
			for qn := range cands {
				targetQN = qn
			}
			dstID, ok := methodSigToNodeID[targetQN]
			if !ok {
				continue
			}
			if eerr := insertEdge(graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "static_calls",
				SrcNodeID: srcID,
				DstNodeID: dstID,
				FromSHA:   ev.SHA,
			}); eerr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, eerr
			}
		}
		if m.EnclosingClass != "" {
			for _, callee := range m.ReceiverCalls {
				target := m.EnclosingClass + "." + callee
				dstID, ok := methodSigToNodeID[target]
				if !ok {
					continue
				}
				if eerr := insertEdge(graphwriter.EdgeInput{
					RepoID:    ev.RepoID,
					Kind:      "static_calls",
					SrcNodeID: srcID,
					DstNodeID: dstID,
					FromSHA:   ev.SHA,
				}); eerr != nil {
					return repoindexer.EmitResult{TouchedNodes: touched}, eerr
				}
			}
		}
	}

	// Pass 2d: overrides (impl method → trait-default trait
	// method, same file only; cross-file misses silently
	// dropped per architecture A4).
	for _, m := range result.Methods {
		if m.LangMeta == nil {
			continue
		}
		traitName, ok := m.LangMeta["trait"].(string)
		if !ok || traitName == "" {
			continue
		}
		traitMethodQN := traitName + "." + lastDottedSegment(m.QualifiedName)
		traitMethod, ok := methodDeclsByQN[traitMethodQN]
		if !ok {
			continue
		}
		if traitMethod.LangMeta == nil || traitMethod.LangMeta["trait_default"] != true {
			continue
		}
		srcID := methodSigToNodeID[m.QualifiedName]
		dstID := methodSigToNodeID[traitMethodQN]
		if srcID == "" || dstID == "" {
			continue
		}
		if eerr := insertEdge(graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "overrides",
			SrcNodeID: srcID,
			DstNodeID: dstID,
			FromSHA:   ev.SHA,
		}); eerr != nil {
			return repoindexer.EmitResult{TouchedNodes: touched}, eerr
		}
	}

	// Best-effort embedding publication: the publisher attached
	// via `WithEmbeddingPublisher` is invoked once per touched
	// Method Node (Class / package Nodes are skipped — Stage 3.3
	// only embeds methods and blocks). The noop default keeps
	// this surface inert until the Stage 6.x embedding wiring
	// lands the real publisher.
	if d.embedPub != nil {
		repoIDStr := ev.RepoID.String()
		for _, tn := range touched {
			if tn.Kind != "method" && tn.Kind != "block" {
				continue
			}
			req := NodeEmbedRequest{
				NodeID:             tn.NodeID,
				RepoID:             repoIDStr,
				Kind:               tn.Kind,
				CanonicalSignature: tn.CanonicalSignature,
				Content:            tn.CanonicalSignature,
				SignatureOnly:      true,
			}
			_, _ = d.embedPub.PublishNodeEmbedding(ctx, req)
		}
	}

	return repoindexer.EmitResult{TouchedNodes: touched}, nil
}

// packageAttrsJSON serialises the attrs that belong on a `package`
// Node — keyed by Module alone, so per-import symbol/alias detail
// is intentionally excluded (those live on the `imports` edge).
func packageAttrsJSON(imp Import) json.RawMessage {
	attrs := map[string]any{}
	if imp.Module != "" {
		attrs["module"] = imp.Module
	}
	if len(attrs) == 0 {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(attrs)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// importsEdgeAttrsJSON serialises the per-import detail that belongs
// on an `imports` Edge — `symbols`, `alias`, `is_type_only`, `line`.
// The `symbols` key is what Stage 5.3 requires on the Rust imports
// edge (e.g. `use std::fmt::Display` ⇒ `["Display"]`).
func importsEdgeAttrsJSON(imp Import) json.RawMessage {
	attrs := map[string]any{}
	if imp.Module != "" {
		attrs["module"] = imp.Module
	}
	if len(imp.Symbols) > 0 {
		attrs["symbols"] = imp.Symbols
	}
	if imp.Alias != "" {
		attrs["alias"] = imp.Alias
	}
	if imp.IsTypeOnly {
		attrs["is_type_only"] = true
	}
	if imp.Line > 0 {
		attrs["line"] = imp.Line
	}
	if len(attrs) == 0 {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(attrs)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// lastDottedSegment returns the right-most dotted segment of a
// qualified name (e.g. "Foo.bar" → "bar", "free_fn" → "free_fn").
// Used by Pass 2b's bare-name multimap and Pass 2d's trait-method
// lookup.
func lastDottedSegment(qn string) string {
	if i := strings.LastIndexByte(qn, '.'); i >= 0 {
		return qn[i+1:]
	}
	return qn
}

// isRelativeImportSpecifier reports whether a module specifier
// is a workspace-relative path that Pass 0 should skip. Matches
// the prefixes used by every v1 parser language whose grammar
// supports dot/up-tree relative imports (`./foo`, `../bar`).
func isRelativeImportSpecifier(mod string) bool {
	return strings.HasPrefix(mod, "./") || strings.HasPrefix(mod, "../")
}
