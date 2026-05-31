// Command codeintel: `scan` subcommand.
//
// Wires the Stage 5.2 contract:
//   - input detection: `file://` URL, an absolute path, or a git
//     URL scheme selects between `LocalDirMaterializer` and
//     `GitMaterializer` (relative paths that exist on disk also
//     route to `LocalDirMaterializer`),
//   - materializer -> `NewAncestryWriter` -> `ast.NewDispatcher`
//     -> `graphsink.Sink` (sqlite default per architecture S9.4),
//   - per-flag plumbing: `--sha`, `--out`, `--lang-hints`,
//   - structured summary on completion per architecture S7.5
//     (files walked, files parsed, nodes by kind, edges by
//     kind, skipped by reason).
//
// Stage 5.4 will add a richer summary aggregator under
// `internal/codeintel/summary/`; this file keeps the counters
// inline so the scan subcommand can ship without that package.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/memory"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// scanFlags are the per-invocation flags `codeintel scan` reads.
// Persistent flags (`--store`, `--db`, `--log`, `--with-embeddings`)
// flow in via the shared `*rootFlags` pointer.
type scanFlags struct {
	sha       string
	out       string
	langHints []string
}

// sinkOpener is the indirection seam unit tests use to drop in a
// pure-memory backend without touching the SQLite CGO build tag.
// Production wiring uses `defaultSinkOpener` which routes to the
// real sqlite / memory / postgres backends.
type sinkOpener func(ctx context.Context, store, dbOrOut string) (graphsink.Sink, func() error, error)

// scanRunner bundles the seams tests override. Production wires
// nil into every field; the `nil`-default fallback inside `runScan`
// installs the real wiring.
type scanRunner struct {
	openSink     sinkOpener
	newGitMat    func() repoindexer.Materializer
	newLocalMat  func() repoindexer.Materializer
	dispatchOpts []ast.DispatcherOption
	stdout       io.Writer
}

func newScanCmdImpl(root *rootFlags) *cobra.Command {
	flags := &scanFlags{}
	cmd := &cobra.Command{
		Use:   "scan <path|git-url>",
		Short: "Scan a single repository (local path or git URL)",
		Long: "Scan walks a local directory or fetches a remote SHA, runs the AST " +
			"dispatcher across every file the workspace surfaces, and persists the " +
			"resulting graph (repo->package->file->class->method->block plus contains, " +
			"imports, extends, implements, static_calls, overrides edges) through the " +
			"selected graphsink backend.\n\n" +
			"Input detection:\n" +
			"  - file://<abs-path>      -> LocalDirMaterializer\n" +
			"  - C:\\..., /...           -> LocalDirMaterializer\n" +
			"  - https://, git@host:... -> GitMaterializer\n" +
			"  - relative path on disk  -> LocalDirMaterializer\n\n" +
			"Coverage-degraded scans (e.g. CGO=0 build skipping .c/.cpp/.go/.rs) exit 0; " +
			"only fatal IO/config errors yield a non-zero exit code.",
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("scan: requires exactly 1 argument <path|git-url>, got %d", len(args))
			}
			slog.Info("codeintel subcommand invoked", "subcommand", "scan")
			return runScan(cmd.Context(), root, flags, args[0], scanRunner{stdout: cmd.OutOrStdout()})
		},
	}

	cmd.Flags().StringVar(&flags.sha, "sha", "",
		"Override the repository SHA. For git inputs this is the commit "+
			"the GitMaterializer fetches (required for git URLs unless the URL embeds it). "+
			"For local inputs it overrides the synthesised SHA (`git rev-parse HEAD` or "+
			"the mtime-tree hash).")
	cmd.Flags().StringVar(&flags.out, "out", "",
		"Output path. For --store=sqlite the .db file path; for --store=memory the "+
			"JSON export file path (omit to skip the export). When omitted and --store=sqlite, "+
			"the persistent --db value is used; if neither is set the scan fails with a "+
			"clear error.")
	cmd.Flags().StringSliceVar(&flags.langHints, "lang-hints", nil,
		"Comma-separated language hints (e.g. --lang-hints=python,typescript). "+
			"Forwarded to AncestryWriter.EnsureRepo and to the dispatcher as a "+
			"tie-breaker for files whose extension does not map to a registered parser.")

	return cmd
}

