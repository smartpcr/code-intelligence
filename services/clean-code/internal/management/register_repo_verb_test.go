package management

// Stage 6.2 -- HTTP-level tests for `mgmt.register_repo`.
//
// The store-side atomicity / idempotency invariants are
// covered by [repo_store_test.go]. THIS suite pins the wire
// layer:
//
//   * happy path -- 200 + canonical body shape + repo_event
//     row appended,
//   * idempotency on URL -- second call returns the
//     existing repo_id with `created=false` AND NO second
//     repo_event(kind='registered') row is appended,
//   * validation (400 on empty url / default_branch,
//     invalid mode, unknown body field),
//   * auth (401 on missing X-OIDC-Subject),
//   * method guard (405 on GET),
//   * wiring guard (503 when repoStore unwired),
//   * actor attribution sourced from X-OIDC-Subject header
//     (the body MUST NOT carry an `actor` field).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
)

// fixedRegisterRepoActor is the OIDC subject used by every
// happy-path test in this file. Pinned so the
// repo_event.payload.actor assertion matches a known value.
const fixedRegisterRepoActor = "alice@example.com"

// fixedRegisterRepoURL is the canonical operator-supplied
// URL used by the happy-path / idempotency tests.
const fixedRegisterRepoURL = "https://github.com/example/repo"

// newWiredRegisterWriter wires an [MgmtWriter] with a
// real [InMemoryRepoStore] + appender so the
// register_repo / set_mode verbs are reachable. The
// retract / rescan deps are stubs (those handlers are
// covered by their own test suite).
func newWiredRegisterWriter(t *testing.T) (*MgmtWriter, *InMemoryRepoStore, *InMemoryRepoEventAppender) {
	t.Helper()
	app := NewInMemoryRepoEventAppender()
	store := NewInMemoryRepoStore(app)
	w := NewMgmtWriter(
		stubSampleResolver{},
		stubRetractDispatcher{},
		stubRescanEnqueuer{},
		app,
		WithMgmtWriterRepoStore(store),
	)
	return w, store, app
}

// registerRepoBody is a wire-payload helper.
func registerRepoBody(t *testing.T, url, branch, mode string) []byte {
	t.Helper()
	payload := map[string]any{
		"repo_url":       url,
		"default_branch": branch,
	}
	if mode != "" {
		payload["mode"] = mode
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal register_repo body: %v", err)
	}
	return b
}

func TestMgmtWriter_RegisterRepo_HappyPath(t *testing.T) {
	t.Parallel()
	w, store, app := newWiredRegisterWriter(t)

	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "main", "")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp registerRepoWireResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rr.Body.String())
	}
	if !resp.Created {
		t.Errorf("created=%v, want true (fresh insert)", resp.Created)
	}
	repoID, err := uuid.FromString(resp.RepoID)
	if err != nil {
		t.Fatalf("response repo_id is not a uuid: %v", err)
	}
	if repoID == uuid.Nil {
		t.Error("response repo_id is the zero UUID")
	}
	if resp.Mode != RepoModeEmbedded {
		t.Errorf("response mode=%q, want %q (default)", resp.Mode, RepoModeEmbedded)
	}
	// Catalog row exists and matches.
	rec, ok := store.Lookup(repoID)
	if !ok {
		t.Fatalf("store.Lookup(%s) not found after RegisterRepo", repoID)
	}
	if rec.RepoURL != fixedRegisterRepoURL {
		t.Errorf("row.repo_url=%q, want %q", rec.RepoURL, fixedRegisterRepoURL)
	}
	if rec.DefaultBranch != "main" {
		t.Errorf("row.default_branch=%q, want %q", rec.DefaultBranch, "main")
	}
	if rec.Mode != RepoModeEmbedded {
		t.Errorf("row.mode=%q, want %q (default)", rec.Mode, RepoModeEmbedded)
	}
	if rec.DisplayName != "repo" {
		t.Errorf("row.display_name=%q, want %q (derived from URL path-tail when omitted)", rec.DisplayName, "repo")
	}

	// Exactly ONE repo_event(kind='registered') was appended.
	events := app.EventsForRepo(repoID)
	if len(events) != 1 {
		t.Fatalf("repo_event count=%d, want 1", len(events))
	}
	if events[0].Kind != RepoEventKindRegistered {
		t.Errorf("event.kind=%q, want %q", events[0].Kind, RepoEventKindRegistered)
	}
	// Payload carries the canonical keys from the brief.
	for _, k := range []string{"repo_url", "default_branch", "mode", "actor"} {
		if _, ok := events[0].Payload[k]; !ok {
			t.Errorf("event.payload missing key %q (got %v)", k, events[0].Payload)
		}
	}
	wantActor := actorPrefix + fixedRegisterRepoActor
	if got, _ := events[0].Payload["actor"].(string); got != wantActor {
		t.Errorf("event.payload.actor=%q, want %q", got, wantActor)
	}
}

