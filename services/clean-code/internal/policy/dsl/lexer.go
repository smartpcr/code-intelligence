package dsl

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// tokenKind enumerates the closed set of token kinds the
// lexer emits. Internal -- callers interact with the lexer
// only through [Parse].
type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokString
	tokNumber
	tokAnd
	tokOr
	tokNot
	tokTrue
	tokFalse
	tokThreshold
	tokLParen
	tokRParen
	tokComma
	tokEQ // ==
	tokNE // !=
	tokGT // >
	tokGE // >=
	tokLT // <
	tokLE // <=
)

// String renders a token kind for use in error messages.
// The wording is what a user will see in
// "unexpected <kind>; want <kind>" diagnostics so it is
// chosen to be human-readable.
func (k tokenKind) String() string {
	switch k {
	case tokEOF:
		return "end of input"
	case tokIdent:
		return "identifier"
	case tokString:
		return "string literal"
	case tokNumber:
		return "number literal"
	case tokAnd:
		return "AND"
	case tokOr:
		return "OR"
	case tokNot:
		return "NOT"
	case tokTrue:
		return "true"
	case tokFalse:
		return "false"
	case tokThreshold:
		return "threshold"
	case tokLParen:
		return "'('"
	case tokRParen:
		return "')'"
	case tokComma:
		return "','"
	case tokEQ:
		return "'=='"
	case tokNE:
		return "'!='"
	case tokGT:
		return "'>'"
	case tokGE:
		return "'>='"
	case tokLT:
		return "'<'"
	case tokLE:
		return "'<='"
	default:
		return fmt.Sprintf("token(%d)", int(k))
	}
}

// token is a single lex result.
type token struct {
	kind tokenKind
	// text is the source-substring the token covers. For
	// strings, text is the UNESCAPED inner content (the
	// surrounding quotes are stripped by the lexer).
	text string
	pos  Position
}

// lexer streams [token] values out of a predicate source
// string. The lexer tracks 1-indexed line + column so each
// emitted token (and any [ErrLex] failure) carries a
// [Position] for the parser's structured error reports.
//
// The lexer is internal; callers go through [Parse].
type lexer struct {
	src   string
	pos   int // byte offset into src
	line  int
	col   int
	width int // width (in bytes) of the rune most recently consumed
}

// newLexer constructs a lexer positioned at the start of
// src. Line/col are 1-indexed.
func newLexer(src string) *lexer {
	return &lexer{
		src:  src,
		line: 1,
		col:  1,
	}
}

// peek returns the next rune without consuming it. Returns
// 0 at EOF.
func (l *lexer) peek() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
	return r
}

// next consumes and returns the next rune, advancing
// line/col. Returns 0 at EOF.
func (l *lexer) next() rune {
	if l.pos >= len(l.src) {
		l.width = 0
		return 0
	}
	r, w := utf8.DecodeRuneInString(l.src[l.pos:])
	l.pos += w
	l.width = w
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

// currentPos returns the lexer's current source position
// (the column of the NEXT rune to be consumed). Used to
// stamp tokens before consuming their first rune.
func (l *lexer) currentPos() Position {
	return Position{Line: l.line, Column: l.col}
}

// skipWhitespace advances past any space, tab, CR, LF, or
// '#'-prefixed line comment. The grammar has no statement
// terminator so whitespace is purely separator.
func (l *lexer) skipWhitespace() {
	for l.pos < len(l.src) {
		r := l.peek()
		switch {
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			l.next()
		case r == '#':
			// Line comment to end of line. Supported so
			// `policy/rulepacks/**/*.yaml` can embed
			// commented DSL strings without escape tricks.
			for l.pos < len(l.src) && l.peek() != '\n' {
				l.next()
			}
		default:
			return
		}
	}
}

// nextToken consumes and returns the next [token]. On EOF
// it returns tokEOF with the lexer's current position.
// Errors are returned as a structured [*Error] with
// [ErrLex] as Kind.
func (l *lexer) nextToken() (token, error) {
	l.skipWhitespace()
	pos := l.currentPos()
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, pos: pos}, nil
	}
	r := l.peek()
	switch {
	case r == '(':
		l.next()
		return token{kind: tokLParen, text: "(", pos: pos}, nil
	case r == ')':
		l.next()
		return token{kind: tokRParen, text: ")", pos: pos}, nil
	case r == ',':
		l.next()
		return token{kind: tokComma, text: ",", pos: pos}, nil
	case r == '=':
		l.next()
		if l.peek() != '=' {
			return token{}, newError(ErrLex, pos,
				"expected '==' but got '=%c' (single '=' is not a valid operator; use '==' for equality)", l.peek())
		}
		l.next()
		return token{kind: tokEQ, text: "==", pos: pos}, nil
	case r == '!':
		l.next()
		if l.peek() != '=' {
			return token{}, newError(ErrLex, pos,
				"expected '!=' but got '!%c' (use NOT for boolean negation; '!=' is the inequality operator)", l.peek())
		}
		l.next()
		return token{kind: tokNE, text: "!=", pos: pos}, nil
	case r == '>':
		l.next()
		if l.peek() == '=' {
			l.next()
			return token{kind: tokGE, text: ">=", pos: pos}, nil
		}
		return token{kind: tokGT, text: ">", pos: pos}, nil
	case r == '<':
		l.next()
		if l.peek() == '=' {
			l.next()
			return token{kind: tokLE, text: "<=", pos: pos}, nil
		}
		return token{kind: tokLT, text: "<", pos: pos}, nil
	case r == '\'':
		return l.readString(pos)
	case r == '-':
		// The grammar has no binary subtraction operator
		// (cmp_op covers all infix operators), so a '-'
		// is valid only as the sign of a number literal.
		// We require the digit to follow IMMEDIATELY so
		// the lexer never needs contextual lookback
		// against the previous token to disambiguate.
		// Negative literals are supported because
		// [Threshold] rows can carry negative float64
		// values (e.g. `velocity_trend` reading negative
		// when the trend is downward), and inline
		// comparisons -- `value < -0.5`, `value == -1.5`,
		// `value > -5` -- must be able to express the
		// same range as the Threshold rows the rule
		// could otherwise reference via a threshold()
		// atom.
		if l.pos+1 < len(l.src) {
			next := l.src[l.pos+1]
			if next >= '0' && next <= '9' {
				return l.readNumber(pos)
			}
		}
		l.next()
		return token{}, newError(ErrLex, pos,
			"unexpected character '-' (a leading '-' is only valid as the sign of a number literal and must be immediately followed by a digit)")
	case r >= '0' && r <= '9':
		return l.readNumber(pos)
	case isIdentStart(r):
		return l.readIdent(pos)
	default:
		l.next()
		return token{}, newError(ErrLex, pos, "unexpected character %q", r)
	}
}

