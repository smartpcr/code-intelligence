// Package ast — canonical Stage 3.2 Dispatcher.
//
// This file implements the full V2 emission pipeline: it picks
// a `LanguageParser` by file extension (with per-event and
// dispatcher-global hint fallback), drives the two-pass insert
// protocol (Pass 1 inserts every Class/Method/Block Node; Pass
// 2 resolves and inserts the static edges that need the local-
// symbol table to be fully populated), and fans Method/Block
// emissions out to the optional NodeEmbeddingPublisher.
//
// The single-source-of-truth for the contract is `doc.go`; the
// dispatcher tests in `dispatcher_test.go`,
// `dispatcher_pass2bd_test.go`, and
// `dispatcher_embedding_test.go` pin every observable behaviour.
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
// connection. `*graphwriter.Writer` satisfies it by virtue of
// having `InsertNode` and `InsertEdge` methods with matching
// signatures.
type nodeEdgeWriter interface {
	InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error)
	InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error)
}

// Dispatcher is the Stage 3.2 ASTEmitter. Plugs into the Repo
// Indexer worker (Stage 3.1) via the `repoindexer.ASTEmitter`
// interface; the worker calls `EmitFile` once per File Node it
// ensures and the dispatcher writes the file's Class / Method
// / Block sub-tree through `graphwriter.Writer`.
//
// Construct with NewDispatcher. The zero-value Dispatcher is
// NOT usable — it has no parsers registered and no writer.
type Dispatcher struct {
	writer        nodeEdgeWriter
	parsers       []LanguageParser
	extMap        map[string]LanguageParser
	languageHints []string
	logger        *slog.Logger
	publisher     NodeEmbeddingPublisher
}

// Compile-time assertion: Dispatcher must satisfy the
// repoindexer.ASTEmitter interface so it can be passed as
// `WorkerOptions.Emitter`.
var _ repoindexer.ASTEmitter = (*Dispatcher)(nil)

// DispatcherOption configures a Dispatcher at construction
// time. Options are applied in the order they are passed to
// NewDispatcher.
type DispatcherOption func(*Dispatcher)

// WithParsers replaces the default parser set (returned by
// `defaultParsers()`) with an explicit list. Useful for tests
// that want to inject a controlled parser set, and for sibling
// per-language workstreams that drive a single parser through
// the dispatcher pipeline (e.g. the C# fixture test).
func WithParsers(parsers ...LanguageParser) DispatcherOption {
	return func(d *Dispatcher) {
		d.parsers = parsers
	}
}

// WithLanguageHints supplies a dispatcher-default
// `language_hints[]` list. The defaults are used as a
// tie-breaker for files whose extension does not map to a
// registered parser AND whose `EmitFileEvent.LanguageHints`
// is empty.
//
// Per-event hints (set on `EmitFileEvent.LanguageHints` by
// the worker from each `repo.language_hints[]` column) ALWAYS
// take precedence over this option — the dispatcher-global
// list exists as a unit-test convenience and a production
// safety net for legacy callers that have not yet migrated to
// per-event hints. Every entry is run through `normalizeHints`
// so callers can use friendly aliases (`cs`, `c#`, `golang`,
// `py`, etc.) and the dispatcher will canonicalise them.
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

// WithEmbeddingPublisher wires the optional embedding
// publisher that receives Method / Block emissions for
// downstream fan-out to the EmbeddingIndex writer. The
// default is `noopNodeEmbeddingPublisher{}` so existing
// callers that do not configure embeddings compile and run
// unchanged.
func WithEmbeddingPublisher(p NodeEmbeddingPublisher) DispatcherOption {
	return func(d *Dispatcher) {
		if p != nil {
			d.publisher = p
		}
	}
}

// NewDispatcher constructs a Dispatcher wired to writer. The
// default parser set is provided by `defaultParsers()`, which
// is build-tag-aware: CGO=on uses the tree-sitter grammars
// (TypeScript / Python / C# / C++) from `parsers_cgo.go`;
// CGO=off uses the stdlib-only scanners from
// `parsers_nocgo.go`.
//
// Panics on nil writer — the dispatcher cannot operate without
// a place to send Node / Edge writes, and silently falling
// back to a no-op would mask a wiring bug.
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

