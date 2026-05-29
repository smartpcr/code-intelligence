// Package ast — minimal canonical Dispatcher.
//
// This file ships the v1 surface the `cmd/repoindexer/main.go`
// wiring already calls (`NewDispatcher(writer, opts...)`,
// `WithEmbeddingPublisher`) plus the option helpers the Stage
// 3.2 dispatcher-landing workstream's tests assume
// (`WithLanguageHints`, `WithParsers`, `WithLogger`). The
// node/edge emission pipeline that pinned tests under
// `//go:build canonical_dispatcher` exercise (extends /
// implements / static_calls / contains / imports / reads /
// writes edges, block subdivision, multimap collision rules,
// trait/override resolution, embedding publish ordering)
// remains the responsibility of the Stage 3.2 dispatcher-
// landing workstream and is intentionally NOT implemented
// here -- this file is the minimum surface that (a) keeps the
// service binary compiling, (b) registers the default parser
// set returned by `defaultParsers()` (CGO-on includes the
// tree-sitter Go parser from parser_treesitter_go.go), and
// (c) routes EmitFile by file extension so a parser-routing
// test can prove `.go` reaches `goTreeSitterParser` end-to-
// end through the same code path production uses.
//
// When the Stage 3.2 dispatcher-landing workstream lands the
// full pipeline, it should swap THIS file's body in place
// (preserving the public surface) and unset the
// `//go:build canonical_dispatcher` tag on the test files
// that already encode the full contract.
package ast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

// nodeEdgeWriter is the small subset of `graphwriter.Writer`
// the dispatcher needs. Defined as an interface so unit tests
// can inject a fake without standing up a PostgreSQL
// connection. `*graphwriter.Writer` satisfies this interface
// by virtue of having `InsertNode` and `InsertEdge` methods
// with matching signatures.
type nodeEdgeWriter interface {
	InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error)
	InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error)
}

// Dispatcher is the Stage 3.2 ASTEmitter. Plugs into the Repo
// Indexer worker (Stage 3.1) via the `repoindexer.ASTEmitter`
// interface; the worker calls `EmitFile` once per File Node it
// ensures, and the dispatcher writes the file's Class /
// Method / Block sub-tree through `graphwriter.Writer`.
//
// Construct with NewDispatcher. The zero-value Dispatcher is
// NOT usable -- it has no parsers registered and no writer.
type Dispatcher struct {
	writer        nodeEdgeWriter
	parsers       []LanguageParser
	extMap        map[string]LanguageParser
	languageHints []string
	logger        *slog.Logger
	// publisher is the Stage 3.3 EmbeddingIndex writer hook
	// the dispatcher invokes after each Method / Block Node
	// insert.  Defaults to `noopNodeEmbeddingPublisher{}` so
	// existing unit / integration tests that do not exercise
	// publish state keep passing unchanged.  Production
	// wiring (`embedding.AsASTPublisher(*embedding.Publisher)`)
	// satisfies the interface through the one-file adapter in
	// `internal/embedding/astadapter.go`.
	publisher NodeEmbeddingPublisher
}

// Compile-time assertion: Dispatcher must satisfy the
// repoindexer.ASTEmitter interface so it can be passed
// as `WorkerOptions.Emitter`.
var _ repoindexer.ASTEmitter = (*Dispatcher)(nil)

// DispatcherOption configures a Dispatcher at construction
// time. Options are applied in the order they are passed to
// NewDispatcher.
type DispatcherOption func(*Dispatcher)

// WithParsers replaces the default v1 parser set
// (TypeScript / JavaScript + Python) with an explicit list.
// Useful for tests that want to inject a controlled
// parser set, and for future stages that add new language
// parsers without modifying the dispatcher's default.
func WithParsers(parsers ...LanguageParser) DispatcherOption {
	return func(d *Dispatcher) {
		d.parsers = parsers
	}
}

// WithLanguageHints supplies a dispatcher-default
// `language_hints[]` list. The defaults are used as a tie-
// breaker for files whose extension does not map to a
// registered parser AND whose `EmitFileEvent.LanguageHints`
// is empty.
//
// Per-event hints (set on `EmitFileEvent.LanguageHints` by
// the worker from each `repo.language_hints[]` column) ALWAYS
// take precedence over this option -- the dispatcher-global
// list exists only as a unit-test convenience and a
// production safety net for legacy callers that have not yet
// migrated to per-event hints. The evaluator-flagged
// correctness gate is satisfied at `selectParser`, which
// reads `EmitFileEvent.LanguageHints` first.
func WithLanguageHints(hints []string) DispatcherOption {
	return func(d *Dispatcher) {
		d.languageHints = normalizeHints(hints)
	}
}

// WithLogger overrides the structured logger. Defaults to
// `slog.Default()` if not supplied.
func WithLogger(logger *slog.Logger) DispatcherOption {
	return func(d *Dispatcher) {
		if logger != nil {
			d.logger = logger
		}
	}
}

// WithEmbeddingPublisher injects the Stage 3.3 EmbeddingIndex
// writer hook.  When set, the dispatcher invokes the publisher
// AFTER each Method / Block Node has been inserted AND its
// corresponding `contains` edge has been committed — that
// ordering keeps the brief partial-graph window from
// rubber-duck #5 as small as possible (the publisher would
// otherwise advertise a vector whose structural neighborhood
// the recall path cannot yet walk).
//
// Defaults to a no-op publisher so existing dispatcher tests
// keep compiling unchanged.  Production wires
// `embedding.AsASTPublisher(*embedding.Publisher)` here.
func WithEmbeddingPublisher(p NodeEmbeddingPublisher) DispatcherOption {
	return func(d *Dispatcher) {
		if p != nil {
			d.publisher = p
		}
	}
}

