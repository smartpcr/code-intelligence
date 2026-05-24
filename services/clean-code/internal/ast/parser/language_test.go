package parser

import "testing"

func TestDetectLanguage_FromExtensions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"foo.go", LanguageGo, true},
		{"pkg/sub/bar.go", LanguageGo, true},
		{"foo.py", LanguagePython, true},
		{"foo.pyi", LanguagePython, true},
		{"foo.ts", LanguageTypeScript, true},
		{"foo.tsx", LanguageTypeScript, true},
		{"foo.mts", LanguageTypeScript, true},
		{"foo.cts", LanguageTypeScript, true},
		{"foo.js", LanguageTypeScript, true},
		{"foo.jsx", LanguageTypeScript, true},
		{"foo.mjs", LanguageTypeScript, true},
		{"foo.cjs", LanguageTypeScript, true},
		{"Sample.java", LanguageJava, true},
		// Negative cases.
		{"foo.cs", "", false},
		{"foo.rs", "", false},
		{"foo.rb", "", false},
		{"Makefile", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			got, ok := DetectLanguage(tc.path, nil)
			if ok != tc.ok {
				t.Fatalf("DetectLanguage(%q) ok = %v; want %v", tc.path, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("DetectLanguage(%q) = %q; want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestDetectLanguage_ShebangSniff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		content string
		want    string
		ok      bool
	}{
		{"env_python3", "script", "#!/usr/bin/env python3\nprint(1)\n", LanguagePython, true},
		{"env_python", "tool", "#!/usr/bin/env python\nprint(1)\n", LanguagePython, true},
		{"direct_python", "tool", "#!/usr/bin/python3\nprint(1)\n", LanguagePython, true},
		{"shell_not_supported", "tool.sh", "#!/bin/sh\necho hi\n", "", false},
		{"no_shebang", "tool", "print(1)\n", "", false},
		{"empty", "tool", "", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := DetectLanguage(tc.path, []byte(tc.content))
			if ok != tc.ok {
				t.Fatalf("DetectLanguage(%q) ok = %v; want %v", tc.name, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("DetectLanguage(%q) = %q; want %q", tc.name, got, tc.want)
			}
		})
	}
}
