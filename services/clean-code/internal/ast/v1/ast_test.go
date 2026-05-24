package astv1_test

import (
	"testing"

	astv1 "github.com/microsoft/code-intelligence/services/clean-code/internal/ast/v1"
	"google.golang.org/protobuf/proto"
)

// TestProtoRoundTrip exercises scenario `proto-round-trip` from
// implementation-plan Stage 2.1. Given a fully-populated
// AstFile, marshal -> unmarshal MUST yield an equal struct
// (no information loss on the wire).
func TestProtoRoundTrip(t *testing.T) {
	t.Parallel()
	in := &astv1.AstFile{
		Language:       "go",
		Path:           "pkg/sample.go",
		ContentSha256:  "abc123",
		ParserVersion:  "v1-tree-sitter-2026.05",
		DegradedReason: "",
		Attrs:          map[string]string{"encoding": "utf-8"},
		Scopes: []*astv1.AstScope{
			{
				ScopeId:       "local:0",
				ScopeKind:     astv1.ScopeKind_SCOPE_KIND_FILE,
				Name:          "sample.go",
				QualifiedName: "sample.sample.go",
				Range:         &astv1.AstRange{StartByte: 0, EndByte: 100, StartLine: 1, EndLine: 10, StartCol: 1, EndCol: 1},
			},
			{
				ScopeId:       "local:1",
				ScopeKind:     astv1.ScopeKind_SCOPE_KIND_PACKAGE,
				Name:          "sample",
				QualifiedName: "sample",
				Range:         &astv1.AstRange{StartByte: 0, EndByte: 10, StartLine: 1, EndLine: 1, StartCol: 1, EndCol: 14},
				Attrs:         map[string]string{"language": "go"},
			},
			{
				ScopeId:       "local:2",
				ScopeKind:     astv1.ScopeKind_SCOPE_KIND_INTERFACE,
				Name:          "Sampler",
				QualifiedName: "sample.sample.go.Sampler",
				Range:         &astv1.AstRange{StartByte: 20, EndByte: 50, StartLine: 3, EndLine: 6, StartCol: 1, EndCol: 1},
				ParentScopeId: "local:0",
			},
			{
				ScopeId:       "local:3",
				ScopeKind:     astv1.ScopeKind_SCOPE_KIND_METHOD,
				Name:          "Sample",
				QualifiedName: "sample.sample.go.MemorySampler.Sample",
				Parameters:    []string{"context.Context", "int"},
				Range:         &astv1.AstRange{StartByte: 60, EndByte: 90, StartLine: 7, EndLine: 9, StartCol: 1, EndCol: 1},
				ParentScopeId: "local:0",
				Attrs:         map[string]string{"receiver_type": "MemorySampler", "returns": "string,error"},
			},
		},
		Symbols: []*astv1.AstSymbol{
			{
				SymbolId: "local:4",
				Name:     "context",
				Kind:     "import",
				ScopeId:  "local:0",
				Range:    &astv1.AstRange{StartByte: 11, EndByte: 19, StartLine: 2, EndLine: 2, StartCol: 1, EndCol: 9},
				Attrs:    map[string]string{"path": "context"},
			},
		},
		Edges: []*astv1.AstEdge{
			{
				EdgeId: "local:5",
				Kind:   "implements",
				From:   &astv1.AstRef{Kind: astv1.AstRefKind_AST_REF_KIND_SCOPE, Id: "local:3"},
				To:     &astv1.AstRef{Kind: astv1.AstRefKind_AST_REF_KIND_SCOPE, Id: "local:2"},
				Attrs:  map[string]string{"explicit": "false"},
			},
		},
	}

	wire, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	if len(wire) == 0 {
		t.Fatalf("proto.Marshal returned empty bytes")
	}
	out := &astv1.AstFile{}
	if err := proto.Unmarshal(wire, out); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("round-trip mismatch:\nin =%v\nout=%v", in, out)
	}
}

// TestProtoRoundTrip_EmptyFile ensures the empty/zero-value
// case round-trips cleanly too. Otherwise a code-path that
// emits a `nil` AstFile and a `&AstFile{}` AstFile would
// behave differently.
func TestProtoRoundTrip_EmptyFile(t *testing.T) {
	t.Parallel()
	in := &astv1.AstFile{}
	wire, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	out := &astv1.AstFile{}
	if err := proto.Unmarshal(wire, out); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("empty-AstFile round-trip mismatch")
	}
}

// TestScopeKind_StableEnumOrdinals pins the canonical
// architecture Sec 5.2.3 ordering of ScopeKind. Re-ordering or
// renaming an enum value here would be a wire-breaking change;
// the test exists to make it loud.
func TestScopeKind_StableEnumOrdinals(t *testing.T) {
	t.Parallel()
	expected := map[astv1.ScopeKind]int32{
		astv1.ScopeKind_SCOPE_KIND_UNSPECIFIED: 0,
		astv1.ScopeKind_SCOPE_KIND_REPO:        1,
		astv1.ScopeKind_SCOPE_KIND_PACKAGE:     2,
		astv1.ScopeKind_SCOPE_KIND_FILE:        3,
		astv1.ScopeKind_SCOPE_KIND_CLASS:       4,
		astv1.ScopeKind_SCOPE_KIND_INTERFACE:   5,
		astv1.ScopeKind_SCOPE_KIND_METHOD:      6,
		astv1.ScopeKind_SCOPE_KIND_BLOCK:       7,
	}
	for kind, want := range expected {
		if got := int32(kind); got != want {
			t.Errorf("ScopeKind(%v) = %d; want %d (architecture Sec 5.2.3 pin)", kind, got, want)
		}
	}
}
