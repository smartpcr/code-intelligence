package churn_test

// Stage 4.4 (ingest churn verb feeds materialiser,
// implementation-plan.md lines 410-425) -- Ingester unit
// tests. These cover the staging adapter in isolation:
// validation, scan_run-handle shape, BuildChurnEvents
// payload->row mapping, IngestResult shape, and the
// in-memory store's defence-in-depth checks.
//
// The wire-layer / "verb writes ZERO metric_sample"
// scenario lives in `handler_test.go` (separate file so a
// reader can find the contract-level proof without scrolling
// through unit cases).

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ingest/churn"
)

// fixedScanRunID is the canonical fixture scan_run_id. Pinned
// alongside fixedRepoID so cross-tests can assert handle
// identity round-trips without re-minting.
var fixedScanRunID = uuid.Must(uuid.FromString("99999999-0000-0000-0000-000000000001"))

// stagedAt is the canonical clock reading the Ingester
// stamps on every event in unit tests. Pinned (NOT
// time.Now) so [IngestResult.StagedAt] and every
// [ChurnEvent.CreatedAt] are deterministic across runs.
var stagedAt = time.Date(2026, 6, 1, 9, 30, 45, 0, time.UTC)

// modifiedAt is the canonical [PayloadRow.ModifiedAt].
// Distinct from stagedAt so a test can prove the staging
// row preserves the source row's timestamp instead of
// overwriting it with the staging clock.
var modifiedAt = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// canonicalHandle returns a ScanRunHandle filled with every
// canonical Stage 4.4 invariant ([Kind] etc.) for tests that
// don't care about variation. Use targeted overrides in
// per-test setups when a field needs to differ.
func canonicalHandle() churn.ScanRunHandle {
	return churn.ScanRunHandle{
		ScanRunID:  fixedScanRunID,
		RepoID:     fixedRepoID,
		Verb:       churn.Verb,
		Kind:       churn.Kind,
		SHABinding: churn.SHABinding,
		ToSHA:      "",
		OpenedAt:   stagedAt.Add(-2 * time.Second),
	}
}

// canonicalPayload returns a two-row payload aligned with
// canonicalHandle() (same RepoID).
func canonicalPayload() *churn.Payload {
	return &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{
				SHA:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				FilePath:   "internal/foo.go",
				ModifiedAt: modifiedAt,
				Author:     "alice@example.com",
			},
			{
				SHA:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				FilePath:   "internal/bar.go",
				ModifiedAt: modifiedAt.Add(2 * time.Hour),
				Author:     "bob@example.com",
			},
		},
	}
}

// uuidMinter returns a counter-backed minter producing
// `aaaaaaaa-0000-0000-0000-00000000000N` -- deterministic
// per call, distinguishable across rows.
func uuidMinter() func() (uuid.UUID, error) {
	var counter atomic.Uint64
	return func() (uuid.UUID, error) {
		n := counter.Add(1)
		return uuid.FromString(fmt.Sprintf("aaaaaaaa-0000-0000-0000-%012d", n))
	}
}

// failingMinter returns the (zero, error) pair on the Nth
// call. Used to prove BuildChurnEvents surfaces minter
// errors without partial output.
func failingMinter(failOnCall int) func() (uuid.UUID, error) {
	var counter atomic.Uint64
	return func() (uuid.UUID, error) {
		n := counter.Add(1)
		if int(n) == failOnCall {
			return uuid.Nil, errors.New("test: synthetic minter failure")
		}
		return uuid.FromString(fmt.Sprintf("aaaaaaaa-0000-0000-0000-%012d", n))
	}
}

func TestValidateScanRunHandle_AcceptsCanonical(t *testing.T) {
	t.Parallel()
	if err := churn.ValidateScanRunHandle(canonicalHandle()); err != nil {
		t.Fatalf("ValidateScanRunHandle(canonical) = %v, want nil", err)
	}
}

func TestValidateScanRunHandle_RejectsZeroScanRunID(t *testing.T) {
	t.Parallel()
	h := canonicalHandle()
	h.ScanRunID = uuid.Nil
	err := churn.ValidateScanRunHandle(h)
	if !errors.Is(err, churn.ErrScanRunHandleZeroID) {
		t.Fatalf("err = %v, want errors.Is(_, ErrScanRunHandleZeroID)", err)
	}
}

