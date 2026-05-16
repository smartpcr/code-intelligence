package retirement

// Pure-Go unit tests for the typed-error surface and the
// argument-validation paths. Behavioural coverage of the Stage
// 2.3 acceptance scenarios ("double retirement rejected" and
// "rename retirement links new node") lives in two layers:
//
//   - service_sqlmock_test.go -- unit-level, no env gate, drives
//     the service through go-sqlmock to assert error
//     classification and INSERT-bind shape.
//   - service_integration_test.go -- live PostgreSQL, asserts
//     end-to-end behaviour against the real UNIQUE / FK / role
//     constraints defined in the migrations.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lib/pq"
)

// TestAlreadyRetired_unwrapAndAs proves the typed error
// participates in the standard errors.As / errors.Is machinery.
// Downstream callers (Repo Indexer, mgmt-api) pattern-match on
// this type, not on string contents.
func TestAlreadyRetired_unwrapAndAs(t *testing.T) {
	t.Parallel()
	inner := errors.New("pq: duplicate key value violates unique constraint")
	e := &AlreadyRetired{
		Kind:     KindNode,
		TargetID: "11111111-1111-1111-1111-111111111111",
		SQLState: pgErrCodeUniqueViolation,
		Err:      inner,
	}
	if got := e.Unwrap(); got != inner {
		t.Errorf("Unwrap = %v, want %v", got, inner)
	}
	if !errors.Is(e, inner) {
		t.Error("errors.Is(e, inner) = false, want true")
	}
	var asTyped *AlreadyRetired
	if !errors.As(e, &asTyped) {
		t.Error("errors.As(*AlreadyRetired) = false, want true")
	}
	msg := e.Error()
	for _, want := range []string{
		"node", "11111111-1111-1111-1111-111111111111",
		pgErrCodeUniqueViolation, "already retired",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q; missing %q", msg, want)
		}
	}
}

// TestAlreadyRetired_emptyTargetIDMessage covers the
// batch-path message shape: PostgreSQL only reports one
// violating row per failed INSERT, so RetireMany surfaces an
// AlreadyRetired with TargetID = "". The message must remain
// readable in that shape.
func TestAlreadyRetired_emptyTargetIDMessage(t *testing.T) {
	t.Parallel()
	e := &AlreadyRetired{
		Kind:     KindNode,
		SQLState: pgErrCodeUniqueViolation,
		Err:      errors.New("pq: duplicate key ..."),
	}
	msg := e.Error()
	if !strings.Contains(msg, "node already retired") {
		t.Errorf("Error() = %q; want \"node already retired\"", msg)
	}
	if strings.Contains(msg, "  ") {
		// A naïve format string would leave a double space when
		// TargetID is empty; this asserts the kind/empty branch
		// formats cleanly.
		t.Errorf("Error() = %q; unexpected double-space artefact", msg)
	}
}

// TestNotFound_unwrapAndAs covers the pre-check error shape
// (where Err is nil) and the FK-violation shape (where Err
// wraps a *pq.Error). Both must round-trip cleanly through
// errors.As without panicking on a nil Unwrap.
func TestNotFound_unwrapAndAs(t *testing.T) {
	t.Parallel()
	preCheck := &NotFound{Kind: KindNode, TargetID: "abc"}
	if got := preCheck.Unwrap(); got != nil {
		t.Errorf("pre-check Unwrap = %v, want nil", got)
	}
	var asTyped *NotFound
	if !errors.As(preCheck, &asTyped) {
		t.Error("errors.As(pre-check, *NotFound) = false")
	}
	if msg := preCheck.Error(); !strings.Contains(msg, "abc") {
		t.Errorf("pre-check Error = %q; missing target id", msg)
	}

	inner := errors.New("pq: insert or update on table violates fkey")
	fkErr := &NotFound{
		Kind: KindEdge, SQLState: pgErrCodeForeignKeyViolation, Err: inner,
	}
	if got := fkErr.Unwrap(); got != inner {
		t.Errorf("fk Unwrap = %v, want inner", got)
	}
	if !errors.Is(fkErr, inner) {
		t.Error("errors.Is(fkErr, inner) = false, want true")
	}
	if msg := fkErr.Error(); !strings.Contains(msg, "edge not found") {
		t.Errorf("fk Error = %q; want \"edge not found\"", msg)
	}
}

// TestWriteContractViolation_unwrapAndAs mirrors the graphwriter
// package's equivalent test so the two typed errors stay in
// lockstep for consumers that depend on errors.As.
func TestWriteContractViolation_unwrapAndAs(t *testing.T) {
	t.Parallel()
	inner := errors.New("pq: permission denied for table node_retirement")
	e := &WriteContractViolation{
		Op: "RetireNode", SQLState: pgErrCodeInsufficientPrivilege, Err: inner,
	}
	if got := e.Unwrap(); got != inner {
		t.Errorf("Unwrap = %v, want inner", got)
	}
	if !errors.Is(e, inner) {
		t.Error("errors.Is(e, inner) = false")
	}
	var asTyped *WriteContractViolation
	if !errors.As(e, &asTyped) {
		t.Error("errors.As(e, *WriteContractViolation) = false")
	}
	msg := e.Error()
	if !strings.Contains(msg, "RetireNode") ||
		!strings.Contains(msg, pgErrCodeInsufficientPrivilege) {
		t.Errorf("Error = %q; should name Op and SQLState", msg)
	}
}

