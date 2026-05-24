package dsl

import (
	"errors"
	"fmt"
)

// Position records the 1-indexed line + column of a token in
// the source predicate string. Used by [Error] so authoring
// tools can point a caret at the offending byte; the Stage
// 5.4 acceptance criterion "rejection of malformed ones with
// line/column error messages" (implementation-plan line 500)
// pins this shape.
type Position struct {
	Line   int
	Column int
}

// IsZero reports whether p has not been populated.
func (p Position) IsZero() bool { return p.Line == 0 && p.Column == 0 }

// String formats p as "<line>:<column>" for inclusion in
// error messages.
func (p Position) String() string {
	if p.IsZero() {
		return "<unknown>"
	}
	return fmt.Sprintf("%d:%d", p.Line, p.Column)
}

// Error is the canonical error shape returned by [Parse] and
// [Bind]. It always carries a [Position] (zero-valued when
// the underlying failure is positionless, e.g. a Bind miss
// on a token that did not survive the lex stream) and a
// human-readable message.
//
// Callers branch on Kind via [errors.Is] against the package-
// level sentinels [ErrLex], [ErrParse], [ErrSemantic],
// [ErrBind], [ErrType]. The wrapped [Position] is exported
// so a future LSP can render carets without re-parsing.
//
// When the failure has an underlying root cause (e.g. a
// [ThresholdResolver] returned [ErrUnknownThreshold]), the
// cause is exposed via [Cause]; `errors.Is` traverses both
// [Kind] and [Cause] through Go 1.20+ multi-target Unwrap.
type Error struct {
	Kind  error
	Pos   Position
	Msg   string
	Cause error
}

// Error renders the structured error as
// `dsl: <kind>: <pos>: <msg>`. The shape is stable so test
// assertions can substring-match against it.
func (e *Error) Error() string {
	kind := "error"
	if e.Kind != nil {
		kind = e.Kind.Error()
	}
	return fmt.Sprintf("dsl: %s: %s: %s", kind, e.Pos, e.Msg)
}

// Unwrap returns the chain targets [Kind] and (when set)
// [Cause] so `errors.Is(err, dsl.ErrParse)` and
// `errors.Is(err, dsl.ErrUnknownThreshold)` both work on the
// same wrapped error. Requires Go 1.20+ for the
// multi-target Unwrap protocol.
func (e *Error) Unwrap() []error {
	if e.Cause == nil {
		if e.Kind == nil {
			return nil
		}
		return []error{e.Kind}
	}
	if e.Kind == nil {
		return []error{e.Cause}
	}
	return []error{e.Kind, e.Cause}
}

// Sentinel error kinds. Each is wrapped in a returned
// [*Error] -- callers branch with `errors.Is`.
var (
	// ErrLex is the kind for tokenizer failures
	// (unterminated string, illegal character, etc.).
	ErrLex = errors.New("lex error")

	// ErrParse is the kind for syntactic failures
	// (unexpected token, missing closing paren, trailing
	// junk, etc.).
	ErrParse = errors.New("parse error")

	// ErrSemantic is the kind for parse-time canon-guard
	// failures: a closed-set string literal that is not in
	// the canonical set (e.g. `metric_kind == 'lines_of_code'`
	// from the `dsl-rejects-unknown-metric-kind` scenario).
	ErrSemantic = errors.New("semantic error")

	// ErrType is the kind for type-checking failures: a
	// comparison between operands whose types do not unify
	// (e.g. `metric_kind > 'cyclo'`, `value == 'foo'`).
	ErrType = errors.New("type error")

	// ErrBind is the kind for [Bind]-time failures: a
	// `threshold('<uuid>')` atom whose uuid is not in the
	// policy's [PolicyVersion.ThresholdRefs] set, or whose
	// string argument is not a valid UUID.
	ErrBind = errors.New("bind error")
)

// newError constructs a structured [*Error] with the given
// kind, position, and message. Internal constructor; tests
// build expected errors via the sentinel kinds.
func newError(kind error, pos Position, format string, args ...any) *Error {
	return &Error{
		Kind: kind,
		Pos:  pos,
		Msg:  fmt.Sprintf(format, args...),
	}
}

// newWrappedError is the [newError] variant that records an
// underlying [Cause]. Used when re-wrapping a resolver
// failure ([ErrUnknownThreshold]) so callers can branch
// with `errors.Is(err, ErrUnknownThreshold)` AND
// `errors.Is(err, ErrBind)` against the same returned error.
func newWrappedError(kind error, pos Position, cause error, format string, args ...any) *Error {
	return &Error{
		Kind:  kind,
		Pos:   pos,
		Msg:   fmt.Sprintf(format, args...),
		Cause: cause,
	}
}
