package webhook_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// fakePGScanRunOpener captures the
// [webhook.PGScanRunOpener] contract so we can drive the
// adapter without a sqlmock-backed PG handle. The fake
// records each call so the adapter's argument-marshalling
// is asserted directly.
type fakePGScanRunOpener struct {
	lastOpenReq metric_ingestor.OpenExternalScanRunRequest
	openRes     metric_ingestor.OpenExternalScanRunResult
	openErr     error

	lastFinalizeID     uuid.UUID
	lastFinalizeStatus metric_ingestor.ScanRunStatus
	lastFinalizeEnded  time.Time
	finalizeErr        error

	lookupStatus   metric_ingestor.ScanRunStatus
	lookupFound    bool
	lookupErr      error
	lookupCalls    int
	lastLookupID   uuid.UUID

	openCalls     int
	finalizeCalls int
}

func (f *fakePGScanRunOpener) OpenExternalScanRun(ctx context.Context, req metric_ingestor.OpenExternalScanRunRequest) (metric_ingestor.OpenExternalScanRunResult, error) {
	f.openCalls++
	f.lastOpenReq = req
	return f.openRes, f.openErr
}

func (f *fakePGScanRunOpener) FinalizeExternalScanRun(ctx context.Context, scanRunID uuid.UUID, status metric_ingestor.ScanRunStatus, endedAt time.Time) error {
	f.finalizeCalls++
	f.lastFinalizeID = scanRunID
	f.lastFinalizeStatus = status
	f.lastFinalizeEnded = endedAt
	return f.finalizeErr
}

func (f *fakePGScanRunOpener) LookupExternalScanRunStatusByID(ctx context.Context, scanRunID uuid.UUID) (metric_ingestor.ScanRunStatus, bool, error) {
	f.lookupCalls++
	f.lastLookupID = scanRunID
	return f.lookupStatus, f.lookupFound, f.lookupErr
}

// TestPGScanRunRepository_OpenExternal_TranslatesShapes pins
// the adapter's primary responsibility: translate
// [webhook.ScanRunRepositoryRequest] into
// [metric_ingestor.OpenExternalScanRunRequest] field-for-field.
func TestPGScanRunRepository_OpenExternal_TranslatesShapes(t *testing.T) {
	t.Parallel()
	repoID, _ := uuid.NewV4()
	expectedID, _ := uuid.NewV4()
	openedAt := time.Date(2026, 1, 14, 12, 0, 0, 0, time.UTC)

	var hash webhook.PayloadHash
	for i := range hash {
		hash[i] = byte(i + 1)
	}

	fake := &fakePGScanRunOpener{
		openRes: metric_ingestor.OpenExternalScanRunResult{
			ScanRunID:      expectedID,
			AlreadyExisted: false,
		},
	}
	repo := webhook.NewPGScanRunRepository(fake)
	res, err := repo.OpenExternal(context.Background(), webhook.ScanRunRepositoryRequest{
		Verb:        "churn",
		Kind:        "external_per_row",
		SHABinding:  "per_row",
		RepoID:      repoID,
		SHA:         "",
		PayloadHash: hash,
		OpenedAt:    openedAt,
	})
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	if res.ScanRunID != expectedID {
		t.Errorf("ScanRunID: want %s, got %s", expectedID, res.ScanRunID)
	}
	if res.AlreadyExisted {
		t.Errorf("AlreadyExisted: want false, got true")
	}
	// Argument-marshalling assertions: the adapter MUST
	// pass each webhook-shape field through to the
	// metric_ingestor-shape field 1:1, and convert
	// PayloadHash to a 32-byte slice. The (Verb, Kind)
	// pair MUST both arrive on the PG store request.
	if fake.openCalls != 1 {
		t.Errorf("openCalls: want 1, got %d", fake.openCalls)
	}
	if fake.lastOpenReq.Verb != "churn" {
		t.Errorf("Verb: want %q, got %q", "churn", fake.lastOpenReq.Verb)
	}
	if fake.lastOpenReq.RepoID != repoID {
		t.Errorf("RepoID: want %s, got %s", repoID, fake.lastOpenReq.RepoID)
	}
	if fake.lastOpenReq.Kind != "external_per_row" {
		t.Errorf("Kind: want %q, got %q", "external_per_row", fake.lastOpenReq.Kind)
	}
	if fake.lastOpenReq.SHABinding != "per_row" {
		t.Errorf("SHABinding: want %q, got %q", "per_row", fake.lastOpenReq.SHABinding)
	}
	if fake.lastOpenReq.ToSHA != "" {
		t.Errorf("ToSHA: want empty, got %q", fake.lastOpenReq.ToSHA)
	}
	if len(fake.lastOpenReq.PayloadHash) != 32 {
		t.Errorf("PayloadHash length: want 32, got %d", len(fake.lastOpenReq.PayloadHash))
	}
	for i := 0; i < 32; i++ {
		if fake.lastOpenReq.PayloadHash[i] != byte(i+1) {
			t.Errorf("PayloadHash[%d]: want %d, got %d", i, i+1, fake.lastOpenReq.PayloadHash[i])
			break
		}
	}
	if !fake.lastOpenReq.OpenedAt.Equal(openedAt) {
		t.Errorf("OpenedAt: want %s, got %s", openedAt, fake.lastOpenReq.OpenedAt)
	}
}

