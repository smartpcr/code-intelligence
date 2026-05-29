package ast

import (
	"fmt"
	"strings"
)

// DefaultBlockThreshold is the §8.2 locked-value threshold a
// Method's normalised-logical-line count must EXCEED before the
// dispatcher subdivides it into Block nodes. The threshold is
// strict: a Method whose count equals the threshold is NOT
// subdivided (acceptance scenario "Method-to-Block split does
// not fire below threshold -- Given a method with 80 normalised
// logical lines ...").
//
// The value is sourced from `tech-spec.md` §8.2 ("Method-to-
// Block split threshold ... 80 logical lines"); changing it
// requires a story under the "Override route" column of that
// table.
const DefaultBlockThreshold = 80

// BlockKind is the closed set of `block_kind` discriminator
// values per `architecture.md` §3.7 / `tech-spec.md` §8.2.
// The set is database-enforced via the `block_kind` ENUM in
// migration 0001; emitting any value outside this set causes
// the INSERT to fail at the SQL layer.
type BlockKind string

// Closed-set `block_kind` values. Stage 3.2 v1 emits only
// `BlockKindEntry` and `BlockKindExit` -- the minimal
// decomposition that satisfies the §8.2 "subdivide" contract
// and matches the acceptance scenario "81 normalised logical
// lines -> 2 Block nodes". Per-control-structure subdivision
// (one Block per branch / loop / try) is a documented future
// enhancement; emitting the additional kinds is a follow-up
// story so the v1 Block count stays exact for the acceptance
// gate.
const (
	BlockKindEntry     BlockKind = "entry"
	BlockKindBranch    BlockKind = "branch"
	BlockKindLoopBody  BlockKind = "loop_body"
	BlockKindException BlockKind = "exception"
	BlockKindExit      BlockKind = "exit"
)

// Block is one decomposition unit returned by SubdivideMethod.
// The dispatcher mints `<methodSig>#block_<Ordinal>_<Kind>` as
// the canonical signature and `attrs_json` carries the kind
// label so consumers can filter without re-parsing the
// signature.
type Block struct {
	// Ordinal is the 0-based position of this Block within
	// its enclosing Method's Block list. Embedded in the
	// canonical signature so two Blocks with the same Kind
	// (e.g. multiple `branch` blocks) get distinct
	// fingerprints.
	Ordinal int
	// Kind is the closed-set discriminator (see BlockKind).
	Kind BlockKind
	// StartLine / EndLine are the 1-based file lines this
	// block spans. File-relative (NOT body-relative) so a
	// future span-to-block resolver can match observed
	// stack-frame line numbers directly (per evaluator
	// finding #6 and rubber-duck #2).
	StartLine int
	EndLine   int
	// StartByte / EndByte are the 0-based file byte offsets
	// of the block's first/last byte. Persisted alongside
	// the line numbers for consumers that work in byte
	// offsets (e.g. LSP-based tools).
	StartByte int
	EndByte   int
}

// SubdivideMethod returns the Block decomposition for the
// supplied method. The function is the §3.7 / §8.2 gate keeper:
//
//   - When `CountLogicalLines(m.BodySource) <= DefaultBlockThreshold`,
//     returns nil (no Blocks). This is the path the
//     acceptance scenario "80 logical lines -> no Block
//     nodes" relies on.
//
//   - When `CountLogicalLines(m.BodySource) > DefaultBlockThreshold`,
//     returns exactly two Blocks -- {entry, exit} -- in
//     ordinal order, with their `StartLine` / `EndLine` /
//     `StartByte` / `EndByte` populated in FILE-RELATIVE
//     coordinates (rubber-duck #2). The split point is the
//     midpoint of the body's logical lines so entry and exit
//     have roughly equal weight; consumers care about the
//     boundary EXISTING for span resolution, not its exact
//     mid-line.
//
// Logical-line counting is performed after comment stripping
// (`stripComments` inside `CountLogicalLines`) so that a
// formatter-only commit that adds `//` lines or reflows
// whitespace does NOT push a previously-subdivided Method
// across the threshold and churn its Block set.
//
// Callers who only have the body string and no file
// coordinates (e.g. block_test.go's unit tests) can pass a
// zero-valued `MethodDecl` with just `BodySource` set; the
// returned Blocks will have body-relative coordinates only
// (StartLine 1..N relative to body), which is sufficient for
// threshold-contract testing.
func SubdivideMethod(m MethodDecl) []Block {
	body := m.BodySource
	if CountLogicalLines(body) <= DefaultBlockThreshold {
		return nil
	}
	totalLines := strings.Count(body, "\n") + 1
	midLine := totalLines / 2
	if midLine < 1 {
		midLine = 1
	}
	midByte := byteOffsetOfLine(body, midLine)

	baseLine := m.BodyStartLine
	if baseLine <= 0 {
		baseLine = 1
	}
	baseByte := m.BodyStartByte

	entryStartLine := baseLine
	entryEndLine := baseLine + midLine - 1
	exitStartLine := baseLine + midLine
	exitEndLine := baseLine + totalLines - 1
	if m.BodyEndLine > 0 {
		exitEndLine = m.BodyEndLine
	}

	entryStartByte := baseByte
	entryEndByte := baseByte + midByte - 1
	if entryEndByte < entryStartByte {
		entryEndByte = entryStartByte
	}
	exitStartByte := baseByte + midByte
	exitEndByte := baseByte + len(body) - 1
	if m.BodyEndByte > 0 {
		exitEndByte = m.BodyEndByte
	}

	return []Block{
		{
			Ordinal:   0,
			Kind:      BlockKindEntry,
			StartLine: entryStartLine,
			EndLine:   entryEndLine,
			StartByte: entryStartByte,
			EndByte:   entryEndByte,
		},
		{
			Ordinal:   1,
			Kind:      BlockKindExit,
			StartLine: exitStartLine,
			EndLine:   exitEndLine,
			StartByte: exitStartByte,
			EndByte:   exitEndByte,
		},
	}
}

// byteOffsetOfLine returns the 0-based byte offset of the
// start of the (1-based) line within s. Used to convert a
// line-based midpoint into a byte boundary for Block attrs.
func byteOffsetOfLine(s string, line int) int {
	if line <= 1 {
		return 0
	}
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			count++
			if count == line-1 {
				return i + 1
			}
		}
	}
	return len(s)
}

// blockSignature mints the canonical signature for a Block under
// the supplied enclosing-method signature. The format
// `<methodSig>#block_<Ordinal>_<Kind>` is dispatcher-stable and
// fingerprint-friendly: distinct (Ordinal, Kind) pairs produce
// distinct strings even when two blocks share the same Kind
// (e.g. multiple `branch` blocks under one method) — exercised
// by `block_test.go::TestBlockSignature_IsUniquePerOrdinal`.
func blockSignature(methodSig string, b Block) string {
	return fmt.Sprintf("%s#block_%d_%s", methodSig, b.Ordinal, b.Kind)
}
