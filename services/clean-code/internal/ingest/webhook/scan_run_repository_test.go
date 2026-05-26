package webhook_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/webhook"
)

// scanRunRepoTestNow returns a fixed time-source used by the
// in-memory scan-run repo tests. Pinned to a non-zero
// instant so OpenedAt / endedAt fields exercise the
// `time.IsZero()` guards correctly.
func scanRunRepoTestNow() time.Time {
	return time.Date(2026, 1, 14, 12, 0, 0, 0, time.UTC)
}

// scanRunRepoTestRequest builds a fully-populated
// [webhook.ScanRunRepositoryRequest] for the
// in-memory scan-run repo tests. Pass `hash` to vary the
// claim slot; everything else is fixed.
func scanRunRepoTestRequest(t *testing.T, hash webhook.PayloadHash) webhook.ScanRunRepositoryRequest {
	t.Helper()
	repoID, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("mint repo_id: %v", err)
	}
	return webhook.ScanRunRepositoryRequest{
		Verb:        "churn",
		Kind:        "external_per_row",
		SHABinding:  "per_row",
		RepoID:      repoID,
		SHA:         "",
		PayloadHash: hash,
		OpenedAt:    scanRunRepoTestNow(),
	}
}

// scanRunHashFor returns a deterministic 32-byte hash for the
// supplied seed byte. Cheap stand-in for sha256 in tests where
// the actual hash value is irrelevant -- only its uniqueness
// matters.
func scanRunHashFor(seed byte) webhook.PayloadHash {
	var h webhook.PayloadHash
	for i := range h {
		h[i] = seed
	}
	return h
}

// TestInMemoryScanRunRepository_OpenExternal_FreshClaim_ReturnsFreshID
// pins the brief's first invariant: a fresh (verb,
// payload_hash) returns AlreadyExisted=false and a non-nil
// scan_run_id.
func TestInMemoryScanRunRepository_OpenExternal_FreshClaim_ReturnsFreshID(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	req := scanRunRepoTestRequest(t, scanRunHashFor(0x01))

	res, err := repo.OpenExternal(context.Background(), req)
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	if res.AlreadyExisted {
		t.Errorf("AlreadyExisted: want false, got true")
	}
	if res.ScanRunID == uuid.Nil {
		t.Errorf("ScanRunID: want non-nil, got zero UUID")
	}
	if got := repo.Len(); got != 1 {
		t.Errorf("repo.Len: want 1, got %d", got)
	}
}

// TestInMemoryScanRunRepository_OpenExternal_DuplicateHash_ReturnsAlreadyExisted
// is the brief's central durable-idempotency invariant: a
// second OpenExternal with the SAME (verb, payload_hash)
// returns the FIRST scan_run_id and AlreadyExisted=true. No
// new row appears in the store.
func TestInMemoryScanRunRepository_OpenExternal_DuplicateHash_ReturnsAlreadyExisted(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	hash := scanRunHashFor(0x02)
	req := scanRunRepoTestRequest(t, hash)

	first, err := repo.OpenExternal(context.Background(), req)
	if err != nil {
		t.Fatalf("first OpenExternal: %v", err)
	}
	if first.AlreadyExisted {
		t.Fatalf("first.AlreadyExisted: want false, got true")
	}

	// Second call mirrors a publisher retry / replica
	// replay -- same hash, same kind.
	second, err := repo.OpenExternal(context.Background(), req)
	if err != nil {
		t.Fatalf("second OpenExternal: %v", err)
	}
	if !second.AlreadyExisted {
		t.Errorf("second.AlreadyExisted: want true, got false")
	}
	if second.ScanRunID != first.ScanRunID {
		t.Errorf("second.ScanRunID: want %s (first), got %s", first.ScanRunID, second.ScanRunID)
	}
	if got := repo.Len(); got != 1 {
		t.Errorf("repo.Len: want 1 (no new row), got %d", got)
	}
}

