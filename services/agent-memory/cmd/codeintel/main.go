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

type rootFlags struct {
	store          string
	db             string
	logFormat      string
	withEmbeddings bool
}

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
		newScanCmd(&flags),
		newScanManyCmd(&flags),
		newDiagramCmd(&flags),
		newServeCmd(&flags),
	)

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
	return fmt.Errorf("%s: %w", name, errors.New("not implemented"))
}

func newScanCmd(_ *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "scan <path|git-url>",
		Short: "Scan a single repository (local path or git URL)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("scan")
		},
	}
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

func newDiagramCmd(_ *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diagram",
		Short: "Project diagrams from a previously-scanned store",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("diagram")
		},
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "module",
			Short: "Build the top-down module/component diagram",
			RunE: func(cmd *cobra.Command, args []string) error {
				return notImplemented("diagram module")
			},
		},
		&cobra.Command{
			Use:   "calls",
			Short: "Build the left-right call-chain diagram from a seed",
			RunE: func(cmd *cobra.Command, args []string) error {
				return notImplemented("diagram calls")
			},
		},
	)
	return cmd
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

func main() {
	root := newRootCmd(os.Stdout, os.Stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "codeintel:", err)
		os.Exit(1)
	}
}