func TestValidateScanRunHandle_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	h := canonicalHandle()
	h.RepoID = uuid.Nil
	err := churn.ValidateScanRunHandle(h)
	if !errors.Is(err, churn.ErrScanRunHandleZeroRepoID) {
		t.Fatalf("err = %v, want errors.Is(_, ErrScanRunHandleZeroRepoID)", err)
	}
}

func TestValidateScanRunHandle_RejectsWrongVerb(t *testing.T) {
	t.Parallel()
	h := canonicalHandle()
	h.Verb = "coverage"
	err := churn.ValidateScanRunHandle(h)
	if !errors.Is(err, churn.ErrScanRunHandleWrongVerb) {
		t.Fatalf("err = %v, want errors.Is(_, ErrScanRunHandleWrongVerb)", err)
	}
}

func TestValidateScanRunHandle_RejectsWrongKind(t *testing.T) {
	t.Parallel()
	h := canonicalHandle()
	h.Kind = "full"
	err := churn.ValidateScanRunHandle(h)
	if !errors.Is(err, churn.ErrScanRunHandleWrongKind) {
		t.Fatalf("err = %v, want errors.Is(_, ErrScanRunHandleWrongKind)", err)
	}
}

func TestValidateScanRunHandle_RejectsWrongSHABinding(t *testing.T) {
	t.Parallel()
	h := canonicalHandle()
	h.SHABinding = "single"
	err := churn.ValidateScanRunHandle(h)
	if !errors.Is(err, churn.ErrScanRunHandleWrongSHABinding) {
		t.Fatalf("err = %v, want errors.Is(_, ErrScanRunHandleWrongSHABinding)", err)
	}
}

func TestValidateScanRunHandle_RejectsNonEmptyToSHA(t *testing.T) {
	t.Parallel()
	h := canonicalHandle()
	h.ToSHA = "cccccccccccccccccccccccccccccccccccccccc"
	err := churn.ValidateScanRunHandle(h)
	if !errors.Is(err, churn.ErrScanRunHandleToSHANotEmpty) {
		t.Fatalf("err = %v, want errors.Is(_, ErrScanRunHandleToSHANotEmpty)", err)
	}
}

func TestValidateScanRunHandle_RejectsZeroOpenedAt(t *testing.T) {
	t.Parallel()
	h := canonicalHandle()
	h.OpenedAt = time.Time{}
	err := churn.ValidateScanRunHandle(h)
	if !errors.Is(err, churn.ErrScanRunHandleZeroOpenedAt) {
		t.Fatalf("err = %v, want errors.Is(_, ErrScanRunHandleZeroOpenedAt)", err)
	}
}

func TestCanonicalConstantsPinned(t *testing.T) {
	t.Parallel()
	if churn.Verb != "churn" {
		t.Errorf("churn.Verb = %q, want %q", churn.Verb, "churn")
	}
	if churn.Kind != "external_per_row" {
		t.Errorf("churn.Kind = %q, want %q", churn.Kind, "external_per_row")
	}
	if churn.SHABinding != "per_row" {
		t.Errorf("churn.SHABinding = %q, want %q", churn.SHABinding, "per_row")
	}
}