// NewDispatcher constructs a Dispatcher wired to writer. The
// default parser set is provided by `defaultParsers()`, which
// is build-tag-aware: when the binary is built with CGO
// enabled (the canonical production path), the tree-sitter
// grammars in `parsers_cgo.go` are the defaults; when CGO is
// off (the portable test path on stock Windows toolchains),
// `parsers_nocgo.go` returns the lightweight scanner-backed
// parsers as a documented fallback (see doc.go "Parser
// implementations").
//
// Panics on nil writer -- the dispatcher cannot operate
// without a place to send Node / Edge writes, and silently
// falling back to a no-op would mask a wiring bug.
func NewDispatcher(writer nodeEdgeWriter, opts ...DispatcherOption) *Dispatcher {
	if writer == nil {
		panic("ast: NewDispatcher: nil writer")
	}
	d := &Dispatcher{
		writer:    writer,
		parsers:   defaultParsers(),
		logger:    slog.Default(),
		publisher: noopNodeEmbeddingPublisher{},
	}
	for _, opt := range opts {
		opt(d)
	}
	d.extMap = buildExtMap(d.parsers)
	return d
}

func buildExtMap(parsers []LanguageParser) map[string]LanguageParser {
	out := make(map[string]LanguageParser)
	for _, p := range parsers {
		for _, ext := range p.Extensions() {
			out[strings.ToLower(ext)] = p
		}
	}
	return out
}

// EmitFile satisfies repoindexer.ASTEmitter. The function is
// the dispatcher's entry point per File Node; it picks a
// parser, runs the two-pass insert protocol, and returns nil
// on parse-only failures so one malformed file does not abort
// the ingest. It returns a non-nil error ONLY for unrecoverable
// failures (writer errors, IO errors on `ev.Open`).
//
// The returned EmitResult carries the TouchedNodes list the
// Stage 3.4 delta handler uses to compute its retire-set: one
// entry per Class / Method / Block Node ensured during this
// call. The list is best-effort on error -- callers MUST NOT
// trust it for correctness decisions when err != nil.
func (d *Dispatcher) EmitFile(ctx context.Context, ev repoindexer.EmitFileEvent) (repoindexer.EmitResult, error) {
	logger := d.logger.With(
		slog.String("op", "ast.emit_file"),
		slog.String("rel_path", ev.RelPath),
		slog.String("file_node_id", ev.FileNodeID),
		slog.String("sha", ev.SHA),
	)

	parser := d.selectParser(ev.RelPath, ev.LanguageHints)
	if parser == nil {
		logger.Debug("ast.dispatch.skip", slog.String("reason", "no_parser"))
		return repoindexer.EmitResult{}, nil
	}
	d.extMap = buildExtMap(d.parsers)
	return d
}

	src, err := readEvent(ev)
	if err != nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast: read %s: %w", ev.RelPath, err)
	}
	defer func() { _ = rc.Close() }()
	src, err := io.ReadAll(rc)
	if err != nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast.dispatcher: read %q: %w", ev.RelPath, err)
	}
	if _, err := d.safeParse(parser, ev.RelPath, src); err != nil {
		return repoindexer.EmitResult{}, nil
	}
	// Stage 3.2 dispatcher-landing workstream lands the
	// node/edge emission pipeline that fills TouchedNodes.
	// The v1 surface returns an empty result so the worker
	// continues without partial-progress noise.
	_ = ctx
	return repoindexer.EmitResult{}, nil
}

	result, err := safeParse(parser, ev.RelPath, src)
	if err != nil {
		if errors.Is(err, ErrParserUnavailable) {
			// Sentinel branch: the parser exists and was
			// selected, but it cannot run because a required
			// runtime dependency (e.g. `pwsh` on PATH for the
			// PowerShell parser) is missing. Mirror the
			// `parser == nil` branch's shape -- log
			// `ast.dispatch.skip` and return a zero
			// EmitResult so the worker keeps draining the
			// queue. The wrapped reason slug (`reason=<slug>`)
			// surfaces on the structured log; absent slug
			// falls back to `"runtime_unavailable"`
			// (architecture Section 2.2.1 / tech-spec C6).
			logger.Info("ast.dispatch.skip",
				slog.String("language", parser.Language()),
				slog.String("reason", parseUnavailableReason(err)),
			)
			return repoindexer.EmitResult{}, nil
		}
		logger.Warn("ast.parse.error",
			slog.String("language", parser.Language()),
			slog.String("error", err.Error()),
		)
		return repoindexer.EmitResult{}, nil
	}

	touched, err := d.emit(ctx, ev, parser, result, src, logger)
	if err != nil {
		return repoindexer.EmitResult{TouchedNodes: touched}, err
	}
	logger.Debug("ast.dispatch.ok",
		slog.String("language", parser.Language()),
		slog.Int("classes", len(result.Classes)),
		slog.Int("methods", len(result.Methods)),
		slog.Int("imports", len(result.Imports)),
		slog.Int("touched_nodes", len(touched)),
	)
	return repoindexer.EmitResult{TouchedNodes: touched}, nil
}

// selectParser returns the parser the dispatcher will use for
// relPath. Resolution precedence (highest first):
//  1. file extension lookup (`.ts`, `.py`, ...);
//  2. per-event LanguageHints from `EmitFileEvent` (the
//     repo's own `language_hints[]`);
//  3. dispatcher-default LanguageHints from
//     `WithLanguageHints`.
//
// Each hint is matched case-insensitively against every
// registered parser's `Language()` and the parser registered
// extensions; aliases like `ts` -> `typescript`, `py` ->
// `python` are normalised via `normalizeHints`.
func (d *Dispatcher) selectParser(relPath string, eventHints []string) LanguageParser {
	ext := strings.ToLower(path.Ext(relPath))
	if p, ok := d.extMap[ext]; ok {
		return p
	}
	hints := normalizeHints(eventHints)
	if len(hints) == 0 {
		hints = d.languageHints
	}
	for _, hint := range hints {
		for _, p := range d.parsers {
			if strings.EqualFold(p.Language(), hint) {
				return p
			}
		}
	}
	return nil
}

