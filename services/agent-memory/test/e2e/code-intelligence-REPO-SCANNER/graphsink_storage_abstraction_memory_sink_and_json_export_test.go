//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/memory"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Shared test fixtures
// ---------------------------------------------------------------------------

const memorySinkRepoURL = "https://github.com/example/scan-target"

func memorySinkRepoID() (fingerprint.RepoID, error) {
	return fingerprint.RepoIDFromURL(memorySinkRepoURL)
}

func memorySinkEnsureRepo(s *memory.Sink) (graphwriter.RepoRecord, error) {
	repoID, err := memorySinkRepoID()
	if err != nil {
		return graphwriter.RepoRecord{}, err
	}
	return s.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL:            memorySinkRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: "deadbeef",
		LanguageHints:  []string{"go", "ts"},
		RepoID:         repoID,
	})
}

// ---------------------------------------------------------------------------
// Scenario: memory-idempotent
// ---------------------------------------------------------------------------

type memoryIdempotentState struct {
	sink         *memory.Sink
	firstNodeID  string
	secondNodeID string
	firstInserted  bool
	secondInserted bool
	nodesLen     int
}

func (st *memoryIdempotentState) theSameNodeInputInsertedTwice() error {
	st.sink = memory.New(memory.Options{})
	if _, err := memorySinkEnsureRepo(st.sink); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}
	return nil
}

func (st *memoryIdempotentState) insertNodeRunsBothTimes() error {
	repoID, err := memorySinkRepoID()
	if err != nil {
		return err
	}
	ctx := context.Background()
	in := graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://example/foo.go",
		FromSHA:            "deadbeef",
		AttrsJSON:          json.RawMessage(`{"lang":"go"}`),
	}
	first, err := st.sink.InsertNode(ctx, in)
	if err != nil {
		return fmt.Errorf("first InsertNode: %w", err)
	}
	st.firstNodeID = first.NodeID
	st.firstInserted = first.Inserted

	second, err := st.sink.InsertNode(ctx, in)
	if err != nil {
		return fmt.Errorf("second InsertNode: %w", err)
	}
	st.secondNodeID = second.NodeID
	st.secondInserted = second.Inserted

	// Capture the internal nodes slice length via Snapshot.
	snap, err := st.sink.Snapshot()
	if err != nil {
		return fmt.Errorf("Snapshot: %w", err)
	}
	st.nodesLen = len(snap.Nodes)
	return nil
}

func (st *memoryIdempotentState) theSecondCallReturnsCachedIDAndSliceLenIs1() error {
	if !st.firstInserted {
		return fmt.Errorf("first InsertNode should set Inserted=true")
	}
	if st.secondInserted {
		return fmt.Errorf("second InsertNode should set Inserted=false (cached re-emit)")
	}
	if st.secondNodeID != st.firstNodeID {
		return fmt.Errorf("re-emit returned different NodeID: first=%s second=%s",
			st.firstNodeID, st.secondNodeID)
	}
	if st.nodesLen != 1 {
		return fmt.Errorf("nodes slice grew on re-emit: len=%d (want 1)", st.nodesLen)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: json-export-key-order
// ---------------------------------------------------------------------------

type jsonExportKeyOrderState struct {
	exportPath string
	sink       *memory.Sink
	exportData []byte
}

func (st *jsonExportKeyOrderState) aScanWithExportPath() error {
	dir, err := os.MkdirTemp("", "e2e-json-export-*")
	if err != nil {
		return err
	}
	st.exportPath = filepath.Join(dir, "graph.json")
	st.sink = memory.New(memory.Options{
		ExportPath: st.exportPath,
		Now: func() time.Time {
			return time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
		},
	})
	if _, err := memorySinkEnsureRepo(st.sink); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}
	repoID, err := memorySinkRepoID()
	if err != nil {
		return err
	}
	ctx := context.Background()
	repoNode, err := st.sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "repo",
		CanonicalSignature: memorySinkRepoURL, FromSHA: "deadbeef",
	})
	if err != nil {
		return fmt.Errorf("InsertNode repo: %w", err)
	}
	_, err = st.sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://example/foo.go",
		ParentNodeID:       repoNode.NodeID,
		FromSHA:            "deadbeef",
	})
	if err != nil {
		return fmt.Errorf("InsertNode file: %w", err)
	}
	return nil
}