// TestMgmtWriter_RegisterRepo_IdempotentOnURL pins the
// `register-repo-idempotent` impl-plan scenario verbatim:
// a second call with the same URL returns the existing
// repo_id with `created=false` and NO second
// `registered` event is appended.
func TestMgmtWriter_RegisterRepo_IdempotentOnURL(t *testing.T) {
	t.Parallel()
	w, store, app := newWiredRegisterWriter(t)

	// First call -- fresh insert.
	r1 := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "main", "")))
	r1.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr1 := httptest.NewRecorder()
	w.RegisterRepo(rr1, r1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first call: status=%d, want 200; body=%s", rr1.Code, rr1.Body.String())
	}
	var resp1 registerRepoWireResponse
	if err := json.Unmarshal(rr1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("unmarshal first response: %v", err)
	}
	if !resp1.Created {
		t.Fatalf("first call: created=%v, want true", resp1.Created)
	}

	// Second call -- SAME url, mode argument deliberately
	// changed (which is ignored on the idempotent path
	// per the contract); the existing repo_id MUST be
	// returned unchanged.
	r2 := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "main", "linked")))
	r2.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr2 := httptest.NewRecorder()
	w.RegisterRepo(rr2, r2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second call: status=%d, want 200; body=%s", rr2.Code, rr2.Body.String())
	}
	var resp2 registerRepoWireResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal second response: %v", err)
	}
	if resp2.Created {
		t.Errorf("second call: created=%v, want false (idempotent re-register)", resp2.Created)
	}
	if resp2.RepoID != resp1.RepoID {
		t.Errorf("second call: repo_id=%q, want existing %q (idempotency)", resp2.RepoID, resp1.RepoID)
	}
	// Existing mode (embedded from first call) is echoed
	// back even though the caller asked for 'linked' --
	// the caller MUST use mgmt.set_mode to change mode
	// on an existing repo.
	if resp2.Mode != RepoModeEmbedded {
		t.Errorf("second call: mode=%q, want %q (existing row's mode is preserved)", resp2.Mode, RepoModeEmbedded)
	}

	// Store carries ONE row only.
	if store.Count() != 1 {
		t.Errorf("store row count=%d, want 1 (no duplicate)", store.Count())
	}
	// Audit log carries ONE registered event only.
	repoID := uuid.Must(uuid.FromString(resp1.RepoID))
	events := app.EventsForRepo(repoID)
	if len(events) != 1 {
		t.Errorf("repo_event count=%d, want 1 (audit log holds one `registered` per lifecycle)", len(events))
	}
}

func TestMgmtWriter_RegisterRepo_ExplicitMode(t *testing.T) {
	t.Parallel()
	w, store, _ := newWiredRegisterWriter(t)

	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "main", RepoModeLinked)))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp registerRepoWireResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Mode != RepoModeLinked {
		t.Errorf("response mode=%q, want %q", resp.Mode, RepoModeLinked)
	}
	rec, _ := store.Lookup(uuid.Must(uuid.FromString(resp.RepoID)))
	if rec.Mode != RepoModeLinked {
		t.Errorf("row.mode=%q, want %q", rec.Mode, RepoModeLinked)
	}
}

func TestMgmtWriter_RegisterRepo_RejectsGET(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	req := httptest.NewRequest(http.MethodGet, VerbMgmtRegisterRepoPath, nil)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405; body=%s", rr.Code, rr.Body.String())
	}
	if allow := rr.Header().Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("Allow header=%q, want substring POST", allow)
	}
}

