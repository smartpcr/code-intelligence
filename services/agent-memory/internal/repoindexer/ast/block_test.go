//go:build canonical_dispatcher

// Block subdivision threshold tests reference V1 `MethodDecl`
// shape and helpers (`repeatStatementLines`, `itoa`) that the
// V2 dispatcher tests in dispatcher_test.go also import. Gated
// behind `canonical_dispatcher` to keep the test surface
// self-consistent until the Stage 3.2 landing workstream
// re-introduces the helpers under the canonical implementation.
package ast

import (
	"strings"
	"testing"
)

// TestSubdivideMethod_DoesNotFireAtOrBelowThreshold pins the
// acceptance scenario "Method-to-Block split does not fire
// below threshold -- Given a method with 80 normalised
// logical lines, When the AST emitter runs, Then no Block
// nodes are emitted for it." The scenario language says
// "below threshold" but the body description fixes 80 as the
// boundary; per the §8.2 table the split fires when count
// EXCEEDS 80, so exactly 80 -> no blocks.
func TestSubdivideMethod_DoesNotFireAtOrBelowThreshold(t *testing.T) {
	for _, count := range []int{0, 1, 40, 79, 80} {
		t.Run("count="+itoa(count), func(t *testing.T) {
			body := repeatStatementLines(count)
			if got := CountLogicalLines(body); got != count {
				t.Fatalf("test setup: CountLogicalLines = %d; want %d", got, count)
			}
			blocks := SubdivideMethod(MethodDecl{BodySource: body})
			if len(blocks) != 0 {
				t.Fatalf("expected 0 blocks at %d lines; got %d (%v)",
					count, len(blocks), blocks)
			}
		})
	}
}

// TestSubdivideMethod_FiresExactlyAtThresholdPlusOne pins the
// acceptance scenario "Method-to-Block split fires at
// threshold -- Given a method with 81 normalised logical
// lines, When the AST emitter runs, Then 2 Block nodes are
// emitted with `parent_node_id` set to the enclosing Method."
//
// The v1 minimal-contract decomposition is {entry, exit} so
// the count is exactly 2; per-control-structure subdivision
// is a documented future enhancement (see block.go).
func TestSubdivideMethod_FiresExactlyAtThresholdPlusOne(t *testing.T) {
	for _, count := range []int{81, 100, 500} {
		t.Run("count="+itoa(count), func(t *testing.T) {
			body := repeatStatementLines(count)
			if got := CountLogicalLines(body); got != count {
				t.Fatalf("test setup: CountLogicalLines = %d; want %d", got, count)
			}
			blocks := SubdivideMethod(MethodDecl{BodySource: body})
			if len(blocks) != 2 {
				t.Fatalf("expected exactly 2 blocks at %d lines; got %d (%v)",
					count, len(blocks), blocks)
			}
			if blocks[0].Kind != BlockKindEntry {
				t.Errorf("blocks[0].Kind = %q; want %q", blocks[0].Kind, BlockKindEntry)
			}
			if blocks[1].Kind != BlockKindExit {
				t.Errorf("blocks[1].Kind = %q; want %q", blocks[1].Kind, BlockKindExit)
			}
			if blocks[0].Ordinal != 0 || blocks[1].Ordinal != 1 {
				t.Errorf("block ordinals = (%d,%d); want (0,1)",
					blocks[0].Ordinal, blocks[1].Ordinal)
			}
		})
	}
}