// TestInMemoryScanRunRepository_OpenExternal_DifferentHashes_DistinctIDs
// pins the brief's negative: two DIFFERENT payload_hashes
// claim DIFFERENT scan_run_ids.
func TestInMemoryScanRunRepository_OpenExternal_DifferentHashes_DistinctIDs(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	reqA := scanRunRepoTestRequest(t, scanRunHashFor(0x03))
	reqB := scanRunRepoTestRequest(t, scanRunHashFor(0x04))

	resA, err := repo.OpenExternal(context.Background(), reqA)
	if err != nil {
		t.Fatalf("OpenExternal(A): %v", err)
	}
	resB, err := repo.OpenExternal(context.Background(), reqB)
	if err != nil {
		t.Fatalf("OpenExternal(B): %v", err)
	}
	if resA.ScanRunID == resB.ScanRunID {
		t.Errorf("scan_run_ids collided across distinct hashes")
	}
	if resA.AlreadyExisted || resB.AlreadyExisted {
		t.Errorf("AlreadyExisted: want false for both, got A=%v B=%v",
			resA.AlreadyExisted, resB.AlreadyExisted)
	}
	if got := repo.Len(); got != 2 {
		t.Errorf("repo.Len: want 2, got %d", got)
	}
}

// TestInMemoryScanRunRepository_Finalize_HappyPath_TransitionsToTerminal
// pins the success-finalize path: a `running` row moves to
// `succeeded`.
func TestInMemoryScanRunRepository_Finalize_HappyPath_TransitionsToTerminal(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	res, err := repo.OpenExternal(context.Background(), scanRunRepoTestRequest(t, scanRunHashFor(0x05)))
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusSucceeded, scanRunRepoTestNow().Add(time.Second)); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	status, _, ok := repo.Lookup(res.ScanRunID)
	if !ok {
		t.Fatalf("Lookup: row missing after Finalize")
	}
	if status != webhook.ScanRunStatusSucceeded {
		t.Errorf("status: want %q, got %q", webhook.ScanRunStatusSucceeded, status)
	}
}

// TestInMemoryScanRunRepository_Finalize_DoubleSameStatus_IsIdempotent
// pins the idempotent re-finalise contract: a second call to
// Finalize with the SAME terminal status is a no-op (matches
// the PG store's `WHERE status='running'` rows-affected=0
// behaviour mapped to nil).
func TestInMemoryScanRunRepository_Finalize_DoubleSameStatus_IsIdempotent(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	res, err := repo.OpenExternal(context.Background(), scanRunRepoTestRequest(t, scanRunHashFor(0x06)))
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusSucceeded, scanRunRepoTestNow().Add(time.Second)); err != nil {
		t.Fatalf("first Finalize: %v", err)
	}
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusSucceeded, scanRunRepoTestNow().Add(2*time.Second)); err != nil {
		t.Errorf("second Finalize (same status): want nil, got %v", err)
	}
}

// TestInMemoryScanRunRepository_Finalize_DoubleDifferentStatus_Errors
// pins the safety-rail: moving a row from `succeeded` to
// `failed` (or vice-versa) is a wiring bug and MUST surface
// as a non-nil error so the operator log names the mismatch.
func TestInMemoryScanRunRepository_Finalize_DoubleDifferentStatus_Errors(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	res, err := repo.OpenExternal(context.Background(), scanRunRepoTestRequest(t, scanRunHashFor(0x07)))
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusSucceeded, scanRunRepoTestNow().Add(time.Second)); err != nil {
		t.Fatalf("first Finalize: %v", err)
	}
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusFailed, scanRunRepoTestNow().Add(2*time.Second)); err == nil {
		t.Errorf("second Finalize (different status): want error, got nil")
	}
}