func TestBuildChurnEvents_PreservesOrderAndStampsFields(t *testing.T) {
	t.Parallel()
	payload := canonicalPayload()
	events, err := churn.BuildChurnEvents(fixedScanRunID, payload, stagedAt, uuidMinter())
	if err != nil {
		t.Fatalf("BuildChurnEvents: %v", err)
	}
	if got, want := len(events), len(payload.Rows); got != want {
		t.Fatalf("len(events) = %d, want %d", got, want)
	}
	for i, ev := range events {
		if ev.PayloadRowIndex != i {
			t.Errorf("events[%d].PayloadRowIndex = %d, want %d", i, ev.PayloadRowIndex, i)
		}
		if ev.ScanRunID != fixedScanRunID {
			t.Errorf("events[%d].ScanRunID = %s, want %s", i, ev.ScanRunID, fixedScanRunID)
		}
		if ev.RepoID != payload.RepoID {
			t.Errorf("events[%d].RepoID = %s, want %s", i, ev.RepoID, payload.RepoID)
		}
		if ev.SHA != payload.Rows[i].SHA {
			t.Errorf("events[%d].SHA = %q, want %q", i, ev.SHA, payload.Rows[i].SHA)
		}
		if ev.FilePath != payload.Rows[i].FilePath {
			t.Errorf("events[%d].FilePath = %q, want %q", i, ev.FilePath, payload.Rows[i].FilePath)
		}
		if !ev.ModifiedAt.Equal(payload.Rows[i].ModifiedAt.UTC()) {
			t.Errorf("events[%d].ModifiedAt = %v, want %v (UTC)", i, ev.ModifiedAt, payload.Rows[i].ModifiedAt.UTC())
		}
		if ev.Author != payload.Rows[i].Author {
			t.Errorf("events[%d].Author = %q, want %q", i, ev.Author, payload.Rows[i].Author)
		}
		if !ev.CreatedAt.Equal(stagedAt.UTC()) {
			t.Errorf("events[%d].CreatedAt = %v, want %v", i, ev.CreatedAt, stagedAt.UTC())
		}
		if ev.ChurnEventID == uuid.Nil {
			t.Errorf("events[%d].ChurnEventID is zero", i)
		}
	}
	// Two distinct row IDs prove the minter ran twice.
	if events[0].ChurnEventID == events[1].ChurnEventID {
		t.Errorf("ChurnEventIDs collide: %s", events[0].ChurnEventID)
	}
}

func TestBuildChurnEvents_RejectsZeroScanRunID(t *testing.T) {
	t.Parallel()
	_, err := churn.BuildChurnEvents(uuid.Nil, canonicalPayload(), stagedAt, uuidMinter())
	if !errors.Is(err, churn.ErrZeroScanRunID) {
		t.Fatalf("err = %v, want errors.Is(_, ErrZeroScanRunID)", err)
	}
}

func TestBuildChurnEvents_RejectsZeroNow(t *testing.T) {
	t.Parallel()
	_, err := churn.BuildChurnEvents(fixedScanRunID, canonicalPayload(), time.Time{}, uuidMinter())
	if !errors.Is(err, churn.ErrZeroNow) {
		t.Fatalf("err = %v, want errors.Is(_, ErrZeroNow)", err)
	}
}

func TestBuildChurnEvents_PropagatesValidateError(t *testing.T) {
	t.Parallel()
	bad := &churn.Payload{RepoID: uuid.Nil, Rows: canonicalPayload().Rows}
	_, err := churn.BuildChurnEvents(fixedScanRunID, bad, stagedAt, uuidMinter())
	if !errors.Is(err, churn.ErrEmptyRepoID) {
		t.Fatalf("err = %v, want errors.Is(_, ErrEmptyRepoID)", err)
	}
}

func TestBuildChurnEvents_PropagatesMinterError(t *testing.T) {
	t.Parallel()
	events, err := churn.BuildChurnEvents(fixedScanRunID, canonicalPayload(), stagedAt, failingMinter(2))
	if !errors.Is(err, churn.ErrUUIDMintFailed) {
		t.Fatalf("err = %v, want errors.Is(_, ErrUUIDMintFailed)", err)
	}
	if events != nil {
		t.Fatalf("events = %v, want nil (no partial output on error)", events)
	}
}

func TestIngest_HappyPath_StagesAllRows(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(store, func() time.Time { return stagedAt }, uuidMinter())
	result, err := ing.Ingest(context.Background(), canonicalHandle(), canonicalPayload())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if result.EventsWritten != 2 {
		t.Errorf("EventsWritten = %d, want 2", result.EventsWritten)
	}
	if result.ScanRunID != fixedScanRunID {
		t.Errorf("ScanRunID = %s, want %s", result.ScanRunID, fixedScanRunID)
	}
	if result.RepoID != fixedRepoID {
		t.Errorf("RepoID = %s, want %s", result.RepoID, fixedRepoID)
	}
	if !result.StagedAt.Equal(stagedAt) {
		t.Errorf("StagedAt = %v, want %v", result.StagedAt, stagedAt)
	}
	if got := store.Len(); got != 2 {
		t.Errorf("store.Len() = %d, want 2", got)
	}
}

