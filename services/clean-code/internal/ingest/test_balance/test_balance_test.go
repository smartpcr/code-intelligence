package test_balance_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/test_balance"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// fixedRepoID is a stable repo_id literal so the deterministic
// UUIDv5-based scope_id derivation (via the in-memory
// [stubScopeResolver]) yields the same UUIDs for every test
// run.
var fixedRepoID = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))

// validSHA returns a canonical 40-char hex SHA built from the
// repeated `c` byte.
func validSHA(c byte) string {
	return strings.Repeat(string(c), 40)
}

// stubScopeResolver is a deterministic [test_balance.ScopeResolver]
// that mints UUIDv5 per (repoID, ref.QualifiedName) so the same
// publisher scope_id consistently maps to the SAME UUID across
// calls -- mirroring [PGScopeBindingResolver]'s natural-key
// stability without the DB dependency.
type stubScopeResolver struct {
	failNext error
	calls    [][]recipes.ScopeRef
}

var stubResolverNamespace = uuid.Must(uuid.FromString("9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d"))

func (s *stubScopeResolver) ResolveScopeIDs(_ context.Context, repoID uuid.UUID, refs []recipes.ScopeRef, _ string) ([]uuid.UUID, error) {
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return nil, err
	}
	s.calls = append(s.calls, append([]recipes.ScopeRef(nil), refs...))
	out := make([]uuid.UUID, len(refs))
	for i, ref := range refs {
		out[i] = uuid.NewV5(stubResolverNamespace, repoID.String()+"|"+string(ref.Kind)+"|"+ref.QualifiedName)
	}
	return out, nil
}

// goodPayload returns a well-formed bare-array payload with
// three rows:
//   - "S1": 3/3 attempts -> ratio=1.0
//   - "S2": 2 attempts, 1 pass -> ratio=0.5
//   - "S3": 0 attempts (skipped)
func goodPayload() test_balance.Payload {
	return test_balance.Payload{
		{ScopeID: "S1", AttemptCount: 3, PassCount: 3},
		{ScopeID: "S2", AttemptCount: 2, PassCount: 1},
		{ScopeID: "S3", AttemptCount: 0, PassCount: 0},
	}
}

// newScanRun returns a valid scan_run context the test_balance
// writer accepts.
func newScanRun(t *testing.T, sha string) (uuid.UUID, metric_ingestor.ScanRunContext) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	return id, metric_ingestor.ScanRunContext{
		ID:     id,
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: fixedRepoID,
		SHA:    sha,
	}
}

