package main

// Unit tests for the qdrant-bootstrap binary.
//
// The HTTP layer is exercised against a local httptest.Server
// so the suite runs without docker / a real Qdrant. A
// real-Qdrant integration test is gated on the
// AGENT_MEMORY_QDRANT_URL environment variable; CI sets it on
// runners that have the qdrant container up, and a developer
// laptop without docker simply skips it.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeQdrant is a minimal Qdrant API stand-in. It only models
// the endpoints the bootstrap actually calls: GET collection,
// PUT collection (create), PUT index, POST snapshots. The
// state map records what was created so assertions can pin
// the request shape.
type fakeQdrant struct {
	mu sync.Mutex
	// collections[name] -> stored vectorsConfig
	collections map[string]vectorsConfig
	// payloadSchema[name][field] -> field schema
	payloadSchema map[string]map[string]string
	// snapshots[name] -> count
	snapshots map[string]int
	// recorded request count by (method, path)
	calls map[string]int
}

func newFakeQdrant() *fakeQdrant {
	return &fakeQdrant{
		collections:   map[string]vectorsConfig{},
		payloadSchema: map[string]map[string]string{},
		snapshots:     map[string]int{},
		calls:         map[string]int{},
	}
}

func (f *fakeQdrant) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()

	// GET /collections (list endpoint -- no name suffix). Required
	// by the implementation-plan §1.4 acceptance scenario:
	// "When `GET /collections` is issued ... all three are present
	// with distance: cosine".
	mux.HandleFunc("/collections", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.Method+" "+r.URL.Path]++
		f.mu.Unlock()
		if r.Method != http.MethodGet {
			http.Error(w, "list endpoint only supports GET",
				http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var resp listCollectionsResponse
		for name := range f.collections {
			resp.Result.Collections = append(
				resp.Result.Collections, struct {
					Name string `json:"name"`
				}{Name: name})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/collections/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.Method+" "+r.URL.Path]++
		f.mu.Unlock()

		// /collections/<name>            -- GET / PUT / DELETE
		// /collections/<name>/index      -- PUT
		// /collections/<name>/snapshots  -- POST
		path := strings.TrimPrefix(r.URL.Path, "/collections/")
		parts := strings.SplitN(path, "/", 2)
		name := parts[0]
		var tail string
		if len(parts) == 2 {
			tail = parts[1]
		}

		switch {
		case tail == "" && r.Method == http.MethodGet:
			f.mu.Lock()
			defer f.mu.Unlock()
			cfg, ok := f.collections[name]
			if !ok {
				http.Error(w, `{"status":{"error":"not found"}}`,
					http.StatusNotFound)
				return
			}
			resp := getCollectionResponse{}
			resp.Result.Config.Params.Vectors = cfg
			resp.Result.PayloadSchema = map[string]struct {
				DataType string `json:"data_type"`
			}{}
			for field, schema := range f.payloadSchema[name] {
				resp.Result.PayloadSchema[field] = struct {
					DataType string `json:"data_type"`
				}{DataType: schema}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case tail == "" && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var req createCollectionRequest
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if _, exists := f.collections[name]; exists {
				// Real Qdrant returns 4xx on duplicate; we
				// surface the same so EnsureCollection's
				// "already exists" branch is taken by GET
				// not by PUT.
				http.Error(w, "exists", http.StatusBadRequest)
				return
			}
			f.collections[name] = req.Vectors
			f.payloadSchema[name] = map[string]string{}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result": true}`))

		case tail == "index" && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var req createPayloadIndexRequest
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if _, ok := f.collections[name]; !ok {
				http.Error(w, "collection missing", http.StatusNotFound)
				return
			}
			f.payloadSchema[name][req.FieldName] = req.FieldSchema
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result": true}`))

		case tail == "snapshots" && r.Method == http.MethodPost:
			f.mu.Lock()
			f.snapshots[name]++
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result": {"name": "snap-1"}}`))

		default:
			http.Error(w, "unhandled", http.StatusNotImplemented)
		}
	})
	return mux
}