func TestIngest_RejectsRepoIDMismatch(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(store, func() time.Time { return stagedAt }, uuidMinter())
	handle := canonicalHandle()
	handle.RepoID = mustParseUUID(t, "22222222-3333-4444-5555-666666666666")
	_, err := ing.Ingest(context.Background(), handle, canonicalPayload())
	if !errors.Is(err, churn.ErrRepoIDMismatch) {
		t.Fatalf("err = %v, want errors.Is(_, ErrRepoIDMismatch)", err)
	}
	// All-or-nothing: nothing should have been staged.
	if got := store.Len(); got != 0 {
		t.Errorf("store.Len() = %d, want 0 (no partial write on error)", got)
	}
}

func TestIngest_RejectsNonCanonicalHandle(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(store, func() time.Time { return stagedAt }, uuidMinter())
	handle := canonicalHandle()
	handle.Kind = "external_single"
	_, err := ing.Ingest(context.Background(), handle, canonicalPayload())
	if !errors.Is(err, churn.ErrScanRunHandleWrongKind) {
		t.Fatalf("err = %v, want errors.Is(_, ErrScanRunHandleWrongKind)", err)
	}
	if got := store.Len(); got != 0 {
		t.Errorf("store.Len() = %d, want 0", got)
	}
}

func TestIngest_RejectsMalformedPayload(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(store, func() time.Time { return stagedAt }, uuidMinter())
	bad := &churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{{
			SHA:        "not-a-sha",
			FilePath:   "internal/foo.go",
			ModifiedAt: modifiedAt,
		}},
	}
	_, err := ing.Ingest(context.Background(), canonicalHandle(), bad)
	if !errors.Is(err, churn.ErrInvalidSHA) {
		t.Fatalf("err = %v, want errors.Is(_, ErrInvalidSHA)", err)
	}
	if got := store.Len(); got != 0 {
		t.Errorf("store.Len() = %d, want 0", got)
	}
}

func TestIngest_RejectsNilPayload(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(store, func() time.Time { return stagedAt }, uuidMinter())
	_, err := ing.Ingest(context.Background(), canonicalHandle(), nil)
	if err == nil {
		t.Fatalf("Ingest(nil payload) returned no error")
	}
}

func TestIngest_NilContextIsAnError(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	ing := churn.NewIngesterWithClocks(store, func() time.Time { return stagedAt }, uuidMinter())
	//nolint:staticcheck // intentionally passing nil context to verify defence.
	_, err := ing.Ingest(nil, canonicalHandle(), canonicalPayload())
	if err == nil {
		t.Fatalf("Ingest(nil ctx) returned no error")
	}
}

// failingWriter returns errFailingWriter on every
// WriteChurnEvents call.
type failingWriter struct{}

var errFailingWriter = errors.New("test: synthetic writer failure")

func (failingWriter) WriteChurnEvents(_ context.Context, _ []churn.ChurnEvent) error {
	return errFailingWriter
}

func TestIngest_WrapsWriterError(t *testing.T) {
	t.Parallel()
	ing := churn.NewIngesterWithClocks(failingWriter{}, func() time.Time { return stagedAt }, uuidMinter())
	_, err := ing.Ingest(context.Background(), canonicalHandle(), canonicalPayload())
	if !errors.Is(err, churn.ErrChurnEventWriteFailed) {
		t.Fatalf("err = %v, want errors.Is(_, ErrChurnEventWriteFailed)", err)
	}
	if !errors.Is(err, errFailingWriter) {
		t.Fatalf("err = %v, want errors.Is(_, errFailingWriter)", err)
	}
}

func TestNewIngester_PanicsOnNilWriter(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatalf("NewIngester(nil) did not panic")
		}
	}()
	_ = churn.NewIngester(nil)
}

