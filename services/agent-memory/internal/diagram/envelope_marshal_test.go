package diagram

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// expectedKeyOrder is the architecture-S4.4-pinned envelope key
// order. The UI single-parser contract assumes this exact sequence
// at the top level of every diagram payload; a reorder is a
// breaking change to the wire format.
var expectedKeyOrder = []string{
	"diagram",
	"repo",
	"generatedAt",
	"layoutHint",
	"nodes",
	"edges",
	"truncated",
	"stats",
}

// TestEnvelopeMarshalKeyOrder asserts the envelope's top-level JSON
// keys serialise in the order pinned by architecture S4.4 and
// repeated in tech-spec S6.2. The check parses the marshalled bytes
// as a json.Token stream so it observes the *physical* key order
// the encoder writes (a map[string]any round-trip would re-sort).
func TestEnvelopeMarshalKeyOrder(t *testing.T) {
	t.Parallel()

	d := Diagram{
		Diagram: KindModule,
		Repo: Repo{
			ID:  "00000000-0000-0000-0000-000000000000",
			URL: "file:///tmp/x",
			SHA: "9d1a2c47b3e95f80a162b08c5d3f4e71",
		},
		GeneratedAt: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
		LayoutHint:  LayoutHierarchicalTopDown,
		Nodes:       []Node{},
		Edges:       []Edge{},
		Truncated:   false,
		Stats: Stats{
			CappedAt: MaxListLimit,
			Skipped:  map[string]int{},
		},
	}

	buf, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := topLevelKeys(t, buf)
	if !reflect.DeepEqual(got, expectedKeyOrder) {
		t.Fatalf("envelope key order mismatch:\n  want %v\n  got  %v\n  raw  %s",
			expectedKeyOrder, got, string(buf))
	}
}

// TestEnvelopeMarshalGolden marshals a representative module
// diagram and compares its rendered JSON against the golden file.
// The golden file is hand-authored to encode both the field order
// AND the canonical formatting (indented two spaces, sorted edge/
// node ordering driven by the caller, not the encoder).
func TestEnvelopeMarshalGolden(t *testing.T) {
	t.Parallel()

	attrs := json.RawMessage(`{
        "decl_kind": "package",
        "start_line": 1,
        "end_line": 1
      }`)

	d := Diagram{
		Diagram: KindModule,
		Repo: Repo{
			ID:  "11111111-2222-3333-4444-555555555555",
			URL: "file:///tmp/example-repo",
			SHA: "9d1a2c47b3e95f80a162b08c5d3f4e71",
		},
		GeneratedAt: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
		LayoutHint:  LayoutHierarchicalTopDown,
		Nodes: []Node{
			{
				ID:       "pkg:github.com/example/app/cmd",
				Label:    "cmd",
				Kind:     "package",
				Language: "go",
				Group:    "github.com/example/app",
				Attrs:    attrs,
			},
			{
				ID:       "pkg:github.com/example/app/internal/svc",
				Label:    "svc",
				Kind:     "package",
				Language: "go",
				Group:    "github.com/example/app",
				Attrs:    attrs,
			},
		},
		Edges: []Edge{
			{
				ID:     "edge-imports-cmd-svc",
				From:   "pkg:github.com/example/app/cmd",
				To:     "pkg:github.com/example/app/internal/svc",
				Kind:   "imports",
				Weight: 3,
				Label:  "imports",
			},
		},
		Truncated: false,
		Stats: Stats{
			NodeCount: 2,
			EdgeCount: 1,
			CappedAt:  MaxListLimit,
			Skipped: map[string]int{
				"no_parser":          0,
				"pwsh_not_available": 0,
			},
		},
	}

	var got bytes.Buffer
	enc := json.NewEncoder(&got)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		t.Fatalf("encode: %v", err)
	}

	goldenPath := filepath.Join("testdata", "envelope_module_golden.json")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	// Both buffers normalise to LF + a trailing newline so a CRLF
	// editor or git autocrlf cannot flake the comparison.
	gotN := normaliseNewlines(got.Bytes())
	wantN := normaliseNewlines(want)

	if !bytes.Equal(gotN, wantN) {
		t.Fatalf("golden mismatch (regenerate testdata/envelope_module_golden.json if intentional):\n--- want ---\n%s\n--- got ---\n%s",
			string(wantN), string(gotN))
	}

	// Sanity: the golden file itself must also obey the key order.
	if keys := topLevelKeys(t, want); !reflect.DeepEqual(keys, expectedKeyOrder) {
		t.Fatalf("golden file key order mismatch: want %v got %v", expectedKeyOrder, keys)
	}
}

