// Command codeintel: `scan-many` subcommand.
//
// `codeintel scan-many <manifest> --out-dir <dir>` iterates a
// newline-delimited manifest where each line is either a
// `<path>` to a local directory or `<git-url>@<sha>`. Blank
// lines and `#`-comments are skipped. Each entry is materialized
// and scanned sequentially (architecture S6.2) and a per-repo
// sqlite `.db` file is written under `--out-dir` (S9.4: one
// `.db` per repo).
//
// Failure isolation: when an individual repo errors (bad git
// URL, unreadable directory, sink open failure, ...), the loop
// records a `failed: <reason>` line in the per-repo summary
// stream AND continues with the next entry. The aggregate
// summary at the end reports `succeeded`, `failed`, and the
// summed nodes/edges across the successful entries. When at
// least one entry failed, the command returns a non-zero exit
// (tech-spec S4.3 partial-failure contract).
package main

import (
	"bufio"
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

	"github.com/spf13/cobra"
)

// scanManyFlags captures the per-invocation flags `scan-many`
// reads. Persistent flags (`--store`, `--db`, `--log`,
// `--with-embeddings`) flow in via the shared `*rootFlags`.
type scanManyFlags struct {
	outDir string
}

// manifestEntry is one (parsed) line of a scan-many manifest.
// `Line` is the 1-based line number used for diagnostics and
// for synthesising a fallback slug when an entry's basename
// collides with an earlier one.
type manifestEntry struct {
	Input string
	SHA   string
	Line  int
}

func newScanManyCmdImpl(root *rootFlags) *cobra.Command {
	flags := &scanManyFlags{}
	cmd := &cobra.Command{
		Use:   "scan-many <manifest>",
		Short: "Scan many repositories listed in a manifest file",
		Long: "scan-many reads a newline-delimited manifest of repositories " +
			"(each line either '<path>' or '<git-url>@<sha>', blank lines and " +
			"'#' comments skipped), runs the scan loop sequentially against " +
			"each entry, and writes one per-repo .db file under --out-dir " +
			"(architecture S6.2 / S9.4).\n\n" +
			"Failure isolation: a per-repo failure is logged as a 'failed: <reason>' " +
			"line and the loop continues; the aggregate summary at the end reports " +
			"succeeded, failed, and summed nodes/edges. A non-zero exit indicates " +
			"at least one entry failed (tech-spec S4.3).",
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				slog.Error("scan-many.invalid_args",
					"subcommand", "scan-many",
					"want_args", 1,
					"got_args", len(args),
				)
				return fmt.Errorf("scan-many: requires exactly 1 argument <manifest>, got %d", len(args))
			}
			slog.Info("codeintel subcommand invoked", "subcommand", "scan-many")
			return runScanMany(cmd.Context(), root, flags, args[0],
				scanManyRunner{stdout: cmd.OutOrStdout()})
		},
	}

	cmd.Flags().StringVar(&flags.outDir, "out-dir", "",
		"Directory where one .db file per manifest entry is written. "+
			"Required: scan-many always writes per-repo stores (architecture S9.4).")
	return cmd
}

// scanManyRunner mirrors scanRunner: a small struct of seams so
// tests can capture stdout and (optionally) inject a per-entry
// scan executor.
type scanManyRunner struct {
	stdout io.Writer
	// runScan is the per-entry executor; defaults to
	// runScanWithSummary. Tests override it to avoid touching
	// disk / the git binary.
	runScan func(ctx context.Context, root *rootFlags, flags *scanFlags, input string, runner scanRunner) (scanSummary, error)
}