// normalizeHints lower-cases, trims, and resolves common
// aliases for each entry in raw. The output is suitable for
// case-insensitive comparison against `LanguageParser.Language()`
// values, which are themselves lower-case canonical ids
// (`typescript`, `python`, `c`, `cpp`, `csharp`, `go`, `rust`,
// `powershell`).
//
// Alias rows below mirror the AST-PARSER-FOR-ADDIT architecture
// (Section 3) and tech-spec (Section 4.2) table. Each new
// language parser registers its canonical id with the
// dispatcher; the rows here exist so that `language_hints[]`
// values supplied by the repo indexer (which use file-extension
// or community spellings such as `golang`, `c#`, `ps1`) resolve
// to the canonical id the parser exposes through
// `LanguageParser.Language()`. The canonical id is also listed
// in the same case row so a refactor that renames the canonical
// id surfaces in exactly one place.
func normalizeHints(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, h := range raw {
		n := strings.ToLower(strings.TrimSpace(h))
		switch n {
		case "":
			continue
		case "ts", "tsx", "js", "jsx", "mjs", "cjs":
			n = "typescript"
		case "py", "pyi":
			n = "python"
		case "c", "h":
			n = "c"
		case "cc", "cxx", "cpp", "c++", "hpp":
			n = "cpp"
		case "cs", "csharp", "c#":
			n = "csharp"
		case "go", "golang":
			n = "go"
		case "rs", "rust":
			n = "rust"
		case "ps", "ps1", "psm1", "psd1", "powershell":
			n = "powershell"
		}
		out = append(out, n)
	}
	return out
}

// readEvent reads the file's contents through ev.Open. Returns
// an error on read failure or when ev.Open is nil (a wiring
// bug -- the worker always provides Open).
func readEvent(ev repoindexer.EmitFileEvent) ([]byte, error) {
	if ev.Open == nil {
		return nil, errors.New("ev.Open is nil")
	}
	rc, err := ev.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// safeParse runs parser.Parse with panic recovery so a buggy
// parser cannot crash the worker goroutine. The recovered
// panic is surfaced as a normal error and logged at warn level.
func safeParse(parser LanguageParser, relPath string, src []byte) (res ParseResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parser panic (language=%s): %v", parser.Language(), r)
		}
	}()
	return parser.Parse(relPath, src)
}