// TestNewEmptyShape ensures the helper returns an envelope whose
// nodes/edges/skipped slices and map are non-nil (so the rendered
// JSON has `[]` and `{}` rather than `null`) and whose CappedAt
// pre-populates from MaxListLimit per S7.3.
func TestNewEmptyShape(t *testing.T) {
	t.Parallel()

	repo := Repo{ID: "r", URL: "file:///x", SHA: "9d1a2c47b3e95f80a162b08c5d3f4e71"}
	ts := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	d := NewEmpty(KindCallChain, LayoutHierarchicalLeftRight, repo, ts)

	if d.Nodes == nil || d.Edges == nil || d.Stats.Skipped == nil {
		t.Fatalf("NewEmpty must return non-nil slices/map: nodes=%v edges=%v skipped=%v",
			d.Nodes, d.Edges, d.Stats.Skipped)
	}
	if d.Stats.CappedAt != MaxListLimit {
		t.Fatalf("CappedAt want %d got %d", MaxListLimit, d.Stats.CappedAt)
	}

	buf, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(buf, []byte(`"nodes":[]`)) || !bytes.Contains(buf, []byte(`"edges":[]`)) {
		t.Fatalf("empty envelope must render []  not null: %s", string(buf))
	}
	if !bytes.Contains(buf, []byte(`"skipped":{}`)) {
		t.Fatalf("empty envelope must render skipped {} not null: %s", string(buf))
	}
}

// TestDirectStructConstructionNilSafe pins the resolution to prior
// evaluator item #2: a directly-constructed `Diagram{}` (without
// the NewEmpty helper, with nil Nodes/Edges/Stats.Skipped) MUST
// still marshal those collections as `[]` / `{}` so the UI
// single-parser contract holds even when a caller bypasses
// NewEmpty (e.g. a projector that builds the envelope field by
// field, or a test fixture). Without the custom MarshalJSON
// methods on Diagram and Stats this test would fail with `null`
// in three places.
func TestDirectStructConstructionNilSafe(t *testing.T) {
	t.Parallel()

	// Intentionally zero-value: Nodes, Edges, Stats.Skipped are nil.
	d := Diagram{
		Diagram: KindModule,
		Repo: Repo{
			ID:  "r",
			URL: "file:///x",
			SHA: "9d1a2c47b3e95f80a162b08c5d3f4e71",
		},
		GeneratedAt: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
		LayoutHint:  LayoutHierarchicalTopDown,
	}

	if d.Nodes != nil || d.Edges != nil || d.Stats.Skipped != nil {
		t.Fatalf("precondition: this test asserts nil-collection behaviour; got non-nil")
	}

	buf, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(buf)

	for _, want := range []string{
		`"nodes":[]`,
		`"edges":[]`,
		`"skipped":{}`,
	} {
		if !bytes.Contains(buf, []byte(want)) {
			t.Fatalf("direct-struct construction must emit %s (not null), got: %s", want, got)
		}
	}
	for _, forbidden := range []string{
		`"nodes":null`,
		`"edges":null`,
		`"skipped":null`,
	} {
		if bytes.Contains(buf, []byte(forbidden)) {
			t.Fatalf("direct-struct construction emitted forbidden %s in: %s", forbidden, got)
		}
	}

	// Key order MUST still hold even with the custom MarshalJSON.
	if keys := topLevelKeys(t, buf); !reflect.DeepEqual(keys, expectedKeyOrder) {
		t.Fatalf("custom MarshalJSON must preserve envelope key order: want %v got %v",
			expectedKeyOrder, keys)
	}
}

// topLevelKeys scans `buf` as a JSON token stream and returns the
// top-level object's keys in the exact order the encoder wrote
// them. Any nested object's / array's keys are skipped by draining
// the nested structure when encountered.
func topLevelKeys(t *testing.T, buf []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(buf))

	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("read opening token: %v", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		t.Fatalf("envelope must marshal to a JSON object, got %v", tok)
	}

	var keys []string
	for dec.More() {
		// At this position the next token must be a string key
		// (we are at the top-level object, just before a member).
		keyTok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key token: %v", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			t.Fatalf("expected string key at top level, got %T (%v)", keyTok, keyTok)
		}
		keys = append(keys, key)

		// Consume the value: a scalar is one Token(); an object
		// or array must be drained until its matching close.
		valTok, err := dec.Token()
		if err != nil {
			t.Fatalf("read value token: %v", err)
		}
		if delim, ok := valTok.(json.Delim); ok {
			drainContainer(t, dec, delim)
		}
	}
	return keys
}

// drainContainer consumes tokens until the container opened by
// `open` ('{' or '[') is fully closed. Handles arbitrary nesting.
func drainContainer(t *testing.T, dec *json.Decoder, open json.Delim) {
	t.Helper()
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("drain token: %v", err)
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	_ = open
}

// normaliseNewlines converts CRLF to LF and ensures exactly one
// trailing \n (empty in -> empty out). Decouples the golden
// assertion from git autocrlf / editor newline policy on Windows.
func normaliseNewlines(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	out := bytes.TrimRight(b, "\n")
	if len(out) == 0 {
		return out
	}
	return append(out, '\n')
}