func TestMgmtWriter_RegisterRepo_RejectsMissingOIDC(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "main", "")))
	// No OIDC subject header.
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing-OIDC status=%d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RegisterRepo_RejectsEmptyURL(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, "", "main", "")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty-URL status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "repo_url") {
		t.Errorf("body=%q, want substring 'repo_url'", rr.Body.String())
	}
}

func TestMgmtWriter_RegisterRepo_RejectsWhitespaceOnlyURL(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, "   ", "main", "")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("whitespace-URL status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RegisterRepo_RejectsEmptyDefaultBranch(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "", "")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty-default-branch status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "default_branch") {
		t.Errorf("body=%q, want substring 'default_branch'", rr.Body.String())
	}
}

func TestMgmtWriter_RegisterRepo_RejectsInvalidMode(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "main", "garbage")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid-mode status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "embedded") || !strings.Contains(body, "linked") {
		t.Errorf("body=%q, want substring with allowed modes", body)
	}
}

func TestMgmtWriter_RegisterRepo_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	// Payload includes `actor` -- attribution MUST come
	// from the OIDC header, not the body. The strict
	// decoder rejects this with 400.
	body := []byte(`{"repo_url":"https://example.com/r","default_branch":"main","actor":"mallory"}`)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(body))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestMgmtWriter_RegisterRepo_AcceptsModesPluralField pins
// the brief contract verbatim: the workstream description
// uses `register_repo(repo_url, default_branch, modes)`
// (plural), so a caller SHOULD be able to supply `modes` on
// the wire and have it interpreted as the single mode
// value. The schema column is singular and the
// `mgmt.set_mode` verb is singular, so internally the
// store still gets one value; the wire just accepts both
// the brief's plural spelling AND the natural singular
// `mode`.
func TestMgmtWriter_RegisterRepo_AcceptsModesPluralField(t *testing.T) {
	t.Parallel()
	w, store, app := newWiredRegisterWriter(t)
	body := []byte(`{"repo_url":"https://example.com/plural","default_branch":"main","modes":"linked"}`)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(body))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp registerRepoWireResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Mode != RepoModeLinked {
		t.Errorf("mode=%q via plural `modes`, want %q", resp.Mode, RepoModeLinked)
	}
	// Verify the row was actually stored with the right mode.
	rec, ok := store.LookupByURL("https://example.com/plural")
	if !ok {
		t.Fatalf("store missing row after plural-modes register")
	}
	if rec.Mode != RepoModeLinked {
		t.Errorf("row.mode=%q, want %q", rec.Mode, RepoModeLinked)
	}
	// And exactly one `registered` event was appended.
	events := app.EventsForRepo(rec.RepoID)
	if len(events) != 1 || events[0].Kind != RepoEventKindRegistered {
		t.Errorf("events=%+v, want exactly one registered event", events)
	}
}

func TestMgmtWriter_RegisterRepo_RejectsBothModeAndModes(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	// Supplying BOTH is ambiguous: which one wins? Reject
	// at the wire so the operator picks one.
	body := []byte(`{"repo_url":"https://example.com/r","default_branch":"main","mode":"embedded","modes":"linked"}`)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(body))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("both-fields status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "mode") || !strings.Contains(rr.Body.String(), "modes") {
		t.Errorf("body=%q, want substring identifying both field names", rr.Body.String())
	}
}

func TestMgmtWriter_RegisterRepo_503WhenStoreNotWired(t *testing.T) {
	t.Parallel()
	// Build a writer WITHOUT the repoStore option.
	w := NewMgmtWriter(
		stubSampleResolver{},
		stubRetractDispatcher{},
		stubRescanEnqueuer{},
		NewInMemoryRepoEventAppender(),
	)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "main", "")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RegisterRepo_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader([]byte(`{"repo_url"`)))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtWriter_RegisterRepo_NoEventAppendedOnValidationFailure(t *testing.T) {
	t.Parallel()
	w, _, app := newWiredRegisterWriter(t)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, "", "main", "")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	w.RegisterRepo(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
	if app.Count() != 0 {
		t.Errorf("repo_event count=%d, want 0 (validation rejected before store call)", app.Count())
	}
}

