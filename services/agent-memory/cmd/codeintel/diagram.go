package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/diagram"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

func newDiagramModuleCmd(flags *rootFlags) *cobra.Command {
	var granularity string
	cmd := &cobra.Command{
		Use:   "module",
		Short: "Build the top-down module/component diagram",
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.store != "sqlite" {
				return fmt.Errorf("codeintel diagram module: only --store=sqlite is supported in this build")
			}
			return runDiagramModule(cmd.Context(), flags, granularity, cmd)
		},
	}
	cmd.Flags().StringVar(&granularity, "granularity", "package",
		"diagram granularity: package|file|class")
	return cmd
}

func runDiagramModule(ctx context.Context, flags *rootFlags, granularity string, cmd *cobra.Command) error {
	dbPath := flags.db
	if dbPath == "" {
		return fmt.Errorf("--db is required: path to the SQLite graph store")
	}

	sink, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	defer sink.Close()

	repos, err := sink.ListRepos(ctx, graphreader.ReaderOptions{})
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}
	if len(repos) == 0 {
		return fmt.Errorf("no repos found in %s", dbPath)
	}

	repoID, err := fingerprint.ParseRepoID(repos[0].RepoID)
	if err != nil {
		return fmt.Errorf("parse repo ID %q: %w", repos[0].RepoID, err)
	}

	d, err := diagram.BuildModuleDiagram(ctx, sink, repoID, granularity)
	if err != nil {
		return fmt.Errorf("build module diagram: %w", err)
	}

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return fmt.Errorf("encode diagram: %w", err)
	}
	return nil
}

func newDiagramCallsCmd(_ *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "calls",
		Short: "Build the left-right call-chain diagram from a seed",
		RunE: func(cmd *cobra.Command, args []string) error {
			return notImplemented("diagram calls")
		},
	}
}

func newDiagramCmdReal(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diagram",
		Short: "Project diagrams from a previously-scanned store",
	}
	cmd.AddCommand(
		newDiagramModuleCmd(flags),
		newDiagramCallsCmd(flags),
	)
	return cmd
}

// writeJSON writes the encoded JSON to the given file, or stdout if path is "-".
func writeJSON(path string, v interface{}) error {
	var w *os.File
	if path == "" || path == "-" {
		w = os.Stdout
	} else {
		var err error
		w, err = os.Create(path)
		if err != nil {
			return err
		}
		defer w.Close()
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