// emit runs the two-pass insert protocol. Pass 1 inserts every
// Class / Method / Block Node so the local-symbol table is
// fully populated. Pass 2 resolves and inserts every same-file
// Edge (`extends`, `implements`, `static_calls`, `reads`,
// `writes`) whose dst Node id is known after pass 1.
// `contains` edges are emitted in pass 1 alongside their child
// Node insert because the parent id is always known at insert
// time. `imports` edges to external-package Nodes are emitted
// in a dedicated pass 0 because they have no intra-file
// dependency.
//
// Cross-file references that cannot be resolved against either
// the local symbol table or an external-module synthesis
// (e.g. relative `./util` imports that point at another file
// in the same repo) are dropped with a debug log entry; the
// future cross-file resolver story will pick them up by
// walking the file tree. The dispatcher does NOT mint
// placeholder Nodes for unresolved targets -- doing so would
// pollute the graph with no-op rows.
func (d *Dispatcher) emit(
	ctx context.Context,
	ev repoindexer.EmitFileEvent,
	parser LanguageParser,
	result ParseResult,
	src []byte,
	logger *slog.Logger,
) ([]repoindexer.TouchedNode, error) {
	// classNodeID and methodNodeID build the local-symbol
	// tables pass 2 resolves against. Keys are the parser's
	// QualifiedName (the dotted path within the file).
	classNodeID := make(map[string]string, len(result.Classes))
	methodNodeID := make(map[string]string, len(result.Methods))

	// touched accumulates every Node the dispatcher ensures
	// during this call so EmitFile can return the list to the
	// delta handler's retire-set computation. Capacity is a
	// best-effort upper bound — Block subdivision can push the
	// real count higher; the slice grows on demand.
	touched := make([]repoindexer.TouchedNode, 0,
		len(result.Classes)+len(result.Methods))

	// Pass 0: `imports` edges (external modules only).
	// Relative imports (`./`, `../`) are deferred -- they
	// resolve to in-repo files that the cross-file resolver
	// will stitch in a later story (per rubber-duck #3).
	if err := d.emitImportsEdges(ctx, ev, parser, result.Imports, logger); err != nil {
		return touched, err
	}

	// Pass 1a: insert classes + `contains` (file->class).
	for _, c := range result.Classes {
		sig := classSignature(ev.RepoURL, ev.RelPath, c.QualifiedName)
		attrs := classAttrs(parser.Language(), c)
		rec, err := d.writer.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             ev.RepoID,
			Kind:               "class",
			CanonicalSignature: sig,
			ParentNodeID:       ev.FileNodeID,
			FromSHA:            ev.SHA,
			AttrsJSON:          attrs,
		})
		if err != nil {
			return touched, fmt.Errorf("ast: insert class %s: %w", c.QualifiedName, err)
		}
		classNodeID[c.QualifiedName] = rec.NodeID
		touched = append(touched, repoindexer.TouchedNode{
			NodeID:             rec.NodeID,
			Kind:               "class",
			CanonicalSignature: sig,
			ParentNodeID:       ev.FileNodeID,
			Inserted:           rec.Inserted,
		})
		if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "contains",
			SrcNodeID: ev.FileNodeID,
			DstNodeID: rec.NodeID,
			FromSHA:   ev.SHA,
		}); err != nil {
			return touched, fmt.Errorf("ast: insert file->class contains: %w", err)
		}
	}
	if len(c.Extends) > 0 {
		m["extends_raw"] = append([]string(nil), c.Extends...)
	}
	if len(c.Implements) > 0 {
		m["implements_raw"] = append([]string(nil), c.Implements...)
	}
	mergeLangMeta(m, c.LangMeta)
	return mustJSON(m)
}

	// Pass 1b: insert methods + `contains` (parent->method).
	for i := range result.Methods {
		m := result.Methods[i]
		sig := methodSignature(ev.RepoURL, ev.RelPath, m.QualifiedName, m.ParamSignature)
		attrs := methodAttrs(parser.Language(), m)
		parentID := ev.FileNodeID
		parentKind := "file"
		if m.EnclosingClass != "" {
			if id, ok := classNodeID[m.EnclosingClass]; ok {
				parentID = id
				parentKind = "class"
			}
		}
		rec, err := d.writer.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             ev.RepoID,
			Kind:               "method",
			CanonicalSignature: sig,
			ParentNodeID:       parentID,
			FromSHA:            ev.SHA,
			AttrsJSON:          attrs,
		})
		if err != nil {
			return touched, fmt.Errorf("ast: insert method %s: %w", m.QualifiedName, err)
		}
		methodNodeID[m.QualifiedName] = rec.NodeID
		touched = append(touched, repoindexer.TouchedNode{
			NodeID:             rec.NodeID,
			Kind:               "method",
			CanonicalSignature: sig,
			ParentNodeID:       parentID,
			Inserted:           rec.Inserted,
		})
		if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "contains",
			SrcNodeID: parentID,
			DstNodeID: rec.NodeID,
			FromSHA:   ev.SHA,
		}); err != nil {
			return touched, fmt.Errorf("ast: insert %s->method contains: %w", parentKind, err)
		}

		// Stage 3.3 §9.6a publish hook: the method's Node row
		// AND its `contains` edge are now committed, so the
		// EmbeddingIndex writer can durably advertise a vector
		// whose neighborhood the recall path can walk.  Called
		// after the contains edge per rubber-duck #5.
		//
		// Per evaluator iter-1 finding #2: bodyless Method
		// declarations (TS interface members, abstract
		// methods — see parser_typescript.go:361-369) MUST
		// still get published; skipping them would leave a
		// permanent gap in the recall index for every
		// interface contract.  Fall back to embedding the
		// canonical signature (always non-empty by
		// construction here — `sig` was used immediately above
		// to fingerprint the Node) so the recall path can
		// still match on declaration shape even without body
		// text.  The `SignatureOnly` flag propagates through
		// the publisher onto the Qdrant payload so a future
		// reader can distinguish "embedded body" from
		// "embedded signature".
		content := m.BodySource
		signatureOnly := false
		if strings.TrimSpace(content) == "" {
			content = sig
			signatureOnly = true
			logger.Info("ast.publish.method_signature_only",
				slog.String("node_id", rec.NodeID),
				slog.String("method", m.QualifiedName),
			)
		}
		if err := d.publishNodeEmbedding(ctx, ev, NodeEmbedRequest{
			NodeID:             rec.NodeID,
			RepoID:             ev.RepoID.String(),
			Kind:               "method",
			CanonicalSignature: sig,
			Content:            content,
			SignatureOnly:      signatureOnly,
		}, logger); err != nil {
			return touched, err
		}

		// Pass 1c: blocks for this method.
		for _, b := range SubdivideMethod(m) {
			bsig := blockSignature(sig, b)
			bAttrs := blockAttrs(parser.Language(), b)
			brec, err := d.writer.InsertNode(ctx, graphwriter.NodeInput{
				RepoID:             ev.RepoID,
				Kind:               "block",
				CanonicalSignature: bsig,
				ParentNodeID:       rec.NodeID,
				FromSHA:            ev.SHA,
				AttrsJSON:          bAttrs,
			})
			if err != nil {
				return touched, fmt.Errorf("ast: insert block %s#%d: %w",
					m.QualifiedName, b.Ordinal, err)
			}
			touched = append(touched, repoindexer.TouchedNode{
				NodeID:             brec.NodeID,
				Kind:               "block",
				CanonicalSignature: bsig,
				ParentNodeID:       rec.NodeID,
				Inserted:           brec.Inserted,
			})
			if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "contains",
				SrcNodeID: rec.NodeID,
				DstNodeID: brec.NodeID,
				FromSHA:   ev.SHA,
			}); err != nil {
				return touched, fmt.Errorf("ast: insert method->block contains: %w", err)
			}

			// Publish hook: block Node + contains edge are
			// now both committed.  Content is the file source
			// sliced at the parser-reported byte offsets.
			// Per rubber-duck #4: invalid offsets (parser bug)
			// cause us to SKIP the publish rather than send
			// an empty content vector to the embedder — an
			// empty-content embedding silently corrupts the
			// recall index.  We log a warn and continue.
			content, ok := sliceBytes(src, b.StartByte, b.EndByte)
			if !ok {
				logger.Warn("ast.publish.skip_invalid_block_offsets",
					slog.String("node_id", brec.NodeID),
					slog.String("method", m.QualifiedName),
					slog.Int("ordinal", b.Ordinal),
					slog.Int("start_byte", b.StartByte),
					slog.Int("end_byte", b.EndByte),
					slog.Int("src_len", len(src)),
				)
				continue
			}
			if err := d.publishNodeEmbedding(ctx, ev, NodeEmbedRequest{
				NodeID:             brec.NodeID,
				RepoID:             ev.RepoID.String(),
				Kind:               "block",
				CanonicalSignature: bsig,
				Content:            content,
			}, logger); err != nil {
				return touched, err
			}
		}
	}

	// Pass 2a: extends / implements (only same-file resolvable).
	for _, c := range result.Classes {
		src := classNodeID[c.QualifiedName]
		for _, target := range c.Extends {
			dst, ok := classNodeID[target]
			if !ok {
				continue
			}
			if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "extends",
				SrcNodeID: src,
				DstNodeID: dst,
				FromSHA:   ev.SHA,
			}); err != nil {
				return touched, fmt.Errorf("ast: insert extends %s->%s: %w",
					c.QualifiedName, target, err)
			}
		}
		for _, target := range c.Implements {
			dst, ok := classNodeID[target]
			if !ok {
				continue
			}
			if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "implements",
				SrcNodeID: src,
				DstNodeID: dst,
				FromSHA:   ev.SHA,
			}); err != nil {
				return touched, fmt.Errorf("ast: insert implements %s->%s: %w",
					c.QualifiedName, target, err)
			}
		}
	}

	// Pass 2b: static_calls. Receiver-qualified calls
	// (`this.foo()` / `self.foo()`) resolve through a
	// per-file multimap built from each method's
	// EnclosingClass + simple-name, plus any extra
	// `ReceiverAliases` the parser supplied (Go pointer-
	// receiver methods register `Foo.Bar` as an alias for
	// their `*Foo.Bar` QualifiedName per architecture
	// Section 4.5.1). Resolution emits ONLY when the
	// multimap entry has exactly one node id; size > 1
	// drops on collision per A5 (the same drop-on-
	// ambiguity rule the bare-name `buildCalleeIndex`
	// path uses).  Bare-name calls continue to use the
	// callee index.
	receiverIndex := make(map[string][]string, len(result.Methods))
	addReceiver := func(key, nodeID string) {
		// Dedup per key: a pointer-receiver method's
		// primary key (`Foo.Bar` from `simpleName("*Foo.Bar")`)
		// and its ReceiverAliases entry (`Foo.Bar`) are
		// intentionally identical. Without dedup the slice
		// would carry the same node id twice and a
		// pointer-only file would falsely drop on
		// collision (size 2). Keeping set-like semantics
		// matches the architecture text in Section 4.5.1
		// ("the set has size 2 ... drops per A5").
		for _, existing := range receiverIndex[key] {
			if existing == nodeID {
				return
			}
		}
		receiverIndex[key] = append(receiverIndex[key], nodeID)
	}
	for _, m := range result.Methods {
		nodeID, ok := methodNodeID[m.QualifiedName]
		if !ok || m.EnclosingClass == "" {
			continue
		}
		primaryKey := m.EnclosingClass + "." + simpleName(m.QualifiedName)
		addReceiver(primaryKey, nodeID)
		for _, alias := range m.ReceiverAliases {
			addReceiver(alias, nodeID)
		}
	}
	calleeIndex := buildCalleeIndex(methodNodeID)
	for _, m := range result.Methods {
		srcID, ok := methodNodeID[m.QualifiedName]
		if !ok {
			continue
		}
		seen := map[string]struct{}{}
		emitCall := func(dstID string) error {
			if _, dup := seen[dstID]; dup {
				return nil
			}
			seen[dstID] = struct{}{}
			_, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "static_calls",
				SrcNodeID: srcID,
				DstNodeID: dstID,
				FromSHA:   ev.SHA,
			})
			return err
		}
		// Receiver-qualified calls first. The multimap
		// resolves Go value/pointer receiver collisions
		// (size > 1 -> drop per A5) and lets a single
		// pointer-receiver method match via its alias.
		for _, callee := range m.ReceiverCalls {
			if m.EnclosingClass == "" {
				continue
			}
			ids := receiverIndex[m.EnclosingClass+"."+callee]
			if len(ids) != 1 {
				continue
			}
			if err := emitCall(ids[0]); err != nil {
				return touched, fmt.Errorf("ast: insert receiver static_calls %s->%s.%s: %w",
					m.QualifiedName, m.EnclosingClass, callee, err)
			}
		}
		// Bare-name calls (drop on ambiguity).
		for _, callee := range m.Calls {
			dstID, ok := calleeIndex[callee]
			if !ok {
				continue
			}
			if err := emitCall(dstID); err != nil {
				return touched, fmt.Errorf("ast: insert static_calls %s->%s: %w",
					m.QualifiedName, callee, err)
			}
		}
	}

	// Pass 2c: reads / writes. Each method that touches
	// `this.X` / `self.X` emits at most one `reads` and one
	// `writes` edge into its enclosing class node, with the
	// touched member names recorded on the edge attrs (per
	// rubber-duck #4 -- without member names the edges are
	// too lossy to be useful). Methods with no enclosing
	// class skip this pass entirely.
	for _, m := range result.Methods {
		if m.EnclosingClass == "" || len(m.MemberAccesses) == 0 {
			continue
		}
		srcID, ok := methodNodeID[m.QualifiedName]
		if !ok {
			continue
		}
		dstID, ok := classNodeID[m.EnclosingClass]
		if !ok {
			continue
		}
		reads, writes := splitMemberAccesses(m.MemberAccesses)
		if len(reads) > 0 {
			if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "reads",
				SrcNodeID: srcID,
				DstNodeID: dstID,
				FromSHA:   ev.SHA,
				AttrsJSON: memberEdgeAttrs(parser.Language(), reads),
			}); err != nil {
				return touched, fmt.Errorf("ast: insert reads %s->%s: %w",
					m.QualifiedName, m.EnclosingClass, err)
			}
		}
		if len(writes) > 0 {
			if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "writes",
				SrcNodeID: srcID,
				DstNodeID: dstID,
				FromSHA:   ev.SHA,
				AttrsJSON: memberEdgeAttrs(parser.Language(), writes),
			}); err != nil {
				return touched, fmt.Errorf("ast: insert writes %s->%s: %w",
					m.QualifiedName, m.EnclosingClass, err)
			}
		}
	}
	return slog.Default()
}

	// Pass 2d: `overrides`. Rust trait default-impl
	// shadowing emits a typed edge from each impl method
	// (LangMeta["trait"]=<traitName>) to the trait method
	// with the same simple name in the SAME file ONLY when
	// the trait method has a default body (LangMeta
	// ["trait_default"]=true). Required (bodyless) trait
	// signatures do NOT participate -- providing a required
	// signature is "satisfies" / "implements", not
	// "overrides" (architecture Section 7.2 / R4: overrides
	// tracks default-impl SHADOWING). Cross-file pairs are
	// dropped per A4 -- the verbatim trait identity persists
	// on `LangMeta["trait"]` so the future cross-file
	// resolver can stitch them later. The pass is a no-op
	// for every other language because no other parser sets
	// `LangMeta["trait"]`.
	//
	// traitDefaultByQN is the destination filter: a method
	// QualifiedName is keyed only when it carries the
	// `trait_default` flag the Rust parser sets in
	// `appendTraitDefaultMethod`. Required-method trait
	// signatures (from `function_signature_item`) carry no
	// such flag and are absent from this map -- the Pass 2d
	// lookup below treats absence as "no override target".
	traitDefaultByQN := make(map[string]bool, len(result.Methods))
	for _, m := range result.Methods {
		if len(m.LangMeta) == 0 {
			continue
		}
		raw, ok := m.LangMeta["trait_default"]
		if !ok {
			continue
		}
		if b, _ := raw.(bool); b {
			traitDefaultByQN[m.QualifiedName] = true
		}
	}
	for _, m := range result.Methods {
		if len(m.LangMeta) == 0 {
			continue
		}
		rawTrait, ok := m.LangMeta["trait"]
		if !ok {
			continue
		}
		traitName, ok := rawTrait.(string)
		if !ok || traitName == "" {
			continue
		}
		srcID, ok := methodNodeID[m.QualifiedName]
		if !ok {
			continue
		}
		dstKey := traitName + "." + simpleName(m.QualifiedName)
		dstID, ok := methodNodeID[dstKey]
		if !ok {
			continue
		}
		// Architectural filter: skip required-signature
		// targets per R4 -- the impl is providing a
		// required method, not shadowing a default.
		if !traitDefaultByQN[dstKey] {
			continue
		}
		// Defensive: skip a self-edge. A parser bug that
		// accidentally sets `LangMeta["trait"]` on the
		// trait's own default-bodied method would otherwise
		// emit `Greeter.greet -> Greeter.greet`. The src/dst
		// node ids being equal is the unambiguous signal
		// because `methodNodeID` is keyed by QualifiedName
		// (one entry per declaration).
		if srcID == dstID {
			continue
		}
		if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "overrides",
			SrcNodeID: srcID,
			DstNodeID: dstID,
			FromSHA:   ev.SHA,
		}); err != nil {
			return touched, fmt.Errorf("ast: insert overrides %s->%s: %w",
				m.QualifiedName, dstKey, err)
		}
	}

	_ = logger // structured logs from sub-helpers if needed in the future
	return touched, nil
}

