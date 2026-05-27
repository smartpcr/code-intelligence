package management

// Stage 6.2 -- HTTP-level tests for `mgmt.set_mode`.
//
// The store-side atomicity contract is covered alongside
// `register_repo` in [register_repo_verb_test.go]. THIS
// suite pins the wire layer:
//
//   * happy path -- mode flipped + `mode_changed` event
//     appended,
//   * no-op when new mode equals current mode (200 +
//     `changed:false`, no event),
//   * 404 on unknown repo_id,
//   * validation (400 on invalid mode, zero repo_id,
//     malformed UUID, unknown body field),
//   * auth (401 on missing X-OIDC-Subject),
//   * method guard (405 on GET),
//   * wiring guard (503 when repoStore unwired),
//   * actor attribution sourced from X-OIDC-Subject header.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
)

// setupSetModeRepo seeds a fresh repo at mode `embedded` via
// the store and returns the writer + store + appender +
// repo_id. Mirrors the impl-plan scenario "Given a repo at
// mode `embedded`".
func setupSetModeRepo(t *testing.T) (*MgmtWriter, *InMemoryRepoStore, *InMemoryRepoEventAppender, uuid.UUID) {
	t.Helper()
	w, store, app := newWiredRegisterWriter(t)
	res, err := store.RegisterRepo(context.Background(), RegisterRepoRowRequest{
		RepoURL:       fixedRegisterRepoURL,
		DefaultBranch: "main",
		Mode:          RepoModeEmbedded,
		Actor:         fixedRegisterRepoActor,
	})
	if err != nil {
		t.Fatalf("seed RegisterRepo: %v", err)
	}
	return w, store, app, res.RepoID
}

// setModeBody is a wire-payload helper.
func setModeBody(t *testing.T, repoID uuid.UUID, mode string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"repo_id": repoID.String(),
		"mode":    mode,
	})
	if err != nil {
		t.Fatalf("marshal set_mode body: %v", err)
	}
	return b
}

// TestMgmtWriter_SetMode_HappyPath pins the impl-plan
// `set-mode-emits-event` scenario verbatim: a transition
// from `embedded` to `linked` writes a `mode_changed`
// repo_event and the row's mode reflects the new value.
func TestMgmtWriter_SetMode_HappyPath(t *testing.T) {
	t.Parallel()
	w, store, app, repoID := setupSetModeRepo(t)
	// Snapshot the seed event count so we don't conflate
	// the `registered` row with the new `mode_changed`.
	seedEvents := len(app.EventsForRepo(repoID))
	if seedEvents != 1 {
		t.Fatalf("seed events count=%d, want 1 (registered)", seedEvents)
	}

	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, repoID, RepoModeLinked)))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp setModeWireResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rr.Body.String())
	}
	if resp.RepoID != repoID.String() {
		t.Errorf("response repo_id=%q, want %q", resp.RepoID, repoID.String())
	}
	if resp.Mode != RepoModeLinked {
		t.Errorf("response mode=%q, want %q", resp.Mode, RepoModeLinked)
	}
	if resp.PreviousMode != RepoModeEmbedded {
		t.Errorf("response previous_mode=%q, want %q", resp.PreviousMode, RepoModeEmbedded)
	}
	if !resp.Changed {
		t.Errorf("response changed=%v, want true", resp.Changed)
	}

	// Row's mode is now `linked`.
	rec, ok := store.Lookup(repoID)
	if !ok {
		t.Fatalf("store.Lookup(%s) missing after set_mode", repoID)
	}
	if rec.Mode != RepoModeLinked {
		t.Errorf("row.mode=%q, want %q", rec.Mode, RepoModeLinked)
	}

	// Exactly ONE NEW repo_event(kind='mode_changed') was
	// appended (plus the seed 'registered' from setup).
	events := app.EventsForRepo(repoID)
	if len(events) != seedEvents+1 {
		t.Fatalf("event count=%d, want %d (one new mode_changed)", len(events), seedEvents+1)
	}
	last := events[len(events)-1]
	if last.Kind != RepoEventKindModeChanged {
		t.Errorf("last event.kind=%q, want %q", last.Kind, RepoEventKindModeChanged)
	}
	if got, _ := last.Payload["mode"].(string); got != RepoModeLinked {
		t.Errorf("event.payload.mode=%q, want %q", got, RepoModeLinked)
	}
	if got, _ := last.Payload["previous_mode"].(string); got != RepoModeEmbedded {
		t.Errorf("event.payload.previous_mode=%q, want %q", got, RepoModeEmbedded)
	}
	wantActor := actorPrefix + fixedRegisterRepoActor
	if got, _ := last.Payload["actor"].(string); got != wantActor {
		t.Errorf("event.payload.actor=%q, want %q", got, wantActor)
	}
}

