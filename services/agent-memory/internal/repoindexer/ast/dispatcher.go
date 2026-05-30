package ast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

// LogEntry represents a single structured log event captured by the dispatcher.
// Retained as a public type for legacy code that built records out of dispatcher
// log calls; the dispatcher itself now emits via `*slog.Logger`.
type LogEntry struct {
	Message string
	Attrs   map[string]string
}

// Logger is the legacy log sink interface that pre-existing e2e fixtures
// implement. Kept as a named interface so older test wiring continues to
// satisfy `var _ Logger = ...` checks; the production dispatcher logs via
// `*slog.Logger` (see `WithLogger`).
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
//
// The canonical type is `*slog.Logger` ΓÇö the dispatcher emits
// `Info` for skip events (`ast.dispatch.skip`) and `Error` for
// real parse failures (`ast.parse.error`), which the
// `slog.TextHandler` renders as `msg=...` along with structured
// attributes (`reason=...`, `language=...`, `file=...`).
func WithLogger(l *slog.Logger) DispatcherOption {
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
	logger          *slog.Logger
	languageHints   []string
}

// NewDispatcher constructs a Dispatcher with the supplied writer
// and option list. When `WithParsers` is omitted, the dispatcher
// uses the build-tagged `defaultParsers()` set so the production
// `main.go` wiring inherits the correct parser list automatically.
func NewDispatcher(w NodeEdgeWriter, opts ...DispatcherOption) *Dispatcher {
	if w == nil {
		panic("ast: dispatcher: nil writer")
	}
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

// dispatcherParsersForTest exposes the dispatcher's extensionΓåÆparser
// routing map for the cgo-tagged routing tests. Test-only accessor;
// production callers MUST use `EmitFile`.
func (d *Dispatcher) dispatcherParsersForTest() map[string]Parser {
	out := make(map[string]Parser, len(d.extMap))
	for k, v := range d.extMap {
		out[k] = v
	}
	return out
}

// selectParser resolves the Parser the dispatcher would route the
// given file to, applying the architecture §4.1 contract:
//
//  1. A known (lower-cased) file extension always wins over hints.
//  2. If the extension is not registered, walk the per-event hint
//     list (preferred) or fall back to the dispatcher-global hint
//     list (set via `WithLanguageHints`). Hints are alias-expanded
//     via `normalizeHints`; the first hint whose canonical language
//     matches a registered parser's `Language()` wins.
//  3. If neither path resolves a parser, returns `nil`. The
//     dispatcher's `EmitFile` interprets that as the
//     `ast.dispatch.skip{reason=no_parser}` branch.
//
// This method is the single source of truth for the routing
// decision; `EmitFile` calls it before opening the file. The
// canonical cross-language routing tests
// (`TestDispatcher_RoutesByExtension`,
// `TestDispatcher_DotHRoutesToC_EvenWithCppHint`,
// `TestDispatcher_NoParserForUnknown`) call this method directly so
// extension routing can be asserted without going through the full
// EmitFile / writer pipeline.
func (d *Dispatcher) selectParser(relPath string, eventHints []string) Parser {
	ext := strings.ToLower(filepath.Ext(relPath))
	if p, ok := d.extMap[ext]; ok {
		return p
	}
	// Per-event hints take precedence over the dispatcher-global
	// hint list; fall back to the global list only when the
	// per-event slice is empty (architecture §4.1, tested by
	// `TestDispatcher_PerEventLanguageHintsOverrideGlobal`).
	hints := eventHints
	if len(hints) == 0 {
		hints = d.languageHints
	}
	if len(hints) == 0 {
		return nil
	}
	for _, hint := range normalizeHints(hints) {
		for _, candidate := range d.parsers {
			if candidate.Language() == hint {
				return candidate
			}
		}
	}
	return nil
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
//     `use std::fmt::Display` ΓçÆ `["Display"]`).
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
//   - Pass 2b (static_calls):  AMBIGUITY-AWARE ΓÇö a bare call
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
	// Routing decision (extension > per-event hints > dispatcher-
	// global hints) lives on `selectParser` so the cross-language
	// dispatcher routing tests can exercise it directly without
	// going through the full Parse / writer pipeline.
	p := d.selectParser(ev.RelPath, ev.LanguageHints)
	if p == nil {
		if d.logger != nil {
			d.logger.Info("ast.dispatch.skip",
				slog.String("file", ev.RelPath),
				slog.String("reason", "no_parser"),
			)
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

	// Wrap Parse in a recover so a panic inside a parser does NOT
	// take down the dispatcher (LanguageParser contract on
	// parser.go: parsers must not panic, but a defensive recover
	// here turns a regression into a logged skip instead of a
	// process abort ΓÇö test:
	// `TestDispatcher_PanicInParserIsRecovered`).
	var result ParseResult
	var parserPanicked bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				parserPanicked = true
				if d.logger != nil {
					d.logger.Error("ast.parse.panic",
						slog.String("file", ev.RelPath),
						slog.String("language", p.Language()),
						slog.Any("panic", r),
					)
				}
			}
		}()
		result, err = p.Parse(ev.RelPath, src)
	}()
	if parserPanicked {
		return repoindexer.EmitResult{}, nil
	}
	if err != nil {
		// Sentinel branch (`ErrParserUnavailable`): the parser is
		// signalling that a required runtime dependency is
		// missing (e.g. PowerShell parser without `pwsh` on PATH)
		// and the worker MUST treat the file as a SKIP, not a
		// failure. Log `ast.dispatch.skip` at INFO with the
		// extracted `reason=<slug>` and return `(EmitResult{}, nil)`
		// so the surrounding queue keeps draining.
		if errors.Is(err, ErrParserUnavailable) {
			if d.logger != nil {
				d.logger.Info("ast.dispatch.skip",
					slog.String("file", ev.RelPath),
					slog.String("language", p.Language()),
					slog.String("reason", parseUnavailableReason(err.Error())),
				)
			}
			return repoindexer.EmitResult{}, nil
		}
		if d.logger != nil {
			d.logger.Error("ast.parse.error",
				slog.String("file", ev.RelPath),
				slog.String("error", err.Error()),
			)
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

	// Inline embedding publisher: invoked once per Method or
	// Block node, IMMEDIATELY after that node's `contains` edge
	// is committed (rubber-duck #5 / canonical_dispatcher test
	// `TestDispatcher_EmbeddingPublisher_PublishesAfterContainsEdge`).
	// Dedup by node ID so an idempotent re-insert of the same
	// signature does NOT trigger a duplicate publish. The
	// two-bucket error policy from `embedding.go` is enforced
	// here: `errors.Is(err, ErrPublishRecordedFailed)` is logged
	// and swallowed (durable failure already recorded by the
	// EmbeddingIndex writer; background flusher will retry);
	// every other non-nil error propagates so the ingest job
	// fails loudly. The `published` set keeps the inline path
	// safe from accidental double-publish via repeated calls.
	published := make(map[string]struct{})
	publishInsertedNode := func(req NodeEmbedRequest) error {
		if d.embedPub == nil || req.NodeID == "" {
			return nil
		}
		if _, dup := published[req.NodeID]; dup {
			return nil
		}
		published[req.NodeID] = struct{}{}
		_, perr := d.embedPub.PublishNodeEmbedding(ctx, req)
		if perr == nil {
			return nil
		}
		if errors.Is(perr, ErrPublishRecordedFailed) {
			if d.logger != nil {
				d.logger.Info("ast.embed.publish.recorded_failed",
					slog.String("node_id", req.NodeID),
					slog.String("kind", req.Kind),
				)
			}
			return nil
		}
		return perr
	}

	// Pass 0: imports ΓåÆ package nodes + imports edges
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
			AttrsJSON:          packageAttrsJSON(imp, p.Language()),
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

	// Pass 1a: classes (build local-class set + sigΓåÆNodeID map).
	// Emits one `class` node per ClassDecl PLUS one `contains`
	// edge `file ΓåÆ class` so the fileΓåÆclassΓåÆmethodΓåÆblock
	// containment chain stays intact (test:
	// `TestDispatcher_BlockSubdivisionFiresThroughEmitter`,
	// `TestPythonFixture_EmitsExpectedNodeAndEdgeSet`,
	// `TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet`,
	// `TestPowerShellFixture_DispatcherEmitsExpectedNodesAndEdges`).
	classSigToNodeID := make(map[string]string, len(result.Classes))
	for _, c := range result.Classes {
		sig := fmt.Sprintf("%s::class::%s#%s", ev.RepoURL, ev.RelPath, c.QualifiedName)
		id, cerr := insertNode(graphwriter.NodeInput{
			RepoID:             ev.RepoID,
			Kind:               "class",
			CanonicalSignature: sig,
			ParentNodeID:       ev.FileNodeID,
			FromSHA:            ev.SHA,
			AttrsJSON:          classAttrs(p.Language(), c),
		})
		if cerr != nil {
			return repoindexer.EmitResult{TouchedNodes: touched}, cerr
		}
		classSigToNodeID[c.QualifiedName] = id
		if id != "" && ev.FileNodeID != "" {
			if eerr := insertEdge(graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "contains",
				SrcNodeID: ev.FileNodeID,
				DstNodeID: id,
				FromSHA:   ev.SHA,
			}); eerr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, eerr
			}
		}
	}

	// Pass 1b: methods (build scoped-QN map for receiver-qualified
	// resolution AND simple-name multimap for ambiguity-aware
	// bare-name resolution).
	methodSigToNodeID := make(map[string]string, len(result.Methods))
	methodDeclsByQN := make(map[string]MethodDecl, len(result.Methods))
	simpleNameToQNs := make(map[string]map[string]struct{}, len(result.Methods))
	// receiverIndex is the Go-multimap surface for receiver-qualified
	// call resolution: a key like `<EnclosingClass>.<simpleName>` maps
	// to the SET of node IDs that can be reached through it. Dedup is
	// by node ID (set semantics), so a pointer-receiver method that
	// registers under both its primary key and a `ReceiverAlias` (the
	// `Foo.Bar` alias attached to `*Foo.Bar`) only counts once. When
	// two distinct nodes collide on the same key (a value-receiver
	// `Foo.Bar` AND a pointer-receiver `*Foo.Bar` aliased to
	// `Foo.Bar`), Pass 2b drops the edge per the A5 ambiguity rule.
	receiverIndex := make(map[string]map[string]struct{}, len(result.Methods))
	registerSimpleName := func(simple, qn string) {
		set, ok := simpleNameToQNs[simple]
		if !ok {
			set = make(map[string]struct{}, 1)
			simpleNameToQNs[simple] = set
		}
		set[qn] = struct{}{}
	}
	registerReceiverKey := func(key, nodeID string) {
		set, ok := receiverIndex[key]
		if !ok {
			set = make(map[string]struct{}, 1)
			receiverIndex[key] = set
		}
		set[nodeID] = struct{}{}
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
			AttrsJSON:          methodAttrs(p.Language(), m),
		})
		if merr != nil {
			return repoindexer.EmitResult{TouchedNodes: touched}, merr
		}
		methodSigToNodeID[m.QualifiedName] = id
		methodDeclsByQN[m.QualifiedName] = m
		registerSimpleName(lastDottedSegment(m.QualifiedName), m.QualifiedName)
		// Register receiver aliases ONLY in the scoped QN map
		// (avoids creating artificial bare-name ambiguity for
		// Go's pointer-receiver `*Foo.Bar` ΓåÆ `Foo.Bar` alias).
		for _, alias := range m.ReceiverAliases {
			methodSigToNodeID[alias] = id
		}
		// Register the primary scoped key on the receiverIndex
		// (e.g. `Foo.Bar` for both `Foo.Bar` and `*Foo.Bar`) plus
		// every explicit ReceiverAlias. Dedup by node ID lets a
		// pointer-only method register under both its primary key
		// and the alias without becoming self-ambiguous.
		if m.EnclosingClass != "" {
			primaryKey := m.EnclosingClass + "." + lastDottedSegment(m.QualifiedName)
			registerReceiverKey(primaryKey, id)
		}
		for _, alias := range m.ReceiverAliases {
			registerReceiverKey(alias, id)
		}
		// Emit the parentΓåÆmethod `contains` edge using the
		// SAME parentID resolution that was used for the
		// node insert (so a method whose declared
		// EnclosingClass is missing locally still chains
		// under the file node, never floats orphaned).
		if id != "" && parentID != "" {
			if eerr := insertEdge(graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "contains",
				SrcNodeID: parentID,
				DstNodeID: id,
				FromSHA:   ev.SHA,
			}); eerr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, eerr
			}
		}
		// Inline publish for the Method node ΓÇö fires AFTER
		// the parentΓåÆmethod contains edge so the publisher
		// invariant (`PublishesAfterContainsEdge`) holds.
		methodContent := m.BodySource
		signatureOnly := false
		if strings.TrimSpace(methodContent) == "" {
			methodContent = methodSig
			signatureOnly = true
		}
		if perr := publishInsertedNode(NodeEmbedRequest{
			NodeID:             id,
			RepoID:             ev.RepoID.String(),
			Kind:               "method",
			CanonicalSignature: methodSig,
			Content:            methodContent,
			SignatureOnly:      signatureOnly,
		}); perr != nil {
			return repoindexer.EmitResult{TouchedNodes: touched}, perr
		}
		// Pass 1c (per-method block subdivision): when the
		// body exceeds the ┬º8.2 logical-line threshold the
		// dispatcher inserts two Block nodes (entry, exit)
		// as children of the method and emits a methodΓåÆblock
		// contains edge per Block. The publisher fires AFTER
		// each block's contains edge so the publish-after-
		// contains invariant holds for Blocks as well.
		blocks := SubdivideMethod(m)
		for _, b := range blocks {
			blockSig := blockSignature(methodSig, b)
			blockContent := sliceBlockSource(src, b)
			bid, berr := insertNode(graphwriter.NodeInput{
				RepoID:             ev.RepoID,
				Kind:               "block",
				CanonicalSignature: blockSig,
				ParentNodeID:       id,
				FromSHA:            ev.SHA,
				AttrsJSON:          blockAttrs(p.Language(), b),
			})
			if berr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, berr
			}
			if bid != "" && id != "" {
				if eerr := insertEdge(graphwriter.EdgeInput{
					RepoID:    ev.RepoID,
					Kind:      "contains",
					SrcNodeID: id,
					DstNodeID: bid,
					FromSHA:   ev.SHA,
				}); eerr != nil {
					return repoindexer.EmitResult{TouchedNodes: touched}, eerr
				}
			}
			if perr := publishInsertedNode(NodeEmbedRequest{
				NodeID:             bid,
				RepoID:             ev.RepoID.String(),
				Kind:               "block",
				CanonicalSignature: blockSig,
				Content:            blockContent,
				SignatureOnly:      strings.TrimSpace(blockContent) == "",
			}); perr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, perr
			}
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

	// Pass 2b: static_calls ΓÇö ambiguity-aware bare-name + scoped
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
				// Drop receiver-qualified edges whose scoped key
				// resolves to MORE THAN ONE distinct method node
				// (architecture A5: prefer missing edges over wrong
				// ones). A single-node set ΓÇö even one populated by
				// both a primary key and a ReceiverAlias ΓÇö still
				// resolves cleanly because dedup is by node ID.
				ids := receiverIndex[target]
				if len(ids) != 1 {
					continue
				}
				var dstID string
				for nid := range ids {
					dstID = nid
				}
				if dstID == "" {
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

	// Pass 2c: reads / writes edges from method body member
	// accesses (architecture ┬º5.3 ΓÇö "every recognised member
	// access becomes a `reads` or `writes` edge from the
	// enclosing method to its declaring class"). Aggregation is
	// per (method, isWrite) pair: emit ONE edge per direction
	// carrying a `members` attr array listing every distinct
	// member name accessed in that direction. Source order is
	// preserved (dedup is first-seen, not lexicographic) so the
	// emitted attrs are deterministic for replay/golden tests.
	// Test: `TestDispatcher_EmitsReadsAndWritesEdgesToEnclosingClass`.
	for _, m := range result.Methods {
		if m.EnclosingClass == "" || len(m.MemberAccesses) == 0 {
			continue
		}
		classID, ok := classSigToNodeID[m.EnclosingClass]
		if !ok {
			continue
		}
		srcID, ok := methodSigToNodeID[m.QualifiedName]
		if !ok {
			continue
		}
		var reads, writes []string
		seenR := map[string]struct{}{}
		seenW := map[string]struct{}{}
		for _, ma := range m.MemberAccesses {
			if ma.Name == "" {
				continue
			}
			if ma.IsWrite {
				if _, dup := seenW[ma.Name]; !dup {
					writes = append(writes, ma.Name)
					seenW[ma.Name] = struct{}{}
				}
			} else {
				if _, dup := seenR[ma.Name]; !dup {
					reads = append(reads, ma.Name)
					seenR[ma.Name] = struct{}{}
				}
			}
		}
		if len(writes) > 0 {
			if eerr := insertEdge(graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "writes",
				SrcNodeID: srcID,
				DstNodeID: classID,
				FromSHA:   ev.SHA,
				AttrsJSON: memberAccessAttrsJSON(writes),
			}); eerr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, eerr
			}
		}
		if len(reads) > 0 {
			if eerr := insertEdge(graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "reads",
				SrcNodeID: srcID,
				DstNodeID: classID,
				FromSHA:   ev.SHA,
				AttrsJSON: memberAccessAttrsJSON(reads),
			}); eerr != nil {
				return repoindexer.EmitResult{TouchedNodes: touched}, eerr
			}
		}
	}

	// Pass 2d: overrides (impl method ΓåÆ trait-default trait
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

	// Embedding publication for Methods and Blocks already
	// fired inline (Pass 1b) so each publish lands AFTER its
	// parentΓåÆnode `contains` edge per the canonical contract
	// (`TestDispatcher_EmbeddingPublisher_PublishesAfterContainsEdge`).
	// No trailing publisher loop is needed.

	return repoindexer.EmitResult{TouchedNodes: touched}, nil
}

