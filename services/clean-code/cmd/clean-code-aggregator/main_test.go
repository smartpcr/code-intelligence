package main

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
)

// TestBuildAggregatorLoop_DisabledReturnsNil pins the
// operator opt-out contract: cfg.DisableAggregator=true must
// short-circuit the wiring so the binary serves /healthz only
// (matches the metric_ingestor stale-sweep opt-out pattern).
func TestBuildAggregatorLoop_DisabledReturnsNil(t *testing.T) {
	t.Parallel()
	cfg := config.Config{DisableAggregator: true}
	loop, err := buildAggregatorLoop(cfg, nil, slog.Default(), nil)
	if err != nil {
		t.Fatalf("buildAggregatorLoop err = %v; want nil", err)
	}
	if loop != nil {
		t.Errorf("buildAggregatorLoop loop = %p; want nil (operator opted out)", loop)
	}
}

// TestBuildAggregatorLoop_NilDBWhenEnabledErrors pins the
// safety guard: an enabled aggregator without a DB handle is
// a deploy-time misconfiguration that must fail fast (vs
// crashing later on the first Tick).
func TestBuildAggregatorLoop_NilDBWhenEnabledErrors(t *testing.T) {
	t.Parallel()
	cfg := config.Config{DisableAggregator: false}
	_, err := buildAggregatorLoop(cfg, nil, slog.Default(), nil)
	if err == nil {
		t.Fatal("buildAggregatorLoop err = nil; want non-nil")
	}
	if !strings.Contains(err.Error(), "no *sql.DB") {
		t.Errorf("err = %v; want substring 'no *sql.DB'", err)
	}
}

// TestBuildAggregatorLoop_WiresSystemTierPipeline pins iter-3
// evaluator item #1 + iter-5 evaluator item #1: the production
// composition root MUST wire the system-tier composer + source
// + writer via [aggregator.WithSystemTier]. The iter-5
// evaluator flagged that the iter-3 version of this test only
// asserted a non-nil [Loop] -- which would still pass if a
// regression dropped the [aggregator.WithSystemTier] option
// from [buildAggregatorLoop] (since [aggregator.NewAggregator]
// succeeds with foundation-only args). The structural fix is
// to call the public observable seam
// [aggregator.Aggregator.SystemTierWired] on the wrapped
// aggregator and assert it is true. Any regression that drops
// or comments out the [aggregator.WithSystemTier] option
// argument now fails this assertion deterministically
// regardless of whether construction itself succeeded.
func TestBuildAggregatorLoop_WiresSystemTierPipeline(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	cfg := config.Config{
		DisableAggregator: false,
		AggregatorCadence: 17 * time.Minute,
	}
	loop, err := buildAggregatorLoop(cfg, db, slog.Default(), nil)
	if err != nil {
		t.Fatalf("buildAggregatorLoop err = %v; want nil", err)
	}
	if loop == nil {
		t.Fatal("buildAggregatorLoop loop = nil; want non-nil")
	}
	agg := loop.Aggregator()
	if agg == nil {
		t.Fatal("loop.Aggregator() = nil; want non-nil so SystemTierWired() can be probed")
	}
	// Iter-5 evaluator item #1: this is the assertion that
	// actually proves [aggregator.WithSystemTier] was
	// applied. Dropping the option from
	// [buildAggregatorLoop] would leave composer/sysSource/
	// sysWriter all nil and SystemTierWired() would return
	// false even though the foundation-only construction
	// path still succeeds.
	if !agg.SystemTierWired() {
		t.Errorf("loop.Aggregator().SystemTierWired() = false; want true (regression: WithSystemTier option not applied -- production composition root would silently skip the system-tier pass on every Tick)")
	}
}

// TestBuildAggregatorLoop_PropagatesCadence pins the cadence
// wiring: a non-default cadence in cfg lands on the
// constructed loop via [aggregator.WithLoopCadence]. A
// regression that drops the cadence option would silently
// fall back to the default tick rate which is hard to
// detect in production. Asserts the observable
// [aggregator.Loop.Cadence] accessor rather than just
// non-nil so a dropped option is caught deterministically
// (same blast-shield shape as the SystemTierWired() pin).
func TestBuildAggregatorLoop_PropagatesCadence(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const want = 23 * time.Minute
	cfg := config.Config{
		DisableAggregator: false,
		AggregatorCadence: want,
	}
	loop, err := buildAggregatorLoop(cfg, db, slog.Default(), nil)
	if err != nil {
		t.Fatalf("buildAggregatorLoop err = %v; want nil", err)
	}
	if loop == nil {
		t.Fatal("buildAggregatorLoop loop = nil; want non-nil")
	}
	if got := loop.Cadence(); got != want {
		t.Errorf("loop.Cadence() = %s; want %s (regression: WithLoopCadence option not applied -- production loop would run at default 15m)", got, want)
	}
}

// TestWiringContract_AggregatorOptionRejectsNil is a
// belt-and-braces guard pinning that [aggregator.WithSystemTier]
// still panics on nil args. If a refactor relaxes the panic
// to a silent skip, [buildAggregatorLoop] could regress to
// the pre-iter-3 "no system-tier rows ever written" state
// without any visible signal. Pinning the panic shape here
// gives the operator one consolidated failure-mode test in
// the binary's own package alongside the cmd wiring tests.
func TestWiringContract_AggregatorOptionRejectsNil(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Error("expected WithSystemTier(nil, nil, nil) to panic; did not")
		}
	}()
	_ = aggregator.WithSystemTier(nil, nil, nil)
}