// TestPGScanRunRepository_OpenExternal_PropagatesAlreadyExisted pins
// the durable-replay invariant: when the underlying store
// reports AlreadyExisted=true (the INSERT was a no-op), the
// adapter MUST surface AlreadyExisted=true + the prior
// scan_run_id + the ExistingStatus to the Router.
func TestPGScanRunRepository_OpenExternal_PropagatesAlreadyExisted(t *testing.T) {
	t.Parallel()
	priorID, _ := uuid.NewV4()
	fake := &fakePGScanRunOpener{
		openRes: metric_ingestor.OpenExternalScanRunResult{
			ScanRunID:      priorID,
			AlreadyExisted: true,
			ExistingStatus: metric_ingestor.ScanRunStatusSucceeded,
		},
	}
	repo := webhook.NewPGScanRunRepository(fake)
	repoID, _ := uuid.NewV4()
	var hash webhook.PayloadHash
	hash[0] = 0xAA

	res, err := repo.OpenExternal(context.Background(), webhook.ScanRunRepositoryRequest{
		Verb:        "churn",
		Kind:        "external_per_row",
		SHABinding:  "per_row",
		RepoID:      repoID,
		PayloadHash: hash,
		OpenedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	if !res.AlreadyExisted {
		t.Errorf("AlreadyExisted: want true, got false")
	}
	if res.ScanRunID != priorID {
		t.Errorf("ScanRunID: want %s, got %s", priorID, res.ScanRunID)
	}
	if res.ExistingStatus != webhook.ScanRunStatusSucceeded {
		t.Errorf("ExistingStatus: want %q, got %q", webhook.ScanRunStatusSucceeded, res.ExistingStatus)
	}
}

// TestPGScanRunRepository_Finalize_TranslatesStatus pins the
// adapter's terminal-status translation:
// [webhook.ScanRunStatusSucceeded] -> [metric_ingestor.ScanRunStatusSucceeded].
func TestPGScanRunRepository_Finalize_TranslatesStatus(t *testing.T) {
	t.Parallel()
	fake := &fakePGScanRunOpener{}
	repo := webhook.NewPGScanRunRepository(fake)
	scanRunID, _ := uuid.NewV4()
	ended := time.Date(2026, 1, 14, 13, 0, 0, 0, time.UTC)
	if err := repo.Finalize(context.Background(), scanRunID, webhook.ScanRunStatusSucceeded, ended); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if fake.finalizeCalls != 1 {
		t.Errorf("finalizeCalls: want 1, got %d", fake.finalizeCalls)
	}
	if fake.lastFinalizeID != scanRunID {
		t.Errorf("scanRunID: want %s, got %s", scanRunID, fake.lastFinalizeID)
	}
	if fake.lastFinalizeStatus != metric_ingestor.ScanRunStatusSucceeded {
		t.Errorf("status: want %q, got %q", metric_ingestor.ScanRunStatusSucceeded, fake.lastFinalizeStatus)
	}
	if !fake.lastFinalizeEnded.Equal(ended) {
		t.Errorf("endedAt: want %s, got %s", ended, fake.lastFinalizeEnded)
	}
}

// TestPGScanRunRepository_Finalize_UnknownStatus_Rejected
// pins the closed-set guard at the adapter level.
func TestPGScanRunRepository_Finalize_UnknownStatus_Rejected(t *testing.T) {
	t.Parallel()
	fake := &fakePGScanRunOpener{}
	repo := webhook.NewPGScanRunRepository(fake)
	scanRunID, _ := uuid.NewV4()
	err := repo.Finalize(context.Background(), scanRunID, "in-flight", time.Now())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, webhook.ErrScanRunRepoUnknownStatus) {
		t.Errorf("error chain: want ErrScanRunRepoUnknownStatus, got %v", err)
	}
	if fake.finalizeCalls != 0 {
		t.Errorf("finalizeCalls: want 0 (validation guard short-circuits), got %d", fake.finalizeCalls)
	}
}

// TestNewPGScanRunRepository_NilStore_Panics pins the
// composition-root guard: a missing PG store is a wiring
// bug that MUST fail loudly at startup.
func TestNewPGScanRunRepository_NilStore_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("expected panic, got none")
		}
	}()
	_ = webhook.NewPGScanRunRepository(nil)
}

