//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const sqliteReaderRepoURL = "https://github.com/example/sqlite-reader-adapter"

func sqliteReaderRepoID() (fingerprint.RepoID, error) {
	return fingerprint.RepoIDFromURL(sqliteReaderRepoURL)
}

// ---------------------------------------------------------------------------
// Scenario: sqlite-list-nodes-by-parent
// ---------------------------------------------------------------------------

type sqliteListNodesByParentState struct {
	sink       *sqlite.Sink
	dbDir      string
	repoID     fingerprint.RepoID
	repoNodeID string
	nodes      []graphreader.Node
}

func (s *sqliteListNodesByParentState) givenRepoWithTwoPackageChildren(ctx context.Context) error {
	repoID, err := sqliteReaderRepoID()
	if err != nil {
		return err
	}
	s.repoID = repoID

	dir, err := os.MkdirTemp("", "sqlite-reader-parent-*")
	if err != nil {
		return err
	}
	s.dbDir = dir

	sink, err := sqlite.Open(context.Background(), filepath.Join(dir, "graph.db"))
	if err != nil {
		return fmt.Errorf("sqlite.Open: %w", err)
	}
	s.sink = sink

	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            sqliteReaderRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: "abc123",
		RepoID:         repoID,
	}); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}

	repoNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "repo",
		CanonicalSignature: sqliteReaderRepoURL,
		FromSHA:            "abc123",
	})
	if err != nil {
		return fmt.Errorf("InsertNode repo: %w", err)
	}
	s.repoNodeID = repoNode.NodeID

	// Two package children of the repo node.
	for _, sig := range []string{"pkg://alpha", "pkg://beta"} {
		if _, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "package",
			CanonicalSignature: sig,
			ParentNodeID:       repoNode.NodeID,
			FromSHA:            "abc123",
		}); err != nil {
			return fmt.Errorf("InsertNode package %s: %w", sig, err)
		}
	}

	// A file node that is NOT a package — must be excluded.
	if _, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://alpha/main.go",
		ParentNodeID:       repoNode.NodeID,
		FromSHA:            "abc123",
	}); err != nil {
		return fmt.Errorf("InsertNode file: %w", err)
	}

	return nil
}

func (s *sqliteListNodesByParentState) whenListNodesRuns(ctx context.Context) error {
	var err error
	s.nodes, err = s.sink.ListNodes(ctx, s.repoID, []string{"package"},
		graphreader.ListNodesFilter{ParentNodeID: s.repoNodeID},
		graphreader.ReaderOptions{})
	if err != nil {
		return fmt.Errorf("ListNodes: %w", err)
	}
	return nil
}

func (s *sqliteListNodesByParentState) thenTwoPackagesReturned(ctx context.Context) error {
	if len(s.nodes) != 2 {
		return fmt.Errorf("expected 2 packages, got %d", len(s.nodes))
	}
	for _, n := range s.nodes {
		if n.Kind != "package" {
			return fmt.Errorf("unexpected kind %q in result", n.Kind)
		}
		if n.ParentNodeID != s.repoNodeID {
			return fmt.Errorf("node %s parent=%q, want %q", n.NodeID, n.ParentNodeID, s.repoNodeID)
		}
	}
	return nil
}

func (s *sqliteListNodesByParentState) cleanup() {
	if s.sink != nil {
		_ = s.sink.Close()
	}
	if s.dbDir != "" {
		_ = os.RemoveAll(s.dbDir)
	}
}

// ---------------------------------------------------------------------------
// Scenario: sqlite-list-edges-from
// ---------------------------------------------------------------------------

type sqliteListEdgesFromState struct {
	sink        *sqlite.Sink
	dbDir       string
	repoID      fingerprint.RepoID
	callerID    string
	edges       []graphreader.Edge
}