// TestRepoStore_RegisterRepo_Atomicity pins the store-level
// atomicity contract: if the appender fails, the in-memory
// row insert is rolled back. This is the in-memory analogue
// of the PG transaction rollback that the follow-up stage
// will deliver.
func TestRepoStore_RegisterRepo_Atomicity(t *testing.T) {
	t.Parallel()
	app := &failingAppender{}
	store := NewInMemoryRepoStore(app)

	_, err := store.RegisterRepo(context.Background(), RegisterRepoRowRequest{
		RepoURL:       fixedRegisterRepoURL,
		DefaultBranch: "main",
		Actor:         fixedRegisterRepoActor,
	})
	if err == nil {
		t.Fatalf("expected error from failing appender, got nil")
	}
	if store.Count() != 0 {
		t.Errorf("store row count=%d after appender failure, want 0 (atomic rollback)", store.Count())
	}
	if _, ok := store.LookupByURL(fixedRegisterRepoURL); ok {
		t.Error("store retained URL index entry after rollback")
	}
}

// TestRepoStore_RegisterRepo_DirectValidation pins the
// store-side validators -- the wire layer normally catches
// these, but a future direct-Go caller (e.g. a CLI tool)
// gets the same defensive guards.
func TestRepoStore_RegisterRepo_DirectValidation(t *testing.T) {
	t.Parallel()
	store := NewInMemoryRepoStore(NewInMemoryRepoEventAppender())

	for _, tc := range []struct {
		name string
		req  RegisterRepoRowRequest
		want error
	}{
		{"empty-url", RegisterRepoRowRequest{RepoURL: "", DefaultBranch: "main"}, ErrRepoStoreEmptyURL},
		{"whitespace-url", RegisterRepoRowRequest{RepoURL: "  ", DefaultBranch: "main"}, ErrRepoStoreEmptyURL},
		{"empty-default-branch", RegisterRepoRowRequest{RepoURL: "https://x", DefaultBranch: ""}, ErrRepoStoreEmptyDefaultBranch},
		{"invalid-mode", RegisterRepoRowRequest{RepoURL: "https://x", DefaultBranch: "main", Mode: "garbage"}, ErrRepoStoreInvalidMode},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := store.RegisterRepo(context.Background(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Errorf("err=%v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

// TestMgmtSurfaceRoutes_MountsRegisterRepo pins the unified
// Management surface mount: a non-nil MgmtWriter wired with
// a repoStore exposes the canonical register_repo path.
func TestMgmtSurfaceRoutes_MountsRegisterRepo(t *testing.T) {
	t.Parallel()
	w, _, _ := newWiredRegisterWriter(t)
	mux := MgmtSurfaceRoutes(w, nil)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, bytes.NewReader(registerRepoBody(t, fixedRegisterRepoURL, "main", "")))
	req.Header.Set(OIDCSubjectHeader, fixedRegisterRepoActor)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMgmtSurfaceRoutes_OmitsRegisterRepo_WhenStoreUnwired(t *testing.T) {
	t.Parallel()
	// Writer without RepoStore.
	w := NewMgmtWriter(
		stubSampleResolver{},
		stubRetractDispatcher{},
		stubRescanEnqueuer{},
		NewInMemoryRepoEventAppender(),
	)
	mux := MgmtSurfaceRoutes(w, nil)
	req := httptest.NewRequest(http.MethodPost, VerbMgmtRegisterRepoPath, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (route MUST NOT be mounted when store is nil)", rr.Code)
	}
}

func TestMgmtSurfaceVerbPaths_IsCanonicalClosedSet(t *testing.T) {
	t.Parallel()
	got := MgmtSurfaceVerbPaths()
	want := []string{
		VerbMgmtRegisterRepoPath,
		VerbMgmtSetModePath,
		VerbMgmtRetractSamplePath,
		VerbMgmtRescanPath,
		VerbMgmtOverridePath,
	}
	if len(got) != len(want) {
		t.Fatalf("len(MgmtSurfaceVerbPaths())=%d, want %d", len(got), len(want))
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("MgmtSurfaceVerbPaths()[%d]=%q, want %q", i, got[i], p)
		}
	}
}

// failingAppender is an [RepoEventAppender] that always
// fails. Used to pin store-level atomicity.
type failingAppender struct{}

func (failingAppender) AppendRepoEvent(_ context.Context, _ uuid.UUID, _ string, _ map[string]any) error {
	return errors.New("test-appender: forced failure")
}
