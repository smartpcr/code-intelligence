package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// decodeOne parses the single JSON line buf is expected to
// contain. Helper for the assertions below.
func decodeOne(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatalf("log buffer is empty")
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		t.Fatalf("expected exactly one log record, got %d lines: %q", strings.Count(line, "\n")+1, line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("decoding JSON record %q: %v", line, err)
	}
	return m
}

func TestNew_EmitsJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := New(Config{Writer: &buf})

	log.Info("hello", "key", "value")

	rec := decodeOne(t, &buf)
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v; want hello", rec["msg"])
	}
	if rec["key"] != "value" {
		t.Errorf("key = %v; want value", rec["key"])
	}
	if rec["service.name"] != "clean-code" {
		t.Errorf("service.name = %v; want clean-code", rec["service.name"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v; want INFO", rec["level"])
	}
}

func TestNew_ServiceNameOverride(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := New(Config{Writer: &buf, ServiceName: "clean-coded-test"})
	log.Info("boot")
	rec := decodeOne(t, &buf)
	if rec["service.name"] != "clean-coded-test" {
		t.Errorf("service.name override failed: %v", rec["service.name"])
	}
}

// TestRequestIDPropagation is the load-bearing test for the
// architecture Sec 8 "request id propagation" telemetry
// invariant: ANY ctx-aware log call must surface the request id
// without the caller explicitly threading it through attrs.
func TestRequestIDPropagation(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := New(Config{Writer: &buf})

	ctx := WithRequestID(context.Background(), "req-12345")
	log.InfoContext(ctx, "handling request")

	rec := decodeOne(t, &buf)
	if rec[AttrRequestID] != "req-12345" {
		t.Fatalf("%s = %v; want req-12345 (propagation failed)", AttrRequestID, rec[AttrRequestID])
	}
}

func TestRequestIDAbsenceIsSilent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := New(Config{Writer: &buf})
	log.InfoContext(context.Background(), "no id")
	rec := decodeOne(t, &buf)
	if _, ok := rec[AttrRequestID]; ok {
		t.Errorf("%s should be absent when ctx carries no id, got %v", AttrRequestID, rec[AttrRequestID])
	}
}

// TestRequestIDSurvivesWithAttrs proves the slog.Logger.With(...)
// path (which calls Handler.WithAttrs) still routes through the
// request-id middleware. A broken WithAttrs implementation here
// is silent in unit tests that only call Info directly.
func TestRequestIDSurvivesWithAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := New(Config{Writer: &buf}).With("subsystem", "ingest")
	ctx := WithRequestID(context.Background(), "req-with")
	log.InfoContext(ctx, "ingest event")
	rec := decodeOne(t, &buf)
	if rec[AttrRequestID] != "req-with" {
		t.Errorf("%s lost after With(): %v", AttrRequestID, rec[AttrRequestID])
	}
	if rec["subsystem"] != "ingest" {
		t.Errorf("subsystem attr lost: %v", rec["subsystem"])
	}
}

// TestRequestIDSurvivesWithGroup proves the WithGroup path is
// also re-wrapped so request-id propagation still emits. NOTE:
// slog's group machinery nests record-level attrs (added via
// `r.AddAttrs` from inside Handle) UNDER the active group, so the
// request_id appears at `api.request_id` rather than the top
// level. That's still queryable from a log aggregator -- the test
// asserts the nested path so an accidental swap of the WithGroup
// wrapper for a no-op surfaces as a failure.
func TestRequestIDSurvivesWithGroup(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := New(Config{Writer: &buf}).WithGroup("api")
	ctx := WithRequestID(context.Background(), "req-grouped")
	log.InfoContext(ctx, "api event", "endpoint", "/foo")
	rec := decodeOne(t, &buf)
	api, ok := rec["api"].(map[string]any)
	if !ok {
		t.Fatalf("api group missing: %v", rec)
	}
	if api[AttrRequestID] != "req-grouped" {
		t.Errorf("api.%s lost after WithGroup(): %v", AttrRequestID, api[AttrRequestID])
	}
	if api["endpoint"] != "/foo" {
		t.Errorf("api.endpoint lost: %v", api["endpoint"])
	}
}

func TestFromContext_NilCtx(t *testing.T) {
	t.Parallel()
	// We're deliberately exercising the defensive nil-ctx
	// branch of FromContext, so the lint nudge to use
	// context.TODO() doesn't apply here.
	var ctx context.Context //nolint:staticcheck // SA1012: testing the nil-ctx defensive path
	if got := FromContext(ctx); got != "" {
		t.Errorf("FromContext(nil) = %q; want empty", got)
	}
}

func TestWithRequestID_NilCtxYieldsBackground(t *testing.T) {
	t.Parallel()
	// Same as above: this test asserts the WithRequestID
	// defensive fall-back to context.Background.
	var ctx context.Context //nolint:staticcheck // SA1012: testing the nil-ctx defensive path
	ctx = WithRequestID(ctx, "req-x")
	if got := FromContext(ctx); got != "req-x" {
		t.Errorf("FromContext after WithRequestID(nil, x) = %q; want req-x", got)
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"err":     slog.LevelError,
		"":        slog.LevelInfo, // fallback
		"verbose": slog.LevelInfo, // fallback
		"INFO":    slog.LevelInfo, // case insensitive
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v; want %v", in, got, want)
		}
	}
}

func TestNewRequestID_HexLength(t *testing.T) {
	t.Parallel()
	id := NewRequestID()
	if len(id) != 16 {
		t.Errorf("NewRequestID len = %d; want 16 (8 bytes -> hex)", len(id))
	}
	// re-rolling should not produce the same id
	other := NewRequestID()
	if id == other {
		t.Errorf("two consecutive NewRequestID calls collided: %s", id)
	}
}

// TestLevelFiltering proves the slog level gate routes records
// out at debug when the configured level is info.
func TestLevelFiltering(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := New(Config{Writer: &buf, Level: slog.LevelInfo})
	log.Debug("invisible")
	if buf.Len() != 0 {
		t.Errorf("Debug should be filtered at level=info; got %q", buf.String())
	}
}