func (st *jsonExportKeyOrderState) closeWritesExport() error {
	if err := st.sink.Close(); err != nil {
		return fmt.Errorf("Close: %w", err)
	}
	data, err := os.ReadFile(st.exportPath)
	if err != nil {
		return fmt.Errorf("ReadFile: %w", err)
	}
	st.exportData = data
	// Clean up temp dir on success.
	defer os.RemoveAll(filepath.Dir(st.exportPath))
	return nil
}

func (st *jsonExportKeyOrderState) topLevelKeysAreRepoNodesEdges() error {
	keys, err := streamTopLevelKeys(st.exportData)
	if err != nil {
		return err
	}
	want := []string{"repo", "nodes", "edges"}
	if len(keys) != len(want) {
		return fmt.Errorf("got %d top-level keys (%v), want %d (%v)",
			len(keys), keys, len(want), want)
	}
	for i, k := range want {
		if keys[i] != k {
			return fmt.Errorf("top-level key[%d] = %q, want %q (full: %v)",
				i, keys[i], k, keys)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: roundtrip-via-loadexport
// ---------------------------------------------------------------------------

type roundtripState struct {
	exportPath string
	wantNodes  int
	wantEdges  int
	gotNodes   int
	gotEdges   int
}

func (st *roundtripState) aMemorySinkScanWrittenToDisk() error {
	dir, err := os.MkdirTemp("", "e2e-roundtrip-*")
	if err != nil {
		return err
	}
	st.exportPath = filepath.Join(dir, "graph.json")
	src := memory.New(memory.Options{ExportPath: st.exportPath})
	if _, err := memorySinkEnsureRepo(src); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}
	repoID, err := memorySinkRepoID()
	if err != nil {
		return err
	}
	ctx := context.Background()

	repoNode, err := src.InsertNode(ctx, graphwriter.NodeInput{
		RepoID: repoID, Kind: "repo",
		CanonicalSignature: memorySinkRepoURL, FromSHA: "deadbeef",
	})
	if err != nil {
		return fmt.Errorf("InsertNode repo: %w", err)
	}
	pkgNode, err := src.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "package",
		CanonicalSignature: "pkg://example",
		ParentNodeID:       repoNode.NodeID,
		FromSHA:            "deadbeef",
	})
	if err != nil {
		return fmt.Errorf("InsertNode pkg: %w", err)
	}
	fileNode, err := src.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://example/foo.go",
		ParentNodeID:       pkgNode.NodeID,
		FromSHA:            "deadbeef",
	})
	if err != nil {
		return fmt.Errorf("InsertNode file: %w", err)
	}
	_, err = src.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID: repoID, Kind: "contains",
		SrcNodeID: repoNode.NodeID, DstNodeID: pkgNode.NodeID,
		FromSHA: "deadbeef",
	})
	if err != nil {
		return fmt.Errorf("InsertEdge contains repo->pkg: %w", err)
	}
	_, err = src.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID: repoID, Kind: "contains",
		SrcNodeID: pkgNode.NodeID, DstNodeID: fileNode.NodeID,
		FromSHA: "deadbeef",
	})
	if err != nil {
		return fmt.Errorf("InsertEdge contains pkg->file: %w", err)
	}

	snap, err := src.Snapshot()
	if err != nil {
		return fmt.Errorf("Snapshot: %w", err)
	}
	st.wantNodes = len(snap.Nodes)
	st.wantEdges = len(snap.Edges)

	if err := src.Close(); err != nil {
		return fmt.Errorf("Close: %w", err)
	}
	return nil
}