// TestMgmtWriter_SetMode_NoOpWhenSameMode pins the
// architecture invariant "mode_changed records a TRANSITION":
// a call that re-asserts the existing mode is 200 +
// `changed:false` and NO event is appended.
func TestMgmtWriter_SetMode_NoOpWhenSameMode(t *testing.T) {
	t.Parallel()
	w, store, app, repoID := setupSetModeRepo(t)
	seedEvents := len(app.EventsForRepo(repoID))

	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, repoID, RepoModeEmbedded)))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp setModeWireResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Changed {
		t.Errorf("changed=%v, want false (no-op)", resp.Changed)
	}
	if resp.Mode != RepoModeEmbedded {
		t.Errorf("mode=%q, want %q", resp.Mode, RepoModeEmbedded)
	}
	if resp.PreviousMode != RepoModeEmbedded {
		t.Errorf("previous_mode=%q, want %q", resp.PreviousMode, RepoModeEmbedded)
	}

	// Row unchanged.
	rec, _ := store.Lookup(repoID)
	if rec.Mode != RepoModeEmbedded {
		t.Errorf("row.mode=%q after no-op, want %q", rec.Mode, RepoModeEmbedded)
	}
	// No new event appended.
	if got := len(app.EventsForRepo(repoID)); got != seedEvents {
		t.Errorf("event count=%d after no-op, want %d (no mode_changed appended)", got, seedEvents)
	}
}

func TestMgmtWriter_SetMode_RoundTripEmbeddedLinkedEmbedded(t *testing.T) {
	t.Parallel()
	w, _, app, repoID := setupSetModeRepo(t)

	// embedded -> linked
	r1 := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, repoID, RepoModeLinked)))
	r1.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr1 := httptest.NewRecorder()
	w.SetMode(rr1, r1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first flip status=%d, want 200", rr1.Code)
	}

	// linked -> embedded
	r2 := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, repoID, RepoModeEmbedded)))
	r2.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr2 := httptest.NewRecorder()
	w.SetMode(rr2, r2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second flip status=%d, want 200", rr2.Code)
	}
	var resp setModeWireResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.PreviousMode != RepoModeLinked {
		t.Errorf("second flip previous_mode=%q, want %q", resp.PreviousMode, RepoModeLinked)
	}

	// Audit log: registered + two mode_changed rows.
	events := app.EventsForRepo(repoID)
	if len(events) != 3 {
		t.Fatalf("event count=%d, want 3 (registered + 2 mode_changed)", len(events))
	}
	if events[1].Kind != RepoEventKindModeChanged || events[2].Kind != RepoEventKindModeChanged {
		t.Errorf("expected last two events to be mode_changed; got %q, %q", events[1].Kind, events[2].Kind)
	}
}

func TestMgmtWriter_SetMode_404OnUnknownRepo(t *testing.T) {
	t.Parallel()
	w, _, app := newWiredRegisterWriter(t)
	missing := uuid.Must(uuid.NewV4())
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, missing, RepoModeLinked)))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	// No event should be appended for a 404.
	if app.Count() != 0 {
		t.Errorf("event count=%d after 404, want 0", app.Count())
	}
}

func TestMgmtWriter_SetMode_RejectsGET(t *testing.T) {
	t.Parallel()
	w, _, _, _ := setupSetModeRepo(t)
	req := httptest.NewRequest(http.MethodGet, VerbMgmtSetModePath, nil)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405", rr.Code)
	}
}