// TestInMemoryScanRunRepository_Finalize_UnknownStatus_Rejected pins
// the closed-set guard: any status other than `succeeded` /
// `failed` is rejected with [webhook.ErrScanRunRepoUnknownStatus].
func TestInMemoryScanRunRepository_Finalize_UnknownStatus_Rejected(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	res, err := repo.OpenExternal(context.Background(), scanRunRepoTestRequest(t, scanRunHashFor(0x08)))
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	err = repo.Finalize(context.Background(), res.ScanRunID, "running", scanRunRepoTestNow().Add(time.Second))
	if err == nil {
		t.Fatalf("Finalize(running): want error, got nil")
	}
	if !errors.Is(err, webhook.ErrScanRunRepoUnknownStatus) {
		t.Errorf("Finalize(running): want ErrScanRunRepoUnknownStatus, got %v", err)
	}
}

// TestInMemoryScanRunRepository_Finalize_UnknownScanRunID_Errors pins
// the diagnostic: Finalize on a scan_run_id that was never
// opened MUST surface a non-nil error (it's a wiring bug
// somewhere upstream).
func TestInMemoryScanRunRepository_Finalize_UnknownScanRunID_Errors(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	id, _ := uuid.NewV4()
	if err := repo.Finalize(context.Background(), id, webhook.ScanRunStatusSucceeded, scanRunRepoTestNow()); err == nil {
		t.Errorf("Finalize(unknown id): want error, got nil")
	}
}

// TestInMemoryScanRunRepository_OpenExternal_ZeroRepoID_Errors pins
// the validation: a zero RepoID fails before any state is
// mutated. Mirrors the PG store's [metric_ingestor.ErrZeroRepoID].
func TestInMemoryScanRunRepository_OpenExternal_ZeroRepoID_Errors(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	req := scanRunRepoTestRequest(t, scanRunHashFor(0x09))
	req.RepoID = uuid.Nil
	if _, err := repo.OpenExternal(context.Background(), req); err == nil {
		t.Errorf("OpenExternal(zero RepoID): want error, got nil")
	}
	if got := repo.Len(); got != 0 {
		t.Errorf("repo.Len: want 0 (no row written on validation failure), got %d", got)
	}
}

