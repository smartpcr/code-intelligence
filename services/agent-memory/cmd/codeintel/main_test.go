package main

import (
	"bytes"
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
	for _, name := range []string{"scan", "scan-many", "diagram", "serve"} {
		if !strings.Contains(out, name) {
			t.Errorf("help output missing subcommand %q: %s", name, out)
		}
	}
}

func TestSubcommandsReturnNotImplemented(t *testing.T) {
	cases := [][]string{
		{"scan"},
		{"scan-many"},
		{"diagram", "module"},
		{"diagram", "calls"},
		{"serve"},
	}
	for _, c := range cases {
		c := c
		t.Run(strings.Join(c, " "), func(t *testing.T) {
			_, _, err := execute(t, c...)
			if err == nil {
				t.Fatalf("expected error from %v", c)
			}
			if !strings.Contains(err.Error(), "not implemented") {
				t.Fatalf("expected 'not implemented' from %v, got %v", c, err)
			}
		})
	}
}

func TestInvalidLogFormatRejected(t *testing.T) {
	_, _, err := execute(t, "--log", "xml", "scan")
	if err == nil || !strings.Contains(err.Error(), "invalid --log") {
		t.Fatalf("expected --log validation error, got %v", err)
	}
}

func TestInvalidStoreRejected(t *testing.T) {
	_, _, err := execute(t, "--store", "mysql", "scan")
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

func TestPersistentFlagsRegistered(t *testing.T) {
	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, name := range []string{"store", "db", "log", "with-embeddings"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("persistent flag %q not registered", name)
		}
	}
}