func newBootstrapperForTest(t *testing.T, baseURL string) *Bootstrapper {
	t.Helper()
	b := NewBootstrapper(baseURL, defaultVectorSize)
	b.Logger = log.New(testWriter{t: t}, "[test] ", 0)
	b.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	return b
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func TestBootstrap_createsAllThreeCollectionsFromEmpty(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	for _, name := range defaultCollections {
		fake.mu.Lock()
		cfg, ok := fake.collections[name]
		schema := fake.payloadSchema[name]
		fake.mu.Unlock()
		if !ok {
			t.Errorf("collection %s not created", name)
			continue
		}
		if cfg.Size != defaultVectorSize {
			t.Errorf("%s.size = %d, want %d", name, cfg.Size, defaultVectorSize)
		}
		if cfg.Distance != distanceCosine {
			t.Errorf("%s.distance = %q, want %q", name, cfg.Distance, distanceCosine)
		}
		for _, idx := range defaultPayloadIndexes {
			got, ok := schema[idx.FieldName]
			if !ok {
				t.Errorf("payload index %s.%s missing", name, idx.FieldName)
				continue
			}
			if got != idx.FieldSchema {
				t.Errorf("payload index %s.%s schema = %q, want %q",
					name, idx.FieldName, got, idx.FieldSchema)
			}
		}
	}
}

func TestBootstrap_isIdempotentOnRerun(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("second Bootstrap (idempotent): %v", err)
	}

	// Practically: the fakeQdrant rejects duplicate PUT with
	// 400, so a leaked second PUT would have failed Bootstrap.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	getCount := 0
	for k, v := range fake.calls {
		if strings.HasPrefix(k, http.MethodGet+" ") {
			getCount += v
		}
	}
	// EnsureCollection issues 1 GET; each EnsurePayloadIndex
	// issues 1 GET. So per collection per run: 1 + 2 = 3 GETs.
	// Across 3 collections * 2 runs: 18 GETs total.
	wantMin := 2 * len(defaultCollections)
	if getCount < wantMin {
		t.Errorf("expected at least %d GETs across two bootstrap runs, got %d",
			wantMin, getCount)
	}
}