func (s *sqliteListEdgesFromState) givenMethodWithThreeStaticCalls(ctx context.Context) error {
	repoID, err := sqliteReaderRepoID()
	if err != nil {
		return err
	}
	s.repoID = repoID

	dir, err := os.MkdirTemp("", "sqlite-reader-edges-*")
	if err != nil {
		return err
	}
	s.dbDir = dir

	sink, err := sqlite.Open(context.Background(), filepath.Join(dir, "graph.db"))
	if err != nil {
		return fmt.Errorf("sqlite.Open: %w", err)
	}
	s.sink = sink

	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            sqliteReaderRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: "abc123",
		RepoID:         repoID,
	}); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}

	caller, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "method",
		CanonicalSignature: "func://caller.Run",
		FromSHA:            "abc123",
	})
	if err != nil {
		return fmt.Errorf("InsertNode caller: %w", err)
	}
	s.callerID = caller.NodeID

	// Three callee method nodes + static_calls edges.
	for i := 0; i < 3; i++ {
		callee, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "method",
			CanonicalSignature: fmt.Sprintf("func://callee.Method%d", i),
			FromSHA:            "abc123",
		})
		if err != nil {
			return fmt.Errorf("InsertNode callee %d: %w", i, err)
		}
		if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    repoID,
			Kind:      "static_calls",
			SrcNodeID: caller.NodeID,
			DstNodeID: callee.NodeID,
			FromSHA:   "abc123",
		}); err != nil {
			return fmt.Errorf("InsertEdge %d: %w", i, err)
		}
	}

	// An unrelated edge kind — must be excluded by the kind filter.
	otherNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://other.go",
		FromSHA:            "abc123",
	})
	if err != nil {
		return fmt.Errorf("InsertNode other: %w", err)
	}
	if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    repoID,
		Kind:      "contains",
		SrcNodeID: caller.NodeID,
		DstNodeID: otherNode.NodeID,
		FromSHA:   "abc123",
	}); err != nil {
		return fmt.Errorf("InsertEdge contains: %w", err)
	}

	return nil
}

func (s *sqliteListEdgesFromState) whenListEdgesFromRuns(ctx context.Context) error {
	var err error
	s.edges, err = s.sink.ListEdgesFrom(ctx, s.callerID, []string{"static_calls"},
		graphreader.ReaderOptions{})
	if err != nil {
		return fmt.Errorf("ListEdgesFrom: %w", err)
	}
	return nil
}

func (s *sqliteListEdgesFromState) thenExactlyThreeEdges(ctx context.Context) error {
	if len(s.edges) != 3 {
		return fmt.Errorf("expected 3 edges, got %d", len(s.edges))
	}
	for _, e := range s.edges {
		if e.Kind != "static_calls" {
			return fmt.Errorf("unexpected edge kind %q", e.Kind)
		}
		if e.SrcNodeID != s.callerID {
			return fmt.Errorf("edge %s src=%q, want %q", e.EdgeID, e.SrcNodeID, s.callerID)
		}
	}
	return nil
}

func (s *sqliteListEdgesFromState) cleanup() {
	if s.sink != nil {
		_ = s.sink.Close()
	}
	if s.dbDir != "" {
		_ = os.RemoveAll(s.dbDir)
	}
}

// ---------------------------------------------------------------------------
// Scenario: sqlite-maxlistlimit-clamp
// ---------------------------------------------------------------------------

type sqliteMaxListLimitState struct {
	sink      *sqlite.Sink
	dbDir     string
	repoID    fingerprint.RepoID
	nodes     []graphreader.Node
	logBuf    bytes.Buffer
	requested int
}

func (s *sqliteMaxListLimitState) given15000MethodNodes(ctx context.Context) error {
	repoID, err := sqliteReaderRepoID()
	if err != nil {
		return err
	}
	s.repoID = repoID

	dir, err := os.MkdirTemp("", "sqlite-reader-clamp-*")
	if err != nil {
		return err
	}
	s.dbDir = dir

	sink, err := sqlite.Open(context.Background(), filepath.Join(dir, "graph.db"))
	if err != nil {
		return fmt.Errorf("sqlite.Open: %w", err)
	}
	s.sink = sink

	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            sqliteReaderRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: "abc123",
		RepoID:         repoID,
	}); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}

	// Bulk-insert 15000 method nodes.
	if err := sink.Flush(ctx); err != nil {
		return fmt.Errorf("pre-Flush: %w", err)
	}
	for i := 0; i < 15000; i++ {
		if _, err := sink.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "method",
			CanonicalSignature: fmt.Sprintf("func://bulk.Method%05d", i),
			FromSHA:            "abc123",
		}); err != nil {
			return fmt.Errorf("InsertNode %d: %w", i, err)
		}
	}

	return nil
}

func (s *sqliteMaxListLimitState) whenListNodesWithLimit20000(ctx context.Context) error {
	s.requested = 20000

	// Install a structured JSON logger so we can capture the clamp
	// log emitted by the SQLite reader's ListNodes when the
	// requested limit exceeds MaxListLimit.
	original := slog.Default()
	defer slog.SetDefault(original)
	handler := slog.NewJSONHandler(&s.logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))

	var err error
	s.nodes, err = s.sink.ListNodes(ctx, s.repoID, []string{"method"},
		graphreader.ListNodesFilter{Limit: s.requested},
		graphreader.ReaderOptions{})
	if err != nil {
		return fmt.Errorf("ListNodes: %w", err)
	}

	return nil
}

