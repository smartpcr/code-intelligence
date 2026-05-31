//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/diagram"
)

// ---------------------------------------------------------------------------
// Shared state for diagram_projector_diagram_envelope_types scenarios
// ---------------------------------------------------------------------------

type envelopeTypesCtx struct {
	envelope    diagram.Diagram
	goldenBytes []byte
	marshalled  []byte
	remarshalled []byte
}

// goldenFilePath returns the absolute path to the golden fixture.
func goldenFilePath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "internal", "diagram", "testdata", "envelope_module_golden.json")
}

// normaliseNewlines converts CRLF to LF and ensures exactly one trailing \n.
func envelopeNormaliseNewlines(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	out := bytes.TrimRight(b, "\n")
	if len(out) == 0 {
		return out
	}
	return append(out, '\n')
}

// ---------------------------------------------------------------------------
// Steps — envelope-marshal-key-order
// ---------------------------------------------------------------------------

func (e *envelopeTypesCtx) anEnvelopeValueMatchingTheGoldenFixture() error {
	attrs := json.RawMessage(`{
        "decl_kind": "package",
        "start_line": 1,
        "end_line": 1
      }`)

	e.envelope = diagram.Diagram{
		Diagram: diagram.KindModule,
		Repo: diagram.Repo{
			ID:  "11111111-2222-3333-4444-555555555555",
			URL: "file:///tmp/example-repo",
			SHA: "9d1a2c47b3e95f80a162b08c5d3f4e71",
		},
		GeneratedAt: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
		LayoutHint:  diagram.LayoutHierarchicalTopDown,
		Nodes: []diagram.Node{
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
		Edges: []diagram.Edge{
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
		Stats: diagram.Stats{
			NodeCount: 2,
			EdgeCount: 1,
			CappedAt:  diagram.MaxListLimit,
			Skipped: map[string]int{
				"no_parser":          0,
				"pwsh_not_available": 0,
			},
		},
	}
	return nil
}

func (e *envelopeTypesCtx) encodingJSONMarshalRunsWithTwoSpaceIndentation() error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e.envelope); err != nil {
		return err
	}
	e.marshalled = buf.Bytes()

	golden, err := os.ReadFile(goldenFilePath())
	if err != nil {
		return err
	}
	e.goldenBytes = golden
	return nil
}

func (e *envelopeTypesCtx) theResultingBytesMatchTheStoredGoldenFileByteForByte() error {
	got := envelopeNormaliseNewlines(e.marshalled)
	want := envelopeNormaliseNewlines(e.goldenBytes)
	if !bytes.Equal(got, want) {
		return fmt.Errorf("golden mismatch:\n--- want ---\n%s\n--- got ---\n%s", string(want), string(got))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Steps — envelope-unmarshal-roundtrip
// ---------------------------------------------------------------------------

func (e *envelopeTypesCtx) marshalledBytesFromTheGoldenFile() error {
	golden, err := os.ReadFile(goldenFilePath())
	if err != nil {
		return err
	}
	e.goldenBytes = golden
	return nil
}

func (e *envelopeTypesCtx) unmarshalledBackIntoTheStructAndReMarshalled() error {
	var d diagram.Diagram
	if err := json.Unmarshal(e.goldenBytes, &d); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return fmt.Errorf("re-marshal: %w", err)
	}
	e.remarshalled = buf.Bytes()
	return nil
}

func (e *envelopeTypesCtx) theSecondPassEqualsTheFirstByteForByte() error {
	got := envelopeNormaliseNewlines(e.remarshalled)
	want := envelopeNormaliseNewlines(e.goldenBytes)
	if !bytes.Equal(got, want) {
		return fmt.Errorf("roundtrip mismatch:\n--- want (golden) ---\n%s\n--- got (re-marshalled) ---\n%s", string(want), string(got))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Initializer + test entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_diagram_projector_diagram_envelope_types(ctx *godog.ScenarioContext) {
	e := &envelopeTypesCtx{}

	// envelope-marshal-key-order
	ctx.Step(`^an envelope value matching the golden fixture$`, e.anEnvelopeValueMatchingTheGoldenFixture)
	ctx.Step(`^encoding/json\.Marshal runs with two-space indentation$`, e.encodingJSONMarshalRunsWithTwoSpaceIndentation)
	ctx.Step(`^the resulting bytes match the stored golden file byte-for-byte$`, e.theResultingBytesMatchTheStoredGoldenFileByteForByte)

	// envelope-unmarshal-roundtrip
	ctx.Step(`^marshalled bytes from the golden file$`, e.marshalledBytesFromTheGoldenFile)
	ctx.Step(`^unmarshalled back into the struct and re-marshalled$`, e.unmarshalledBackIntoTheStructAndReMarshalled)
	ctx.Step(`^the second pass equals the first byte-for-byte$`, e.theSecondPassEqualsTheFirstByteForByte)
}

func TestE2E_diagram_projector_diagram_envelope_types(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_diagram_projector_diagram_envelope_types,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"diagram_projector_diagram_envelope_types.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog suite failed")
	}
}
