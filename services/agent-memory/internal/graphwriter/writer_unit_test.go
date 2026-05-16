package graphwriter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestNormaliseAttrs_emptyDefaultsToObject pins the behaviour
// the column contract relies on: a nil / empty input becomes
// the literal `{}` so the SQL `attrs_json jsonb NOT NULL
// DEFAULT '{}'` invariant is upheld even when callers omit the
// field entirely.
func TestNormaliseAttrs_emptyDefaultsToObject(t *testing.T) {
	t.Parallel()
	for _, in := range []json.RawMessage{nil, {}} {
		got, err := normaliseAttrs(in)
		if err != nil {
			t.Fatalf("normaliseAttrs(%q): %v", string(in), err)
		}
		if string(got) != "{}" {
			t.Errorf("normaliseAttrs(%q) = %q, want %q", string(in), string(got), "{}")
		}
	}
}

// TestNormaliseAttrs_validObjectPassesThrough confirms the
// fast path: a syntactically valid JSON object survives
// normalisation byte-for-byte (no re-encoding) so callers can
// pre-marshal once and reuse.
func TestNormaliseAttrs_validObjectPassesThrough(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{"visibility":"public","is_async":true}`)
	got, err := normaliseAttrs(in)
	if err != nil {
		t.Fatalf("normaliseAttrs: %v", err)
	}
	if string(got) != string(in) {
		t.Errorf("normaliseAttrs round-trip changed bytes: got %q, want %q",
			string(got), string(in))
	}
}

// TestNormaliseAttrs_rejectsNonObjects exercises the "JSON object,
// not array / scalar / null" rule the architecture pins on
// attrs_json. Each rejected case must surface as an error rather
// than slipping through to the SQL layer.
func TestNormaliseAttrs_rejectsNonObjects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"array", `["a","b"]`},
		{"string", `"hello"`},
		{"number", `42`},
		{"bool", `true`},
		{"null", `null`},
		{"invalid json", `{not json}`},
		{"whitespace only", `   `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normaliseAttrs(json.RawMessage(tc.in)); err == nil {
				t.Errorf("normaliseAttrs(%q) returned nil error; want rejection", tc.in)
			}
		})
	}
}

// TestWriteContractViolation_unwrapAndIsAs makes sure the typed
// error participates in the standard errors.As / errors.Is
// machinery. Downstream consumers (agent-api, mgmt-api) will
// pattern-match on this type, not on string contents.
func TestWriteContractViolation_unwrapAndIsAs(t *testing.T) {
	t.Parallel()
	inner := errors.New("permission denied for table node")
	wcv := &WriteContractViolation{
		Op:       "InsertNode",
		SQLState: pgErrCodeInsufficientPrivilege,
		Err:      inner,
	}
	if got := wcv.Unwrap(); got != inner {
		t.Errorf("Unwrap = %v, want %v", got, inner)
	}
	if !errors.Is(wcv, inner) {
		t.Error("errors.Is(wcv, inner) = false, want true")
	}
	var as *WriteContractViolation
	if !errors.As(wcv, &as) {
		t.Error("errors.As(wcv, *WriteContractViolation) = false, want true")
	}
	if !strings.Contains(wcv.Error(), "InsertNode") || !strings.Contains(wcv.Error(), "42501") {
		t.Errorf("Error() = %q; want it to mention Op and SQLState", wcv.Error())
	}
}

// auditTestWriter constructs a Writer with a JSON-handler slog
// logger pointed at the supplied buffer. The DB field is left
// nil because the audit-middleware unit tests never call
// runInTx; they invoke emitAudit / auditDefer directly so they
// can run without PostgreSQL.
func auditTestWriter(buf *bytes.Buffer) *Writer {
	return &Writer{
		logger: slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// decodeOneLogLine asserts that buf contains exactly one JSON
// record and returns it parsed.
func decodeOneLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d:\n%s", len(lines), buf.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(lines[0], &entry); err != nil {
		t.Fatalf("unmarshal log line: %v\nline: %s", err, lines[0])
	}
	return entry
}

// TestEmitAudit_successEmitsInfoWithUniformShape pins the
// success path: one Info record at msg=graphwriter.<op> carrying
// op/repo_id/kind/fingerprint_hex/sha plus any Extras. The
// `contract_violation` and `error` keys MUST be absent.
func TestEmitAudit_successEmitsInfoWithUniformShape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := auditTestWriter(&buf)
	fields := auditFields{
		RepoID:         "00000000-0000-0000-0000-000000000001",
		Kind:           "method",
		FingerprintHex: "0a0a0a0a",
		SHA:            "abc123",
		Extras: []slog.Attr{
			slog.String("node_id", "11111111-1111-1111-1111-111111111111"),
			slog.Bool("inserted", true),
		},
	}
	w.emitAudit("insert_node", fields, nil)

	entry := decodeOneLogLine(t, &buf)
	if entry["msg"] != "graphwriter.insert_node" {
		t.Errorf("msg = %v, want graphwriter.insert_node", entry["msg"])
	}
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", entry["level"])
	}
	if entry["op"] != "insert_node" {
		t.Errorf("op = %v, want insert_node", entry["op"])
	}
	for _, want := range []struct{ k, v string }{
		{"repo_id", fields.RepoID},
		{"kind", fields.Kind},
		{"fingerprint_hex", fields.FingerprintHex},
		{"sha", fields.SHA},
		{"node_id", "11111111-1111-1111-1111-111111111111"},
	} {
		if entry[want.k] != want.v {
			t.Errorf("%s = %v, want %s", want.k, entry[want.k], want.v)
		}
	}
	if _, present := entry["contract_violation"]; present {
		t.Errorf("success record unexpectedly carried contract_violation key")
	}
	if _, present := entry["error"]; present {
		t.Errorf("success record unexpectedly carried error key")
	}
}

// TestEmitAudit_failureEmitsErrorWithContractViolationFlag
// pins the failure path: a *WriteContractViolation must surface
// as `contract_violation: true`, level=ERROR,
// msg=graphwriter.<op>.failed, and carry both `error` and
// `error_type`.
func TestEmitAudit_failureEmitsErrorWithContractViolationFlag(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := auditTestWriter(&buf)
	wcv := &WriteContractViolation{
		Op:       "InsertNode",
		SQLState: pgErrCodeInsufficientPrivilege,
		Err:      errors.New("pq: permission denied for table node"),
	}
	w.emitAudit("insert_node", auditFields{
		RepoID: "00000000-0000-0000-0000-000000000001",
		Kind:   "method", FingerprintHex: "0a0a", SHA: "abc",
	}, wcv)

	entry := decodeOneLogLine(t, &buf)
	if entry["msg"] != "graphwriter.insert_node.failed" {
		t.Errorf("msg = %v, want graphwriter.insert_node.failed", entry["msg"])
	}
	if entry["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", entry["level"])
	}
	if cv, _ := entry["contract_violation"].(bool); !cv {
		t.Errorf("contract_violation = %v, want true", entry["contract_violation"])
	}
	if et, _ := entry["error_type"].(string); !strings.Contains(et, "WriteContractViolation") {
		t.Errorf("error_type = %v, want substring WriteContractViolation", entry["error_type"])
	}
}

// TestEmitAudit_failureWithPlainErrorClearsContractViolationFlag
// asserts the inverse: a non-WriteContractViolation failure
// records `contract_violation: false`. Operators rely on this
// flag to distinguish G5-violations from transient errors.
func TestEmitAudit_failureWithPlainErrorClearsContractViolationFlag(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := auditTestWriter(&buf)
	w.emitAudit("insert_node", auditFields{
		RepoID: "00000000-0000-0000-0000-000000000001",
	}, errors.New("connection reset by peer"))

	entry := decodeOneLogLine(t, &buf)
	if entry["msg"] != "graphwriter.insert_node.failed" {
		t.Errorf("msg = %v, want graphwriter.insert_node.failed", entry["msg"])
	}
	if cv, _ := entry["contract_violation"].(bool); cv {
		t.Errorf("contract_violation = %v, want false for non-WCV error", entry["contract_violation"])
	}
}

// TestAuditDefer_panicLogsFailureAndRepanics covers the panic
// path: if the wrapped function panics, auditDefer must (a)
// emit one error-level audit record carrying a `panic` extra,
// and (b) re-raise the panic so the goroutine still crashes.
// The rubber-duck flagged this as a blocker — without it,
// panics would be silently logged as success.
func TestAuditDefer_panicLogsFailureAndRepanics(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := auditTestWriter(&buf)

	// Wrap the panic-and-defer in a deferred recover at this
	// test layer so we can assert on the re-raise without the
	// test runner reporting a real crash.
	caught := func() (panicked any) {
		defer func() { panicked = recover() }()
		func() {
			fields := auditFields{RepoID: "r"}
			var err error
			defer w.auditDefer("buggy_op", &fields, &err)()
			panic("boom")
		}()
		return nil
	}()
	if caught == nil {
		t.Fatal("expected the panic to propagate past auditDefer; got nil")
	}
	if got, _ := caught.(string); got != "boom" {
		t.Errorf("re-raised panic = %v, want \"boom\"", caught)
	}

	entry := decodeOneLogLine(t, &buf)
	if entry["msg"] != "graphwriter.buggy_op.failed" {
		t.Errorf("msg = %v, want graphwriter.buggy_op.failed", entry["msg"])
	}
	if entry["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", entry["level"])
	}
	if entry["panic"] != "boom" {
		t.Errorf("panic = %v, want \"boom\"", entry["panic"])
	}
	if cv, _ := entry["contract_violation"].(bool); cv {
		t.Errorf("contract_violation should be false for a panic: got %v", entry["contract_violation"])
	}
}

// TestAuditDefer_returnsErrorLogsFailure verifies the typical
// failure path: the wrapped function assigns a non-nil err via
// a named return, and auditDefer flushes the failure record.
func TestAuditDefer_returnsErrorLogsFailure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := auditTestWriter(&buf)
	calleErr := fmt.Errorf("upstream failed")

	(func() (err error) {
		fields := auditFields{RepoID: "r", Kind: "method", FingerprintHex: "0a", SHA: "s"}
		defer w.auditDefer("test_op", &fields, &err)()
		err = calleErr
		return
	})()

	entry := decodeOneLogLine(t, &buf)
	if entry["msg"] != "graphwriter.test_op.failed" {
		t.Errorf("msg = %v, want graphwriter.test_op.failed", entry["msg"])
	}
	if entry["error"] != "upstream failed" {
		t.Errorf("error = %v, want upstream failed", entry["error"])
	}
	for _, k := range []string{"repo_id", "kind", "fingerprint_hex", "sha"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("failure record missing audit key %q", k)
		}
	}
}
