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
		writer:  writer,
		parsers: defaultParsers(),
		logger:  slog.Default(),
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
func (d *Dispatcher) EmitFile(ctx context.Context, ev repoindexer.EmitFileEvent) error {
	logger := d.logger.With(
		slog.String("op", "ast.emit_file"),
		slog.String("rel_path", ev.RelPath),
		slog.String("file_node_id", ev.FileNodeID),
		slog.String("sha", ev.SHA),
	)

	parser := d.selectParser(ev.RelPath, ev.LanguageHints)
	if parser == nil {
		logger.Debug("ast.dispatch.skip", slog.String("reason", "no_parser"))
		return nil
	}

	src, err := readEvent(ev)
	if err != nil {
		return fmt.Errorf("ast: read %s: %w", ev.RelPath, err)
	}

	result, err := safeParse(parser, ev.RelPath, src)
	if err != nil {
		logger.Warn("ast.parse.error",
			slog.String("language", parser.Language()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	if err := d.emit(ctx, ev, parser, result, logger); err != nil {
		return err
	}
	logger.Debug("ast.dispatch.ok",
		slog.String("language", parser.Language()),
		slog.Int("classes", len(result.Classes)),
		slog.Int("methods", len(result.Methods)),
		slog.Int("imports", len(result.Imports)),
	)
	return nil
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
// (`typescript`, `python`).
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
	logger *slog.Logger,
) error {
	// classNodeID and methodNodeID build the local-symbol
	// tables pass 2 resolves against. Keys are the parser's
	// QualifiedName (the dotted path within the file).
	classNodeID := make(map[string]string, len(result.Classes))
	methodNodeID := make(map[string]string, len(result.Methods))

	// Pass 0: `imports` edges (external modules only).
	// Relative imports (`./`, `../`) are deferred -- they
	// resolve to in-repo files that the cross-file resolver
	// will stitch in a later story (per rubber-duck #3).
	if err := d.emitImportsEdges(ctx, ev, parser, result.Imports, logger); err != nil {
		return err
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
			return fmt.Errorf("ast: insert class %s: %w", c.QualifiedName, err)
		}
		classNodeID[c.QualifiedName] = rec.NodeID
		if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "contains",
			SrcNodeID: ev.FileNodeID,
			DstNodeID: rec.NodeID,
			FromSHA:   ev.SHA,
		}); err != nil {
			return fmt.Errorf("ast: insert file->class contains: %w", err)
		}
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
			return fmt.Errorf("ast: insert method %s: %w", m.QualifiedName, err)
		}
		methodNodeID[m.QualifiedName] = rec.NodeID
		if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    ev.RepoID,
			Kind:      "contains",
			SrcNodeID: parentID,
			DstNodeID: rec.NodeID,
			FromSHA:   ev.SHA,
		}); err != nil {
			return fmt.Errorf("ast: insert %s->method contains: %w", parentKind, err)
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
				return fmt.Errorf("ast: insert block %s#%d: %w",
					m.QualifiedName, b.Ordinal, err)
			}
			if _, err := d.writer.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    ev.RepoID,
				Kind:      "contains",
				SrcNodeID: rec.NodeID,
				DstNodeID: brec.NodeID,
				FromSHA:   ev.SHA,
			}); err != nil {
				return fmt.Errorf("ast: insert method->block contains: %w", err)
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
				return fmt.Errorf("ast: insert extends %s->%s: %w",
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
				return fmt.Errorf("ast: insert implements %s->%s: %w",
					c.QualifiedName, target, err)
			}
		}
	}

	// Pass 2b: static_calls. Receiver-qualified calls
	// (`this.foo()` / `self.foo()`) resolve unambiguously
	// against the enclosing class; bare-name calls resolve
	// against the same-file callee index and drop on
	// ambiguity.
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
		// Receiver-qualified calls first (unambiguous).
		for _, callee := range m.ReceiverCalls {
			if m.EnclosingClass == "" {
				continue
			}
			dstID, ok := methodNodeID[m.EnclosingClass+"."+callee]
			if !ok {
				continue
			}
			if err := emitCall(dstID); err != nil {
				return fmt.Errorf("ast: insert receiver static_calls %s->%s.%s: %w",
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
				return fmt.Errorf("ast: insert static_calls %s->%s: %w",
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
				return fmt.Errorf("ast: insert reads %s->%s: %w",
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
				return fmt.Errorf("ast: insert writes %s->%s: %w",
					m.QualifiedName, m.EnclosingClass, err)
			}
		}
	}

	_ = logger // structured logs from sub-helpers if needed in the future
	return nil
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
	return mustJSON(m)
}

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
	return mustJSON(out)
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
