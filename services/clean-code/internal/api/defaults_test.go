package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewDefaultRegistry_MountsEveryCanonicalVerb(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	for _, cv := range CanonicalVerbs {
		v, ok := reg.Lookup(cv.Namespace, cv.Name)
		if !ok {
			t.Errorf("verb %s not registered", cv.DottedName())
			continue
		}
		if v.Handler == nil {
			t.Errorf("verb %s has nil handler", cv.DottedName())
		}
		if cv.RepoIDSource == RepoIDFromHeader && v.RepoIDExtractor == nil {
			t.Errorf("verb %s has nil RepoIDExtractor (RepoIDSource=Header)", cv.DottedName())
		}
	}
	if got, want := len(reg.Verbs()), len(CanonicalVerbs); got != want {
		t.Errorf("registry has %d verbs, want %d", got, want)
	}
}

func TestNewDefaultRegistry_StubReturns503VerbNotWired(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	v, ok := reg.Lookup("mgmt", "register_repo")
	if !ok {
		t.Fatalf("mgmt.register_repo not registered")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/mgmt/register_repo", strings.NewReader(`{"repo_id":"r"}`))
	v.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", w.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Code != "VERB_NOT_WIRED" {
		t.Errorf("code=%q, want VERB_NOT_WIRED", env.Code)
	}
}

func TestVerbRegistry_ReplaceSwapsHandler(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	called := false
	real := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	reg.Replace("mgmt.register_repo", real, nil)
	v, _ := reg.Lookup("mgmt", "register_repo")
	w := httptest.NewRecorder()
	v.Handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/mgmt/register_repo", nil))
	if !called {
		t.Fatalf("replacement handler was not invoked")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200", w.Code)
	}
}

func TestVerbRegistry_ReplaceDottedReadVerb(t *testing.T) {
	t.Parallel()
	// `mgmt.read.repo` is split as ns=mgmt, name=read.repo
	// (everything after the first dot).
	reg := NewDefaultRegistry()
	called := false
	real := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})
	reg.Replace("mgmt.read.repo", real, nil)
	v, ok := reg.Lookup("mgmt", "read.repo")
	if !ok {
		t.Fatalf("mgmt.read.repo missing after Replace")
	}
	v.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Errorf("read.repo replacement handler was not invoked")
	}
}

func TestVerbRegistry_ReplacePanicsOnUnknownVerb(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	defer func() {
		if recover() == nil {
			t.Errorf("Replace of unknown verb did not panic")
		}
	}()
	reg.Replace("mgmt.does_not_exist", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil)
}

func TestVerbRegistry_ReplacePanicsOnNilHandler(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	defer func() {
		if recover() == nil {
			t.Errorf("Replace with nil handler did not panic")
		}
	}()
	reg.Replace("mgmt.register_repo", nil, nil)
}

func TestVerbRegistry_ReplacePanicsOnNoDot(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	defer func() {
		if recover() == nil {
			t.Errorf("Replace with no-dot name did not panic")
		}
	}()
	reg.Replace("nodot", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil)
}

func TestVerbRegistry_ReplaceSwapsExtractorWhenProvided(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	customCalled := false
	custom := func(r *http.Request) (string, *http.Request, error) {
		customCalled = true
		return "override", r, nil
	}
	reg.Replace(
		"mgmt.register_repo",
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		custom,
	)
	v, _ := reg.Lookup("mgmt", "register_repo")
	if v.RepoIDExtractor == nil {
		t.Fatalf("extractor not set")
	}
	id, _, _ := v.RepoIDExtractor(httptest.NewRequest("POST", "/x", nil))
	if !customCalled || id != "override" {
		t.Errorf("custom extractor not invoked (called=%v id=%q)", customCalled, id)
	}
}

func TestVerbRegistry_ReplacePreservesExtractorWhenNil(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	before, _ := reg.Lookup("mgmt", "register_repo")
	if before.RepoIDExtractor == nil {
		t.Fatalf("default extractor unexpectedly nil")
	}
	reg.Replace(
		"mgmt.register_repo",
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		nil,
	)
	after, _ := reg.Lookup("mgmt", "register_repo")
	if after.RepoIDExtractor == nil {
		t.Errorf("Replace with nil extractor wiped the default extractor")
	}
}

