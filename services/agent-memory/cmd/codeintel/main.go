// Command codeintel is the developer-facing CLI for the
// repo-scanner. This file scaffolds the cobra root command and
// the four leaf subcommands (`scan`, `scan-many`, `diagram`,
// `serve`). The subcommands intentionally return
// `errors.New("not implemented")` here -- their real bodies are
// landed in subsequent workstreams under the same phase.
package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// errNotImplemented is returned verbatim by every scaffolded
// subcommand in this stage so test fixtures and downstream
// callers can `errors.Is` against a single sentinel. Stage 5.2+
// workstreams replace each RunE body with the real
// implementation and stop returning this sentinel.
var errNotImplemented = errors.New("not implemented")

type rootFlags struct {
	store          string
	db             string
	logFormat      string
	withEmbeddings bool
}

// Build-time metadata stamped via -ldflags
//
//	-X main.version=v0.1.0 -X main.commit=<sha> -X main.buildDate=<iso8601>
//
// Defaults make `codeintel version` legible even on a plain
// `go build` without ldflags.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func defaultRootFlags() rootFlags {
	return rootFlags{
		store:          "sqlite",
		db:             "",
		logFormat:      "text",
		withEmbeddings: false,
	}
}

var validLogFormats = map[string]struct{}{
	"text": {},
	"json": {},
}

var validStores = map[string]struct{}{
	"sqlite":   {},
	"postgres": {},
	"memory":   {},
}

func newRootCmd(out, errOut io.Writer) *cobra.Command {
	flags := defaultRootFlags()

	cmd := &cobra.Command{
		Use:   "codeintel",
		Short: "Scan repositories and generate code-intelligence diagrams",
		Long: "codeintel drives the agent-memory AST dispatcher against " +
			"local or remote repositories, persists the resulting graph " +
			"to a pluggable store, and projects module/call-chain " +
			"diagrams for the React UI.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			format := strings.ToLower(flags.logFormat)
			if _, ok := validLogFormats[format]; !ok {
				return fmt.Errorf("invalid --log %q: must be one of text|json", flags.logFormat)
			}
			if _, ok := validStores[flags.store]; !ok {
				return fmt.Errorf("invalid --store %q: must be one of sqlite|postgres|memory", flags.store)
			}
			slog.SetDefault(newLogger(errOut, format))
			return nil
		},
	}

	cmd.SetOut(out)
	cmd.SetErr(errOut)

	cmd.PersistentFlags().StringVar(&flags.store, "store", flags.store,
		"graph store backend: sqlite|postgres|memory")
	cmd.PersistentFlags().StringVar(&flags.db, "db", flags.db,
		"store connection string or file path (sqlite: file path; postgres: DSN; memory: ignored)")
	cmd.PersistentFlags().StringVar(&flags.logFormat, "log", flags.logFormat,
		"log handler format: text|json")
	cmd.PersistentFlags().BoolVar(&flags.withEmbeddings, "with-embeddings", flags.withEmbeddings,
		"opt in to the embedding publisher (requires the recall stack)")

	cmd.AddCommand(
		newScanCmdImpl(&flags),
		newScanManyCmd(&flags),
		newDiagramCmdReal(&flags),
		newServeCmd(&flags),
		newVersionCmd(),
	)

	// Cobra's built-in --version flag template, populated from
	// the same ldflags-driven vars as the `version` subcommand
	// so `codeintel --version` and `codeintel version` agree.
	cmd.Version = version
	cmd.SetVersionTemplate(versionString() + "\n")

	return cmd
}

func newLogger(w io.Writer, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts))
	default:
		return slog.New(slog.NewTextHandler(w, opts))
	}
}

func notImplemented(name string) error {
	// Emit a single info-level log line before returning so the
	// configured slog handler (text by default, JSON when
	// --log=json) actually exercises the wiring. Stage 5.1
	// scenario `log-flag-respected` covers this.
	slog.Info("codeintel subcommand invoked", "subcommand", name)
	return errNotImplemented
}

func newScanManyCmd(_ *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "scan-many <manifest>",
		Short: "Scan many repositories listed in a manifest file",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("scan-many")
		},
	}
}

func newServeCmd(_ *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve diagram JSON over HTTP for the React UI",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("serve")
		},
	}
}

// versionString renders the multi-line `version` output. Kept
// as a top-level helper so both `codeintel version` and the
// cobra root `--version` template share one formatter.
func versionString() string {
	return fmt.Sprintf("codeintel %s\n  commit:     %s\n  build date: %s",
		version, commit, buildDate)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the codeintel version, commit, and build date",
		// version bypasses the root command's PersistentPreRunE
		// so it answers "what build is this?" even when --store
		// or --log have invalid values. Cobra runs the nearest
		// parent's PersistentPreRunE; defining a no-op here
		// shadows the root's validator chain.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), versionString())
			return nil
		},
	}
}

func main() {
	root := newRootCmd(os.Stdout, os.Stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "codeintel:", err)
		os.Exit(1)
	}
}
