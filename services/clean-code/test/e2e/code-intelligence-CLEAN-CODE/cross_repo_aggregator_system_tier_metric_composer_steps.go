package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/aggregator"
)

// lookupEnv wraps os.LookupEnv for use by requireEnv in the _test.go
// file (which cannot call os.LookupEnv directly through a build-tag
// boundary).
func lookupEnv(name string) (string, bool) {
	v := os.Getenv(name)
	if v == "" {
		return "", false
	}
	return v, true
}

// serviceRoot returns the absolute path to services/clean-code.
func serviceRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(thisFile)
	root := filepath.Join(dir, "..", "..", "..")
	return filepath.Abs(root)
}

// composerPackageExists returns true when the aggregator package has
// at least one .go file, indicating the impl branch has landed.
func composerPackageExists(svcRoot string) bool {
	pkg := filepath.Join(svcRoot, "internal", "aggregator")
	entries, err := os.ReadDir(pkg)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// metricSampleRow is a lightweight representation of a system-tier
// metric_sample row emitted by the composer or synthesised by the
// test setup for acceptance-scenario validation.
type metricSampleRow struct {
	MetricKind     string
	Pack           string
	Degraded       bool
	DegradedReason string
	ValuePresent   bool
}

// systemTierState holds per-scenario state shared across steps.
type systemTierState struct {
	composer       *aggregator.SystemTierComposer
	metricKinds    []string
	samples        []metricSampleRow
	composeErr     error
	degradedCounts map[string]int
}

func newSystemTierState() *systemTierState {
	return &systemTierState{
		degradedCounts: make(map[string]int),
	}
}

// collectSamples converts aggregator output into metricSampleRows and
// updates degradedCounts.
func (s *systemTierState) collectSamples(raw []aggregator.SystemTierSample) {
	s.samples = make([]metricSampleRow, 0, len(raw))
	for _, sample := range raw {
		s.samples = append(s.samples, metricSampleRow{
			MetricKind:     sample.MetricKind,
			Pack:           sample.Pack,
			Degraded:       sample.Degraded,
			DegradedReason: sample.DegradedReason,
			ValuePresent:   sample.Value != nil,
		})
		if sample.Degraded && sample.DegradedReason != "" {
			s.degradedCounts[sample.DegradedReason]++
		}
	}
}

// --- Scenario 1: system-tier-only-canonical-kinds ---

func (s *systemTierState) theSystemTierComposerAtRuntime() error {
	c, err := aggregator.NewSystemTierComposer()
	if err != nil {
		return fmt.Errorf("NewSystemTierComposer: %w", err)
	}
	s.composer = c
	return nil
}

func (s *systemTierState) listingTheMetricKindsItWillWrite() error {
	s.metricKinds = make([]string, len(aggregator.CanonicalSystemTierMetricKinds))
	copy(s.metricKinds, aggregator.CanonicalSystemTierMetricKinds)
	sort.Strings(s.metricKinds)
	return nil
}

func (s *systemTierState) theSetIsExactly(expected string) error {
	parts := strings.Split(expected, ", ")
	want := make([]string, len(parts))
	copy(want, parts)
	sort.Strings(want)

	if len(s.metricKinds) != len(want) {
		return fmt.Errorf("metric_kinds count mismatch: got %d (%v), want %d (%v)",
			len(s.metricKinds), s.metricKinds, len(want), want)
	}
	for i := range want {
		if s.metricKinds[i] != want[i] {
			return fmt.Errorf("metric_kinds[%d] = %q, want %q (full: got %v, want %v)",
				i, s.metricKinds[i], want[i], s.metricKinds, want)
		}
	}
	return nil
}

func (s *systemTierState) noMetricKindMatchesBannedPatterns() error {
	banned := []string{"p50.system", "p90.system", "p95.system", "p99.system"}
	kindSet := aggregator.SystemTierMetricKindSet()
	for _, b := range banned {
		if _, ok := kindSet[b]; ok {
			return fmt.Errorf("canonical set unexpectedly contains banned metric_kind %q", b)
		}
	}
	return nil
}

// --- Scenario 2: embedded-mode-writes-degraded-row ---

func (s *systemTierState) theAggregatorInEmbeddedModeWithNoXrepoEdges() error {
	c, err := aggregator.NewSystemTierComposer()
	if err != nil {
		return fmt.Errorf("NewSystemTierComposer: %w", err)
	}
	s.composer = c

	repoID, _ := uuid.NewV4()
	runID, _ := uuid.NewV4()
	pkgScope, _ := uuid.NewV4()
	repoScope, _ := uuid.NewV4()
	methodScope, _ := uuid.NewV4()

	in := aggregator.SystemTierInput{
		Mode:          aggregator.SystemTierModeEmbedded,
		RepoID:        repoID,
		SHA:           "abc123def456",
		ProducerRunID: runID,
		Scopes: []aggregator.ScopeRef{
			{ScopeID: pkgScope, ScopeKind: "package"},
			{ScopeID: repoScope, ScopeKind: "repo"},
			{ScopeID: methodScope, ScopeKind: "method"},
		},
		XRepoEdges:          nil,
		XRepoEdgesAvailable: false,
		CallEdges:           nil,
		CallEdgesAvailable:  false,
		Foundation:          nil,
		VelocityWindows:     nil,
	}

	raw, err := s.composer.Compose(context.Background(), in)
	if err != nil {
		s.composeErr = err
		return nil
	}
	s.collectSamples(raw)

	return nil
}

func (s *systemTierState) itComposesMetricKindForAnAffectedScope(metricKind string) error {
	if s.composeErr != nil {
		return fmt.Errorf("Compose returned error: %w", s.composeErr)
	}
	for _, sample := range s.samples {
		if sample.MetricKind == metricKind {
			return nil
		}
	}
	return fmt.Errorf("metric_kind %q not found in compose output (got %d samples)", metricKind, len(s.samples))
}

// --- Shared Then steps ---

func (s *systemTierState) aMetricSampleRowIsWrittenWith(metricKind, pack, degraded, degradedReason string) error {
	if s.composeErr != nil {
		return fmt.Errorf("Compose returned error: %w", s.composeErr)
	}
	wantDegraded := degraded == "true"
	for _, sample := range s.samples {
		if sample.MetricKind == metricKind &&
			sample.Pack == pack &&
			sample.Degraded == wantDegraded &&
			sample.DegradedReason == degradedReason {
			return nil
		}
	}
	var found []string
	for _, sample := range s.samples {
		if sample.MetricKind == metricKind {
			found = append(found, fmt.Sprintf("{pack=%s degraded=%v reason=%s}",
				sample.Pack, sample.Degraded, sample.DegradedReason))
		}
	}
	return fmt.Errorf("no matching sample: metric_kind=%s pack=%s degraded=%s degraded_reason=%s; found for kind: %v",
		metricKind, pack, degraded, degradedReason, found)
}

func (s *systemTierState) theDegradedCounterLabelledReasonIncrements(reason string) error {
	if count, ok := s.degradedCounts[reason]; !ok || count < 1 {
		return fmt.Errorf("degraded counter for reason=%q did not increment (count=%d)", reason, count)
	}
	return nil
}

// --- Scenario 3: samples-pending-writes-degraded-row ---

func (s *systemTierState) missingFoundationSamplesForScopeAtSHA(foundationKind string) error {
	c, err := aggregator.NewSystemTierComposer()
	if err != nil {
		return fmt.Errorf("NewSystemTierComposer: %w", err)
	}
	s.composer = c

	repoID, _ := uuid.NewV4()
	runID, _ := uuid.NewV4()
	repoScope, _ := uuid.NewV4()

	in := aggregator.SystemTierInput{
		Mode:          aggregator.SystemTierModeEmbedded,
		RepoID:        repoID,
		SHA:           "deadbeef1234",
		ProducerRunID: runID,
		Scopes: []aggregator.ScopeRef{
			{ScopeID: repoScope, ScopeKind: "repo"},
		},
		Foundation:      nil,
		VelocityWindows: nil,
	}

	raw, err := s.composer.Compose(context.Background(), in)
	if err != nil {
		s.composeErr = err
		return nil
	}
	s.collectSamples(raw)
	return nil
}

func (s *systemTierState) theAggregatorComposesMetricKindAtThatSHA(metricKind string) error {
	if s.composeErr != nil {
		return fmt.Errorf("Compose returned error: %w", s.composeErr)
	}
	for _, sample := range s.samples {
		if sample.MetricKind == metricKind {
			return nil
		}
	}
	return fmt.Errorf("metric_kind %q not found in compose output", metricKind)
}

func (s *systemTierState) theValueMayBeNULL() error {
	if s.composeErr != nil {
		return fmt.Errorf("Compose returned error: %w", s.composeErr)
	}
	for _, sample := range s.samples {
		if sample.Degraded && !sample.ValuePresent {
			return nil
		}
	}
	return fmt.Errorf("expected at least one degraded sample with nil Value")
}

// InitializeScenario_cross_repo_aggregator_system_tier_metric_composer
// registers all Given/When/Then steps for this stage's scenarios.
func InitializeScenario_cross_repo_aggregator_system_tier_metric_composer(ctx *godog.ScenarioContext) {
	s := newSystemTierState()

	// Scenario 1: system-tier-only-canonical-kinds
	ctx.Given(`^the system_tier composer at runtime$`, func() error {
		return s.theSystemTierComposerAtRuntime()
	})
	ctx.When(`^listing the metric_kinds it will write$`, func() error {
		return s.listingTheMetricKindsItWillWrite()
	})
	ctx.Then(`^the set is exactly "([^"]*)"$`, func(expected string) error {
		return s.theSetIsExactly(expected)
	})
	ctx.Then(`^no metric_kind matches "p50\.system" or "p90\.system" or "p95\.system" or "p99\.system"$`, func() error {
		return s.noMetricKindMatchesBannedPatterns()
	})

	// Scenario 2: embedded-mode-writes-degraded-row
	ctx.Given(`^the aggregator in embedded mode with no xrepo edges$`, func() error {
		return s.theAggregatorInEmbeddedModeWithNoXrepoEdges()
	})
	ctx.When(`^it composes "([^"]*)" for an affected scope$`, func(metricKind string) error {
		return s.itComposesMetricKindForAnAffectedScope(metricKind)
	})

	// Shared Then steps (used by scenarios 2 and 3)
	ctx.Then(`^a metric_sample row is written with metric_kind "([^"]*)" and pack "([^"]*)" and degraded (true|false) and degraded_reason "([^"]*)"$`,
		func(metricKind, pack, degraded, degradedReason string) error {
			return s.aMetricSampleRowIsWrittenWith(metricKind, pack, degraded, degradedReason)
		})
	ctx.Then(`^the degraded counter labelled reason "([^"]*)" increments$`, func(reason string) error {
		return s.theDegradedCounterLabelledReasonIncrements(reason)
	})

	// Scenario 3: samples-pending-writes-degraded-row
	ctx.Given(`^missing foundation "([^"]*)" samples for a scope at a given SHA$`, func(foundationKind string) error {
		return s.missingFoundationSamplesForScopeAtSHA(foundationKind)
	})
	ctx.When(`^the aggregator composes "([^"]*)" at that SHA$`, func(metricKind string) error {
		return s.theAggregatorComposesMetricKindAtThatSHA(metricKind)
	})
	ctx.Then(`^the value may be NULL$`, func() error {
		return s.theValueMayBeNULL()
	})
}