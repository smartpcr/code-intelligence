package dsl

import (
	"errors"
	"strings"
	"testing"
)

// helper: run the lexer to exhaustion and return all tokens
// (excluding tokEOF, which is returned at the end), or the
// first error encountered. Tests use this to inspect the
// sequence the lexer emits.
func lexAll(t *testing.T, src string) ([]token, error) {
	t.Helper()
	lx := newLexer(src)
	var out []token
	for {
		tok, err := lx.nextToken()
		if err != nil {
			return out, err
		}
		if tok.kind == tokEOF {
			return out, nil
		}
		out = append(out, tok)
	}
}

func TestLexer_BasicTokens(t *testing.T) {
	t.Parallel()
	src := "metric_kind == 'lcom4' AND value > 10"
	toks, err := lexAll(t, src)
	if err != nil {
		t.Fatalf("lexAll: %v", err)
	}
	want := []tokenKind{tokIdent, tokEQ, tokString, tokAnd, tokIdent, tokGT, tokNumber}
	if len(toks) != len(want) {
		t.Fatalf("token count: got %d, want %d (%v)", len(toks), len(want), toks)
	}
	for i, k := range want {
		if toks[i].kind != k {
			t.Errorf("toks[%d].kind = %v, want %v", i, toks[i].kind, k)
		}
	}
	if toks[2].text != "lcom4" {
		t.Errorf("string token text = %q, want %q", toks[2].text, "lcom4")
	}
}

func TestLexer_TracksLineColumn(t *testing.T) {
	t.Parallel()
	// Position of the THIRD token (tokString 'lcom4') should
	// be line 2 col 16 -- after "metric_kind == " on the
	// second line.
	src := "value > 10\nmetric_kind == 'lcom4'"
	toks, err := lexAll(t, src)
	if err != nil {
		t.Fatalf("lexAll: %v", err)
	}
	// First token on line 1.
	if toks[0].pos.Line != 1 || toks[0].pos.Column != 1 {
		t.Errorf("toks[0].pos = %s, want 1:1", toks[0].pos)
	}
	// "metric_kind" identifier on line 2 col 1.
	if toks[3].pos.Line != 2 || toks[3].pos.Column != 1 {
		t.Errorf("toks[3].pos = %s, want 2:1", toks[3].pos)
	}
	// "'lcom4'" string on line 2 col 16.
	if toks[5].pos.Line != 2 || toks[5].pos.Column != 16 {
		t.Errorf("toks[5].pos = %s, want 2:16", toks[5].pos)
	}
}

func TestLexer_AllOperators(t *testing.T) {
	t.Parallel()
	cases := []struct {
		src  string
		want tokenKind
	}{
		{"==", tokEQ},
		{"!=", tokNE},
		{">", tokGT},
		{">=", tokGE},
		{"<", tokLT},
		{"<=", tokLE},
	}
	for _, c := range cases {
		c := c
		t.Run(c.src, func(t *testing.T) {
			t.Parallel()
			toks, err := lexAll(t, c.src)
			if err != nil {
				t.Fatalf("%q: %v", c.src, err)
			}
			if len(toks) != 1 || toks[0].kind != c.want {
				t.Fatalf("%q: got %v, want [%v]", c.src, toks, c.want)
			}
		})
	}
}

func TestLexer_KeywordsCaseInsensitive(t *testing.T) {
	t.Parallel()
	// AND / OR / NOT may be written in any case, matching
	// SQL convention. Field names stay case-sensitive.
	src := "and or not AND Or NoT"
	toks, err := lexAll(t, src)
	if err != nil {
		t.Fatalf("lexAll: %v", err)
	}
	want := []tokenKind{tokAnd, tokOr, tokNot, tokAnd, tokOr, tokNot}
	if len(toks) != len(want) {
		t.Fatalf("count mismatch: got %d, want %d", len(toks), len(want))
	}
	for i, k := range want {
		if toks[i].kind != k {
			t.Errorf("toks[%d].kind = %v, want %v", i, toks[i].kind, k)
		}
	}
}

func TestLexer_RejectsBareEquals(t *testing.T) {
	t.Parallel()
	// `=` alone is not an operator -- it could be confused
	// with assignment in copy-pasted code. The lexer points
	// at the bad position.
	_, err := lexAll(t, "value = 10")
	if err == nil {
		t.Fatalf("expected lex error for bare '=' but got nil")
	}
	var dsErr *Error
	if !errors.As(err, &dsErr) {
		t.Fatalf("err = %T %v, want *dsl.Error", err, err)
	}
	if !errors.Is(err, ErrLex) {
		t.Errorf("err Kind = %v, want ErrLex", dsErr.Kind)
	}
	if dsErr.Pos.Line != 1 || dsErr.Pos.Column != 7 {
		t.Errorf("err Pos = %s, want 1:7", dsErr.Pos)
	}
}

func TestLexer_UnterminatedString(t *testing.T) {
	t.Parallel()
	// Unterminated string at EOF.
	_, err := lexAll(t, "metric_kind == 'lcom4")
	if err == nil {
		t.Fatalf("expected lex error for unterminated string but got nil")
	}
	if !errors.Is(err, ErrLex) {
		t.Errorf("err = %v, want ErrLex", err)
	}
	if !strings.Contains(err.Error(), "unterminated string") {
		t.Errorf("err = %v, want message about unterminated string", err)
	}

	// Unterminated by embedded newline.
	_, err2 := lexAll(t, "metric_kind == 'lcom4\nAND value > 10")
	if !errors.Is(err2, ErrLex) {
		t.Errorf("err = %v, want ErrLex", err2)
	}
}

func TestLexer_StringEscapes(t *testing.T) {
	t.Parallel()
	toks, err := lexAll(t, `'a\'b' 'c\\d' 'e\nf'`)
	if err != nil {
		t.Fatalf("lexAll: %v", err)
	}
	want := []string{"a'b", `c\d`, "e\nf"}
	for i, w := range want {
		if toks[i].text != w {
			t.Errorf("toks[%d].text = %q, want %q", i, toks[i].text, w)
		}
	}
}

func TestLexer_LineComment(t *testing.T) {
	t.Parallel()
	src := "# leading comment\nmetric_kind == 'lcom4' # trailing\nAND value > 10"
	toks, err := lexAll(t, src)
	if err != nil {
		t.Fatalf("lexAll: %v", err)
	}
	// Expected non-comment tokens.
	want := []tokenKind{tokIdent, tokEQ, tokString, tokAnd, tokIdent, tokGT, tokNumber}
	if len(toks) != len(want) {
		t.Fatalf("count = %d, want %d", len(toks), len(want))
	}
}

func TestLexer_NumberLiterals(t *testing.T) {
	t.Parallel()
	toks, err := lexAll(t, "10 3.14 0.5 12345")
	if err != nil {
		t.Fatalf("lexAll: %v", err)
	}
	want := []string{"10", "3.14", "0.5", "12345"}
	for i, w := range want {
		if toks[i].kind != tokNumber || toks[i].text != w {
			t.Errorf("toks[%d] = %v(%q), want tokNumber(%q)", i, toks[i].kind, toks[i].text, w)
		}
	}
}

func TestLexer_NumberWithDanglingDot(t *testing.T) {
	t.Parallel()
	_, err := lexAll(t, "3. AND value > 10")
	if err == nil {
		t.Fatalf("expected lex error for '3.' but got nil")
	}
	if !errors.Is(err, ErrLex) {
		t.Errorf("err = %v, want ErrLex", err)
	}
}