// TestInMemoryScanRunRepository_OpenExternal_ConcurrentSameHash_CollapsesToOneRow
// pins the brief's "concurrent retries collapse to single
// execution" invariant at the repository layer: 32
// goroutines racing on the SAME (verb, payload_hash) observe
// exactly one underlying row -- 31 of them see
// AlreadyExisted=true.
func TestInMemoryScanRunRepository_OpenExternal_ConcurrentSameHash_CollapsesToOneRow(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	hash := scanRunHashFor(0x0A)
	req := scanRunRepoTestRequest(t, hash)

	const concurrency = 32
	type opResult struct {
		res webhook.ScanRunRepositoryResult
		err error
	}
	results := make([]opResult, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := repo.OpenExternal(context.Background(), req)
			results[i] = opResult{res: r, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	var winningID uuid.UUID
	for _, r := range results {
		if r.err != nil {
			t.Errorf("OpenExternal error: %v", r.err)
			continue
		}
		if !r.res.AlreadyExisted {
			winners++
			winningID = r.res.ScanRunID
		}
	}
	if winners != 1 {
		t.Errorf("winners: want exactly 1, got %d", winners)
	}
	if got := repo.Len(); got != 1 {
		t.Errorf("repo.Len: want 1, got %d", got)
	}
	for i, r := range results {
		if r.err == nil && r.res.ScanRunID != winningID {
			t.Errorf("result[%d].ScanRunID: want %s (winning), got %s", i, winningID, r.res.ScanRunID)
		}
	}
}

// TestInMemoryScanRunRepository_OpenExternal_DifferentVerbs_SamePayload_GetIndependentRuns
// (iter-3 evaluator item #2) pins the per-verb idempotency
// boundary: two distinct verbs (churn + defects) that share
// `Kind=external_per_row` AND have the SAME payload_hash
// MUST receive INDEPENDENT scan_run_ids, NOT collapse to a
// single replay. A `(kind, payload_hash)` key (iter-2's
// shape) would incorrectly collide these. The iter-3
// `(verb, payload_hash)` key keeps them separate.
func TestInMemoryScanRunRepository_OpenExternal_DifferentVerbs_SamePayload_GetIndependentRuns(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	hash := scanRunHashFor(0xC1)

	// Two verbs that share kind=external_per_row but have
	// distinct canonical body shapes downstream.
	churnReq := scanRunRepoTestRequest(t, hash)
	churnReq.Verb = "churn"

	defectsReq := scanRunRepoTestRequest(t, hash)
	defectsReq.Verb = "defects"

	churnRes, err := repo.OpenExternal(context.Background(), churnReq)
	if err != nil {
		t.Fatalf("OpenExternal(churn): %v", err)
	}
	if churnRes.AlreadyExisted {
		t.Errorf("churn AlreadyExisted: want false, got true")
	}

	defectsRes, err := repo.OpenExternal(context.Background(), defectsReq)
	if err != nil {
		t.Fatalf("OpenExternal(defects): %v", err)
	}
	if defectsRes.AlreadyExisted {
		t.Errorf("defects AlreadyExisted: want false (per-verb track), got true (would mean churn collapsed defects)")
	}
	if churnRes.ScanRunID == defectsRes.ScanRunID {
		t.Errorf("scan_run_id collision: churn=%s defects=%s -- two verbs MUST get independent runs",
			churnRes.ScanRunID, defectsRes.ScanRunID)
	}
	if got := repo.Len(); got != 2 {
		t.Errorf("repo.Len: want 2 (one per verb), got %d", got)
	}

	// A second POST for either verb MUST still resolve to
	// that verb's own row, not the other verb's row.
	churnReplay, err := repo.OpenExternal(context.Background(), churnReq)
	if err != nil {
		t.Fatalf("OpenExternal(churn replay): %v", err)
	}
	if !churnReplay.AlreadyExisted {
		t.Errorf("churn replay AlreadyExisted: want true, got false")
	}
	if churnReplay.ScanRunID != churnRes.ScanRunID {
		t.Errorf("churn replay id: want %s, got %s", churnRes.ScanRunID, churnReplay.ScanRunID)
	}
}

// TestInMemoryScanRunRepository_Finalize_SameTerminal_ReturnsNil pins
// the (iter-3 item #4) same-terminal-double-finalize contract
// at the in-memory layer: the second call MUST return nil.
func TestInMemoryScanRunRepository_Finalize_SameTerminal_ReturnsNil(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	res, err := repo.OpenExternal(context.Background(), scanRunRepoTestRequest(t, scanRunHashFor(0xF1)))
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	ended := scanRunRepoTestNow().Add(time.Second)
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusSucceeded, ended); err != nil {
		t.Fatalf("Finalize#1: %v", err)
	}
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusSucceeded, ended); err != nil {
		t.Errorf("Finalize#2 (same terminal): want nil per contract, got %v", err)
	}
}

// TestInMemoryScanRunRepository_Finalize_DifferentTerminal_ReturnsError
// pins the negative contract: a double-finalize with a
// DIFFERENT terminal status MUST surface a wrapped error so
// the operator log names the mismatch.
func TestInMemoryScanRunRepository_Finalize_DifferentTerminal_ReturnsError(t *testing.T) {
	t.Parallel()
	repo := webhook.NewInMemoryScanRunRepository()
	res, err := repo.OpenExternal(context.Background(), scanRunRepoTestRequest(t, scanRunHashFor(0xF2)))
	if err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	ended := scanRunRepoTestNow().Add(time.Second)
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusSucceeded, ended); err != nil {
		t.Fatalf("Finalize#1: %v", err)
	}
	if err := repo.Finalize(context.Background(), res.ScanRunID, webhook.ScanRunStatusFailed, ended); err == nil {
		t.Errorf("Finalize#2 (different terminal): want error (status mismatch), got nil")
	}
}