// TestSubdivideMethod_StableUnderFormatterOnlyEdits proves a
// formatter-only edit (added blank lines, added `//`
// comments) does NOT shift a method above-or-below the
// threshold. The §9.9 risk mitigation depends on this
// invariant -- otherwise a formatter commit churns every
// Block fingerprint under the affected methods.
func TestSubdivideMethod_StableUnderFormatterOnlyEdits(t *testing.T) {
	body80 := repeatStatementLines(80)
	body80Loose := injectBlankAndCommentLines(body80, 50)

	blocksTight := SubdivideMethod(MethodDecl{BodySource: body80})
	blocksLoose := SubdivideMethod(MethodDecl{BodySource: body80Loose})
	if len(blocksTight) != 0 || len(blocksLoose) != 0 {
		t.Fatalf("expected 0 blocks for body80; got tight=%d loose=%d",
			len(blocksTight), len(blocksLoose))
	}

	body81 := repeatStatementLines(81)
	body81Loose := injectBlankAndCommentLines(body81, 50)
	if len(SubdivideMethod(MethodDecl{BodySource: body81})) != 2 ||
		len(SubdivideMethod(MethodDecl{BodySource: body81Loose})) != 2 {
		t.Fatalf("expected exactly 2 blocks for body81 under both forms")
	}
}

// TestSubdivideMethod_EmitsFileRelativeCoordinates pins
// evaluator finding #6: Block boundaries MUST be expressed
// in file-relative line / byte coordinates so the future
// span-to-block resolver can match observed-stack-frame line
// numbers without re-walking the method's body offsets.
//
// Given a method whose body opens on file line 100, byte
// offset 2_048, and contains 81 logical lines spanning to
// file line 180, the entry block's StartLine MUST be 100
// (not 1) and the exit block's EndLine MUST be 180 (not 81).
func TestSubdivideMethod_EmitsFileRelativeCoordinates(t *testing.T) {
	body := repeatStatementLines(81)
	const baseLine = 100
	const baseByte = 2048
	endByte := baseByte + len(body) - 1
	m := MethodDecl{
		BodySource:    body,
		BodyStartLine: baseLine,
		BodyEndLine:   baseLine + 80, // 81 lines inclusive
		BodyStartByte: baseByte,
		BodyEndByte:   endByte,
	}
	blocks := SubdivideMethod(m)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks; got %d", len(blocks))
	}
	if blocks[0].StartLine != baseLine {
		t.Errorf("entry StartLine = %d; want %d", blocks[0].StartLine, baseLine)
	}
	if blocks[1].EndLine != baseLine+80 {
		t.Errorf("exit EndLine = %d; want %d", blocks[1].EndLine, baseLine+80)
	}
	if blocks[0].StartByte != baseByte {
		t.Errorf("entry StartByte = %d; want %d", blocks[0].StartByte, baseByte)
	}
	if blocks[1].EndByte != endByte {
		t.Errorf("exit EndByte = %d; want %d", blocks[1].EndByte, endByte)
	}
	if blocks[0].EndLine >= blocks[1].StartLine {
		t.Errorf("entry/exit must be disjoint: entry ends %d, exit starts %d",
			blocks[0].EndLine, blocks[1].StartLine)
	}
}

// TestBlockSignature_IsUniquePerOrdinal verifies the block
// canonical-signature scheme prevents two Blocks of the same
// kind under the same method from sharing a fingerprint.
// Without the ordinal embed the future per-control-structure
// subdivision would mint duplicate Nodes.
func TestBlockSignature_IsUniquePerOrdinal(t *testing.T) {
	sig := "https://x.example/y/z::method::a/b.ts#Foo.bar()"
	a := blockSignature(sig, Block{Ordinal: 0, Kind: BlockKindEntry})
	b := blockSignature(sig, Block{Ordinal: 1, Kind: BlockKindEntry})
	if a == b {
		t.Fatalf("blockSignature collision for distinct ordinals: %q", a)
	}
	c := blockSignature(sig, Block{Ordinal: 0, Kind: BlockKindExit})
	if a == c {
		t.Fatalf("blockSignature collision across kinds: %q", a)
	}
}

// --- helpers ---

func repeatStatementLines(n int) string {
	if n <= 0 {
		return ""
	}
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		lines[i] = "x = " + itoa(i) + ";"
	}
	return strings.Join(lines, "\n")
}

func injectBlankAndCommentLines(body string, count int) string {
	lines := strings.Split(body, "\n")
	var out []string
	for i, l := range lines {
		out = append(out, l)
		if i < count {
			out = append(out, "")
			out = append(out, "// formatter-added comment "+itoa(i))
		}
	}
	return strings.Join(out, "\n")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
