package recipes_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestDefaultProjectRegistry_Stage25BasePack pins the
// canonical Stage 2.5 project-level base-pack registry to
// EXACTLY `{cycle_member, duplication_ratio}`. A regression
// where a future recipe quietly lands in DefaultProjectRegistry
// without arch sign-off MUST fail this test.
func TestDefaultProjectRegistry_Stage25BasePack(t *testing.T) {
	t.Parallel()
	reg := recipes.DefaultProjectRegistry()
	got := reg.MetricKinds()
	want := []string{"cycle_member", "duplication_ratio"}
	if len(got) != len(want) {
		t.Fatalf("MetricKinds() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MetricKinds()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestProjectRegistry_DuplicateRegistrationPanics asserts the
// closed-set guard rejects duplicate registrations.
func TestProjectRegistry_DuplicateRegistrationPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate registration, got none")
		}
	}()
	reg := recipes.NewProjectRegistry()
	reg.Register(recipes.NewCycleMemberRecipe())
	reg.Register(recipes.NewCycleMemberRecipe())
}

// TestProjectRegistry_NilRegisterPanics asserts nil recipes
// are rejected at registration.
func TestProjectRegistry_NilRegisterPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil recipe, got none")
		}
	}()
	reg := recipes.NewProjectRegistry()
	reg.Register(nil)
}

// TestDefaultProjectRegistryWithLog_LogsAndReturns asserts
// the composition-root convenience constructs the default
// project registry AND emits a structured INFO log line
// containing the registered metric_kinds. Mirrors the
// existing [TestDefaultRegistryWithLog_LogsAndReturns]
// pattern for the per-file registry.
func TestDefaultProjectRegistryWithLog_LogsAndReturns(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := recipes.DefaultProjectRegistryWithLog(logger)
	if reg == nil {
		t.Fatalf("DefaultProjectRegistryWithLog returned nil")
	}
	out := buf.String()
	if !strings.Contains(out, `"component":"recipes.project_registry"`) {
		t.Errorf("expected log line carrying component=recipes.project_registry, got: %s", out)
	}
	if !strings.Contains(out, `"cycle_member"`) {
		t.Errorf("expected log line to mention cycle_member, got: %s", out)
	}
	if !strings.Contains(out, `"duplication_ratio"`) {
		t.Errorf("expected log line to mention duplication_ratio, got: %s", out)
	}
}

// TestProjectRegistry_LogRegistered_NilLoggerNoop asserts a
// nil logger is safe (composition root may construct the
// registry before logger wiring).
func TestProjectRegistry_LogRegistered_NilLoggerNoop(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LogRegistered with nil logger panicked: %v", r)
		}
	}()
	recipes.DefaultProjectRegistry().LogRegistered(nil)
}

// TestProjectRegistry_LogRegistered_NilReceiverNoop asserts a
// nil *ProjectRegistry's LogRegistered is a no-op (defensive
// path mirroring [Registry.LogRegistered]).
func TestProjectRegistry_LogRegistered_NilReceiverNoop(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	var reg *recipes.ProjectRegistry
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-ProjectRegistry LogRegistered panicked: %v", r)
		}
	}()
	reg.LogRegistered(logger)
	if buf.Len() != 0 {
		t.Errorf("nil-ProjectRegistry LogRegistered emitted %d bytes; want 0", buf.Len())
	}
}