// TestWriter_HappyPath asserts the canonical
// {1.0, 0.5, skipped-on-zero} emission shape from the e2e
// scenario at e2e-scenarios.md lines 646-653.
func TestWriter_HappyPath(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	resolver := &stubScopeResolver{}
	w := test_balance.NewWriter(mem, resolver)
	scanRunID, scanRun := newScanRun(t, validSHA('a'))

	res, err := w.Run(context.Background(), scanRun, goodPayload())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SamplesWritten != 2 {
		t.Errorf("SamplesWritten = %d; want 2 (S1 + S2)", res.SamplesWritten)
	}
	if res.RowsSkipped != 1 {
		t.Errorf("RowsSkipped = %d; want 1 (S3 attempt_count=0)", res.RowsSkipped)
	}
	records := mem.Records()
	if len(records) != 2 {
		t.Fatalf("Records = %d; want 2", len(records))
	}
	// Records preserve input order; index 0 is S1 (1.0),
	// index 1 is S2 (0.5).
	if got := records[0].Value; got != 1.0 {
		t.Errorf("records[0].Value = %v; want 1.0", got)
	}
	if got := records[1].Value; got != 0.5 {
		t.Errorf("records[1].Value = %v; want 0.5", got)
	}
	for i, r := range records {
		if r.MetricKind != test_balance.MetricKind {
			t.Errorf("records[%d].MetricKind = %q; want %q", i, r.MetricKind, test_balance.MetricKind)
		}
		if r.MetricVersion != test_balance.MetricVersion {
			t.Errorf("records[%d].MetricVersion = %d; want %d", i, r.MetricVersion, test_balance.MetricVersion)
		}
		if r.Pack != recipes.PackIngested {
			t.Errorf("records[%d].Pack = %q; want %q", i, r.Pack, recipes.PackIngested)
		}
		if r.Source != recipes.SourceIngested {
			t.Errorf("records[%d].Source = %q; want %q", i, r.Source, recipes.SourceIngested)
		}
		if r.SHA != validSHA('a') {
			t.Errorf("records[%d].SHA = %q; want all-a", i, r.SHA)
		}
		if r.RepoID != fixedRepoID {
			t.Errorf("records[%d].RepoID = %s; want %s", i, r.RepoID, fixedRepoID)
		}
		if r.ProducerRunID != scanRunID {
			t.Errorf("records[%d].ProducerRunID = %s; want %s (same-ScanRun invariant)", i, r.ProducerRunID, scanRunID)
		}
		if r.ScopeID == uuid.Nil {
			t.Errorf("records[%d].ScopeID is the zero UUID", i)
		}
	}
	// ScopeIDs are deterministic UUIDv5 -- S1 and S2 differ.
	if records[0].ScopeID == records[1].ScopeID {
		t.Errorf("records[0].ScopeID == records[1].ScopeID = %s; want distinct UUIDs", records[0].ScopeID)
	}

	// Resolver received TWO refs (S3 skipped) under the
	// `.ingested/test_balance/` namespace; assert Kind=file
	// and the namespaced path.
	if got := len(resolver.calls); got != 1 {
		t.Fatalf("resolver.calls = %d; want 1 batched call", got)
	}
	refs := resolver.calls[0]
	if got := len(refs); got != 2 {
		t.Fatalf("refs in batch = %d; want 2 (S3 skipped)", got)
	}
	for i, ref := range refs {
		if ref.Kind != scope.KindFile {
			t.Errorf("refs[%d].Kind = %q; want %q", i, ref.Kind, scope.KindFile)
		}
		if !strings.HasPrefix(ref.QualifiedName, test_balance.ScopePathNamespace) {
			t.Errorf("refs[%d].QualifiedName = %q; want prefix %q", i, ref.QualifiedName, test_balance.ScopePathNamespace)
		}
		if ref.Path != ref.QualifiedName {
			t.Errorf("refs[%d].Path != QualifiedName: %q vs %q", i, ref.Path, ref.QualifiedName)
		}
		if ref.LocalID == "" {
			t.Errorf("refs[%d].LocalID is empty", i)
		}
	}
	if refs[0].QualifiedName != test_balance.ScopePathNamespace+"S1" {
		t.Errorf("refs[0].QualifiedName = %q; want %q", refs[0].QualifiedName, test_balance.ScopePathNamespace+"S1")
	}
	if refs[1].QualifiedName != test_balance.ScopePathNamespace+"S2" {
		t.Errorf("refs[1].QualifiedName = %q; want %q", refs[1].QualifiedName, test_balance.ScopePathNamespace+"S2")
	}
}

// TestWriter_ScopeNamespacePreventsCollision pins the
// namespace-isolation invariant: a publisher posting an
// opaque scope_id that LOOKS like a real repo-relative file
// path ("internal/foo.go") MUST NOT collide with the AST
// adapter's `KindFile` scope_binding row for that file. The
// namespace prefix `.ingested/test_balance/` keeps the two
// natural-key buckets distinct.
func TestWriter_ScopeNamespacePreventsCollision(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	resolver := &stubScopeResolver{}
	w := test_balance.NewWriter(mem, resolver)
	_, scanRun := newScanRun(t, validSHA('a'))

	payload := test_balance.Payload{
		{ScopeID: "internal/foo.go", AttemptCount: 1, PassCount: 1},
	}
	if _, err := w.Run(context.Background(), scanRun, payload); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ref := resolver.calls[0][0]
	want := test_balance.ScopePathNamespace + "internal/foo.go"
	if ref.QualifiedName != want {
		t.Errorf("QualifiedName = %q; want %q (namespace must isolate from AST file scope)", ref.QualifiedName, want)
	}
	if !strings.HasPrefix(ref.QualifiedName, ".") {
		t.Errorf("ref.QualifiedName = %q; must start with `.` so AST-emitted KindFile paths never collide", ref.QualifiedName)
	}
}