// runScanMany is the testable entry point.
func runScanMany(ctx context.Context, root *rootFlags, flags *scanManyFlags, manifestPath string, runner scanManyRunner) error {
	if root == nil {
		return errors.New("scan-many: nil root flags")
	}
	if manifestPath = strings.TrimSpace(manifestPath); manifestPath == "" {
		return errors.New("scan-many: empty <manifest> argument")
	}
	if strings.TrimSpace(flags.outDir) == "" {
		return errors.New("scan-many: --out-dir is required")
	}

	stdout := runner.stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	exec := runner.runScan
	if exec == nil {
		exec = runScanWithSummary
	}

	// Parse the manifest up front so a malformed line is reported
	// without partially scanning some repos: the brief's
	// "iterate sequentially" contract is per ENTRY, not per
	// byte of the manifest.
	f, err := os.Open(manifestPath)
	if err != nil {
		return fmt.Errorf("scan-many: open manifest: %w", err)
	}
	entries, parseErr := parseManifest(f)
	closeErr := f.Close()
	if parseErr != nil {
		return fmt.Errorf("scan-many: parse manifest: %w", parseErr)
	}
	if closeErr != nil {
		return fmt.Errorf("scan-many: close manifest: %w", closeErr)
	}

	if err := os.MkdirAll(flags.outDir, 0o755); err != nil {
		return fmt.Errorf("scan-many: create --out-dir: %w", err)
	}

	agg := newManyAggregate()
	// usedSlugs tracks the full set of ASSIGNED slugs (both
	// natural and disambiguated). The int value is the next
	// disambiguator suffix to try for the base slug. Tracking
	// assigned disambiguated slugs is what prevents a collision
	// like {foo, foo-2, foo}: without it the second `foo` would
	// re-disambiguate to `foo-2` and clobber the natural `foo-2`
	// entry's .db, violating the one-.db-per-repo contract
	// (architecture S9.4).
	usedSlugs := make(map[string]int)
	for _, e := range entries {
		base := slugForEntry(e)
		slug := base
		// Disambiguate same-basename entries so they don't share
		// a single .db file (architecture S9.4 mandates one .db
		// per repo). Probe until the candidate slug is unique
		// across both natural and previously-disambiguated slugs.
		if _, ok := usedSlugs[slug]; ok {
			for {
				n := usedSlugs[base]
				usedSlugs[base] = n + 1
				slug = fmt.Sprintf("%s-%d", base, n+1)
				if _, taken := usedSlugs[slug]; !taken {
					break
				}
			}
		}
		usedSlugs[slug] = 1
		outPath := filepath.Join(flags.outDir, slug+".db")

		// Per evaluator iter-1 feedback item 3: scan-many's
		// per-repo .db contract (architecture S9.4) is sqlite-only.
		// Pin --store=sqlite for the per-entry run regardless of
		// the operator's root --store choice; the memory/postgres
		// backends would (mis)treat the .db path as a JSON-export
		// path or as a Postgres DSN respectively.
		perRoot := *root
		perRoot.store = "sqlite"
		perRoot.db = "" // each entry's --out is the source of truth

		perFlags := &scanFlags{
			sha: e.SHA,
			out: outPath,
		}
		// Per evaluator iter-1 feedback item 1: route the per-repo
		// summary directly to the outer stdout. The previous
		// (nil-stdout) wiring sent the summary to os.Stdout AND
		// we then re-wrote it via writeScanSummary, duplicating
		// every successful entry's block.
		perRunner := scanRunner{stdout: stdout}
		sum, err := exec(ctx, &perRoot, perFlags, e.Input, perRunner)
		if err != nil {
			agg.failed++
			agg.failures = append(agg.failures, manyFailure{
				Line:   e.Line,
				Input:  e.Input,
				SHA:    e.SHA,
				Reason: err.Error(),
			})
			// Per evaluator iter-1 feedback item 2: clean up the
			// (possibly empty) .db that the SQLite sink may have
			// created before the materialize / walk failed. The
			// brief mandates one .db per *succeeded* repo; a
			// failure must NOT leave an artifact behind.
			if rmErr := os.Remove(outPath); rmErr != nil && !os.IsNotExist(rmErr) {
				slog.Warn("scan-many.cleanup_failed_artifact_failed",
					"path", outPath,
					"error", rmErr.Error())
			}
			writeManyFailureLine(stdout, e, err, root.logFormat)
			continue
		}
		agg.succeeded++
		agg.add(sum)
		// Per-repo summary is already written by runScanWithSummary
		// through perRunner.stdout (== stdout). Do NOT re-render
		// it here (evaluator iter-1 feedback item 1).
	}

	if err := writeManyAggregate(stdout, agg, root.logFormat); err != nil {
		return fmt.Errorf("scan-many: write aggregate: %w", err)
	}

	if agg.failed > 0 {
		return fmt.Errorf("scan-many: %d of %d entries failed", agg.failed, agg.succeeded+agg.failed)
	}
	return nil
}

// ----- manifest parser -------------------------------------------