// TestPGScanRunRepository_Finalize_ConcurrentSameTerminal_ReturnsNil
// (iter-3 evaluator item #4) pins the [ScanRunRepository]
// interface contract: a double-finalize where the underlying
// PG store returns ErrConcurrentFinalize but the row IS
// already in the requested terminal status MUST return nil
// (this is the benign "sibling replica raced ahead with the
// same outcome" case the in-memory implementation accepts).
func TestPGScanRunRepository_Finalize_ConcurrentSameTerminal_ReturnsNil(t *testing.T) {
	t.Parallel()
	fake := &fakePGScanRunOpener{
		finalizeErr:  metric_ingestor.ErrConcurrentFinalize,
		lookupStatus: metric_ingestor.ScanRunStatusSucceeded,
		lookupFound:  true,
	}
	repo := webhook.NewPGScanRunRepository(fake)
	scanRunID, _ := uuid.NewV4()
	err := repo.Finalize(context.Background(), scanRunID, webhook.ScanRunStatusSucceeded, time.Now())
	if err != nil {
		t.Fatalf("Finalize: want nil (same-terminal contract), got %v", err)
	}
	if fake.finalizeCalls != 1 {
		t.Errorf("finalizeCalls: want 1, got %d", fake.finalizeCalls)
	}
	if fake.lookupCalls != 1 {
		t.Errorf("lookupCalls: want 1 (must SELECT to honour same-terminal contract), got %d", fake.lookupCalls)
	}
	if fake.lastLookupID != scanRunID {
		t.Errorf("lookup id: want %s, got %s", scanRunID, fake.lastLookupID)
	}
}

// TestPGScanRunRepository_Finalize_ConcurrentDifferentTerminal_ReturnsError
// (iter-3 evaluator item #4) pins the OTHER half of the
// Finalize contract: when the underlying store reports
// ErrConcurrentFinalize AND the row is in a DIFFERENT
// terminal status, the adapter MUST surface the mismatch
// to the operator (not silently swallow it).
func TestPGScanRunRepository_Finalize_ConcurrentDifferentTerminal_ReturnsError(t *testing.T) {
	t.Parallel()
	fake := &fakePGScanRunOpener{
		finalizeErr:  metric_ingestor.ErrConcurrentFinalize,
		lookupStatus: metric_ingestor.ScanRunStatusFailed,
		lookupFound:  true,
	}
	repo := webhook.NewPGScanRunRepository(fake)
	scanRunID, _ := uuid.NewV4()
	err := repo.Finalize(context.Background(), scanRunID, webhook.ScanRunStatusSucceeded, time.Now())
	if err == nil {
		t.Fatalf("expected error (mismatched terminal status), got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrConcurrentFinalize) {
		t.Errorf("error chain: must still carry ErrConcurrentFinalize so the operator log names the cause; got %v", err)
	}
	if fake.lookupCalls != 1 {
		t.Errorf("lookupCalls: want 1, got %d", fake.lookupCalls)
	}
}

// TestPGScanRunRepository_Finalize_ConcurrentRowMissing_ReturnsError
// pins the third Finalize branch: the row is unexpectedly
// gone (a stale-sweep DELETE between FinalizeExternalScanRun
// and LookupExternalScanRunStatusByID). The adapter MUST
// surface ErrConcurrentFinalize so the operator can
// investigate -- silently returning nil would mask a
// catalog-integrity problem.
func TestPGScanRunRepository_Finalize_ConcurrentRowMissing_ReturnsError(t *testing.T) {
	t.Parallel()
	fake := &fakePGScanRunOpener{
		finalizeErr:  metric_ingestor.ErrConcurrentFinalize,
		lookupStatus: "",
		lookupFound:  false,
	}
	repo := webhook.NewPGScanRunRepository(fake)
	scanRunID, _ := uuid.NewV4()
	err := repo.Finalize(context.Background(), scanRunID, webhook.ScanRunStatusSucceeded, time.Now())
	if err == nil {
		t.Fatalf("expected error (row missing on lookup), got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrConcurrentFinalize) {
		t.Errorf("error chain: must carry ErrConcurrentFinalize; got %v", err)
	}
}