// runScan is the testable entry point. The `runner` argument lets
// tests substitute the sink opener / materializers and capture
// stdout without going through cobra.
func runScan(ctx context.Context, root *rootFlags, flags *scanFlags, input string, runner scanRunner) error {
	if root == nil {
		return errors.New("scan: nil root flags")
	}
	if input = strings.TrimSpace(input); input == "" {
		return errors.New("scan: empty <path|git-url> argument")
	}
	if root.withEmbeddings {
		// Stage 5.5 wires this; until then opt-in is rejected so
		// the operator does not silently get a no-op publisher.
		return errors.New("scan: --with-embeddings is not implemented yet (Stage 5.5)")
	}

	stdout := runner.stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	// 1. Resolve the input form once -- the decision drives both
	// the materializer choice AND the canonical repo URL the
	// AncestryWriter records.
	kind := detectInputKind(input)

	var (
		mat     repoindexer.Materializer
		matArg  string
		shaArg  = flags.sha
	)
	switch kind {
	case inputKindGitURL:
		if runner.newGitMat != nil {
			mat = runner.newGitMat()
		} else {
			mat = &repoindexer.GitMaterializer{}
		}
		matArg = input
		if shaArg == "" {
			return errors.New("scan: --sha is required when the input is a git URL")
		}
	case inputKindFileURL, inputKindLocalPath:
		if runner.newLocalMat != nil {
			mat = runner.newLocalMat()
		} else {
			mat = &repoindexer.LocalDirMaterializer{}
		}
		matArg = input
	default:
		return fmt.Errorf("scan: cannot classify input %q (use file://, an absolute path, or a git URL)", input)
	}

	// 2. Open the sink BEFORE materializing so an unwritable
	// --out fails fast without paying the git fetch / Walk cost.
	opener := runner.openSink
	if opener == nil {
		opener = defaultSinkOpener
	}
	dbOrOut := flags.out
	if dbOrOut == "" {
		dbOrOut = root.db
	}
	sink, sinkClose, err := opener(ctx, root.store, dbOrOut)
	if err != nil {
		return fmt.Errorf("scan: open store %q: %w", root.store, err)
	}
	defer func() {
		if cerr := sinkClose(); cerr != nil {
			slog.Warn("scan.sink_close_failed", "error", cerr.Error())
		}
	}()

	// 3. Materialize -- with a CLI cancel-safe context.
	ws, err := mat.Materialize(ctx, matArg, shaArg)
	if err != nil {
		return fmt.Errorf("scan: materialize: %w", err)
	}
	defer func() {
		if cerr := ws.Close(); cerr != nil {
			slog.Warn("scan.workspace_close_failed", "error", cerr.Error())
		}
	}()

	// 4. Wrap the sink with the counting decorator so the
	// per-scan summary tracks nodes/edges by kind without
	// teaching every backend to count.
	counter := newCountingSink(sink)

	// 5. Pre-walk ancestry. The repoURL recorded on the
	// AncestryWriter is the synthesised file:// for local
	// scans (Workspace.URL()) or the raw git URL for remote
	// scans -- both fold deterministically into the canonical
	// signatures the dispatcher mints later.
	aw := repoindexer.NewAncestryWriter(counter, ws.URL(), ws.SHA())
	ancestry, err := aw.EnsureRepoAndCommit(ctx, "" /* default_branch */, flags.langHints)
	if err != nil {
		return fmt.Errorf("scan: ancestry pre-walk: %w", err)
	}

	// 6. Dispatcher wiring. Attach a skip-aware logger so the
	// summary can report skipped-by-reason without requiring
	// the dispatcher itself to be summary-aware.
	skipTally := newSkipTally()
	dispatchLogger := slog.New(skipTally.tee(slog.Default().Handler()))

	dopts := append([]ast.DispatcherOption{},
		ast.WithLogger(dispatchLogger),
		ast.WithLanguageHints(flags.langHints),
	)
	dopts = append(dopts, runner.dispatchOpts...)

	dispatcher := ast.NewDispatcher(counter, dopts...)

	// 7. Per-file walk: file ancestry then EmitFile. Mirrors
	// `worker.runFull` so a repo scanned via the CLI yields a
	// graph byte-identical to the worker (architecture parity).
	var (
		walked int64
		parsed int64
	)
	walkErr := ws.Walk(func(file repoindexer.WalkFile) error {
		atomic.AddInt64(&walked, 1)
		fa, eErr := aw.EnsureFile(ctx, file)
		if eErr != nil {
			return fmt.Errorf("ancestry per-file (%s): %w", file.RelPath, eErr)
		}
		_, emErr := dispatcher.EmitFile(ctx, repoindexer.EmitFileEvent{
			RepoID:        ancestry.RepoID,
			RepoURL:       ws.URL(),
			SHA:           ws.SHA(),
			FileNodeID:    fa.FileNodeID,
			RepoNodeID:    ancestry.RepoNodeID,
			RelPath:       file.RelPath,
			AbsPath:       file.AbsPath,
			LanguageHints: flags.langHints,
			Open:          file.Reader,
		})
		if emErr != nil {
			// Per tech-spec C7 a coverage-degraded scan must
			// still exit 0. The dispatcher already swallows
			// parser-unavailable into a skip and a `no_parser`
			// extension into a skip; any error that escapes
			// is therefore a true failure (parser crash, IO).
			return fmt.Errorf("ast emitter (%s): %w", file.RelPath, emErr)
		}
		atomic.AddInt64(&parsed, 1)
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("scan: walk: %w", walkErr)
	}

	// 8. Flush buffered writes (SQLite is inline; memory writes
	// the JSON export on Close, which runs in the deferred
	// closer above).
	if ferr := sink.Flush(ctx); ferr != nil {
		return fmt.Errorf("scan: flush: %w", ferr)
	}

	// 9. Render the summary. JSON when --log=json so the line
	// is mechanically parseable; text otherwise.
	sum := scanSummary{
		Repo:    summaryRepo{URL: ws.URL(), SHA: ws.SHA(), NodeID: ancestry.RepoNodeID},
		Walked:  int(walked),
		Parsed:  int(parsed),
		Nodes:   counter.snapshotNodes(),
		Edges:   counter.snapshotEdges(),
		Skipped: skipTally.snapshot(),
	}
	return writeScanSummary(stdout, root.logFormat, sum)
}

// ----- input-kind detection --------------------------------------

type inputKind int

const (
	inputKindUnknown inputKind = iota
	inputKindFileURL
	inputKindLocalPath
	inputKindGitURL
)

// gitURLPrefix is the conservative set of schemes that route to
// GitMaterializer. `git@host:owner/repo` is the SCP-like SSH form
// and is handled below by a separate predicate.
var gitURLPrefixes = []string{
	"https://", "http://", "git://", "ssh://", "git+https://", "git+ssh://",
}

// scpLikeGitURL matches the SSH-with-scp-syntax form, e.g.
// `git@github.com:owner/repo.git`. We use a strict pattern so a
// Windows drive (`C:\`) cannot be misclassified -- the user@host
// segment is required to contain at least one letter and end on
// `:`.
var scpLikeGitURL = regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+:[^\\]`)

func detectInputKind(input string) inputKind {
	if strings.HasPrefix(input, "file://") {
		return inputKindFileURL
	}
	lower := strings.ToLower(input)
	for _, p := range gitURLPrefixes {
		if strings.HasPrefix(lower, p) {
			return inputKindGitURL
		}
	}
	// Windows drive-letter path: C:\foo, c:/foo. MUST be tested
	// before scp-like so `C:\code\repo` is not mistaken for
	// `user@host:path` (the scp regex already requires `[^\\]`
	// after the colon to block that, but the explicit check
	// keeps the intent obvious).
	if isWindowsDrivePath(input) {
		return inputKindLocalPath
	}
	if scpLikeGitURL.MatchString(input) {
		return inputKindGitURL
	}
	if filepath.IsAbs(input) {
		return inputKindLocalPath
	}
	// POSIX-style absolute path on a Windows host: filepath.IsAbs
	// only returns true for drive-letter paths there, but the
	// brief includes `/...` as an absolute-path indicator. A
	// leading `/` followed by a path char is a clear positive
	// signal (and unambiguous -- not a git URL scheme).
	if strings.HasPrefix(input, "/") {
		return inputKindLocalPath
	}
	// Relative path: route to local if it exists on disk; else
	// "unknown" so the caller surfaces a clear classification
	// error rather than calling the git binary with garbage.
	if info, err := os.Stat(input); err == nil && info.IsDir() {
		return inputKindLocalPath
	}
	return inputKindUnknown
}

func isWindowsDrivePath(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
		return false
	}
	if s[1] != ':' {
		return false
	}
	return s[2] == '\\' || s[2] == '/'
}

// ----- sink opener ------------------------------------------------

func defaultSinkOpener(ctx context.Context, store, dbOrOut string) (graphsink.Sink, func() error, error) {
	switch store {
	case "sqlite":
		if dbOrOut == "" {
			return nil, nil, errors.New("--out (or --db) is required for --store=sqlite")
		}
		return openSqliteSink(ctx, dbOrOut)
	case "memory":
		// dbOrOut is the optional JSON export path; empty means
		// "discard on close" (still useful for unit tests and
		// dry-runs that only care about the summary).
		s := memory.New(memory.Options{ExportPath: dbOrOut})
		return s, s.Close, nil
	case "postgres":
		// Postgres-backed CLI scan needs a pgxpool + the
		// graphwriter migrations; that wiring belongs in a
		// follow-up workstream. Fail explicitly so the operator
		// does not get silent data loss.
		return nil, nil, errors.New("--store=postgres is not yet supported by `codeintel scan` (use --store=sqlite or --store=memory)")
	default:
		return nil, nil, fmt.Errorf("unknown --store %q", store)
	}
}

// ----- counting sink decorator -----------------------------------

// countingSink wraps a graphsink.Sink and tallies node/edge
// insertions by kind so the summary printer doesn't need a second
// pass over the backing store. Only INSERT calls that report
// Inserted=true are counted -- idempotent re-hits on an existing
// row reflect dedupe, not new graph data.
type countingSink struct {
	inner graphsink.Sink
	nodes map[string]*atomic.Int64
	edges map[string]*atomic.Int64
}

func newCountingSink(inner graphsink.Sink) *countingSink {
	nodeKinds := []string{"repo", "package", "file", "class", "method", "block"}
	edgeKinds := []string{
		"contains", "imports", "static_calls", "observed_calls",
		"extends", "implements", "overrides", "reads", "writes", "renamed_to",
	}
	c := &countingSink{
		inner: inner,
		nodes: make(map[string]*atomic.Int64, len(nodeKinds)),
		edges: make(map[string]*atomic.Int64, len(edgeKinds)),
	}
	for _, k := range nodeKinds {
		c.nodes[k] = new(atomic.Int64)
	}
	for _, k := range edgeKinds {
		c.edges[k] = new(atomic.Int64)
	}
	return c
}

func (c *countingSink) EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	// The AncestryWriter today does NOT precompute RepoID on
	// the legacy Postgres path (Stage 2.3); it relies on
	// `gen_random_uuid()` and reads the value out of the
	// returned `RepoRecord.ID`. The SQLite and memory backends
	// require a non-zero `in.RepoID` (architecture S3.4 backend
	// parity). To bridge the two without forcing every caller
	// to wire the deterministic ID through, this decorator
	// computes it from `in.URL` when the caller passed zero.
	// The fingerprint helper is canonical -- the same hash the
	// Postgres path will fold in once `EnsureRepoWithID` is
	// wired -- so this injection is byte-compatible with the
	// production path.
	if in.RepoID.IsZero() && in.URL != "" {
		if rid, err := fingerprint.RepoIDFromURL(in.URL); err == nil {
			in.RepoID = rid
		}
	}
	return c.inner.EnsureRepo(ctx, in)
}
func (c *countingSink) EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	return c.inner.EnsureCommit(ctx, in)
}
func (c *countingSink) InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	rec, err := c.inner.InsertNode(ctx, in)
	if err == nil && rec.Inserted {
		c.tickNode(in.Kind)
	}
	return rec, err
}
func (c *countingSink) InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	rec, err := c.inner.InsertEdge(ctx, in)
	if err == nil && rec.Inserted {
		c.tickEdge(in.Kind)
	}
	return rec, err
}
func (c *countingSink) Flush(ctx context.Context) error { return c.inner.Flush(ctx) }
func (c *countingSink) Close() error                    { return c.inner.Close() }

func (c *countingSink) tickNode(kind string) {
	if ctr, ok := c.nodes[kind]; ok {
		ctr.Add(1)
		return
	}
	// Unknown kind: lazily add a counter so the summary still
	// reports it. Single-writer in the CLI path so no lock
	// needed, but the dispatcher may invoke from a goroutine
	// in the future -- the map is set up once at Scan start so
	// adding new keys here is the only mutation; gate it with
	// the per-bucket atomic.Int64 once seen.
	ctr := new(atomic.Int64)
	ctr.Add(1)
	c.nodes[kind] = ctr
}

func (c *countingSink) tickEdge(kind string) {
	if ctr, ok := c.edges[kind]; ok {
		ctr.Add(1)
		return
	}
	ctr := new(atomic.Int64)
	ctr.Add(1)
	c.edges[kind] = ctr
}

func (c *countingSink) snapshotNodes() map[string]int {
	out := make(map[string]int, len(c.nodes))
	for k, v := range c.nodes {
		out[k] = int(v.Load())
	}
	return out
}

func (c *countingSink) snapshotEdges() map[string]int {
	out := make(map[string]int, len(c.edges))
	for k, v := range c.edges {
		out[k] = int(v.Load())
	}
	return out
}

// ----- skip tally (slog handler that intercepts dispatch skips) --

// skipTally is a slog.Handler middleware that captures the
// dispatcher's `ast.dispatch.skip` events and groups them by
// reason. The wrapped handler still receives the record so the
// operator's structured log stream is unchanged.
type skipTally struct {
	counts map[string]int
	byExt  map[string]int
}

func newSkipTally() *skipTally {
	return &skipTally{
		counts: make(map[string]int),
		byExt:  make(map[string]int),
	}
}

func (t *skipTally) tee(inner slog.Handler) slog.Handler {
	if inner == nil {
		inner = slog.NewTextHandler(io.Discard, nil)
	}
	return &skipHandler{inner: inner, tally: t}
}

type skipHandler struct {
	inner slog.Handler
	tally *skipTally
}

func (h *skipHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

func (h *skipHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "ast.dispatch.skip" {
		var reason, file string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "reason":
				reason = a.Value.String()
			case "file":
				file = a.Value.String()
			}
			return true
		})
		if reason == "" {
			reason = "unknown"
		}
		h.tally.counts[reason]++
		if reason == "no_parser" && file != "" {
			ext := strings.ToLower(filepath.Ext(file))
			if ext == "" {
				ext = "<no-ext>"
			}
			h.tally.byExt[ext]++
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *skipHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &skipHandler{inner: h.inner.WithAttrs(attrs), tally: h.tally}
}

func (h *skipHandler) WithGroup(name string) slog.Handler {
	return &skipHandler{inner: h.inner.WithGroup(name), tally: h.tally}
}

type skipSnapshot struct {
	ByReason map[string]int `json:"by_reason"`
	ByExt    map[string]int `json:"no_parser_by_ext,omitempty"`
}

func (t *skipTally) snapshot() skipSnapshot {
	br := make(map[string]int, len(t.counts))
	for k, v := range t.counts {
		br[k] = v
	}
	bx := make(map[string]int, len(t.byExt))
	for k, v := range t.byExt {
		bx[k] = v
	}
	return skipSnapshot{ByReason: br, ByExt: bx}
}

// ----- summary rendering -----------------------------------------

type summaryRepo struct {
	URL    string `json:"url"`
	SHA    string `json:"sha"`
	NodeID string `json:"node_id"`
}

type scanSummary struct {
	Repo    summaryRepo    `json:"repo"`
	Walked  int            `json:"walked"`
	Parsed  int            `json:"parsed"`
	Nodes   map[string]int `json:"nodes"`
	Edges   map[string]int `json:"edges"`
	Skipped skipSnapshot   `json:"skipped"`
}

func writeScanSummary(w io.Writer, format string, s scanSummary) error {
	if strings.ToLower(format) == "json" {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(s)
	}
	// Text format. Sorted keys keep golden tests stable.
	fmt.Fprintf(w, "repo: %s @ %s (node_id=%s)\n", s.Repo.URL, s.Repo.SHA, s.Repo.NodeID)
	fmt.Fprintf(w, "walked: %d\n", s.Walked)
	fmt.Fprintf(w, "parsed: %d\n", s.Parsed)
	fmt.Fprintf(w, "nodes: %s\n", formatKindMap(s.Nodes))
	fmt.Fprintf(w, "edges: %s\n", formatKindMap(s.Edges))
	fmt.Fprintf(w, "skipped: %s\n", formatKindMap(s.Skipped.ByReason))
	if len(s.Skipped.ByExt) > 0 {
		fmt.Fprintf(w, "skipped.no_parser_by_ext: %s\n", formatKindMap(s.Skipped.ByExt))
	}
	return nil
}

func formatKindMap(m map[string]int) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%d", k, m[k])
	}
	b.WriteByte('}')
	return b.String()
}
