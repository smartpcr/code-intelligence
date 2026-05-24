package main

import (
	"reflect"
	"testing"
)

func TestAppendOrReplace_AddsWhenAbsent(t *testing.T) {
	in := []string{"PATH=/usr/bin", "HOME=/root"}
	got := appendOrReplace(in, "CGO_ENABLED", "0")
	want := []string{"PATH=/usr/bin", "HOME=/root", "CGO_ENABLED=0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendOrReplace add: got %#v, want %#v", got, want)
	}
}

func TestAppendOrReplace_ReplacesWhenPresent(t *testing.T) {
	in := []string{"PATH=/usr/bin", "CGO_ENABLED=1", "HOME=/root"}
	got := appendOrReplace(in, "CGO_ENABLED", "0")
	want := []string{"PATH=/usr/bin", "CGO_ENABLED=0", "HOME=/root"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendOrReplace replace: got %#v, want %#v", got, want)
	}
}

func TestAppendOrReplace_OnlyMatchesExactKey(t *testing.T) {
	// CGO_ENABLED_EXTRA must not be treated as a hit for CGO_ENABLED.
	in := []string{"CGO_ENABLED_EXTRA=1"}
	got := appendOrReplace(in, "CGO_ENABLED", "0")
	want := []string{"CGO_ENABLED_EXTRA=1", "CGO_ENABLED=0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appendOrReplace exact-key: got %#v, want %#v", got, want)
	}
}