func (s *sqliteMaxListLimitState) thenExactly10000Returned(ctx context.Context) error {
	want := graphreader.MaxListLimit // 10_000
	if len(s.nodes) != want {
		return fmt.Errorf("expected %d nodes (MaxListLimit), got %d", want, len(s.nodes))
	}

	// Assert the structured log emitted by the SQLite reader (SUT)
	// in reader.go ListNodes contains the clamp record with the
	// correct fields: requested > MaxListLimit, effective ==
	// MaxListLimit, clamped == true.
	logData := s.logBuf.Bytes()
	if len(logData) == 0 {
		return fmt.Errorf("no structured log output captured; expected a limit_clamped record")
	}

	// The JSON handler writes one JSON object per line. Scan for the
	// clamp record.
	found := false
	for _, line := range bytes.Split(logData, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		if msg != "graphsink.reader.limit_clamped" {
			continue
		}
		requested, _ := rec["requested"].(float64)
		effective, _ := rec["effective"].(float64)
		clamped, _ := rec["clamped"].(bool)

		if int(requested) != s.requested {
			return fmt.Errorf("clamp log requested=%v, want %d", requested, s.requested)
		}
		if int(effective) != graphreader.MaxListLimit {
			return fmt.Errorf("clamp log effective=%v, want %d", effective, graphreader.MaxListLimit)
		}
		if !clamped {
			return fmt.Errorf("clamp log clamped=%v, want true", clamped)
		}
		found = true
		break
	}
	if !found {
		return fmt.Errorf("structured log missing graphsink.reader.limit_clamped record; log output: %s", logData)
	}

	return nil
}

func (s *sqliteMaxListLimitState) cleanup() {
	if s.sink != nil {
		_ = s.sink.Close()
	}
	if s.dbDir != "" {
		_ = os.RemoveAll(s.dbDir)
	}
}

// ---------------------------------------------------------------------------
// Initializer + test entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_graphsink_storage_abstraction_sqlite_reader_adapter(ctx *godog.ScenarioContext) {
	// Scenario: sqlite-list-nodes-by-parent
	parentState := &sqliteListNodesByParentState{}
	ctx.Step(`^a repo with two package children in a SQLite sink$`, parentState.givenRepoWithTwoPackageChildren)
	ctx.Step(`^ListNodes with kinds package and ParentNodeID equal to the repo node runs$`, parentState.whenListNodesRuns)
	ctx.Step(`^the two packages are returned and nothing else$`, parentState.thenTwoPackagesReturned)
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if sc.Name == "sqlite-list-nodes-by-parent" {
			parentState.cleanup()
		}
		return ctx, nil
	})

	// Scenario: sqlite-list-edges-from
	edgesState := &sqliteListEdgesFromState{}
	ctx.Step(`^a method with three outbound static_calls edges in a SQLite sink$`, edgesState.givenMethodWithThreeStaticCalls)
	ctx.Step(`^ListEdgesFrom with srcNodeID and kinds static_calls runs$`, edgesState.whenListEdgesFromRuns)
	ctx.Step(`^exactly three Edges are returned$`, edgesState.thenExactlyThreeEdges)
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if sc.Name == "sqlite-list-edges-from" {
			edgesState.cleanup()
		}
		return ctx, nil
	})

	// Scenario: sqlite-maxlistlimit-clamp
	clampState := &sqliteMaxListLimitState{}
	ctx.Step(`^15000 Nodes of kind method in a SQLite sink$`, clampState.given15000MethodNodes)
	ctx.Step(`^ListNodes with kinds method and Limit 20000 runs$`, clampState.whenListNodesWithLimit20000)
	ctx.Step(`^exactly 10000 are returned and a structured log records the clamp$`, clampState.thenExactly10000Returned)
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if sc.Name == "sqlite-maxlistlimit-clamp" {
			clampState.cleanup()
		}
		return ctx, nil
	})
}

func TestE2E_graphsink_storage_abstraction_sqlite_reader_adapter(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_graphsink_storage_abstraction_sqlite_reader_adapter,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"graphsink_storage_abstraction_sqlite_reader_adapter.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero exit from godog suite")
	}
}