func (st *roundtripState) loadExportReReads() error {
	defer os.RemoveAll(filepath.Dir(st.exportPath))

	_, reader, err := memory.LoadExport(st.exportPath)
	if err != nil {
		return fmt.Errorf("LoadExport: %w", err)
	}
	repoID, err := memorySinkRepoID()
	if err != nil {
		return err
	}
	ctx := context.Background()
	nodes, err := reader.ListNodes(ctx, repoID, nil,
		graphreader.ListNodesFilter{}, graphreader.ReaderOptions{})
	if err != nil {
		return fmt.Errorf("ListNodes: %w", err)
	}
	st.gotNodes = len(nodes)

	// Count edges by listing outbound from every node.
	edgeSet := make(map[string]bool)
	for _, n := range nodes {
		edges, err := reader.ListEdgesFrom(ctx, n.NodeID, nil, graphreader.ReaderOptions{})
		if err != nil {
			return fmt.Errorf("ListEdgesFrom %s: %w", n.NodeID, err)
		}
		for _, e := range edges {
			edgeSet[e.EdgeID] = true
		}
	}
	st.gotEdges = len(edgeSet)
	return nil
}

func (st *roundtripState) rehydratedReaderMatchesCounts() error {
	if st.gotNodes != st.wantNodes {
		return fmt.Errorf("rehydrated nodes = %d, want %d", st.gotNodes, st.wantNodes)
	}
	if st.gotEdges != st.wantEdges {
		return fmt.Errorf("rehydrated edges = %d, want %d", st.gotEdges, st.wantEdges)
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSON streaming helpers (duplicated from internal tests to
// avoid importing the internal test package).
// ---------------------------------------------------------------------------

func streamTopLevelKeys(data []byte) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("Token open: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected '{', got %v", tok)
	}
	var keys []string
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("Token key: %w", err)
		}
		k, ok := ktok.(string)
		if !ok {
			return nil, fmt.Errorf("expected key string, got %v", ktok)
		}
		keys = append(keys, k)
		if err := streamConsumeValue(dec); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func streamConsumeValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("Token value: %w", err)
	}
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			for dec.More() {
				if _, err := dec.Token(); err != nil {
					return fmt.Errorf("Token nested key: %w", err)
				}
				if err := streamConsumeValue(dec); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return fmt.Errorf("Token closing }: %w", err)
			}
		case '[':
			for dec.More() {
				if err := streamConsumeValue(dec); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return fmt.Errorf("Token closing ]: %w", err)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_graphsink_storage_abstraction_memory_sink_and_json_export(ctx *godog.ScenarioContext) {
	// Scenario: memory-idempotent
	idempotent := &memoryIdempotentState{}
	ctx.Given(`^the same NodeInput inserted twice into a memory sink$`, idempotent.theSameNodeInputInsertedTwice)
	ctx.When(`^InsertNode runs both times$`, idempotent.insertNodeRunsBothTimes)
	ctx.Then(`^the second call returns the cached id and the slice length is 1$`, idempotent.theSecondCallReturnsCachedIDAndSliceLenIs1)

	// Scenario: json-export-key-order
	keyOrder := &jsonExportKeyOrderState{}
	ctx.Given(`^a scan that produces repo, nodes, and edges in a memory sink with an export path$`, keyOrder.aScanWithExportPath)
	ctx.When(`^Close writes the export$`, keyOrder.closeWritesExport)
	ctx.Then(`^the top-level keys are "repo", "nodes", "edges" in that order verified by streaming-decode$`, keyOrder.topLevelKeysAreRepoNodesEdges)

	// Scenario: roundtrip-via-loadexport
	roundtrip := &roundtripState{}
	ctx.Given(`^a memory-sink scan written to disk via Close$`, roundtrip.aMemorySinkScanWrittenToDisk)
	ctx.When(`^LoadExport re-reads the export file$`, roundtrip.loadExportReReads)
	ctx.Then(`^the rehydrated Reader returns the same Node and Edge counts as the original scan$`, roundtrip.rehydratedReaderMatchesCounts)
}

func TestE2E_graphsink_storage_abstraction_memory_sink_and_json_export(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"graphsink_storage_abstraction_memory_sink_and_json_export.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_graphsink_storage_abstraction_memory_sink_and_json_export,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featurePath},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