// packageAttrsJSON serialises the attrs that belong on a `package`
// Node — keyed by Module alone, so per-import symbol/alias detail
// is intentionally excluded (those live on the `imports` edge).
// The `language` parameter is the producing parser's Language()
// string; it is tagged on the package node so downstream filters
// (and the per-language dispatcher tests, e.g.
// `TestCFixture_EmitFile_EmitsExpectedNodesAndEdges`) can pin the
// language that minted the package without re-deriving it from
// the file path.
func packageAttrsJSON(imp Import, language string) json.RawMessage {
	attrs := map[string]any{}
	if imp.Module != "" {
		attrs["module"] = imp.Module
	}
	// Pass 0 only walks non-relative imports (relative ones are
	// skipped in the loop body above), so every package node it
	// mints represents an EXTERNAL module. Tag the attrs with
	// `source = "external"` so the worker-emitted first-party
	// package nodes (which carry `source = "first_party"` or are
	// untagged) remain distinguishable for downstream filtering
	// (test: `TestDispatcher_EmitsImportsEdgesForExternalModules`).
	attrs["source"] = "external"
	if language != "" {
		attrs["language"] = language
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
// on an `imports` Edge ΓÇö `symbols`, `alias`, `is_type_only`, `line`.
// The `symbols` key is what Stage 5.3 requires on the Rust imports
// edge (e.g. `use std::fmt::Display` ΓçÆ `["Display"]`).
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

// memberAccessAttrsJSON wraps a deduped, source-order member-name
// slice into the canonical `{"members": [...]}` shape expected on
// the `reads` / `writes` edges emitted by Pass 2c. An empty input
// is conservatively rendered as `{}` so the edge attrs JSON is
// never broken (the caller does not emit edges with empty inputs,
// so this branch is defensive-only).
func memberAccessAttrsJSON(names []string) json.RawMessage {
	if len(names) == 0 {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(map[string]any{"members": names})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// blockAttrs serialises the file-relative position metadata that
// belongs on a `block` Node minted by Pass 1c's subdivision. The
// discriminator key is `block_kind` (not `kind`) so it never
// collides with the top-level `kind` graph column. Tests:
// `TestDispatcher_RecordsFileRelativeBlockBoundariesInAttrs`,
// `TestDispatcher_BlockSubdivisionFiresThroughEmitter`.
func blockAttrs(language string, b Block) json.RawMessage {
	attrs := map[string]any{
		"block_kind": string(b.Kind),
		"start_line": b.StartLine,
		"end_line":   b.EndLine,
		"start_byte": b.StartByte,
		"end_byte":   b.EndByte,
		"ordinal":    b.Ordinal,
	}
	if language != "" {
		attrs["language"] = language
	}
	raw, err := json.Marshal(attrs)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// sliceBlockSource extracts the source bytes for a Block from the
// surrounding file buffer. `Block.EndByte` is INCLUSIVE per
// `block.go`, so the slice upper bound is `EndByte+1`. The
// function is defensive: out-of-range / inverted offsets yield
// the empty string so a malformed block never panics the
// dispatcher.
func sliceBlockSource(src []byte, b Block) string {
	if len(src) == 0 {
		return ""
	}
	start := b.StartByte
	end := b.EndByte + 1
	if start < 0 {
		start = 0
	}
	if end > len(src) {
		end = len(src)
	}
	if start >= end {
		return ""
	}
	return string(src[start:end])
}

// lastDottedSegment returns the right-most dotted segment of a
// qualified name (e.g. "Foo.bar" ΓåÆ "bar", "free_fn" ΓåÆ "free_fn").
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

// parseUnavailableReason extracts the `reason=<slug>` token from
// a wrapped `ErrParserUnavailable` message and returns the slug.
// Falls back to `"runtime_unavailable"` when the wrapper omitted
// the reason hint.
//
// Tolerant of two wrapper shapes used in tests/production:
//
//	"powershell: parser: runtime dependency unavailable (reason=pwsh_not_available)"
//	"powershell: parser: runtime dependency unavailable"
//
// The slug terminates on `)`, whitespace, or end-of-string.
func parseUnavailableReason(msg string) string {
	const marker = "reason="
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return "runtime_unavailable"
	}
	rest := msg[idx+len(marker):]
	end := len(rest)
	for i, r := range rest {
		if r == ')' || r == ' ' || r == '\t' || r == '\n' {
			end = i
			break
		}
	}
	slug := strings.TrimSpace(rest[:end])
	if slug == "" {
		return "runtime_unavailable"
	}
	return slug
}

// mergeLangMeta folds a parser-emitted LangMeta map into the
// dispatcher's first-class attrs map under the architecture C11 /
// ┬º4.4.2 "first-class key wins on collision" rule. Presence ΓÇö
// not value ΓÇö of a key in `out` is the gate: a first-class attr
// that the dispatcher intentionally set to nil is still treated
// as "set" and a LangMeta entry for the same key is dropped.
//
// Nil / empty `in` maps are a no-op. Slice / map values pass
// through by reference; callers that re-use the input map after
// the merge must defensively clone if they need write isolation.
func mergeLangMeta(out, in map[string]any) {
	if len(in) == 0 {
		return
	}
	for k, v := range in {
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}
}

// hintAliasTable maps the v1 language-hint aliases the
// `canonical_dispatcher` `TestNormalizeHints_AliasExpansion`
// table enumerates onto their canonical language ids. Unknown
// entries (`java`, anything else) pass through unchanged after
// lower-casing + whitespace trim.
var hintAliasTable = map[string]string{
	// TypeScript family
	"ts":         "typescript",
	"tsx":        "typescript",
	"js":         "typescript",
	"jsx":        "typescript",
	"mjs":        "typescript",
	"cjs":        "typescript",
	"typescript": "typescript",
	// Python
	"py":     "python",
	"pyi":    "python",
	"python": "python",
	// C
	"c": "c",
	"h": "c",
	// C++
	"cc":  "cpp",
	"cxx": "cpp",
	"cpp": "cpp",
	"c++": "cpp",
	"hpp": "cpp",
	"hh":  "cpp",
	"hxx": "cpp",
	// C#
	"cs":     "csharp",
	"csharp": "csharp",
	"c#":     "csharp",
	// Go
	"go":     "go",
	"golang": "go",
	// Rust
	"rs":   "rust",
	"rust": "rust",
	// PowerShell
	"ps":         "powershell",
	"ps1":        "powershell",
	"psm1":       "powershell",
	"psd1":       "powershell",
	"powershell": "powershell",
}

// normalizeHints lower-cases, trims, and alias-expands the
// supplied hint list, dropping empty/whitespace-only entries
// while preserving input order. Unknown entries pass through
// (lower-cased + trimmed) so a parser registered under a new
// language id can be hint-routed without a table update.
//
// Returns `nil` for nil / fully-empty input so callers can rely
// on `len(...) == 0` regardless of empty-vs-nil distinction.
func normalizeHints(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	var out []string
	for _, h := range in {
		trimmed := strings.ToLower(strings.TrimSpace(h))
		if trimmed == "" {
			continue
		}
		if canonical, ok := hintAliasTable[trimmed]; ok {
			out = append(out, canonical)
		} else {
			out = append(out, trimmed)
		}
	}
	return out
}

// classAttrs serialises a class Node's `attrs_json` blob with the
// mandatory first-class keys (`language`, `decl_kind`,
// `start_line`, `end_line`), optionally appends the
// `extends_raw` / `implements_raw` arrays when populated, then
// folds the parser-supplied `LangMeta` map under the
// first-class-key-wins rule (┬º4.4.2).
func classAttrs(language string, c ClassDecl) json.RawMessage {
	attrs := map[string]any{
		"language":   language,
		"decl_kind":  c.Kind,
		"start_line": c.StartLine,
		"end_line":   c.EndLine,
	}
	if len(c.Extends) > 0 {
		attrs["extends_raw"] = c.Extends
	}
	if len(c.Implements) > 0 {
		attrs["implements_raw"] = c.Implements
	}
	mergeLangMeta(attrs, c.LangMeta)
	raw, err := json.Marshal(attrs)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// methodAttrs serialises a method Node's `attrs_json` blob with
// the mandatory first-class keys (`language`, `start_line`,
// `end_line`, `params_raw`) and optional `calls_raw` /
// `modifiers` / `enclosing_class` when those parser-supplied
// slices/fields are populated, then folds `LangMeta` last under
// the first-class-key-wins rule. Empty optional inputs are
// omitted so the TS/JS/Python baseline byte-print stays
// identical to the pre-helper world (rubber-duck #1).
func methodAttrs(language string, m MethodDecl) json.RawMessage {
	attrs := map[string]any{
		"language":   language,
		"start_line": m.StartLine,
		"end_line":   m.EndLine,
		"params_raw": m.ParamSignature,
	}
	if len(m.Calls) > 0 {
		attrs["calls_raw"] = m.Calls
	}
	if len(m.Modifiers) > 0 {
		attrs["modifiers"] = m.Modifiers
	}
	if m.EnclosingClass != "" {
		attrs["enclosing_class"] = m.EnclosingClass
	}
	mergeLangMeta(attrs, m.LangMeta)
	raw, err := json.Marshal(attrs)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