// TestClassifyErr_mapsPostgresCodes proves the SQLSTATE → typed
// error mapping is wired correctly for the three classes the
// service depends on (23505 / 23503 / 42501) and that any other
// SQLSTATE or non-pq error passes through unchanged.
func TestClassifyErr_mapsPostgresCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		code    pq.ErrorCode
		wantTyp string
	}{
		{"unique", pgErrCodeUniqueViolation, "*retirement.AlreadyRetired"},
		{"fk", pgErrCodeForeignKeyViolation, "*retirement.NotFound"},
		{"privilege", pgErrCodeInsufficientPrivilege,
			"*retirement.WriteContractViolation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &pq.Error{Code: tc.code, Message: "synthetic"}
			out := classifyErr("RetireNode", KindNode, "abc", src)
			switch tc.wantTyp {
			case "*retirement.AlreadyRetired":
				var typed *AlreadyRetired
				if !errors.As(out, &typed) {
					t.Fatalf("got %T, want %s", out, tc.wantTyp)
				}
				if typed.TargetID != "abc" {
					t.Errorf("TargetID = %q, want abc", typed.TargetID)
				}
			case "*retirement.NotFound":
				var typed *NotFound
				if !errors.As(out, &typed) {
					t.Fatalf("got %T, want %s", out, tc.wantTyp)
				}
				if typed.SQLState != pgErrCodeForeignKeyViolation {
					t.Errorf("SQLState = %q, want %q",
						typed.SQLState, pgErrCodeForeignKeyViolation)
				}
			case "*retirement.WriteContractViolation":
				var typed *WriteContractViolation
				if !errors.As(out, &typed) {
					t.Fatalf("got %T, want %s", out, tc.wantTyp)
				}
				if typed.Op != "RetireNode" {
					t.Errorf("Op = %q, want RetireNode", typed.Op)
				}
			}
		})
	}
}

// TestClassifyErr_passThroughForNonPqError pins the "do nothing"
// branch: a plain Go error must not be wrapped in any typed
// shape -- otherwise context-cancelled / network errors would
// be misreported as constraint violations.
func TestClassifyErr_passThroughForNonPqError(t *testing.T) {
	t.Parallel()
	in := errors.New("connection reset by peer")
	out := classifyErr("RetireNode", KindNode, "abc", in)
	if out != in {
		t.Errorf("classifyErr changed a non-pq error: %v -> %v", in, out)
	}
}

// TestClassifyErr_passThroughForUnknownPgCode pins the
// unrecognised-code branch: a SQLSTATE we don't handle must
// stay raw so the caller's logging captures the real code
// instead of a misleading typed value.
func TestClassifyErr_passThroughForUnknownPgCode(t *testing.T) {
	t.Parallel()
	in := &pq.Error{Code: "08006", Message: "connection failure"}
	out := classifyErr("RetireNode", KindNode, "abc", in)
	if out != error(in) {
		t.Errorf("classifyErr wrapped an unhandled SQLSTATE: %T -> %T", in, out)
	}
}

// TestClassifyErr_idempotentOnTypedError covers the contract
// runInTx relies on: a body that has already classified its
// error returns a typed value, and the outer classifyErr call
// at the runInTx boundary must NOT double-wrap.
func TestClassifyErr_idempotentOnTypedError(t *testing.T) {
	t.Parallel()
	already := &AlreadyRetired{
		Kind: KindNode, TargetID: "x", SQLState: pgErrCodeUniqueViolation,
		Err: &pq.Error{Code: pgErrCodeUniqueViolation},
	}
	out := classifyErr("RetireNode", "", "", already)
	var asTyped *AlreadyRetired
	if !errors.As(out, &asTyped) {
		t.Fatalf("typed error lost through classifyErr: got %T", out)
	}
	if asTyped != already {
		t.Errorf("classifyErr returned a different *AlreadyRetired value; want pass-through")
	}
}

// TestRetireNode_validatesEmptyArgs covers the cheap input
// guards that need no database round-trip. The brief calls out
// `(node_id, retired_at_sha)` as the required argument set, so
// an empty value in either slot is a caller bug that must be
// surfaced before any SQL runs.
func TestRetireNode_validatesEmptyArgs(t *testing.T) {
	t.Parallel()
	// We rely on the empty-arg checks running BEFORE BeginTx,
	// so the *sql.DB never opens a connection. A nil-DB Service
	// would panic on db.BeginTx; that's the assertion: the
	// early-return must short-circuit it.
	s := &Service{logger: slog.Default()}
	ctx := context.Background()
	if _, err := s.RetireNode(ctx, NodeRetirementInput{}); err == nil {
		t.Error("RetireNode({}): expected error, got nil")
	}
	if _, err := s.RetireNode(ctx, NodeRetirementInput{
		RetiredAtSHA: "abc",
	}); err == nil {
		t.Error("RetireNode(no NodeID): expected error, got nil")
	}
	if _, err := s.RetireNode(ctx, NodeRetirementInput{
		NodeID: "abc",
	}); err == nil {
		t.Error("RetireNode(no RetiredAtSHA): expected error, got nil")
	}
}