// buildExtMap is the constructor-time helper that flattens
// each parser's `Extensions()` into a single lookup table.
// Lower-cases every extension so the routing is
// case-insensitive (`.TS` and `.ts` route the same).
func buildExtMap(parsers []LanguageParser) map[string]LanguageParser {
	out := make(map[string]LanguageParser, len(parsers)*2)
	for _, p := range parsers {
		for _, ext := range p.Extensions() {
			out[strings.ToLower(ext)] = p
		}
	}
	return out
}

// EmitFile satisfies repoindexer.ASTEmitter. It picks a
// parser, runs the two-pass insert protocol, and returns the
// EmitResult naming every Node it ensured during the call.
// Returns a non-nil error ONLY for unrecoverable failures:
//
//   - IO errors from `ev.Open` / `io.ReadAll`.
//   - Writer errors (PostgreSQL down, schema mismatch).
//   - Non-recorded embedding publisher errors.
//
// Parser-only failures are swallowed (the worker continues
// draining its queue):
//
//   - `ErrParserUnavailable` → INFO-level `ast.dispatch.skip`
//     log with `reason=<slug>`; returns `(EmitResult{}, nil)`.
//   - Parser panics → ERROR-level `ast.parse.panic` log;
//     returns `(EmitResult{}, nil)`.
//   - Other parser errors → WARN-level `ast.parse.error`;
//     returns `(EmitResult{}, nil)`.
//   - Unknown extensions with no language-hint match → DEBUG
//     `ast.dispatch.skip{reason=no_parser}`; returns
//     `(EmitResult{}, nil)`.
func (d *Dispatcher) EmitFile(ctx context.Context, ev repoindexer.EmitFileEvent) (repoindexer.EmitResult, error) {
	logger := d.log().With(
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

	src, err := readEvent(ev)
	if err != nil {
		return repoindexer.EmitResult{}, fmt.Errorf("ast: read %s: %w", ev.RelPath, err)
	}

	result, err := d.safeParse(parser, ev.RelPath, src, logger)
	if err != nil {
		// safeParse has already logged the appropriate
		// classification (skip / panic / parse error). The
		// outer EmitFile contract is: swallow parser failures
		// so the worker drains its queue.
		return repoindexer.EmitResult{}, nil
	}

	emitted, err := d.emit(ctx, ev, parser, result, src, logger)
	if err != nil {
		return repoindexer.EmitResult{TouchedNodes: emitted}, err
	}
	logger.Debug("ast.dispatch.ok",
		slog.String("language", parser.Language()),
		slog.Int("classes", len(result.Classes)),
		slog.Int("methods", len(result.Methods)),
		slog.Int("imports", len(result.Imports)),
		slog.Int("touched", len(emitted)),
	)
	return repoindexer.EmitResult{TouchedNodes: emitted}, nil
}

// selectParser returns the parser the dispatcher will use for
// relPath. Resolution precedence (highest first):
//  1. file extension lookup (`.ts`, `.py`, `.cs`, ...);
//  2. per-event LanguageHints from `EmitFileEvent` (the
//     repo's own `language_hints[]`);
//  3. dispatcher-default LanguageHints from
//     `WithLanguageHints`.
//
// Each hint is matched case-insensitively against every
// registered parser's `Language()` value AFTER it has been
// canonicalised by `normalizeHints` (`ts` → `typescript`,
// `cs` / `c#` → `csharp`, `golang` → `go`, etc.).
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
// (`typescript`, `python`, `csharp`, `go`, `rust`, `cpp`,
// `c`, `powershell`).
//
// Alias table (architecture Section 3 / tech-spec Section 4.2):
//
//   - TypeScript / JavaScript: `ts`, `tsx`, `js`, `jsx`,
//     `mjs`, `cjs` → `typescript`.
//   - Python: `py`, `pyi` → `python`.
//   - C: `c`, `h` → `c`.
//   - C++: `cc`, `cxx`, `cpp`, `c++`, `hpp`, `hxx`, `hh`,
//     `h++` → `cpp`.
//   - C#: `cs`, `csharp`, `c#` → `csharp`.
//   - Go: `go`, `golang` → `go`.
//   - Rust: `rs`, `rust` → `rust`.
//   - PowerShell: `ps`, `ps1`, `psm1`, `psd1`, `powershell` →
//     `powershell`.
//
// Unknown hints pass through unchanged (lower-cased and
// trimmed) so the worker's downstream `selectParser` can still
// match a parser whose canonical id wasn't aliased here. Blank
// / whitespace-only entries are dropped.
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
		case "cc", "cxx", "cpp", "c++", "hpp", "hxx", "hh", "h++":
			n = "cpp"
		case "cs", "c#":
			n = "csharp"
		case "golang":
			n = "go"
		case "rs":
			n = "rust"
		case "ps", "ps1", "psm1", "psd1":
			n = "powershell"
		}
		out = append(out, n)
	}
	return out
}

