package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

const scanPlaceholderSHA = "0000000000000000000000000000000000000000"

func newScanCmdReal(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "scan <path|git-url>",
		Short: "Scan a single repository (local path or git URL)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.store != "sqlite" {
				return fmt.Errorf("codeintel scan: only --store=sqlite is supported in this build")
			}
			return runScan(cmd.Context(), flags, args[0])
		},
	}
}

func runScan(ctx context.Context, flags *rootFlags, target string) error {
	absPath, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve path %s: %w", target, err)
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("scan target must be an existing directory: %s", absPath)
	}

	dbPath := flags.db
	if dbPath == "" {
		dbPath = "polyglot.db"
	}

	sink, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	defer sink.Close()

	repoURL := "file:///" + filepath.ToSlash(absPath)
	repoID, err := fingerprint.RepoIDFromURL(repoURL)
	if err != nil {
		return fmt.Errorf("compute repo ID: %w", err)
	}

	_, err = sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            repoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: scanPlaceholderSHA,
		RepoID:         repoID,
	})
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}

	_, err = sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         scanPlaceholderSHA,
		CommittedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("ensure commit: %w", err)
	}

	// Insert the root repo node in the graph.
	repoNodeRec, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "repo",
		CanonicalSignature: repoURL + "::repo",
		FromSHA:            scanPlaceholderSHA,
	})
	if err != nil {
		return fmt.Errorf("insert repo node: %w", err)
	}
	repoNodeID := repoNodeRec.NodeID

	dispatcher := ast.NewDispatcher(sink)

	packages := make(map[string]string) // relDir → nodeID
	var fileCount, nodeCount int

	walkErr := filepath.WalkDir(absPath, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(absPath, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}

		// Ensure package node for the file's directory.
		parentNodeID := repoNodeID
		if dir != "" {
			if _, ok := packages[dir]; !ok {
				pkgRec, err := sink.InsertNode(ctx, graphwriter.NodeInput{
					RepoID:             repoID,
					Kind:               "package",
					CanonicalSignature: repoURL + "::" + dir,
					ParentNodeID:       repoNodeID,
					FromSHA:            scanPlaceholderSHA,
					AttrsJSON:          mustJSON(map[string]string{"path": dir}),
				})
				if err != nil {
					slog.Warn("scan: insert package", "dir", dir, "err", err)
					return nil
				}
				packages[dir] = pkgRec.NodeID
				nodeCount++
				if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
					RepoID:    repoID,
					Kind:      "contains",
					SrcNodeID: repoNodeID,
					DstNodeID: pkgRec.NodeID,
					FromSHA:   scanPlaceholderSHA,
				}); err != nil {
					slog.Warn("scan: contains edge", "err", err)
				}
			}
			parentNodeID = packages[dir]
		}

		// Insert file node.
		fileRec, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "file",
			CanonicalSignature: repoURL + "::" + rel,
			ParentNodeID:       parentNodeID,
			FromSHA:            scanPlaceholderSHA,
			AttrsJSON:          mustJSON(map[string]string{"path": rel}),
		})
		if err != nil {
			slog.Warn("scan: insert file", "file", rel, "err", err)
			return nil
		}
		nodeCount++
		fileCount++

		if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "contains",
			SrcNodeID: parentNodeID,
			DstNodeID: fileRec.NodeID,
			FromSHA:   scanPlaceholderSHA,
		}); err != nil {
			slog.Warn("scan: contains edge", "err", err)
		}

		// Dispatch AST parsing for the file.
		absFile, _ := filepath.Abs(p)
		_, _ = dispatcher.EmitFile(ctx, repoindexer.EmitFileEvent{
			RepoID:     repoID,
			RepoURL:    repoURL,
			SHA:        scanPlaceholderSHA,
			FileNodeID: fileRec.NodeID,
			RepoNodeID: repoNodeID,
			RelPath:    rel,
			AbsPath:    absFile,
			Open: func() (repoindexer.ReadCloser, error) {
				return os.Open(absFile)
			},
		})

		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk %s: %w", absPath, walkErr)
	}

	if err := sink.Flush(ctx); err != nil {
		return fmt.Errorf("flush: %w", err)
	}

	slog.Info("scan complete",
		"store", dbPath,
		"files", fileCount,
		"nodes", nodeCount,
	)
	return nil
}

func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