func TestCanonicalVerbNames_DeterministicAndComplete(t *testing.T) {
	t.Parallel()
	names := CanonicalVerbNames()
	if len(names) != len(CanonicalVerbs) {
		t.Fatalf("CanonicalVerbNames=%d, want %d", len(names), len(CanonicalVerbs))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("not sorted: %q >= %q", names[i-1], names[i])
		}
	}
	// Spot-check a few key verbs from architecture Sec 6.2-6.5.
	want := map[string]bool{
		"eval.gate":               false,
		"mgmt.register_repo":      false,
		"mgmt.read.repo":          false,
		"policy.publish":          false,
		"policy.keys.list_active": false,
		"ingest.coverage":         false,
	}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("canonical verb %q missing", k)
		}
	}
}

func TestCanonicalVerb_PathAndDottedName(t *testing.T) {
	t.Parallel()
	cv := CanonicalVerb{Namespace: "mgmt", Name: "register_repo"}
	if cv.DottedName() != "mgmt.register_repo" {
		t.Errorf("DottedName=%q", cv.DottedName())
	}
	if cv.Path() != "/v1/mgmt/register_repo" {
		t.Errorf("Path=%q", cv.Path())
	}
}

func TestCanonicalVerb_ExtractorForCoverage(t *testing.T) {
	t.Parallel()
	for src, want := range map[RepoIDSource]bool{
		RepoIDFromJSONBody: true,
		RepoIDFromQuery:    true,
		RepoIDFromHeader:   true,
		RepoIDNone:         true,
		RepoIDSource(99):   true, // unknown -> NoRepoIDExtractor
	} {
		cv := CanonicalVerb{Namespace: "x", Name: "y", RepoIDSource: src}
		got := cv.ExtractorFor()
		if (got != nil) != want {
			t.Errorf("ExtractorFor(%d) nil-ness mismatch", src)
		}
	}
}

// ---------------------------------------------------------------------------
// Wiring + NewWiredRegistry -- Item #1 from iter-2 feedback.
// The composition root populates Wiring with real handlers
// adapted from the existing per-namespace packages; the
// registry must swap them in over the 503 stub.
// ---------------------------------------------------------------------------

func TestNewWiredRegistry_EmptyWiringKeepsAllStubs(t *testing.T) {
	t.Parallel()
	reg := NewWiredRegistry(Wiring{})
	if got, want := len(reg.Verbs()), len(CanonicalVerbs); got != want {
		t.Errorf("registry has %d verbs, want %d", got, want)
	}
	// Sample a few canonical verbs and confirm they all
	// still return the 503 stub.
	for _, name := range []string{"mgmt.register_repo", "eval.gate", "ingest.coverage"} {
		ns, n, _ := splitDottedName(name)
		v, ok := reg.Lookup(ns, n)
		if !ok {
			t.Fatalf("verb %q missing from default-wired registry", name)
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/"+ns+"/"+n, strings.NewReader(`{}`))
		v.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("nil-slot verb %q returned %d, want 503", name, w.Code)
		}
	}
}