func TestNewIngesterWithClocks_PanicsOnNilArgs(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	for name, fn := range map[string]func(){
		"nil writer": func() {
			_ = churn.NewIngesterWithClocks(nil, func() time.Time { return stagedAt }, uuidMinter())
		},
		"nil clock": func() {
			_ = churn.NewIngesterWithClocks(store, nil, uuidMinter())
		},
		"nil minter": func() {
			_ = churn.NewIngesterWithClocks(store, func() time.Time { return stagedAt }, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewIngesterWithClocks(%s) did not panic", name)
				}
			}()
			fn()
		})
	}
}

func TestInMemoryChurnEventStore_RejectsDuplicateRowIndex(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	ev := churn.ChurnEvent{
		ChurnEventID:    mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000001"),
		ScanRunID:       fixedScanRunID,
		RepoID:          fixedRepoID,
		SHA:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FilePath:        "internal/foo.go",
		ModifiedAt:      modifiedAt,
		PayloadRowIndex: 0,
		CreatedAt:       stagedAt,
	}
	if err := store.WriteChurnEvents(context.Background(), []churn.ChurnEvent{ev}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Second write -- same (scan_run_id, payload_row_index).
	ev.ChurnEventID = mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000002")
	err := store.WriteChurnEvents(context.Background(), []churn.ChurnEvent{ev})
	if err == nil {
		t.Fatalf("second write succeeded; want error from unique-constraint defence")
	}
}

func TestInMemoryChurnEventStore_ListChurnEventsForRepo(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	otherRepo := mustParseUUID(t, "22222222-3333-4444-5555-666666666666")
	events := []churn.ChurnEvent{
		{
			ChurnEventID:    mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000001"),
			ScanRunID:       fixedScanRunID,
			RepoID:          fixedRepoID,
			SHA:             "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			FilePath:        "internal/foo.go",
			ModifiedAt:      modifiedAt,
			PayloadRowIndex: 0,
			CreatedAt:       stagedAt,
		},
		{
			ChurnEventID:    mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000002"),
			ScanRunID:       fixedScanRunID,
			RepoID:          fixedRepoID,
			SHA:             "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			FilePath:        "internal/bar.go",
			ModifiedAt:      modifiedAt.Add(2 * time.Hour),
			PayloadRowIndex: 1,
			CreatedAt:       stagedAt,
		},
		{
			ChurnEventID:    mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000003"),
			ScanRunID:       fixedScanRunID,
			RepoID:          otherRepo,
			SHA:             "cccccccccccccccccccccccccccccccccccccccc",
			FilePath:        "internal/baz.go",
			ModifiedAt:      modifiedAt,
			PayloadRowIndex: 2,
			CreatedAt:       stagedAt,
		},
	}
	if err := store.WriteChurnEvents(context.Background(), events); err != nil {
		t.Fatalf("WriteChurnEvents: %v", err)
	}
	got, err := store.ListChurnEventsForRepo(context.Background(), fixedRepoID, time.Time{})
	if err != nil {
		t.Fatalf("ListChurnEventsForRepo: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (other-repo row filtered)", len(got))
	}
	// Sorted by modified_at DESC -- the bar.go row (2h later) comes first.
	if got[0].FilePath != "internal/bar.go" {
		t.Errorf("got[0].FilePath = %q, want internal/bar.go (DESC by modified_at)", got[0].FilePath)
	}
	if got[1].FilePath != "internal/foo.go" {
		t.Errorf("got[1].FilePath = %q, want internal/foo.go", got[1].FilePath)
	}
	// `since` filter applied.
	got2, err := store.ListChurnEventsForRepo(context.Background(), fixedRepoID, modifiedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListChurnEventsForRepo(since): %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("len(got2) = %d, want 1 (since filter)", len(got2))
	}
	if got2[0].FilePath != "internal/bar.go" {
		t.Errorf("got2[0].FilePath = %q, want internal/bar.go", got2[0].FilePath)
	}
}

func TestInMemoryChurnEventStore_RejectsZeroRepoIDOnRead(t *testing.T) {
	t.Parallel()
	store := churn.NewInMemoryChurnEventStore()
	_, err := store.ListChurnEventsForRepo(context.Background(), uuid.Nil, time.Time{})
	if err == nil {
		t.Fatalf("ListChurnEventsForRepo(zero repo) returned no error")
	}
}