// readEvent reads the file's contents through ev.Open. Returns
// an error on read failure or when ev.Open is nil (a wiring
// bug — the worker always provides Open).
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

// safeParse wraps `parser.Parse` with a panic recovery and the
// `ErrParserUnavailable` classification documented on
// `parser.go::ErrParserUnavailable`. The two log paths match
// the pinning in `dispatcher_pass2bd_test.go`:
//
//   - `errors.Is(err, ErrParserUnavailable)` → INFO-level
//     `ast.dispatch.skip{language=…, reason=<slug>}`.
//   - any other error → WARN-level `ast.parse.error`.
//
// In both cases the error is returned to EmitFile, which then
// swallows it (returns `(EmitResult{}, nil)`) so the worker
// continues draining.
func (d *Dispatcher) safeParse(parser LanguageParser, relPath string, src []byte, logger *slog.Logger) (res ParseResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("ast.parse.panic",
				slog.String("language", parser.Language()),
				slog.Any("panic", r),
			)
			err = fmt.Errorf("parser panic (language=%s): %v", parser.Language(), r)
		}
	}()
	res, err = parser.Parse(relPath, src)
	if err != nil {
		if errors.Is(err, ErrParserUnavailable) {
			logger.Info("ast.dispatch.skip",
				slog.String("language", parser.Language()),
				slog.String("reason", extractReasonSlug(err)),
			)
			return ParseResult{}, err
		}
		logger.Warn("ast.parse.error",
			slog.String("language", parser.Language()),
			slog.String("error", err.Error()),
		)
		return ParseResult{}, err
	}
	return res, nil
}

// extractReasonSlug pulls the `(reason=<slug>)` annotation
// from a wrapped `ErrParserUnavailable` per the parser.go
// convention. Falls back to `runtime_unavailable` when the
// wrapper did not embed a slug.
func extractReasonSlug(err error) string {
	if err == nil {
		return "runtime_unavailable"
	}
	msg := err.Error()
	idx := strings.Index(msg, "(reason=")
	if idx < 0 {
		return "runtime_unavailable"
	}
	tail := msg[idx+len("(reason="):]
	end := strings.IndexByte(tail, ')')
	if end < 0 {
		return "runtime_unavailable"
	}
	slug := tail[:end]
	if slug == "" {
		return "runtime_unavailable"
	}
	return slug
}

// log returns the dispatcher's logger or slog.Default().
func (d *Dispatcher) log() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// dispatcherParsersForTest exposes the dispatcher's internal
// extension→parser map for tests in the same package. Returns
// a defensive copy so a caller mutating the map cannot affect
// dispatcher state.
func (d *Dispatcher) dispatcherParsersForTest() map[string]LanguageParser {
	out := make(map[string]LanguageParser, len(d.extMap))
	for k, v := range d.extMap {
		out[k] = v
	}
	return out
}