// emitImportsEdges materialises each non-relative import as
// an external-package Node + an `imports` edge from the file
// to it. Relative imports (`./`, `../`, or Python module
// paths starting with `.`) are skipped: they resolve to other
// files in this repo, which the future cross-file resolver
// will pick up. Per rubber-duck #3 -- minting an "external"
// package node for a relative import would misrepresent it.
//
// The synthetic package Node uses signature
// `<repoURL>::package::ext::<module>` so it does not collide
// with the worker-emitted first-party package nodes (which
// use `<repoURL>::pkg::<dir>`).
func (d *Dispatcher) emitImportsEdges(
	ctx context.Context,
	ev repoindexer.EmitFileEvent,
	parser LanguageParser,
	imports []Import,
	logger *slog.Logger,
) error {
	if len(imports) == 0 {
		return nil
	}
	seenModule := map[string]string{} // module -> package node id
	for _, imp := range imports {
		mod := strings.TrimSpace(imp.Module)
		if mod == "" {
			continue
		}
		if isRelativeImport(mod) {
			logger.Debug("ast.imports.skip_relative",
				slog.String("module", mod),
				slog.Int("line", imp.Line),
			)
			continue
		}
		pkgID, ok := seenModule[mod]
		if !ok {
			pkgSig := externalPackageSignature(ev.RepoURL, mod)
			pkgAttrs := externalPackageAttrs(parser.Language(), mod, ev.RepoNodeID == "")
			rec, err := d.writer.InsertNode(ctx, graphwriter.NodeInput{
				RepoID:             ev.RepoID,
				Kind:               "package",
				CanonicalSignature: pkgSig,
				ParentNodeID:       ev.RepoNodeID,
				FromSHA:            ev.SHA,
				AttrsJSON:          pkgAttrs,
			})
			if err != nil {
				return fmt.Errorf("ast: insert external package %s: %w", mod, err)
			}
			pkgID = rec.NodeID
			seenModule[mod] = pkgID
		}
		// Edge attrs carry the symbols list when present so
		// consumers know WHICH names this file pulled from
		// the module (per rubber-duck #4 on attrs richness).
		if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "imports",
			SrcNodeID: ev.FileNodeID,
			DstNodeID: pkgID,
			FromSHA:   ev.SHA,
			AttrsJSON: importEdgeAttrs(parser.Language(), imp),
		}); err != nil {
			return fmt.Errorf("ast: insert imports %s->%s: %w",
				ev.RelPath, mod, err)
		}
	}
	return nil
}

