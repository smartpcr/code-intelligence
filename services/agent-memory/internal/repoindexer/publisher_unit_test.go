package repoindexer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestEvent_MarshalPayload_jsonShape pins the on-the-wire JSON
// format the pg_notify payload uses. Downstream subscribers
// decode this; a silent field rename would break them without
// any Go-level compile error.
func TestEvent_MarshalPayload_jsonShape(t *testing.T) {
	t.Parallel()
	ev := Event{
		Kind:   EventKindRepoRegistered,
		RepoID: "00000000-0000-0000-0000-000000000001",
		SHA:    "deadbeef",
		JobID:  "11111111-2222-3333-4444-555555555555",
		Time:   time.Date(2024, 5, 1, 10, 11, 12, 0, time.UTC),
	}
	got, err := ev.MarshalPayload()
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v\npayload: %s", err, got)
	}
	for k, want := range map[string]string{
		"kind":    EventKindRepoRegistered,
		"repo_id": "00000000-0000-0000-0000-000000000001",
		"sha":     "deadbeef",
		"job_id":  "11111111-2222-3333-4444-555555555555",
	} {
		if back[k] != want {
			t.Errorf("payload[%q] = %v, want %s", k, back[k], want)
		}
	}
	// time round-trips as RFC3339-ish string -- enough to
	// confirm the field name and order, not the exact format.
	if _, ok := back["time"].(string); !ok {
		t.Errorf("payload.time was %T (%v); want string", back["time"], back["time"])
	}
}

// TestEvent_MarshalPayload_rejectsEmptyKind guards the closed-set
// invariant: a Publish call with a blank Kind would land an
// unparseable record on the channel.
func TestEvent_MarshalPayload_rejectsEmptyKind(t *testing.T) {
	t.Parallel()
	ev := Event{RepoID: "x", SHA: "y", JobID: "z", Time: time.Now()}
	_, err := ev.MarshalPayload()
	if err == nil {
		t.Fatal("expected empty-kind rejection; got nil")
	}
	if !strings.Contains(err.Error(), "empty kind") {
		t.Errorf("error does not mention empty kind: %v", err)
	}
}

// TestRecordingPublisher_capturesAllPublishedEvents is a sanity
// test for the testhook publisher used by the worker integration
// tests. Pinning behaviour here keeps the integration tests
// short.
func TestRecordingPublisher_capturesAllPublishedEvents(t *testing.T) {
	t.Parallel()
	p := &recordingEventPublisher{}
	for i := 0; i < 3; i++ {
		if err := p.Publish(context.Background(), Event{Kind: "k", RepoID: "r", SHA: "s", JobID: "j"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	if got := len(p.events()); got != 3 {
		t.Errorf("captured %d events, want 3", got)
	}
}

// TestRecordingPublisher_failureModePropagates lets the worker
// integration tests assert "publisher failure rolls the
// done-transition tx back" without coupling to pg_notify. The
// recorder's Publish/PublishTx returns the configured error
// BEFORE recording the event so subsequent assertions on
// `events()` see only successful publishes.
func TestRecordingPublisher_failureModePropagates(t *testing.T) {
	t.Parallel()
	p := &recordingEventPublisher{err: errors.New("boom")}
	if err := p.Publish(context.Background(), Event{Kind: "k"}); err == nil {
		t.Fatal("want error from configured failure mode; got nil")
	}
	if len(p.events()) != 0 {
		t.Errorf("failed publish recorded the event (got %d); want 0", len(p.events()))
	}
}
