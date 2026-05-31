//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// stubSink — minimal Sink implementation for compile-time proof.
// The var _ assertion below this block is the canonical Go idiom
// that proves stubSink satisfies graphsink.Sink at compile time.
// ---------------------------------------------------------------------------

type stubSinkGraphsink struct{}

func (s *stubSinkGraphsink) EnsureRepo(_ context.Context, _ graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	return graphwriter.RepoRecord{}, nil
}

func (s *stubSinkGraphsink) EnsureCommit(_ context.Context, _ graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	return graphwriter.CommitRecord{}, nil
}

func (s *stubSinkGraphsink) InsertNode(_ context.Context, _ graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	return graphwriter.NodeRecord{}, nil
}

func (s *stubSinkGraphsink) InsertEdge(_ context.Context, _ graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	return graphwriter.EdgeRecord{}, nil
}

func (s *stubSinkGraphsink) Flush(_ context.Context) error { return nil }
func (s *stubSinkGraphsink) Close() error                  { return nil }

// Compile-time assertion: stubSinkGraphsink satisfies graphsink.Sink.
var _ graphsink.Sink = (*stubSinkGraphsink)(nil)

// ---------------------------------------------------------------------------
// stubReaderGraphsink — minimal Reader implementation for compile-time proof.
// ---------------------------------------------------------------------------

type stubReaderGraphsink struct{}

func (r *stubReaderGraphsink) ListRepos(_ context.Context, _ graphreader.ReaderOptions) ([]graphreader.RepoSummary, error) {
	return nil, nil
}

func (r *stubReaderGraphsink) ListNodes(_ context.Context, _ fingerprint.RepoID, _ []string, _ graphreader.ListNodesFilter, _ graphreader.ReaderOptions) ([]graphreader.Node, error) {
	return nil, nil
}

func (r *stubReaderGraphsink) ListEdgesFrom(_ context.Context, _ string, _ []string, _ graphreader.ReaderOptions) ([]graphreader.Edge, error) {
	return nil, nil
}

func (r *stubReaderGraphsink) ListEdgesTo(_ context.Context, _ string, _ []string, _ graphreader.ReaderOptions) ([]graphreader.Edge, error) {
	return nil, nil
}

func (r *stubReaderGraphsink) GetNode(_ context.Context, _ string, _ graphreader.ReaderOptions) (graphreader.Node, error) {
	return graphreader.Node{}, nil
}

func (r *stubReaderGraphsink) LookupBySignature(_ context.Context, _ fingerprint.RepoID, _ string, _ string, _ graphreader.ReaderOptions) (graphreader.Node, error) {
	return graphreader.Node{}, nil
}

// Compile-time assertion: stubReaderGraphsink satisfies graphsink.Reader.
var _ graphsink.Reader = (*stubReaderGraphsink)(nil)

// Compile-time assertion: *graphwriter.Writer still satisfies
// repoindexer.RepoCommitNodeEdgeWriter (unchanged from the
// existing ancestry_writer_iface.go assertion — this scenario
// proves the Phase-3 interface addition did NOT break it).
var _ repoindexer.RepoCommitNodeEdgeWriter = (*graphwriter.Writer)(nil)

// sinkSkeletonModuleRoot returns the services/agent-memory directory
// (the Go module root, 3 levels up from this file's directory) so
// exec.Command can set its working directory for `go vet` / `go build`.
func sinkSkeletonModuleRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile is <mod>/test/e2e/code-intelligence-REPO-SCANNER/<file>.go
	// module root is three directories up from the file's directory.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type sinkSkeletonState struct {
	goVetExitCode int
	goVetOutput   string
	buildExitCode int
	buildOutput   string
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (st *sinkSkeletonState) anEmptyStubSinkImpl() error {
	// Precondition: stubSinkGraphsink is defined above with all
	// required methods AND the var _ assertion compiles. The Given
	// step simply confirms the precondition holds (which it does
	// if this test binary was built successfully).
	var _ graphsink.Sink = (*stubSinkGraphsink)(nil)
	return nil
}

func (st *sinkSkeletonState) aStubReaderImplIncludingListRepos() error {
	// Precondition: stubReaderGraphsink is defined above with
	// ListRepos + all other methods AND the var _ assertion
	// compiles.
	var _ graphsink.Reader = (*stubReaderGraphsink)(nil)
	return nil
}

func (st *sinkSkeletonState) theUnchangedGraphwriterWriter() error {
	// Precondition: *graphwriter.Writer is unchanged and the
	// var _ assertion above compiles.
	var _ repoindexer.RepoCommitNodeEdgeWriter = (*graphwriter.Writer)(nil)
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

// goVetGraphsinkRuns executes `go vet ./internal/graphsink/...`
// against the module root. This is the real executable proof: if
// the graphsink package (including its _test files) has any type
// error or vet diagnostic, the step fails.
func (st *sinkSkeletonState) goVetGraphsinkRuns() error {
	modRoot := sinkSkeletonModuleRoot()
	cmd := exec.Command("go", "vet", "./internal/graphsink/...")
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	st.goVetOutput = strings.TrimSpace(string(out))
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			st.goVetExitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("go vet exec error: %w", err)
		}
	}
	return nil
}

// aTestAssignsToRepoCommitNodeEdgeWriter executes `go build`
// against the graphsink and repoindexer packages to prove the
// *graphwriter.Writer assignment compiles. The compile-time var _
// assertion in ancestry_writer_iface.go is the canonical proof;
// go build will fail if the shape drifts.
func (st *sinkSkeletonState) aTestAssignsToRepoCommitNodeEdgeWriter() error {
	modRoot := sinkSkeletonModuleRoot()
	cmd := exec.Command("go", "build", "./internal/repoindexer/...")
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	st.buildOutput = strings.TrimSpace(string(out))
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			st.buildExitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("go build exec error: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (st *sinkSkeletonState) theImplSatisfiesSink() error {
	if st.goVetExitCode != 0 {
		return fmt.Errorf(
			"go vet ./internal/graphsink/... failed (exit %d): %s",
			st.goVetExitCode, st.goVetOutput,
		)
	}
	return nil
}

func (st *sinkSkeletonState) theStubSatisfiesReader() error {
	if st.goVetExitCode != 0 {
		return fmt.Errorf(
			"go vet ./internal/graphsink/... failed (exit %d): %s",
			st.goVetExitCode, st.goVetOutput,
		)
	}
	return nil
}

func (st *sinkSkeletonState) theAssignmentCompilesWithoutModification() error {
	if st.buildExitCode != 0 {
		return fmt.Errorf(
			"go build ./internal/repoindexer/... failed (exit %d): %s",
			st.buildExitCode, st.buildOutput,
		)
	}
	return nil
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