func TestBootstrap_failsWhenExistingCollectionDisagreesOnSize(t *testing.T) {
	fake := newFakeQdrant()
	// Seed a collection with the WRONG vector size to simulate
	// a stale or hand-edited deployment.
	fake.collections[defaultCollections[0]] = vectorsConfig{
		Size:     1024,
		Distance: distanceCosine,
	}
	fake.payloadSchema[defaultCollections[0]] = map[string]string{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	b.VectorSize = defaultVectorSize // 768 != 1024
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := b.Bootstrap(ctx)
	if err == nil {
		t.Fatal("expected size-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "vector size") {
		t.Errorf("error %q does not mention 'vector size'", err.Error())
	}
}

func TestBootstrap_failsWhenExistingCollectionDisagreesOnDistance(t *testing.T) {
	fake := newFakeQdrant()
	fake.collections[defaultCollections[0]] = vectorsConfig{
		Size:     defaultVectorSize,
		Distance: "Euclid", // wrong on purpose
	}
	fake.payloadSchema[defaultCollections[0]] = map[string]string{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := b.Bootstrap(ctx)
	if err == nil {
		t.Fatal("expected distance-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "distance") {
		t.Errorf("error %q does not mention 'distance'", err.Error())
	}
}

func TestBootstrap_failsWhenExistingIndexHasWrongSchema(t *testing.T) {
	fake := newFakeQdrant()
	first := defaultCollections[0]
	fake.collections[first] = vectorsConfig{
		Size:     defaultVectorSize,
		Distance: distanceCosine,
	}
	fake.payloadSchema[first] = map[string]string{
		"repo_id": "integer", // wrong: must be uuid
	}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := b.Bootstrap(ctx)
	if err == nil {
		t.Fatal("expected payload-index mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "repo_id") {
		t.Errorf("error %q does not mention 'repo_id'", err.Error())
	}
}

func TestBootstrap_dryRunIssuesNoWrites(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	b.DryRun = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("dry-run Bootstrap: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for k := range fake.calls {
		if strings.HasPrefix(k, http.MethodPut+" ") ||
			strings.HasPrefix(k, http.MethodPost+" ") {
			t.Errorf("dry-run issued mutating call %s", k)
		}
	}
	if len(fake.collections) != 0 {
		t.Errorf("dry-run created %d collections; want 0", len(fake.collections))
	}
}

func TestBootstrap_snapshotFlagRequestsBaseline(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	b.Snapshot = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, name := range defaultCollections {
		if fake.snapshots[name] != 1 {
			t.Errorf("snapshots[%s] = %d, want 1",
				name, fake.snapshots[name])
		}
	}
}

func TestBootstrap_rejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		b    *Bootstrapper
	}{
		{
			name: "empty url",
			b:    &Bootstrapper{BaseURL: "", VectorSize: 768},
		},
		{
			name: "zero vector size",
			b:    &Bootstrapper{BaseURL: "http://x", VectorSize: 0},
		},
		{
			name: "negative vector size",
			b:    &Bootstrapper{BaseURL: "http://x", VectorSize: -1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.b.Bootstrap(context.Background())
			if err == nil {
				t.Errorf("Bootstrap with bad input should error")
			}
		})
	}
}

// TestBootstrap_collectionsFieldOverridesPackageDefault pins
// the contract the live test (and any future cross-environment
// caller) relies on: setting Bootstrapper.Collections drives
// Bootstrap (and the snapshot scheduler) at the configured
// list, NOT the package-level defaultCollections. Without this
// guarantee the Collections field is theatre.
//
// This test also doubles as a regression guard: a future
// refactor that re-introduces a `for _, name := range
// defaultCollections` in Bootstrap would be caught by the
// "production names must NOT have been provisioned" assertion
// at the bottom of the test.
func TestBootstrap_collectionsFieldOverridesPackageDefault(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	custom := []string{"override_alpha", "override_beta"}

	b := newBootstrapperForTest(t, srv.URL)
	b.Collections = custom

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, name := range custom {
		if _, ok := fake.collections[name]; !ok {
			t.Errorf("custom collection %s not provisioned; "+
				"Collections override is not wired into Bootstrap",
				name)
		}
	}
	for _, name := range defaultCollections {
		if _, ok := fake.collections[name]; ok {
			t.Errorf("package-default collection %s was "+
				"provisioned even though Collections override "+
				"was set; Bootstrap is still iterating the "+
				"global default", name)
		}
	}
}

// TestBootstrap_listCollectionsReportsAllThreeWithCosine
// exercises the implementation-plan.md Stage 1.4 acceptance
// scenario verbatim: "When `GET /collections` is issued
// against Qdrant, Then all three collections
// (agent_memory_method, agent_memory_block,
// agent_memory_concept) are present with distance: cosine".
//
// We assert against the LIST endpoint specifically (not just
// per-collection GETs), because the scenario calls out the
// list endpoint by name.
func TestBootstrap_listCollectionsReportsAllThreeWithCosine(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Issue GET /collections (list endpoint, no name).
	got, err := b.ListCollections(ctx)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}

	// Build a presence set so order doesn't matter.
	present := make(map[string]bool, len(got))
	for _, n := range got {
		present[n] = true
	}
	for _, want := range defaultCollections {
		if !present[want] {
			t.Errorf("collection %s missing from GET /collections "+
				"response (got %v)", want, got)
		}
	}

	// Per-collection distance assertion. The list endpoint
	// returns names only, so we still need a per-name GET to
	// verify "with distance: cosine". CollectionDistance is
	// the typed wrapper for that follow-up GET.
	for _, name := range defaultCollections {
		dist, err := b.CollectionDistance(ctx, name)
		if err != nil {
			t.Errorf("CollectionDistance(%s): %v", name, err)
			continue
		}
		if !strings.EqualFold(dist, distanceCosine) {
			t.Errorf("collection %s distance = %q, want %q",
				name, dist, distanceCosine)
		}
	}

	// Sanity: the list call actually hit the list endpoint
	// (not a per-name GET). Catches a future regression that
	// silently rewrites ListCollections to N per-name GETs
	// (which would not satisfy the scenario's literal wording).
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls[http.MethodGet+" /collections"] < 1 {
		t.Errorf("ListCollections did not hit GET /collections; "+
			"recorded calls = %v", fake.calls)
	}
}

// TestBootstrap_snapshotLoopRunsRecurringly verifies the
// scheduler half of the bootstrap. The Qdrant REST API has no
// recurring-snapshot endpoint, so this binary IS the schedule;
// the test therefore drives SnapshotLoop directly with a tiny
// interval and asserts that snapshot POSTs accumulate over
// time.
//
// Timing: we use a 25ms interval and wait until the fakeQdrant
// has observed ≥ 2 snapshot rounds (≥ 2 POSTs per collection).
// We poll on a 5s deadline so a slow CI host doesn't false-fail.
func TestBootstrap_snapshotLoopRunsRecurringly(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	// Run the bootstrap first so the collections exist for
	// snapshot POSTs to land against.
	bootstrapCtx, bootstrapCancel := context.WithTimeout(
		context.Background(), 5*time.Second)
	defer bootstrapCancel()
	if err := b.Bootstrap(bootstrapCtx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Now drive SnapshotLoop with a short interval and a
	// cancellable context.
	loopCtx, loopCancel := context.WithCancel(context.Background())
	defer loopCancel()

	loopDone := make(chan error, 1)
	go func() {
		loopDone <- b.SnapshotLoop(loopCtx, 25*time.Millisecond)
	}()

	// Wait until every collection has accumulated at least 2
	// snapshot POSTs (= 2 schedule rounds).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		have := true
		for _, name := range defaultCollections {
			if fake.snapshots[name] < 2 {
				have = false
				break
			}
		}
		fake.mu.Unlock()
		if have {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	loopCancel()
	select {
	case err := <-loopDone:
		// Cancelled context returns context.Canceled; treat
		// that as success.
		if err != nil && err != context.Canceled {
			t.Errorf("SnapshotLoop returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SnapshotLoop did not exit after cancel within 2s")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, name := range defaultCollections {
		if fake.snapshots[name] < 2 {
			t.Errorf("snapshots[%s] = %d, want >= 2 (scheduler "+
				"did not fire enough rounds)",
				name, fake.snapshots[name])
		}
	}
}

// TestBootstrap_snapshotLoopRejectsZeroInterval guards the
// daemon-mode entrypoint: an unconfigured interval must fail
// loud rather than spin a hot loop.
func TestBootstrap_snapshotLoopRejectsZeroInterval(t *testing.T) {
	b := newBootstrapperForTest(t, "http://unused")
	err := b.SnapshotLoop(context.Background(), 0)
	if err == nil {
		t.Fatal("SnapshotLoop(0) must error")
	}
	if !strings.Contains(err.Error(), "interval") {
		t.Errorf("error %q does not mention 'interval'", err.Error())
	}
}

// TestBootstrap_runWithScheduleNoIntervalIsBootstrapOnly proves
// that --snapshot-interval=0 (the default) keeps the binary in
// one-shot mode -- it must NOT enter the loop.
func TestBootstrap_runWithScheduleNoIntervalIsBootstrapOnly(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	// Default: SnapshotInterval=0, no recurring schedule.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := b.RunWithSchedule(ctx); err != nil {
		t.Fatalf("RunWithSchedule: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("RunWithSchedule with no interval took %v; "+
			"expected near-immediate return", elapsed)
	}

	// Sanity: one-shot RunWithSchedule must NOT have issued
	// snapshot POSTs (--snapshot was false too).
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, name := range defaultCollections {
		if fake.snapshots[name] != 0 {
			t.Errorf("snapshots[%s] = %d, want 0 (no --snapshot, "+
				"no --snapshot-interval)", name, fake.snapshots[name])
		}
	}
}

// TestBootstrap_runWithScheduleStopsOnContextCancel proves the
// daemon path exits cleanly when its context is cancelled
// (e.g. SIGTERM during a graceful shutdown).
func TestBootstrap_runWithScheduleStopsOnContextCancel(t *testing.T) {
	fake := newFakeQdrant()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	b := newBootstrapperForTest(t, srv.URL)
	b.SnapshotInterval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- b.RunWithSchedule(ctx)
	}()

	// Let the loop spin up at least one round, then cancel.
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Errorf("RunWithSchedule exited with %v; want nil or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithSchedule did not exit within 2s of cancel")
	}
}

// TestBootstrap_againstLiveQdrant exercises the bootstrap
// against a real Qdrant when AGENT_MEMORY_QDRANT_URL is set
// in the environment. The test creates collections with a
// unique suffix so it doesn't collide with a developer's
// shared instance; cleanup happens via DELETE at the end.
func TestBootstrap_againstLiveQdrant(t *testing.T) {
	url := os.Getenv("AGENT_MEMORY_QDRANT_URL")
	if url == "" {
		t.Skip("AGENT_MEMORY_QDRANT_URL not set; skipping live Qdrant test")
	}

	// Use a unique per-run collection set so we don't disturb
	// a shared Qdrant. Setting Bootstrapper.Collections scopes
	// the override to this Bootstrapper instance -- no global
	// mutation, no save/restore ceremony, no data race risk if
	// a future contributor adds t.Parallel() to a sibling test
	// (rubber-duck #4).
	suffix := time.Now().UTC().Format("20060102_150405")
	testCollections := []string{
		"am_test_method_" + suffix,
		"am_test_block_" + suffix,
		"am_test_concept_" + suffix,
	}

	b := NewBootstrapper(url, 8) // tiny vector size for speed
	b.Collections = testCollections
	b.Logger = log.New(testWriter{t: t}, "[live] ", 0)
	b.HTTPClient = &http.Client{Timeout: 15 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("live Bootstrap: %v", err)
	}
	// Idempotency on real Qdrant.
	if err := b.Bootstrap(ctx); err != nil {
		t.Fatalf("live Bootstrap (rerun): %v", err)
	}

	// implementation-plan.md Stage 1.4 acceptance scenario:
	// "When `GET /collections` is issued against Qdrant, Then
	// all three collections are present with distance: cosine".
	// We verify against testCollections (the set THIS run
	// provisioned), not defaultCollections, because a shared
	// Qdrant may legitimately host the production names from
	// other deploys -- we only own the suffixed names.
	got, err := b.ListCollections(ctx)
	if err != nil {
		t.Fatalf("live ListCollections: %v", err)
	}
	present := make(map[string]bool, len(got))
	for _, n := range got {
		present[n] = true
	}
	for _, want := range testCollections {
		if !present[want] {
			t.Errorf("live: collection %s missing from "+
				"GET /collections (got %v)", want, got)
		}
		dist, derr := b.CollectionDistance(ctx, want)
		if derr != nil {
			t.Errorf("live CollectionDistance(%s): %v", want, derr)
			continue
		}
		if !strings.EqualFold(dist, distanceCosine) {
			t.Errorf("live: %s distance = %q, want %q",
				want, dist, distanceCosine)
		}
	}

	// Best-effort cleanup so subsequent runs don't accumulate
	// leftovers in a shared instance.
	for _, name := range testCollections {
		req, err := http.NewRequestWithContext(ctx,
			http.MethodDelete, url+"/collections/"+name, nil)
		if err != nil {
			t.Logf("build cleanup DELETE %s: %v", name, err)
			continue
		}
		req.Header.Set("User-Agent", httpUserAgent)
		resp, derr := b.HTTPClient.Do(req)
		if derr != nil {
			t.Logf("cleanup DELETE %s: %v", name, derr)
			continue
		}
		_ = resp.Body.Close()
	}
}