// readString consumes a single-quoted string literal. The
// returned token's text is the UNESCAPED inner content; the
// surrounding quotes are dropped. Supported escapes inside
// the string body: `\\` -> `\`, `\'` -> `'`, `\n` -> newline.
// Any other escape is an [ErrLex] failure.
func (l *lexer) readString(start Position) (token, error) {
	l.next() // consume opening '
	var sb strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, newError(ErrLex, start,
				"unterminated string literal (missing closing quote)")
		}
		r := l.next()
		switch r {
		case '\'':
			return token{kind: tokString, text: sb.String(), pos: start}, nil
		case '\\':
			if l.pos >= len(l.src) {
				return token{}, newError(ErrLex, l.currentPos(),
					"unterminated escape sequence at end of input")
			}
			esc := l.next()
			switch esc {
			case '\\':
				sb.WriteByte('\\')
			case '\'':
				sb.WriteByte('\'')
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			default:
				return token{}, newError(ErrLex, l.currentPos(),
					"unsupported escape sequence \\%c (supported: \\\\, \\', \\n, \\t)", esc)
			}
		case '\n':
			return token{}, newError(ErrLex, l.currentPos(),
				"unterminated string literal (newline before closing quote)")
		default:
			sb.WriteRune(r)
		}
	}
}

// readNumber consumes a decimal number literal. Grammar:
// `'-'? [0-9]+ ( '.' [0-9]+ )?`. Scientific notation is
// NOT supported in v1; thresholds live in [Threshold] rows
// so the DSL only sees integer/decimal constants used as
// inline comparators. A leading '-' is consumed when
// present so inline literals stay symmetric with the
// float64 range Threshold rows carry -- a metric like
// `velocity_trend` reads negative when trending downward
// and rule authors must be able to write `value < -0.5`
// inline without being forced through a threshold() atom.
//
// Precondition: nextToken's dispatch only enters this
// path when (a) the leading rune is a digit, OR (b) the
// leading rune is '-' and the immediately following byte
// is a digit. The leading digit-consumption loop is
// therefore guaranteed to advance at least once.
func (l *lexer) readNumber(start Position) (token, error) {
	startOff := l.pos
	if l.peek() == '-' {
		l.next()
	}
	for l.pos < len(l.src) && unicode.IsDigit(l.peek()) {
		l.next()
	}
	if l.pos < len(l.src) && l.peek() == '.' {
		l.next()
		if l.pos >= len(l.src) || !unicode.IsDigit(l.peek()) {
			return token{}, newError(ErrLex, start,
				"expected digit after '.' in number literal")
		}
		for l.pos < len(l.src) && unicode.IsDigit(l.peek()) {
			l.next()
		}
	}
	return token{
		kind: tokNumber,
		text: l.src[startOff:l.pos],
		pos:  start,
	}, nil
}

// readIdent consumes an identifier and folds reserved
// keywords into their dedicated token kinds. Identifiers
// match `[A-Za-z_][A-Za-z0-9_]*`.
func (l *lexer) readIdent(start Position) (token, error) {
	startOff := l.pos
	for l.pos < len(l.src) && isIdentContinue(l.peek()) {
		l.next()
	}
	text := l.src[startOff:l.pos]
	// Keywords are case-INSENSITIVE for AND/OR/NOT only --
	// matching SQL convention. Field names and other
	// identifiers are case-SENSITIVE because the closed-set
	// canon-guards do exact string equality against the DB
	// ENUM labels (which are lowercase).
	switch strings.ToUpper(text) {
	case "AND":
		return token{kind: tokAnd, text: text, pos: start}, nil
	case "OR":
		return token{kind: tokOr, text: text, pos: start}, nil
	case "NOT":
		return token{kind: tokNot, text: text, pos: start}, nil
	}
	switch text {
	case "true":
		return token{kind: tokTrue, text: text, pos: start}, nil
	case "false":
		return token{kind: tokFalse, text: text, pos: start}, nil
	case "threshold":
		return token{kind: tokThreshold, text: text, pos: start}, nil
	}
	return token{kind: tokIdent, text: text, pos: start}, nil
}

func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdentContinue(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}