// TestWriter_ClampsRatioToOne pins the clamp-to-[0,1] contract.
// When pass_count > attempt_count (publisher bug) the writer
// caps the emitted ratio at 1.0 instead of rejecting the row.
func TestWriter_ClampsRatioToOne(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	w := test_balance.NewWriter(mem, &stubScopeResolver{})
	_, scanRun := newScanRun(t, validSHA('b'))

	payload := test_balance.Payload{
		{ScopeID: "overcount", AttemptCount: 2, PassCount: 5},
	}
	res, err := w.Run(context.Background(), scanRun, payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SamplesWritten != 1 {
		t.Fatalf("SamplesWritten = %d; want 1", res.SamplesWritten)
	}
	records := mem.Records()
	if got := records[0].Value; got != 1.0 {
		t.Errorf("clamped value = %v; want 1.0", got)
	}
}

// TestWriter_SkipsAllZeroAttemptRows asserts the no-op shape
// when every row has attempt_count=0: no records, no writer
// invocation, no scope-resolver invocation, no error.
func TestWriter_SkipsAllZeroAttemptRows(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	mem.FailNext(errors.New("writer must not be called when every row is skipped"))
	resolver := &stubScopeResolver{
		failNext: errors.New("resolver must not be called when every row is skipped"),
	}
	w := test_balance.NewWriter(mem, resolver)
	_, scanRun := newScanRun(t, validSHA('c'))

	payload := test_balance.Payload{
		{ScopeID: "empty1", AttemptCount: 0, PassCount: 0},
		{ScopeID: "empty2", AttemptCount: 0, PassCount: 0},
	}
	res, err := w.Run(context.Background(), scanRun, payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SamplesWritten != 0 {
		t.Errorf("SamplesWritten = %d; want 0", res.SamplesWritten)
	}
	if res.RowsSkipped != 2 {
		t.Errorf("RowsSkipped = %d; want 2", res.RowsSkipped)
	}
	if got := len(mem.Records()); got != 0 {
		t.Errorf("Records = %d; want 0", got)
	}
}

// TestWriter_ValidationErrors table-tests every sentinel the
// classifier maps to 400.
func TestWriter_ValidationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload test_balance.Payload
		wantErr error
	}{
		{
			"EmptyRows",
			test_balance.Payload{},
			test_balance.ErrEmptyRows,
		},
		{
			"EmptyScopeID",
			test_balance.Payload{{ScopeID: "  ", AttemptCount: 1, PassCount: 1}},
			test_balance.ErrEmptyScopeID,
		},
		{
			"NegativeAttemptCount",
			test_balance.Payload{{ScopeID: "S", AttemptCount: -1, PassCount: 0}},
			test_balance.ErrNegativeAttemptCount,
		},
		{
			"NegativePassCount",
			test_balance.Payload{{ScopeID: "S", AttemptCount: 1, PassCount: -1}},
			test_balance.ErrNegativePassCount,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mem := metric_ingestor.NewInMemoryMetricSampleWriter()
			w := test_balance.NewWriter(mem, &stubScopeResolver{})
			_, scanRun := newScanRun(t, validSHA('a'))
			_, err := w.Run(context.Background(), scanRun, tc.payload)
			if err == nil {
				t.Fatalf("Run: want error %v, got nil", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Run err = %v; want errors.Is(%v)", err, tc.wantErr)
			}
			if got := len(mem.Records()); got != 0 {
				t.Errorf("Records = %d; want 0 (validation must NOT call writer)", got)
			}
		})
	}
}

