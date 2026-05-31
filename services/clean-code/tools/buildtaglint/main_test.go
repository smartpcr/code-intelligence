// -----------------------------------------------------------------------
// <copyright file="main_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildExprExcludesProd covers the constraint forms
// actually written in this repo plus a handful of edge cases
// that future bypass files could plausibly grow into.
func TestBuildExprExcludesProd(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{"!prod", true},
		{"!prod && !release", true},
		{"!release && !prod", true},
		{"!(prod)", false},          // conservative: parens not collapsed
		{"prod", false},             // gated TO prod
		{"!release", false},         // says nothing about prod
		{"", false},                 // empty constraint
		{"!prod || other", false},   // disjunction conservatively rejected
		{"cgo && !prod", true},      // combined positive + !prod
		{"prod && !release", false}, // positive prod present
	}
	for _, c := range cases {
		c := c
		t.Run(c.expr, func(t *testing.T) {
			got := buildExprExcludesProd(c.expr)
			if got != c.want {
				t.Fatalf("buildExprExcludesProd(%q) = %v; want %v",
					c.expr, got, c.want)
			}
		})
	}
}

// TestLintFile_HappyPath confirms a file that constructs a
// `steward.PolicyVersion{Signature: nil, ...}` while carrying
// `//go:build !prod` lints clean.
func TestLintFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "happy.go")
	mustWrite(t, path, `//go:build !prod

package devpolicy

import "stub/steward"

func _example() steward.PolicyVersion {
	return steward.PolicyVersion{
		Name:      "x",
		Signature: nil,
	}
}
`)
	findings, err := lintFile(path, false)
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings; got %d: %+v", len(findings), findings)
	}
}

// TestLintFile_MissingTag confirms a violation when the build
// tag is absent.
func TestLintFile_MissingTag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.go")
	mustWrite(t, path, `package devpolicy

import "stub/steward"

func _example() steward.PolicyVersion {
	return steward.PolicyVersion{Name: "x"}
}
`)
	findings, err := lintFile(path, false)
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding; got %d: %+v", len(findings), findings)
	}
}

// TestLintFile_ProdTagViolates confirms that a positive
// `//go:build prod` constraint does NOT satisfy the rule
// (the file would compile into prod).
func TestLintFile_ProdTagViolates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prodbad.go")
	mustWrite(t, path, `//go:build prod

package devpolicy

import "stub/steward"

func _example() steward.PolicyVersion {
	return steward.PolicyVersion{Name: "x", Signature: nil}
}
`)
	findings, err := lintFile(path, false)
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding under //go:build prod; got %d", len(findings))
	}
}

// TestLintFile_SignedIgnored confirms a literal with a non-nil
// Signature is not flagged regardless of build tag.
func TestLintFile_SignedIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signed.go")
	mustWrite(t, path, `package devpolicy

import "stub/steward"

func sign() []byte { return []byte("sig") }

func _example() steward.PolicyVersion {
	return steward.PolicyVersion{Name: "x", Signature: sign()}
}
`)
	findings, err := lintFile(path, false)
	if err != nil {
		t.Fatalf("lintFile: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("signed PolicyVersion should not be flagged; got %d findings", len(findings))
	}
}

// TestLintRepoDevpolicy: when run against the real
// `internal/cli/devpolicy` tree this MUST be clean. This is
// the regression guard: if a future commit drops the
// `//go:build !prod` tag from a bypass file, the test (and
// `make lint-cli`) will fail.
func TestLintRepoDevpolicy(t *testing.T) {
	// Resolve `internal/cli/devpolicy` relative to this
	// test file (which lives under `tools/buildtaglint`).
	root := filepath.Join("..", "..", "internal", "cli", "devpolicy")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("devpolicy tree not present at %q: %v", root, err)
	}
	res, err := scan(root, false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.scanned == 0 {
		t.Fatalf("scan walked zero files under %q", root)
	}
	if len(res.findings) != 0 {
		for _, f := range res.findings {
			t.Errorf("%s", f.String())
		}
		t.Fatalf("buildtaglint reported %d violation(s) in real devpolicy tree", len(res.findings))
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
