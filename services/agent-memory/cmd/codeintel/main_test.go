package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func execute(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRootCmd(&out, &errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestRootHelpListsSubcommands(t *testing.T) {
	out, _, err := execute(t, "--help")
	if err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	for _, name := range []string{"scan", "scan-many", "diagram", "serve", "version"} {
		if !strings.Contains(out, name) {
			t.Errorf("help output missing subcommand %q: %s", name, out)
		}
	}
}

func TestSubcommandsReturnErrNotImplemented(t *testing.T) {
	// Only scan-many, diagram calls, and serve are still stubbed.
	// scan and diagram module have real implementations now.
	cases := [][]string{
		{"scan-many"},
		{"diagram", "calls"},
		{"serve"},
	}
	for _, c := range cases {
		c := c
		t.Run(strings.Join(c, " "), func(t *testing.T) {
			_, _, err := execute(t, c...)
			if !errors.Is(err, errNotImplemented) {
				t.Fatalf("expected errNotImplemented from %v, got %v", c, err)
			}
			if err == nil || err.Error() != "not implemented" {
				t.Fatalf("expected error string exactly %q, got %q", "not implemented", err)
			}
		})
	}
}

func TestScanRequiresArg(t *testing.T) {
	_, _, err := execute(t, "scan")
	if err == nil {
		t.Fatalf("expected error when scan called without arg")
	}
}

func TestDiagramModuleRequiresDB(t *testing.T) {
	_, _, err := execute(t, "diagram", "module")
	if err == nil {
		t.Fatalf("expected error when diagram module called without --db")
	}
	if !strings.Contains(err.Error(), "--db is required") {
		t.Fatalf("expected --db required error, got %v", err)
	}
}

func TestInvalidLogFormatRejected(t *testing.T) {
	_, _, err := execute(t, "--log", "xml", "scan-many")
	if err == nil || !strings.Contains(err.Error(), "invalid --log") {
		t.Fatalf("expected --log validation error, got %v", err)
	}
}

func TestInvalidStoreRejected(t *testing.T) {
	_, _, err := execute(t, "--store", "mysql", "scan-many")
	if err == nil || !strings.Contains(err.Error(), "invalid --store") {
		t.Fatalf("expected --store validation error, got %v", err)
	}
}

func TestJSONLogHandlerSelected(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf, "json")
	logger.Info("hello", "k", "v")
	s := buf.String()
	if !strings.Contains(s, "\"msg\":\"hello\"") || !strings.Contains(s, "\"k\":\"v\"") {
		t.Fatalf("expected JSON output, got %q", s)
	}
}

func TestTextLogHandlerDefault(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf, "text")
	logger.Info("hello", "k", "v")
	s := buf.String()
	if !strings.Contains(s, "msg=hello") || !strings.Contains(s, "k=v") {
		t.Fatalf("expected text output, got %q", s)
	}
}

// TestSubcommandJSONLogIsValidJSON exercises stage-5.1 scenario
// `log-flag-respected`: when --log=json is set, the log line a
// scaffolded subcommand emits must be parseable by
// encoding/json. We capture stderr (where slog writes) and feed
// it to json.Unmarshal.
func TestSubcommandJSONLogIsValidJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	root := newRootCmd(&out, &errOut)
	root.SetArgs([]string{"--log", "json", "scan-many"})
	_ = root.Execute() // scan-many returns errNotImplemented; that's expected.

	line := strings.TrimSpace(errOut.String())
	if line == "" {
		t.Fatalf("expected a JSON log line on stderr, got empty output")
	}
	firstLine := line
	if i := strings.Index(line, "\n"); i >= 0 {
		firstLine = line[:i]
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(firstLine), &decoded); err != nil {
		t.Fatalf("emitted log line is not valid JSON: %v\nline: %q", err, firstLine)
	}
	if decoded["msg"] != "codeintel subcommand invoked" {
		t.Errorf("expected msg field present, got %v", decoded["msg"])
	}
	if decoded["subcommand"] != "scan-many" {
		t.Errorf("expected subcommand=scan-many, got %v", decoded["subcommand"])
	}
}

func TestPersistentFlagsRegistered(t *testing.T) {
	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, name := range []string{"store", "db", "log", "with-embeddings"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("persistent flag %q not registered", name)
		}
	}
}

func TestVersionSubcommandPrintsBuildMetadata(t *testing.T) {
	out, _, err := execute(t, "version")
	if err != nil {
		t.Fatalf("version returned error: %v", err)
	}
	for _, want := range []string{"codeintel", version, commit, buildDate} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q: %s", want, out)
		}
	}
}

// TestVersionBypassesFlagValidation guards the documented
// contract that `codeintel version` answers even when other
// persistent-flag validators (e.g. an unknown --store) would
// otherwise abort the command. Without the version-cmd's own
// PersistentPreRunE the root's validator runs first and aborts.
func TestVersionBypassesFlagValidation(t *testing.T) {
	out, _, err := execute(t, "--store", "mysql", "version")
	if err != nil {
		t.Fatalf("version with invalid --store should still succeed, got %v", err)
	}
	if !strings.Contains(out, "codeintel") {
		t.Fatalf("expected version output, got %q", out)
	}
}

func TestUnknownSubcommandFails(t *testing.T) {
	_, _, err := execute(t, "bogus")
	if err == nil {
		t.Fatalf("expected error for unknown subcommand, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected error to name the offending subcommand, got %v", err)
	}
}