// TestWriter_RejectsWrongScanRunKind pins the kind-pinning
// guard: a caller passing `external_per_row` cannot smuggle
// per-row SHA semantics into the test_balance writer.
func TestWriter_RejectsWrongScanRunKind(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	w := test_balance.NewWriter(mem, &stubScopeResolver{})
	scanRun := metric_ingestor.ScanRunContext{
		ID:     uuid.Must(uuid.NewV7()),
		Kind:   metric_ingestor.ScanRunKindExternalPerRow,
		RepoID: fixedRepoID,
		SHA:    validSHA('a'),
	}
	_, err := w.Run(context.Background(), scanRun, goodPayload())
	if err == nil {
		t.Fatalf("Run(external_per_row scan_run): want error, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrInvalidScanRunKind) {
		t.Errorf("err = %v; want errors.Is(ErrInvalidScanRunKind)", err)
	}
}

// TestWriter_RejectsEmptySHA pins the
// `external_single requires one SHA per call` invariant
// (architecture Sec 6.4 lines 1367-1368).
func TestWriter_RejectsEmptySHA(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	w := test_balance.NewWriter(mem, &stubScopeResolver{})
	scanRun := metric_ingestor.ScanRunContext{
		ID:     uuid.Must(uuid.NewV7()),
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: fixedRepoID,
		SHA:    "",
	}
	_, err := w.Run(context.Background(), scanRun, goodPayload())
	if err == nil {
		t.Fatalf("Run(empty SHA): want error, got nil")
	}
}

// TestWriter_RejectsZeroScanRunID pins the zero-ID guard.
func TestWriter_RejectsZeroScanRunID(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	w := test_balance.NewWriter(mem, &stubScopeResolver{})
	scanRun := metric_ingestor.ScanRunContext{
		ID:     uuid.Nil,
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: fixedRepoID,
		SHA:    validSHA('a'),
	}
	_, err := w.Run(context.Background(), scanRun, goodPayload())
	if err == nil {
		t.Fatalf("Run(zero scan_run ID): want error, got nil")
	}
}

// TestWriter_RejectsZeroScanRunRepoID pins the zero-repo
// guard at the scan-run layer.
func TestWriter_RejectsZeroScanRunRepoID(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	w := test_balance.NewWriter(mem, &stubScopeResolver{})
	scanRun := metric_ingestor.ScanRunContext{
		ID:     uuid.Must(uuid.NewV7()),
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: uuid.Nil,
		SHA:    validSHA('a'),
	}
	_, err := w.Run(context.Background(), scanRun, goodPayload())
	if err == nil {
		t.Fatalf("Run(zero scan_run RepoID): want error, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrZeroRepoID) {
		t.Errorf("err = %v; want errors.Is(ErrZeroRepoID)", err)
	}
}

// TestWriter_PropagatesWriterFailure pins the writer-failure
// wrap so the classifier maps to 500 WRITER_FAILURE.
func TestWriter_PropagatesWriterFailure(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	mem.FailNext(errors.New("simulated PG outage"))
	w := test_balance.NewWriter(mem, &stubScopeResolver{})
	_, scanRun := newScanRun(t, validSHA('a'))

	_, err := w.Run(context.Background(), scanRun, goodPayload())
	if err == nil {
		t.Fatalf("Run: want writer-failure error, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrWriterFailure) {
		t.Errorf("err = %v; want errors.Is(ErrWriterFailure)", err)
	}
}

// TestWriter_PropagatesScopeResolverFailure pins the
// resolver-failure wrap so the classifier maps to 500
// SCOPE_RESOLUTION_FAILED.
func TestWriter_PropagatesScopeResolverFailure(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	resolver := &stubScopeResolver{failNext: errors.New("simulated scope_binding outage")}
	w := test_balance.NewWriter(mem, resolver)
	_, scanRun := newScanRun(t, validSHA('a'))

	_, err := w.Run(context.Background(), scanRun, goodPayload())
	if err == nil {
		t.Fatalf("Run: want resolver-failure error, got nil")
	}
	if !errors.Is(err, test_balance.ErrScopeResolutionFailed) {
		t.Errorf("err = %v; want errors.Is(ErrScopeResolutionFailed)", err)
	}
	if got := len(mem.Records()); got != 0 {
		t.Errorf("Records = %d; want 0 (resolver failed before any insert)", got)
	}
}

// TestNewWriter_PanicsOnNilWriter pins the wiring guard.
func TestNewWriter_PanicsOnNilWriter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewWriter(nil, _) did not panic")
		}
	}()
	_ = test_balance.NewWriter(nil, &stubScopeResolver{})
}

// TestNewWriter_PanicsOnNilResolver pins the iter-2 wiring
// guard: nil scope-resolver is a structural misconfig that
// would silently reintroduce the production FK gap.
func TestNewWriter_PanicsOnNilResolver(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewWriter(_, nil) did not panic")
		}
	}()
	_ = test_balance.NewWriter(metric_ingestor.NewInMemoryMetricSampleWriter(), nil)
}

// TestNewWriterWithUUID_PanicsOnNilGenerator pins the UUID
// generator guard.
func TestNewWriterWithUUID_PanicsOnNilGenerator(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewWriterWithUUID(_, _, nil) did not panic")
		}
	}()
	_ = test_balance.NewWriterWithUUID(
		metric_ingestor.NewInMemoryMetricSampleWriter(),
		&stubScopeResolver{},
		nil,
	)
}

// TestWriter_DeterministicSampleID exercises the test-only
// UUID-generator override.
func TestWriter_DeterministicSampleID(t *testing.T) {
	t.Parallel()
	mem := metric_ingestor.NewInMemoryMetricSampleWriter()
	counter := 0
	w := test_balance.NewWriterWithUUID(mem, &stubScopeResolver{}, func() (uuid.UUID, error) {
		counter++
		// 16-byte deterministic seed; we only need
		// distinguishable UUIDs, not v7 ordering.
		return uuid.FromStringOrNil(fmt.Sprintf("00000000-0000-0000-0000-%012d", counter)), nil
	})
	_, scanRun := newScanRun(t, validSHA('a'))
	if _, err := w.Run(context.Background(), scanRun, goodPayload()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	records := mem.Records()
	if records[0].SampleID == records[1].SampleID {
		t.Errorf("SampleIDs collided: %s", records[0].SampleID)
	}
}