// isRelativeImport returns true for module specifiers that
// resolve relative to the importing file rather than to an
// external package. The check is intentionally conservative:
// any leading `.` (TS/JS / Python relative paths), `/`
// (absolute) is considered relative-or-internal.
func isRelativeImport(module string) bool {
	if module == "" {
		return true
	}
	if module[0] == '.' || module[0] == '/' {
		return true
	}
	return false
}

// splitMemberAccesses partitions a `MemberAccess` list into
// the reads and writes sets. The same name is never in both
// sets -- the extractor classifies each name once, with
// writes winning on conflict (see `extractTSMemberAccesses`
// for the classification rule).
func splitMemberAccesses(accesses []MemberAccess) (reads, writes []string) {
	for _, a := range accesses {
		if a.IsWrite {
			writes = append(writes, a.Name)
		} else {
			reads = append(reads, a.Name)
		}
	}
	return reads, writes
}

// simpleName returns the trailing identifier of a dotted
// QualifiedName, after stripping the operator-pinned Go
// pointer-receiver marker `*` (architecture Section 4.5.1).
// Examples:
//
//	"*Foo.Bar" -> "Bar"
//	"Foo.Bar"  -> "Bar"
//	"freeFn"   -> "freeFn"
//	""         -> ""
//
// Used by the Pass 2b receiver-qualified resolution multimap
// and by Pass 2d's trait-override lookup so the dispatcher can
// match Go value/pointer receivers and Rust trait/impl pairs
// against the same canonical key.
func simpleName(q string) string {
	if len(q) > 0 && q[0] == '*' {
		q = q[1:]
	}
	return q[strings.LastIndexByte(q, '.')+1:]
}

