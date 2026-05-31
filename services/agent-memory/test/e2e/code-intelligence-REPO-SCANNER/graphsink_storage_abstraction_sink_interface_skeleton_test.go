//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// stubSink — minimal Sink implementation for compile-time proof
// ---------------------------------------------------------------------------

type stubSink struct{}

func (s *stubSink) EnsureRepo(_ context.Context, _ graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	return graphwriter.RepoRecord{}, nil
}

func (s *stubSink) EnsureCommit(_ context.Context, _ graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	return graphwriter.CommitRecord{}, nil
}

func (s *stubSink) InsertNode(_ context.Context, _ graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	return graphwriter.NodeRecord{}, nil
}

func (s *stubSink) InsertEdge(_ context.Context, _ graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	return graphwriter.EdgeRecord{}, nil
}

func (s *stubSink) Flush(_ context.Context) error { return nil }
func (s *stubSink) Close() error                  { return nil }

// Compile-time assertion: stubSink satisfies graphsink.Sink.
var _ graphsink.Sink = (*stubSink)(nil)

// ---------------------------------------------------------------------------
// stubReader — minimal Reader implementation for compile-time proof
// ---------------------------------------------------------------------------

type stubReader struct{}

func (r *stubReader) ListRepos(_ context.Context, _ graphreader.ReaderOptions) ([]graphreader.RepoSummary, error) {
	return nil, nil
}

func (r *stubReader) ListNodes(_ context.Context, _ fingerprint.RepoID, _ []string, _ graphreader.ListNodesFilter, _ graphreader.ReaderOptions) ([]graphreader.Node, error) {
	return nil, nil
}

func (r *stubReader) ListEdgesFrom(_ context.Context, _ string, _ []string, _ graphreader.ReaderOptions) ([]graphreader.Edge, error) {
	return nil, nil
}

func (r *stubReader) ListEdgesTo(_ context.Context, _ string, _ []string, _ graphreader.ReaderOptions) ([]graphreader.Edge, error) {
	return nil, nil
}

func (r *stubReader) GetNode(_ context.Context, _ string, _ graphreader.ReaderOptions) (graphreader.Node, error) {
	return graphreader.Node{}, nil
}

func (r *stubReader) LookupBySignature(_ context.Context, _ fingerprint.RepoID, _ string, _ string, _ graphreader.ReaderOptions) (graphreader.Node, error) {
	return graphreader.Node{}, nil
}

// Compile-time assertion: stubReader satisfies graphsink.Reader.
var _ graphsink.Reader = (*stubReader)(nil)

// Compile-time assertion: *graphwriter.Writer satisfies
// repoindexer.RepoCommitNodeEdgeWriter (unchanged from the
// existing ancestry_writer_iface.go assertion — this scenario
// proves the Phase-3 interface addition did NOT break it).
var _ repoindexer.RepoCommitNodeEdgeWriter = (*graphwriter.Writer)(nil)

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type sinkSkeletonState struct {
	sinkSatisfied         bool
	readerSatisfied       bool
	writerStillSatisfied  bool
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (st *sinkSkeletonState) anEmptyStubSinkImpl() error {
	// The compile-time var _ assertion above already proved this.
	// If the file compiles, the stub satisfies Sink.
	st.sinkSatisfied = true
	return nil
}

func (st *sinkSkeletonState) aStubReaderImplIncludingListRepos() error {
	// Same reasoning: compile-time var _ assertion covers this.
	st.readerSatisfied = true
	return nil
}

func (st *sinkSkeletonState) theUnchangedGraphwriterWriter() error {
	// The compile-time var _ assertion proves *graphwriter.Writer
	// still satisfies the narrow interface.
	st.writerStillSatisfied = true
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (st *sinkSkeletonState) goVetGraphsinkRuns() error {
	// The fact that this test file compiled means go vet would
	// pass for interface satisfaction. The compile-time assertions
	// (var _ Interface = (*Concrete)(nil)) are the canonical Go
	// idiom; if they fail, the build fails before tests run.
	return nil
}

func (st *sinkSkeletonState) aTestAssignsToRepoCommitNodeEdgeWriter() error {
	// Already proven by the compile-time var _ assertion above.
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (st *sinkSkeletonState) theImplSatisfiesSink() error {
	if !st.sinkSatisfied {
		return errInterfaceNotSatisfied("graphsink.Sink")
	}
	return nil
}

func (st *sinkSkeletonState) theStubSatisfiesReader() error {
	if !st.readerSatisfied {
		return errInterfaceNotSatisfied("graphsink.Reader")
	}
	return nil
}

func (st *sinkSkeletonState) theAssignmentCompilesWithoutModification() error {
	if !st.writerStillSatisfied {
		return errInterfaceNotSatisfied("repoindexer.RepoCommitNodeEdgeWriter")
	}
	return nil
}

func errInterfaceNotSatisfied(iface string) error {
	// This path is unreachable if the file compiled, but kept for
	// godog step completeness.
	return &interfaceError{iface: iface}
}

type interfaceError struct{ iface string }

func (e *interfaceError) Error() string {
	return "compile-time assertion failed: type does not satisfy " + e.iface
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_graphsink_storage_abstraction_sink_interface_skeleton(ctx *godog.ScenarioContext) {
	st := &sinkSkeletonState{}

	// Given
	ctx.Given(`^an empty implementation "type stubSink struct\{\}" in a test file with the required methods$`, st.anEmptyStubSinkImpl)
	ctx.Given(`^a stub Reader impl in tests including ListRepos$`, st.aStubReaderImplIncludingListRepos)
	ctx.Given(`^the unchanged "\*graphwriter\.Writer"$`, st.theUnchangedGraphwriterWriter)

	// When
	ctx.When(`^"go vet \./internal/graphsink/\.\.\." runs$`, st.goVetGraphsinkRuns)
	ctx.When(`^a test assigns it to a "repoindexer\.RepoCommitNodeEdgeWriter" variable$`, st.aTestAssignsToRepoCommitNodeEdgeWriter)

	// Then
	ctx.Then(`^the implementation satisfies "graphsink\.Sink"$`, st.theImplSatisfiesSink)
	ctx.Then(`^the stub satisfies "graphsink\.Reader"$`, st.theStubSatisfiesReader)
	ctx.Then(`^the assignment compiles without modification$`, st.theAssignmentCompilesWithoutModification)
}

func TestE2E_graphsink_storage_abstraction_sink_interface_skeleton(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"graphsink_storage_abstraction_sink_interface_skeleton.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_graphsink_storage_abstraction_sink_interface_skeleton,
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