// TestRetireEdge_validatesEmptyArgs is the edge-side mirror.
func TestRetireEdge_validatesEmptyArgs(t *testing.T) {
	t.Parallel()
	s := &Service{logger: slog.Default()}
	ctx := context.Background()
	if _, err := s.RetireEdge(ctx, EdgeRetirementInput{}); err == nil {
		t.Error("RetireEdge({}): expected error, got nil")
	}
	if _, err := s.RetireEdge(ctx, EdgeRetirementInput{
		RetiredAtSHA: "abc",
	}); err == nil {
		t.Error("RetireEdge(no EdgeID): expected error, got nil")
	}
	if _, err := s.RetireEdge(ctx, EdgeRetirementInput{
		EdgeID: "abc",
	}); err == nil {
		t.Error("RetireEdge(no RetiredAtSHA): expected error, got nil")
	}
}

// TestRetireMany_validatesArgs covers the no-op, missing-sha
// and empty-id branches. The zero-length input is a successful
// no-op by design (callers shouldn't have to guard the call
// site); a missing SHA or an embedded empty id is a caller bug.
func TestRetireMany_validatesArgs(t *testing.T) {
	t.Parallel()
	s := &Service{logger: slog.Default()}
	ctx := context.Background()

	// Zero-length input must NOT touch the database. The Service
	// has a nil *sql.DB here, so any SQL attempt would crash.
	res, err := s.RetireMany(ctx, nil, "abc")
	if err != nil {
		t.Errorf("RetireMany(nil, sha): err = %v, want nil", err)
	}
	if res.InsertedCount != 0 || len(res.Records) != 0 {
		t.Errorf("RetireMany(nil, sha): got %+v, want zero-value", res)
	}

	// Missing SHA is a hard error -- the column is NOT NULL.
	if _, err := s.RetireMany(ctx, []string{"id"}, ""); err == nil {
		t.Error("RetireMany([id], \"\"): expected error, got nil")
	}

	// Embedded empty id is rejected before any SQL runs.
	_, err = s.RetireMany(ctx, []string{"good-id", "", "other"}, "abc")
	if err == nil {
		t.Error("RetireMany([_, \"\", _]): expected error, got nil")
	}
	if err != nil && !strings.Contains(err.Error(), "[1]") {
		t.Errorf("error should name offending index: %v", err)
	}
}

// TestNew_panicsOnNilDB pins the contract documented on New():
// a nil *sql.DB is a programming error and must crash loudly
// instead of producing a Service that silently no-ops or panics
// at the first call.
func TestNew_panicsOnNilDB(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("New(nil, ...) did not panic")
		}
	}()
	_ = New(nil, slog.Default())
}

// TestNew_defaultsLoggerWhenNil pins the documented default:
// passing a nil *slog.Logger should not nil-deref later when
// emitSuccess / emitFailure run.
func TestNew_defaultsLoggerWhenNil(t *testing.T) {
	t.Parallel()
	// Use a stub *sql.DB to satisfy the nil-guard.
	s := New(&sql.DB{}, nil)
	if s.logger == nil {
		t.Error("logger left nil; New must fall back to slog.Default()")
	}
}

// TestEmitSuccess_writesOneInfoRecord pins the structured-log
// shape every successful call produces. Operators grep the
// `retirement.<op>` records to triage; the test guards against
// silent shape drift.
func TestEmitSuccess_writesOneInfoRecord(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := &Service{
		logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}
	s.emitSuccess("RetireNode", "node-x", "sha-y")
	if c := strings.Count(buf.String(), "\n"); c != 1 {
		t.Errorf("expected 1 log line, got %d:\n%s", c, buf.String())
	}
	for _, want := range []string{
		`"msg":"retirement.RetireNode"`,
		`"op":"RetireNode"`,
		`"target":"node-x"`,
		`"retired_at_sha":"sha-y"`,
		`"level":"INFO"`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %q; got:\n%s", want, buf.String())
		}
	}
}

// TestEmitFailure_classifiesTypedError pins that a typed error
// surfaces both `error_type` and the matching boolean flag the
// emitFailure helper exposes (so operators can build alert
// rules on `already_retired:true` etc).
func TestEmitFailure_classifiesTypedError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := &Service{
		logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}
	s.emitFailure("RetireNode", "node-x", &AlreadyRetired{
		Kind: KindNode, TargetID: "node-x",
		SQLState: pgErrCodeUniqueViolation,
		Err:      errors.New("pq: duplicate"),
	})
	for _, want := range []string{
		`"msg":"retirement.RetireNode.failed"`,
		`"level":"ERROR"`,
		`"already_retired":true`,
		`"not_found":false`,
		`"contract_violation":false`,
		`"error_type":"*retirement.AlreadyRetired"`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %q; got:\n%s", want, buf.String())
		}
	}
}