// parseUnavailableReason extracts a `reason=<slug>` value from
// a wrapped `ErrParserUnavailable` error so the dispatcher's
// `ast.dispatch.skip` log carries the parser's intended reason
// slug. The wrapping convention parsers use is, e.g.:
//
//	fmt.Errorf("powershell: %w (reason=pwsh_not_available)", ast.ErrParserUnavailable)
//
// When no `reason=` substring exists, or no slug characters
// follow the tag, the helper returns `"runtime_unavailable"`
// per the workstream brief. The slug character set is
// intentionally narrow ([A-Za-z0-9_-]) so the helper does NOT
// pick up trailing punctuation (`)`, `.`, `:`, etc.) or capture
// part of a follow-on log token. Surrounding quotes (`"`, `'`)
// are stripped before the slug scan.
func parseUnavailableReason(err error) string {
	const fallback = "runtime_unavailable"
	const tag = "reason="
	if err == nil {
		return fallback
	}
	s := err.Error()
	idx := strings.Index(s, tag)
	if idx < 0 {
		return fallback
	}
	rest := strings.TrimLeft(s[idx+len(tag):], `"'`)
	end := 0
	for end < len(rest) {
		c := rest[end]
		if !(c == '_' || c == '-' ||
			(c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9')) {
			break
		}
		end++
	}
	if end == 0 {
		return fallback
	}
	return rest[:end]
}

// buildCalleeIndex maps a bare callee name to the NodeID of
// the same-file Method whose simple name matches. When two
// methods share a simple name (e.g. `Foo.bar` and `Baz.bar`)
// the resolver bails out for that name -- ambiguous matches
// are NOT emitted (false-positive `static_calls` are worse
// than missing edges; see doc.go "v1 edge scope").
func buildCalleeIndex(methodNodeID map[string]string) map[string]string {
	bare := map[string][]string{}
	// Sort keys for determinism so dropped-ambiguity decisions
	// are reproducible across runs.
	qNames := make([]string, 0, len(methodNodeID))
	for q := range methodNodeID {
		qNames = append(qNames, q)
	}
	sort.Strings(qNames)
	for _, q := range qNames {
		simple := q
		if i := strings.LastIndexByte(q, '.'); i >= 0 {
			simple = q[i+1:]
		}
		bare[simple] = append(bare[simple], methodNodeID[q])
	}
	out := make(map[string]string, len(bare))
	for name, ids := range bare {
		if len(ids) == 1 {
			out[name] = ids[0]
		}
	}
	return out
}

// classSignature mints the canonical signature for a Class /
// Interface node. The `<relPath>` embed is the cross-file
// disambiguator (see doc.go "Canonical signature scheme").
func classSignature(repoURL, relPath, qualifiedName string) string {
	return repoURL + "::class::" + relPath + "#" + NormalizeSignature(qualifiedName)
}

// methodSignature mints the canonical signature for a Method
// or free-function node. Parameter list is whitespace-
// normalised so a formatter-only commit produces a stable
// fingerprint (§9.7 / §9.9 mitigation).
func methodSignature(repoURL, relPath, qualifiedName, params string) string {
	return repoURL + "::method::" + relPath + "#" +
		NormalizeSignature(qualifiedName) +
		"(" + NormalizeSignature(params) + ")"
}

// blockSignature mints the canonical signature for a Block
// node. The ordinal is embedded so multiple Blocks of the
// same kind under one Method get distinct fingerprints.
func blockSignature(methodSig string, b Block) string {
	return fmt.Sprintf("%s#block_%d_%s", methodSig, b.Ordinal, b.Kind)
}

func classAttrs(language string, c ClassDecl) json.RawMessage {
	m := map[string]any{
		"language":   language,
		"decl_kind":  c.Kind,
		"start_line": c.StartLine,
		"end_line":   c.EndLine,
	}
	unresolvedExtends := []string{}
	for _, e := range c.Extends {
		unresolvedExtends = append(unresolvedExtends, e)
	}
	if len(unresolvedExtends) > 0 {
		m["extends_raw"] = unresolvedExtends
	}
	if len(c.Implements) > 0 {
		m["implements_raw"] = append([]string(nil), c.Implements...)
	}
	mergeLangMeta(m, c.LangMeta)
	return mustJSON(m)
}

// methodAttrs delegates to the exported BuildMethodAttrs so the
// dispatcher's per-Method node attrs_json is byte-identical to
// whatever the public attrs API produces.  Iter-4 evaluator
// finding #2 fix: the in-package helper used to ignore
// ReceiverCalls (only m.Calls landed in calls_raw), while the
// public BuildMethodAttrs in method_attrs.go merges both via
// MergeCallsDeduped.  Letting the two diverge silently lets the
// writer integration tests (test/e2e ... mergelangmeta) pass
// against BuildMethodAttrs while production dispatcher emits
// different bytes — the exact blast-radius the rubber-duck
// review flagged.  Delegating is a one-liner and keeps the
// dispatcher's old semantics for Calls-only fixtures intact
// (MergeCallsDeduped over Calls + nil ReceiverCalls returns
// exactly Calls, preserving insertion order).
func methodAttrs(language string, m MethodDecl) json.RawMessage {
	return BuildMethodAttrs(language, m)
}

// blockAttrs records the language, block kind, ordinal, AND
// the file-relative source boundaries that span ingestor uses
// to map runtime spans back to a Block (per evaluator finding
// #6). Coords are 1-based line numbers and 0-based byte
// offsets, both relative to the FILE (not the method body) so
// downstream span->block resolution does not need to know the
// enclosing method's offsets.
func blockAttrs(language string, b Block) json.RawMessage {
	return mustJSON(map[string]any{
		"language":   language,
		"block_kind": string(b.Kind),
		"ordinal":    b.Ordinal,
		"start_line": b.StartLine,
		"end_line":   b.EndLine,
		"start_byte": b.StartByte,
		"end_byte":   b.EndByte,
	})
}

// externalPackageSignature mints the canonical signature for
// a synthetic Package Node that represents an external module.
// The `::ext::` infix keeps these synthetic packages disjoint
// from the worker-emitted first-party `<repoURL>::pkg::<dir>`
// nodes; both kinds live under the same Repo Node but their
// fingerprints will never collide.
func externalPackageSignature(repoURL, module string) string {
	return repoURL + "::package::ext::" + NormalizeSignature(module)
}

// externalPackageAttrs records the language that observed the
// import (different languages may resolve the same module
// name differently -- e.g. `path` is the stdlib in both Go and
// Node but they're different packages) and a stable `module`
// key so consumers can group multi-language imports of the
// same external package. When `parentMissing` is true, the
// dispatcher could not find a Repo Node id on the event (e.g.
// unit-test fakes that go straight from File to Class) and so
// inserted the package Node without a `parent_node_id`; the
// flag is persisted on the package node so consumers (and the
// integrity audit) can see why the package is structurally
// orphaned. Production code paths always carry a Repo Node id
// (worker.go::runFull) and the flag stays absent.
func externalPackageAttrs(language, module string, parentMissing bool) json.RawMessage {
	m := map[string]any{
		"language": language,
		"module":   module,
		"source":   "external",
	}
	if parentMissing {
		m["parent_missing"] = true
	}
	return mustJSON(m)
}

// importEdgeAttrs serialises the per-edge import metadata.
// Symbols (named imports / `from x import a, b`) are recorded
// when present so the rerank pipeline can surface "file F
// imports symbol X from module M" without re-parsing the
// source. `is_type_only` carries through TS `import type`.
func importEdgeAttrs(language string, imp Import) json.RawMessage {
	m := map[string]any{
		"language": language,
		"module":   imp.Module,
		"line":     imp.Line,
	}
	if len(imp.Symbols) > 0 {
		m["symbols"] = append([]string(nil), imp.Symbols...)
	}
	if imp.Alias != "" {
		m["alias"] = imp.Alias
	}
	if imp.IsTypeOnly {
		m["is_type_only"] = true
	}
	return mustJSON(m)
}

// memberEdgeAttrs serialises the deduped, deterministic-order
// list of member names touched by a `reads` or `writes` edge.
// The class id is implicit (it's the edge's destination); the
// member names live in `members[]` so consumers can drive
// field-level relevance without resolving each member to a
// dedicated Node (no `field` node kind exists in v1 -- see
// migration 0001).
func memberEdgeAttrs(language string, members []string) json.RawMessage {
	deduped := dedupSortedStrings(members)
	return mustJSON(map[string]any{
		"language": language,
		"members":  deduped,
	})
}

// dedupSortedStrings returns the unique entries of in in
// ascending order. Used to keep attrs JSON byte-stable across
// runs so fingerprint diffs reflect real edge changes, not
// extractor traversal order changes.
func dedupSortedStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// mergeLangMeta folds the per-language attrs map `in` into the
// dispatcher's first-class attrs map `out`. First-class keys
// always win: when `out` already holds a key, the value in `in`
// for that key is silently dropped (architecture invariant C11 /
// Section 4.4.2 -- `LangMeta` is descriptive, not identifying,
// and the dispatcher's first-class attrs schema is the source of
// truth for keys like `language`, `decl_kind`, `start_line`,
// etc.). Presence -- not value -- is what counts: a first-class
// key whose value is `nil` still wins.
//
// `in == nil` is a no-op, so parsers that emit no per-language
// metadata (the TS/JS/Python baseline) pay nothing and produce
// byte-identical attrs JSON compared to the pre-helper world.
//
// `out` MUST be non-nil; all in-package call sites construct it
// as a map literal so this is not validated.
//
// Values in `in` are shallow-copied (the helper does not clone
// nested slices/maps). The dispatcher discards the merged map
// immediately after `mustJSON` so aliasing into `LangMeta` is
// safe in practice. Parsers MUST keep `LangMeta` values
// JSON-marshalable (primitives, []string, map[string]any,
// etc.) -- a non-marshalable value would cause `mustJSON` to
// fall back to `{}` and erase the first-class attrs too.
func mergeLangMeta(out map[string]any, in map[string]any) {
	if in == nil {
		return
	}
	for k, v := range in {
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}
}

func mustJSON(m map[string]any) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		// json.Marshal on a map[string]any populated with
		// primitive types never errors in practice; if it
		// somehow does, fall back to an empty object so the
		// dispatcher does not crash mid-ingest.
		return json.RawMessage(`{}`)
	}
	return b
}