func TestNewWiredRegistry_NonNilSlotSwapsHandler(t *testing.T) {
	t.Parallel()
	realCalled := false
	realHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		realCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	reg := NewWiredRegistry(Wiring{
		MgmtRegisterRepo: realHandler,
	})
	v, ok := reg.Lookup("mgmt", "register_repo")
	if !ok {
		t.Fatalf("mgmt.register_repo missing")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/mgmt/register_repo", strings.NewReader(`{"repo_id":"r"}`))
	v.Handler.ServeHTTP(w, req)
	if !realCalled {
		t.Errorf("Wiring slot handler was not invoked")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (wired handler should respond, not the 503 stub)", w.Code)
	}
	// Verify SIBLING verbs (left nil) still return 503 -- only
	// the targeted slot was swapped.
	other, _ := reg.Lookup("mgmt", "set_mode")
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/mgmt/set_mode", strings.NewReader(`{}`))
	other.Handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusServiceUnavailable {
		t.Errorf("sibling verb status=%d, want 503 (only register_repo was wired)", w2.Code)
	}
}

func TestWiring_MissingAndWiredVerbs(t *testing.T) {
	t.Parallel()
	stub := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	w := Wiring{
		MgmtRegisterRepo: stub,
		EvalGate:         stub,
	}
	wired := w.WiredVerbs()
	if len(wired) != 2 {
		t.Errorf("WiredVerbs has %d entries, want 2", len(wired))
	}
	missing := w.MissingVerbs()
	if len(missing) != len(CanonicalVerbs)-2 {
		t.Errorf("MissingVerbs has %d entries, want %d", len(missing), len(CanonicalVerbs)-2)
	}
	// MissingVerbs + WiredVerbs partition CanonicalVerbs.
	partition := map[string]bool{}
	for _, v := range wired {
		partition[v] = true
	}
	for _, v := range missing {
		if partition[v] {
			t.Errorf("verb %q appears in both Wired and Missing", v)
		}
		partition[v] = true
	}
	if len(partition) != len(CanonicalVerbs) {
		t.Errorf("Wired+Missing covers %d, want %d", len(partition), len(CanonicalVerbs))
	}
}

func TestWiringSlots_MatchCanonicalVerbsExactly(t *testing.T) {
	t.Parallel()
	if err := validateWiringSlots(); err != nil {
		t.Fatalf("wiringSlots vs CanonicalVerbs drift: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Item #3 from iter-2 feedback: mgmt.override must extract
// repo_id from the nested scope_filter.repo_id field, not
// from headers or top-level body.
// ---------------------------------------------------------------------------

func TestDefaults_MgmtOverride_UsesNestedScopeFilterExtractor(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	v, ok := reg.Lookup("mgmt", "override")
	if !ok {
		t.Fatalf("mgmt.override not registered")
	}
	if v.RepoIDExtractor == nil {
		t.Fatalf("mgmt.override has nil RepoIDExtractor -- spans will be missing repo_id")
	}
	body := `{"rule_id":"r-1","scope_filter":{"repo_id":"repo-42","scope_kind":"file"},"mute":true}`
	req := httptest.NewRequest("POST", "/v1/mgmt/override", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	repoID, _, err := v.RepoIDExtractor(req)
	if err != nil {
		t.Fatalf("extractor err: %v", err)
	}
	if repoID != "repo-42" {
		t.Errorf("repoID=%q, want repo-42 (extracted from scope_filter.repo_id)", repoID)
	}
}

// ---------------------------------------------------------------------------
// Item #2 from iter-2 feedback: ingest.churn / ingest.defects
// must read repo_id from JSON body (the canonical source per
// webhook handlers), NOT from header.
// ---------------------------------------------------------------------------

func TestDefaults_IngestChurn_ReadsRepoIDFromJSONBody(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	v, _ := reg.Lookup("ingest", "churn")
	if v.RepoIDExtractor == nil {
		t.Fatalf("ingest.churn has nil RepoIDExtractor")
	}
	body := `{"repo_id":"repo-55","commits":[]}`
	req := httptest.NewRequest("POST", "/v1/ingest/churn", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	repoID, _, err := v.RepoIDExtractor(req)
	if err != nil {
		t.Fatalf("extractor err: %v", err)
	}
	if repoID != "repo-55" {
		t.Errorf("repoID=%q, want repo-55 (item #2: must read JSON body, not header)", repoID)
	}
}

func TestDefaults_IngestDefects_ReadsRepoIDFromJSONBody(t *testing.T) {
	t.Parallel()
	reg := NewDefaultRegistry()
	v, _ := reg.Lookup("ingest", "defects")
	if v.RepoIDExtractor == nil {
		t.Fatalf("ingest.defects has nil RepoIDExtractor")
	}
	body := `{"repo_id":"repo-77","defects":[]}`
	req := httptest.NewRequest("POST", "/v1/ingest/defects", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	repoID, _, err := v.RepoIDExtractor(req)
	if err != nil {
		t.Fatalf("extractor err: %v", err)
	}
	if repoID != "repo-77" {
		t.Errorf("repoID=%q, want repo-77 (item #2: must read JSON body, not header)", repoID)
	}
}

// splitDottedName is a test helper that parses "ns.name" into
// the (namespace, name) pair the registry indexes by. Mirrors
// the production split inside VerbRegistry.Replace.
func splitDottedName(dotted string) (ns, name string, ok bool) {
	idx := strings.Index(dotted, ".")
	if idx < 0 {
		return "", "", false
	}
	return dotted[:idx], dotted[idx+1:], true
}