func TestMgmtWriter_SetMode_RejectsMissingOIDC(t *testing.T) {
	t.Parallel()
	w, _, _, repoID := setupSetModeRepo(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, repoID, RepoModeLinked)))
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_SetMode_RejectsInvalidMode(t *testing.T) {
	t.Parallel()
	w, _, _, repoID := setupSetModeRepo(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, repoID, "garbage")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid-mode status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, RepoModeEmbedded) || !strings.Contains(body, RepoModeLinked) {
		t.Errorf("body=%q, want substring with allowed modes", body)
	}
}

func TestMgmtWriter_SetMode_RejectsEmptyMode(t *testing.T) {
	t.Parallel()
	w, _, _, repoID := setupSetModeRepo(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, repoID, "")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty-mode status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_SetMode_RejectsZeroRepoID(t *testing.T) {
	t.Parallel()
	w, _, _, _ := setupSetModeRepo(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, uuid.Nil, RepoModeLinked)))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("zero-uuid status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_SetMode_RejectsMalformedRepoID(t *testing.T) {
	t.Parallel()
	w, _, _, _ := setupSetModeRepo(t)
	body := []byte(`{"repo_id":"not-a-uuid","mode":"linked"}`)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(body))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed-uuid status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_SetMode_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	w, _, _, repoID := setupSetModeRepo(t)
	body := []byte(`{"repo_id":"` + repoID.String() + `","mode":"linked","actor":"mallory"}`)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(body))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_SetMode_503WhenStoreNotWired(t *testing.T) {
	t.Parallel()
	w := NewMgmtWriter(
		stubSampleResolver{},
		stubRetractDispatcher{},
		stubRescanEnqueuer{},
		NewInMemoryRepoEventAppender(),
	)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, uuid.Must(uuid.NewV4()), RepoModeLinked)))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.SetMode(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

// TestMgmtSurfaceRoutes_MountsSetMode pins the unified
// Management surface mount: set_mode is reachable through
// [MgmtSurfaceRoutes] when the writer is wired with a
// RepoStore.
func TestMgmtSurfaceRoutes_MountsSetMode(t *testing.T) {
	t.Parallel()
	w, _, _, repoID := setupSetModeRepo(t)
	mux := MgmtSurfaceRoutes(w, nil)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(setModeBody(t, repoID, RepoModeLinked)))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandler_RoutesIncludesStage62Verbs pins the iter-2
// invariant: when the composition root wires a writer with a
// non-nil RepoStore via [NewHandlerWithWriter] +
// [WithMgmtWriterRepoStore], the Stage 6.2 verb paths
// `/v1/mgmt/register_repo` and `/v1/mgmt/set_mode` are
// mounted on the SAME Handler.Routes() mux that the service
// exposes (i.e. reachable from production HTTP, not only
// from package-local tests).
func TestHandler_RoutesIncludesStage62Verbs_WhenWriterWiredWithStore(t *testing.T) {
	t.Parallel()
	app := NewInMemoryRepoEventAppender()
	store := NewInMemoryRepoStore(app)
	writer := NewMgmtWriter(
		stubSampleResolver{},
		stubRetractDispatcher{},
		stubRescanEnqueuer{},
		app,
		WithMgmtWriterRepoStore(store),
	)
	h := NewHandlerWithWriter(NewReader(nil), writer)
	mux := h.Routes()
	for _, path := range []string{VerbMgmtRegisterRepoPath, VerbMgmtSetModePath} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("path=%s status=%d, want 405 (route is mounted; GET rejected by method guard)", path, rr.Code)
		}
	}
}

// TestHandler_OmitsStage62Verbs_WhenStoreUnwired pins the
// inverse: a writer WITHOUT a RepoStore must NOT advertise
// the new endpoints.
func TestHandler_OmitsStage62Verbs_WhenStoreUnwired(t *testing.T) {
	t.Parallel()
	writer := NewMgmtWriter(
		stubSampleResolver{},
		stubRetractDispatcher{},
		stubRescanEnqueuer{},
		NewInMemoryRepoEventAppender(),
	)
	h := NewHandlerWithWriter(NewReader(nil), writer)
	mux := h.Routes()
	for _, path := range []string{VerbMgmtRegisterRepoPath, VerbMgmtSetModePath} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("path=%s status=%d, want 404 (route MUST NOT be mounted when store is nil)", path, rr.Code)
		}
	}
}