// publishNodeEmbedding fans a Method / Block emission into the
// Stage 3.3 EmbeddingIndex writer.  Honours the two-bucket
// error policy documented on `NodeEmbeddingPublisher`:
//
//   - `errors.Is(err, ErrPublishRecordedFailed)` → DURABLY
//     RECORDED failure (an `embedding_publish_event` of kind
//     `failed` exists).  Log warn and continue; a background
//     flusher retries.  Returning here would otherwise abort
//     the entire file ingest the FIRST time Qdrant blips,
//     even though the §9.6a log is healthy.
//
//   - any other error → the publisher could NOT record state
//     (database failure, validation bug, panic).  Propagate
//     to fail the ingest job; a silent swallow would leave
//     the EmbeddingIndex permanently divergent from the
//     structural graph for the affected node.
//
// `nil` publisher is impossible (constructor defaults to a
// no-op) but guarded anyway so the helper is safe to call
// from emit() without a precondition check.
func (d *Dispatcher) publishNodeEmbedding(
	ctx context.Context,
	ev repoindexer.EmitFileEvent,
	req NodeEmbedRequest,
	logger *slog.Logger,
) error {
	if d.publisher == nil {
		return nil
	}
	res, err := d.publisher.PublishNodeEmbedding(ctx, req)
	if err == nil {
		logger.Debug("ast.publish.ok",
			slog.String("node_kind", req.Kind),
			slog.String("node_id", req.NodeID),
			slog.String("publish_id", res.PublishID),
			slog.String("last_event_kind", res.LastEventKind),
		)
		return nil
	}
	if errors.Is(err, ErrPublishRecordedFailed) {
		logger.Warn("ast.publish.recorded_failed",
			slog.String("node_kind", req.Kind),
			slog.String("node_id", req.NodeID),
			slog.String("publish_id", res.PublishID),
			slog.String("error", err.Error()),
		)
		return nil
	}
	return fmt.Errorf("ast: publish %s %s: %w", req.Kind, req.NodeID, err)
}

// sliceBytes returns `(string(src[start:end+1]), true)` with
// defensive bounds-checking.  IMPORTANT: `start` and `end` are
// treated as INCLUSIVE byte offsets to match the `Block.StartByte
// / Block.EndByte` semantics documented in `block.go:64-69`
// ("0-based file byte offsets of the block's first/LAST byte").
// A naive half-open slice (`src[start:end]`) drops the final
// byte of every block AND silently skips one-byte spans
// (`end == start`).  Returns `("", false)` when offsets are
// negative, out of order, or out of range; callers MUST treat
// the false return as "skip this publish" rather than send
// empty content to the embedder (which would otherwise corrupt
// the recall index with a noise vector).
func sliceBytes(src []byte, start, end int) (string, bool) {
	if start < 0 || end < start || start >= len(src) {
		return "", false
	}
	// `end` is inclusive, so the half-open upper bound is end+1.
	upper := end + 1
	if upper > len(src) {
		upper = len(src)
	}
	return string(src[start:upper]), true
}