// emit runs the two-pass insert protocol. Pass 0 inserts
// external-package nodes + `imports` edges (these have no
// intra-file dependencies). Pass 1 inserts every Class /
// Method / Block Node so the local-symbol table is fully
// populated. Pass 2 resolves and inserts the remaining static
// edges (`extends`, `implements`, `static_calls`, `reads`,
// `writes`, `overrides`) whose dst Node id is known after
// pass 1.
//
// `contains` edges are emitted in pass 1 alongside their child
// Node insert because the parent id is always known at insert
// time. The Method / Block insert paths also fire the embedding
// publisher AFTER the `contains` edge is committed (rubber-duck
// #5: publish-after-edge ordering).
//
// Cross-file references that cannot be resolved against either
// the local symbol table or an external-module synthesis (e.g.
// relative `./util` imports that point at another file in the
// same repo) are dropped with a debug log entry; the future
// cross-file resolver story will pick them up by walking the
// file tree. The dispatcher does NOT mint placeholder Nodes
// for unresolved targets — doing so would pollute the graph
// with no-op rows.
//
// Returns the TouchedNodes list (NodeID + Kind +
// CanonicalSignature + ParentNodeID + Inserted) so the Stage
// 3.4 delta handler can compute the retire-set. External
// package nodes are NOT included — only class / method / block
// nodes participate in the delta computation.
func (d *Dispatcher) emit(
	ctx context.Context,
	ev repoindexer.EmitFileEvent,
	parser LanguageParser,
	result ParseResult,
	src []byte,
	logger *slog.Logger,
) ([]repoindexer.TouchedNode, error) {
	// classNodeID and methodNodeID build the local-symbol
	// tables pass 2 resolves against. classNodeID keys on
	// QualifiedName; methodNodeID is a MULTIMAP (qualified
	// name -> list of node ids) so overloaded methods that
	// share a QualifiedName (e.g. C# `Foo.Bar(int)` and
	// `Foo.Bar(string)`) each contribute their own NodeID
	// instead of overwriting. Resolvers downstream (Pass 2d
	// destination, `buildCalleeIndex`) treat any qualified
	// name with > 1 distinct NodeIDs as ambiguous and DROP
	// rather than emit a false positive edge (tech-spec
	// Section 5.3 A5 — "false `static_calls` edges are worse
	// than missing edges").
	//
	// methodNodeIDs is the parallel per-method slice so each
	// `result.Methods[i]` knows its own NodeID for the source
	// side of static_calls / reads / writes / overrides
	// (regardless of overload collisions on the multimap key).
	classNodeID := make(map[string]string, len(result.Classes))
	methodNodeID := make(map[string][]string, len(result.Methods))
	methodNodeIDs := make([]string, len(result.Methods))

	// receiverIndex is the Pass 2b multimap that maps
	// `<EnclosingClass>.<simpleName>` → set of node ids. A
	// receiver-qualified caller (`this.bar()` / `r.bar()`)
	// looks up `<EnclosingClass>.bar` and drops the call when
	// the entry has > 1 unique node ids (A5 ambiguity rule).
	// The Go parser populates `ReceiverAliases` so a
	// pointer-receiver method `(*Foo).Bar` (QualifiedName
	// `*Foo.Bar`) ALSO registers under `Foo.Bar`, letting a
	// sibling caller's `r.Bar()` resolve through the alias.
	receiverIndex := map[string]map[string]struct{}{}
	registerReceiver := func(key, nodeID string) {
		if key == "" || nodeID == "" {
			return
		}
		entry, ok := receiverIndex[key]
		if !ok {
			entry = map[string]struct{}{}
			receiverIndex[key] = entry
		}
		entry[nodeID] = struct{}{}
	}

	touched := make([]repoindexer.TouchedNode, 0, len(result.Classes)+len(result.Methods))

	// Pass 0: `imports` edges (external modules only).
	// Relative imports (`./`, `../`) are deferred — they
	// resolve to in-repo files that the cross-file resolver
	// will stitch in a later story.
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

	// Pass 1b: insert methods + `contains` (parent->method)
	// + publish embedding + subdivide into blocks.
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
		methodNodeID[m.QualifiedName] = append(methodNodeID[m.QualifiedName], rec.NodeID)
		methodNodeIDs[i] = rec.NodeID
		touched = append(touched, repoindexer.TouchedNode{
			NodeID:             rec.NodeID,
			Kind:               "method",
			CanonicalSignature: sig,
			ParentNodeID:       parentID,
			Inserted:           rec.Inserted,
		})

		// Register multimap keys for Pass 2b. Primary key is
		// `<EnclosingClass>.<simpleName>` derived from the
		// QualifiedName; each ReceiverAliases entry registers
		// the same node id under that alternate key (the Go
		// pointer-receiver alias is the only producer in v1).
		if m.EnclosingClass != "" {
			primary := m.EnclosingClass + "." + simpleName(m.QualifiedName)
			registerReceiver(primary, rec.NodeID)
		}
		for _, alias := range m.ReceiverAliases {
			registerReceiver(alias, rec.NodeID)
		}

		if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "contains",
			SrcNodeID: parentID,
			DstNodeID: rec.NodeID,
			FromSHA:   ev.SHA,
		}); err != nil {
			return touched, fmt.Errorf("ast: insert %s->method contains: %w", parentKind, err)
		}

		// Publisher hook fires AFTER the contains edge is
		// committed (rubber-duck #5). For a bodyless method
		// (interface declarations, abstract methods, .pyi
		// stubs) the parser leaves BodySource empty; we fall
		// back to the canonical signature so the publish hook
		// still fires for every emitted Method node.
		content := m.BodySource
		signatureOnly := false
		if strings.TrimSpace(content) == "" {
			content = sig
			signatureOnly = true
		}
		if err := d.publish(ctx, NodeEmbedRequest{
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
			// Per-block embedding publish: Content is the
			// file source sliced at the Block's byte range
			// (file-relative). Fall back to the canonical
			// signature when the byte range is invalid.
			blockContent := sliceBlockContent(src, b)
			if strings.TrimSpace(blockContent) == "" {
				blockContent = bsig
			}
			if err := d.publish(ctx, NodeEmbedRequest{
				NodeID:             brec.NodeID,
				RepoID:             ev.RepoID.String(),
				Kind:               "block",
				CanonicalSignature: bsig,
				Content:            blockContent,
			}, logger); err != nil {
				return touched, err
			}
		}
	}

	// Pass 2a: extends / implements (only same-file resolvable).
	for _, c := range result.Classes {
		srcID := classNodeID[c.QualifiedName]
		for _, target := range c.Extends {
			dst, ok := classNodeID[target]
			if !ok {
				continue
			}
			if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "extends",
				SrcNodeID: srcID,
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
				SrcNodeID: srcID,
				DstNodeID: dst,
				FromSHA:   ev.SHA,
			}); err != nil {
				return touched, fmt.Errorf("ast: insert implements %s->%s: %w",
					c.QualifiedName, target, err)
			}
		}
	}

	// Pass 2b: static_calls. Receiver-qualified calls resolve
	// against the receiverIndex multimap (drop when >1 distinct
	// node ids); bare-name calls resolve against the same-file
	// callee index and drop on ambiguity. Per-method source
	// IDs come from `methodNodeIDs[i]` so overloaded methods
	// each emit calls from their own node (the multimap key
	// would alias overloads into a single source).
	calleeIndex := buildCalleeIndex(result.Methods, methodNodeIDs)
	for i, m := range result.Methods {
		srcID := methodNodeIDs[i]
		if srcID == "" {
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
		// Receiver-qualified calls first. Resolve via the
		// multimap; A5 drops on size > 1.
		if m.EnclosingClass != "" {
			for _, callee := range m.ReceiverCalls {
				key := m.EnclosingClass + "." + callee
				entry, ok := receiverIndex[key]
				if !ok || len(entry) != 1 {
					continue
				}
				var dstID string
				for id := range entry {
					dstID = id
				}
				if err := emitCall(dstID); err != nil {
					return touched, fmt.Errorf("ast: insert receiver static_calls %s->%s: %w",
						m.QualifiedName, key, err)
				}
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
	// rubber-duck #4). Methods with no enclosing class skip
	// this pass entirely.
	for i, m := range result.Methods {
		if m.EnclosingClass == "" || len(m.MemberAccesses) == 0 {
			continue
		}
		srcID := methodNodeIDs[i]
		if srcID == "" {
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

	// Pass 2d: trait/override edges (Rust). An impl method
	// whose LangMeta["trait"] names a same-file trait emits an
	// `overrides` edge from the impl method node to the trait
	// default-method node. Cross-file pairs are dropped (the
	// trait identity persists on attrs_json["trait"] for the
	// future cross-file resolver). Destination lookup uses the
	// methodNodeID MULTIMAP and drops on overload ambiguity:
	// if `<trait>.<simpleName>` resolves to > 1 distinct nodes
	// (the trait has overloaded default methods with the same
	// simple name) the edge is suppressed per the A5 conservative-
	// drop rule.
	for i, m := range result.Methods {
		traitVal, ok := m.LangMeta["trait"]
		if !ok {
			continue
		}
		trait, ok := traitVal.(string)
		if !ok || trait == "" {
			continue
		}
		srcID := methodNodeIDs[i]
		if srcID == "" {
			continue
		}
		dstKey := trait + "." + simpleName(m.QualifiedName)
		dstIDs := methodNodeID[dstKey]
		if len(dstIDs) != 1 {
			continue
		}
		dstID := dstIDs[0]
		if srcID == dstID {
			// A method cannot override itself (the trait's
			// own default-bodied method has LangMeta["trait"]
			// unset; if a parser ever sets it on the trait's
			// own method, drop the self-loop defensively).
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

	return touched, nil
}

// publish invokes the embedding publisher hook with the
// two-bucket error policy described on `NodeEmbeddingPublisher`:
//
//   - `errors.Is(err, ErrPublishRecordedFailed)` → swallow
//     (the publisher already wrote a `failed` event row; a
//     background flusher will retry).
//   - any other error → propagate so the worker can fail the
//     ingest (the EmbeddingIndex is in an unknown state).
//
// `errors.Is` correctly walks `errors.Join` chains under Go
// 1.20+ so callers that wrap `ErrPublishRecordedFailed`
// alongside the underlying transport error get the
// swallow-path.
func (d *Dispatcher) publish(ctx context.Context, req NodeEmbedRequest, logger *slog.Logger) error {
	if d.publisher == nil {
		return nil
	}
	_, err := d.publisher.PublishNodeEmbedding(ctx, req)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrPublishRecordedFailed) {
		logger.Debug("ast.publish.recorded_failed",
			slog.String("node_id", req.NodeID),
			slog.String("kind", req.Kind),
			slog.String("error", err.Error()),
		)
		return nil
	}
	return fmt.Errorf("ast: publish embedding (kind=%s node=%s): %w",
		req.Kind, req.NodeID, err)
}

// sliceBlockContent returns the file bytes spanning the
// Block's byte range. The Block's EndByte is INCLUSIVE per
// `block.go`, so the slice end is `EndByte+1`. Falls back to
// the empty string when the range is out of bounds or
// inverted; the caller substitutes the canonical signature in
// that case so the publish hook still fires with non-empty
// content.
func sliceBlockContent(src []byte, b Block) string {
	if b.StartByte < 0 || b.EndByte < b.StartByte {
		return ""
	}
	end := b.EndByte + 1
	if b.StartByte >= len(src) {
		return ""
	}
	if end > len(src) {
		end = len(src)
	}
	return string(src[b.StartByte:end])
}

// emitImportsEdges materialises each non-relative import as
// an external-package Node + an `imports` edge from the file
// to it. Relative imports (`./`, `../`, or Python module
// paths starting with `.`) are skipped: they resolve to other
// files in this repo, which the future cross-file resolver
// will pick up.
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
// any leading `.` (TS/JS / Python relative paths) or `/`
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
// sets — the extractor classifies each name once, with writes
// winning on conflict.
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

// buildCalleeIndex maps a bare callee name to the NodeID of
// the same-file Method whose simple name matches. When two
// methods share a simple name (e.g. `Foo.bar` and `Baz.bar`)
// OR two overloads share the same QualifiedName (e.g. C#
// `Foo.Bar(int)` and `Foo.Bar(string)`), the resolver bails
// out for that name — ambiguous matches are NOT emitted
// (false-positive `static_calls` edges are worse than
// missing edges; see doc.go "v1 edge scope" and tech-spec
// Section 5.3 A5).
//
// The input is the per-method node-id slice (parallel to
// `methods`) so overloads each get their own NodeID; the
// resolver folds them into the bare-name multimap and
// detects ambiguity at the simple-name layer.
func buildCalleeIndex(methods []MethodDecl, methodNodeIDs []string) map[string]string {
	bare := map[string][]string{}
	for i, m := range methods {
		nodeID := methodNodeIDs[i]
		if nodeID == "" {
			continue
		}
		s := simpleName(m.QualifiedName)
		if s == "" {
			continue
		}
		// Dedup against the same node id so a method whose
		// alias matches its own primary key still counts as
		// one entry (defence in depth — parsers should not
		// publish the same NodeID twice but this keeps the
		// ambiguity counter honest).
		ids := bare[s]
		dup := false
		for _, existing := range ids {
			if existing == nodeID {
				dup = true
				break
			}
		}
		if !dup {
			bare[s] = append(ids, nodeID)
		}
	}
	out := make(map[string]string, len(bare))
	for name, ids := range bare {
		if len(ids) == 1 {
			out[name] = ids[0]
		}
	}
	return out
}

// simpleName returns the last `.`-separated segment of a
// QualifiedName, stripping any operator-pinned receiver-
// pointer prefix (`*Foo.Bar` → `Bar`).
func simpleName(qualified string) string {
	if i := strings.LastIndexByte(qualified, '.'); i >= 0 {
		return qualified[i+1:]
	}
	return strings.TrimPrefix(qualified, "*")
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

// classAttrs builds the attrs_json for a ClassDecl. The
// first-class keys (language, decl_kind, start_line, end_line,
// extends_raw, implements_raw) win over any LangMeta entry
// with the same name; mergeLangMeta enforces the rule.
func classAttrs(language string, c ClassDecl) json.RawMessage {
	m := map[string]any{
		"language":   language,
		"decl_kind":  c.Kind,
		"start_line": c.StartLine,
		"end_line":   c.EndLine,
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

// methodAttrs builds the attrs_json for a MethodDecl. The
// first-class keys (language, enclosing_class, start_line,
// end_line, params_raw, calls_raw, modifiers) win over any
// LangMeta entry with the same name.
func methodAttrs(language string, m MethodDecl) json.RawMessage {
	out := map[string]any{
		"language":   language,
		"start_line": m.StartLine,
		"end_line":   m.EndLine,
		"params_raw": m.ParamSignature,
	}
	if m.EnclosingClass != "" {
		out["enclosing_class"] = m.EnclosingClass
	}
	if len(m.Modifiers) > 0 {
		out["modifiers"] = append([]string(nil), m.Modifiers...)
	}
	if len(m.Calls) > 0 {
		out["calls_raw"] = append([]string(nil), m.Calls...)
	}
	mergeLangMeta(out, m.LangMeta)
	return mustJSON(out)
}

// blockAttrs records the language, block kind, ordinal, AND
// the file-relative source boundaries that span ingestor uses
// to map runtime spans back to a Block. Coords are 1-based
// line numbers and 0-based byte offsets, both relative to the
// FILE so downstream span->block resolution does not need to
// know the enclosing method's offsets.
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
// nodes.
func externalPackageSignature(repoURL, module string) string {
	return repoURL + "::package::ext::" + NormalizeSignature(module)
}

// externalPackageAttrs records the language that observed the
// import (different languages may resolve the same module
// name differently) and a stable `module` key so consumers
// can group multi-language imports of the same external
// package. When `parentMissing` is true, the dispatcher could
// not find a Repo Node id on the event and inserted the
// package Node without a `parent_node_id`; the flag is
// persisted on the package node so consumers can see why the
// package is structurally orphaned.
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
// Per-language LangMeta keys (e.g. PowerShell `cmdlet_verb`,
// Go `dot_import` / `blank_import`) are folded in via
// `mergeLangMeta` so dispatcher first-class keys still win.
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
	mergeLangMeta(m, imp.LangMeta)
	return mustJSON(m)
}

// memberEdgeAttrs serialises the deduped, deterministic-order
// list of member names touched by a `reads` or `writes` edge.
// The class id is implicit (it's the edge's destination); the
// member names live in `members[]` so consumers can drive
// field-level relevance without resolving each member to a
// dedicated Node (no `field` node kind exists in v1 — see
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

// mergeLangMeta folds the parser-supplied LangMeta map into
// the dispatcher's attrs map. First-class keys already
// present in attrs (`language`, `decl_kind`, `extends_raw`,
// etc.) win — any LangMeta entry whose key already exists in
// attrs is silently dropped (architecture invariant C11). A
// first-class key with a nil value still wins (the helper
// uses `_, exists := attrs[k]` to detect presence so a
// dispatcher-pinned nil is preserved).
//
// Nested slice / map values pass through by reference; this
// is the documented shallow-copy contract (parsers that
// mutate a slice they put into LangMeta after the merge will
// see the change leak, but the dispatcher serialises with
// mustJSON immediately and discards the merged map
// afterwards).
func mergeLangMeta(attrs map[string]any, lang map[string]any) {
	if len(lang) == 0 {
		return
	}
	for k, v := range lang {
		if _, exists := attrs[k]; exists {
			continue
		}
		attrs[k] = v
	}
}

// mustJSON marshals m and returns the encoded bytes. Falls
// back to `{}` on the (practically impossible) error path so
// the dispatcher does not crash mid-ingest.
func mustJSON(m map[string]any) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