// shaPattern matches a hex SHA of git-typical length. The
// boundary between a git URL and its trailing SHA is the LAST
// `@` whose suffix matches this pattern -- the scp form
// (`git@host:owner/repo.git`) already contains an `@` and we
// MUST NOT mistake the user portion for a SHA.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// parseManifest reads a manifest stream and returns the
// non-blank, non-comment entries in order. Each entry is either
// a path or a `<url>@<sha>` form.
func parseManifest(r io.Reader) ([]manifestEntry, error) {
	var entries []manifestEntry
	s := bufio.NewScanner(r)
	// Allow long lines (git URLs + SHA can exceed the default
	// 64 KiB token only in pathological cases, but bump anyway
	// so the manifest parser is never the limiting factor).
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for s.Scan() {
		lineNo++
		raw := s.Text()
		e, ok := parseManifestLine(raw)
		if !ok {
			continue
		}
		e.Line = lineNo
		entries = append(entries, e)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// parseManifestLine parses one raw manifest line. Returns
// `ok=false` for blank lines and `#`-comments. For valid lines
// the returned entry's `Line` field is zero (the caller sets
// it).
func parseManifestLine(raw string) (manifestEntry, bool) {
	// Strip a trailing CR so CRLF manifests are handled.
	line := strings.TrimRight(raw, "\r")
	line = strings.TrimSpace(line)
	if line == "" {
		return manifestEntry{}, false
	}
	if strings.HasPrefix(line, "#") {
		return manifestEntry{}, false
	}
	// Split at the LAST `@` only when the suffix looks like a
	// git SHA (7..40 hex chars). This keeps `git@host:owner/repo`
	// (scp form, no SHA) intact AND lets `https://h/r.git@<sha>`
	// pick up the SHA correctly.
	if i := strings.LastIndex(line, "@"); i > 0 && i < len(line)-1 {
		suffix := line[i+1:]
		if shaPattern.MatchString(suffix) {
			return manifestEntry{
				Input: strings.TrimSpace(line[:i]),
				SHA:   suffix,
			}, true
		}
	}
	return manifestEntry{Input: line}, true
}

// ----- per-entry slugging ----------------------------------------

func slugForEntry(e manifestEntry) string {
	kind := detectInputKind(e.Input)
	slug := repoBaseName(e.Input, kind)
	if slug == "" {
		slug = fmt.Sprintf("repo-line-%d", e.Line)
	}
	return slug
}

// ----- aggregate / summary rendering ------------------------------

type manyFailure struct {
	Line   int    `json:"line"`
	Input  string `json:"input"`
	SHA    string `json:"sha,omitempty"`
	Reason string `json:"reason"`
}

type manyAggregate struct {
	succeeded int
	failed    int
	walked    int
	parsed    int
	nodes     map[string]int
	edges     map[string]int
	failures  []manyFailure
}

func newManyAggregate() *manyAggregate {
	return &manyAggregate{
		nodes: make(map[string]int),
		edges: make(map[string]int),
	}
}

func (a *manyAggregate) add(s scanSummary) {
	a.walked += s.Walked
	a.parsed += s.Parsed
	for k, v := range s.Nodes {
		a.nodes[k] += v
	}
	for k, v := range s.Edges {
		a.edges[k] += v
	}
}

// manyAggregateJSON is the wire shape for --log=json: matches
// the text-format keys 1:1 so log scrapers do not need a
// branch.
type manyAggregateJSON struct {
	Succeeded int            `json:"succeeded"`
	Failed    int            `json:"failed"`
	Walked    int            `json:"walked"`
	Parsed    int            `json:"parsed"`
	Nodes     map[string]int `json:"nodes"`
	Edges     map[string]int `json:"edges"`
	Failures  []manyFailure  `json:"failures,omitempty"`
}

func writeManyAggregate(w io.Writer, a *manyAggregate, format string) error {
	if strings.ToLower(format) == "json" {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(manyAggregateJSON{
			Succeeded: a.succeeded,
			Failed:    a.failed,
			Walked:    a.walked,
			Parsed:    a.parsed,
			Nodes:     a.nodes,
			Edges:     a.edges,
			Failures:  a.failures,
		})
	}
	fmt.Fprintln(w, "scan-many aggregate:")
	fmt.Fprintf(w, "  succeeded: %d\n", a.succeeded)
	fmt.Fprintf(w, "  failed:    %d\n", a.failed)
	fmt.Fprintf(w, "  walked:    %d\n", a.walked)
	fmt.Fprintf(w, "  parsed:    %d\n", a.parsed)
	fmt.Fprintf(w, "  nodes:     %s\n", formatKindMap(a.nodes))
	fmt.Fprintf(w, "  edges:     %s\n", formatKindMap(a.edges))
	if len(a.failures) > 0 {
		// Stable order: by manifest line number.
		sorted := append([]manyFailure(nil), a.failures...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Line < sorted[j].Line })
		fmt.Fprintln(w, "  failures:")
		for _, f := range sorted {
			fmt.Fprintf(w, "    line %d: %s -- %s\n", f.Line, f.Input, f.Reason)
		}
	}
	return nil
}

// writeManyFailureLine emits the per-repo `failed: <reason>`
// marker the brief mandates. JSON mode emits a one-line JSON
// object so log scrapers do not need a format branch.
func writeManyFailureLine(w io.Writer, e manifestEntry, err error, format string) {
	if strings.ToLower(format) == "json" {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]any{
			"event":  "scan-many.entry_failed",
			"line":   e.Line,
			"input":  e.Input,
			"sha":    e.SHA,
			"failed": err.Error(),
		})
		return
	}
	if e.SHA != "" {
		fmt.Fprintf(w, "entry line %d (%s@%s): failed: %s\n", e.Line, e.Input, e.SHA, err.Error())
	} else {
		fmt.Fprintf(w, "entry line %d (%s): failed: %s\n", e.Line, e.Input, err.Error())
	}
}
