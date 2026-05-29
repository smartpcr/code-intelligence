package management

// Stage 10.2 -- Reader-level wiring tests for
// `mgmt.read.insights.aged_mutes`. Covers the
// [WithAgedMutes] option, the default + override threshold
// paths, the [ErrBackendUnavailable] unwired-substrate
// branch, and the response envelope's [ReadMode] tag.
//
// The pure projection logic is tested in
// `internal/management/insights/aged_mutes_test.go`. These
// tests only assert the wiring: Reader -> insights.AgedMutes
// -> response envelope.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/management/insights"
)

// fixedAgedMuteClock pins a deterministic [insights.Clock] for
// the Reader-level wiring tests so the threshold paths are
// reproducible across CI runs.
type fixedAgedMuteClock struct{ t time.Time }

func (f fixedAgedMuteClock) Now() time.Time { return f.t }

// fakeOverrideReader is a slim in-memory
// [insights.OverrideReader]. Production wiring adapts a
// `steward.Store` to this interface in the composition root;
// the Reader-level tests use the in-memory shape to keep the
// suite free of a SQL dependency.
type fakeOverrideReader struct {
	rows []insights.OverrideRecord
}

func (f *fakeOverrideReader) ListAllOverrides(ctx context.Context) ([]insights.OverrideRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]insights.OverrideRecord, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// newReaderWithAgedMutes builds a Reader wired with the
// supplied fake override reader and clock so each test reads
// as data, not boilerplate.
func newReaderWithAgedMutes(backend *fakeOverrideReader, now time.Time) *Reader {
	am := insights.NewAgedMutes(backend, fixedAgedMuteClock{now})
	return NewReader(nil, WithAgedMutes(am))
}

// TestReader_ReadAgedMutes_BackendUnavailable pins the
// "verb mounted, substrate unwired -> ErrBackendUnavailable"
// contract -- mirrors [TestReader_ReadCrossRepo_BackendUnavailable].
func TestReader_ReadAgedMutes_BackendUnavailable(t *testing.T) {
	t.Parallel()
	r := NewReader(nil)
	_, err := r.ReadAgedMutes(context.Background(), nil)
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("err=%v, want ErrBackendUnavailable", err)
	}
}

// TestReader_ReadAgedMutes_DefaultThresholdSurfacesAgedMute
// pins the canonical happy path AND scenario
// `aged-mute-listed-not-enforced`: a 100-day mute with no
// threshold argument is reported under the default 90-day
// threshold.
func TestReader_ReadAgedMutes_DefaultThresholdSurfacesAgedMute(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	backend := &fakeOverrideReader{
		rows: []insights.OverrideRecord{
			{
				OverrideID: "11111111-1111-1111-1111-111111111111",
				RuleID:     "solid.srp.lcom4_high",
				Scope: insights.OverrideScope{
					RepoID:             "00000000-0000-0000-0000-000000000001",
					ScopeKind:          "class",
					ScopeSignatureGlob: "com.example.legacy.OrderProcessor",
				},
				Mute:      true,
				Reason:    "noisy in v1; revisit Q3",
				ActorID:   "operator@example.com",
				CreatedAt: now.Add(-100 * 24 * time.Hour),
			},
		},
	}
	r := newReaderWithAgedMutes(backend, now)

	resp, err := r.ReadAgedMutes(context.Background(), nil)
	if err != nil {
		t.Fatalf("ReadAgedMutes: %v", err)
	}
	if resp.Mode != ReadModeLatestDashboard {
		t.Errorf("Mode=%q, want %q", resp.Mode, ReadModeLatestDashboard)
	}
	if resp.ThresholdDays != insights.AgedMuteDefaultThresholdDays {
		t.Errorf("ThresholdDays=%d, want %d (default)", resp.ThresholdDays, insights.AgedMuteDefaultThresholdDays)
	}
	if len(resp.AgedMutes) != 1 {
		t.Fatalf("len(AgedMutes)=%d, want 1", len(resp.AgedMutes))
	}
	if resp.AgedMutes[0].AgeDays != 100 {
		t.Errorf("AgeDays=%d, want 100", resp.AgedMutes[0].AgeDays)
	}
	if resp.AgedMutes[0].RuleID != "solid.srp.lcom4_high" {
		t.Errorf("RuleID=%q, want %q", resp.AgedMutes[0].RuleID, "solid.srp.lcom4_high")
	}
}

// TestReader_ReadAgedMutes_UnmuteRemovesPair pins scenario
// `unmute-removes-from-report` at the Reader wire boundary:
// after appending an `override(mute=false)` for the same
// (rule_id, scope) tuple, the next read returns an empty
// slice and an empty (non-nil) JSON array on the wire.
func TestReader_ReadAgedMutes_UnmuteRemovesPair(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	muteScope := insights.OverrideScope{
		RepoID:             "00000000-0000-0000-0000-000000000001",
		ScopeKind:          "class",
		ScopeSignatureGlob: "com.example.legacy.OrderProcessor",
	}
	backend := &fakeOverrideReader{
		rows: []insights.OverrideRecord{
			{
				OverrideID: "11111111-1111-1111-1111-111111111111",
				RuleID:     "solid.srp.lcom4_high",
				Scope:      muteScope,
				Mute:       true,
				Reason:     "noisy in v1",
				ActorID:    "operator@example.com",
				CreatedAt:  now.Add(-100 * 24 * time.Hour),
			},
		},
	}
	r := newReaderWithAgedMutes(backend, now)

	firstResp, err := r.ReadAgedMutes(context.Background(), nil)
	if err != nil {
		t.Fatalf("first ReadAgedMutes: %v", err)
	}
	if len(firstResp.AgedMutes) != 1 {
		t.Fatalf("first len(AgedMutes)=%d, want 1", len(firstResp.AgedMutes))
	}

	// Operator appends an override(mute=false) row for the
	// SAME (rule_id, scope) -- simulated here by appending
	// to the backend's slice (the production wiring would
	// route through `mgmt.override`).
	backend.rows = append(backend.rows, insights.OverrideRecord{
		OverrideID: "22222222-2222-2222-2222-222222222222",
		RuleID:     "solid.srp.lcom4_high",
		Scope:      muteScope,
		Mute:       false,
		Reason:     "",
		ActorID:    "operator@example.com",
		CreatedAt:  now.Add(-1 * time.Hour),
	})

	secondResp, err := r.ReadAgedMutes(context.Background(), nil)
	if err != nil {
		t.Fatalf("second ReadAgedMutes: %v", err)
	}
	if len(secondResp.AgedMutes) != 0 {
		t.Fatalf("second len(AgedMutes)=%d, want 0 (unmute should drop the pair)", len(secondResp.AgedMutes))
	}
	if secondResp.AgedMutes == nil {
		t.Error("AgedMutes=nil; want a non-nil empty slice (JSON `[]`, not `null`)")
	}
}

// TestReader_ReadAgedMutes_OverrideThresholdHonored pins the
// caller-supplied `threshold_days` plumb-through.
// 30-day mute is FRESH under default 90d, AGED under 14d.
func TestReader_ReadAgedMutes_OverrideThresholdHonored(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	backend := &fakeOverrideReader{
		rows: []insights.OverrideRecord{
			{
				OverrideID: "11111111-1111-1111-1111-111111111111",
				RuleID:     "r", Scope: insights.OverrideScope{RepoID: "repo", ScopeKind: "class", ScopeSignatureGlob: "Foo"},
				Mute: true, Reason: "x", ActorID: "op",
				CreatedAt: now.Add(-30 * 24 * time.Hour),
			},
		},
	}
	r := newReaderWithAgedMutes(backend, now)

	defaultResp, err := r.ReadAgedMutes(context.Background(), nil)
	if err != nil {
		t.Fatalf("default ReadAgedMutes: %v", err)
	}
	if len(defaultResp.AgedMutes) != 0 {
		t.Errorf("default len=%d, want 0 (30d < 90d)", len(defaultResp.AgedMutes))
	}

	t14 := 14
	twoWeekResp, err := r.ReadAgedMutes(context.Background(), &t14)
	if err != nil {
		t.Fatalf("14d ReadAgedMutes: %v", err)
	}
	if len(twoWeekResp.AgedMutes) != 1 {
		t.Errorf("14d len=%d, want 1", len(twoWeekResp.AgedMutes))
	}
	if twoWeekResp.ThresholdDays != 14 {
		t.Errorf("ThresholdDays=%d, want 14 (echoed)", twoWeekResp.ThresholdDays)
	}
}

// TestReader_ReadAgedMutes_NonPositiveThresholdFallsBackToDefault
// guards against `?threshold_days=0` (or a negative value)
// silently surfacing every mute.
func TestReader_ReadAgedMutes_NonPositiveThresholdFallsBackToDefault(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	backend := &fakeOverrideReader{
		rows: []insights.OverrideRecord{
			{
				OverrideID: "11111111-1111-1111-1111-111111111111",
				RuleID:     "r", Scope: insights.OverrideScope{RepoID: "repo", ScopeKind: "class", ScopeSignatureGlob: "Foo"},
				Mute: true, Reason: "x", ActorID: "op",
				CreatedAt: now.Add(-30 * 24 * time.Hour),
			},
		},
	}
	r := newReaderWithAgedMutes(backend, now)

	for _, v := range []int{0, -1, -90} {
		v := v
		t.Run("threshold_days="+itoaForTest(v), func(t *testing.T) {
			resp, err := r.ReadAgedMutes(context.Background(), &v)
			if err != nil {
				t.Fatalf("ReadAgedMutes(%d): %v", v, err)
			}
			if len(resp.AgedMutes) != 0 {
				t.Errorf("len=%d, want 0 (fallback to default 90d; 30d mute is fresh)", len(resp.AgedMutes))
			}
			if resp.ThresholdDays != insights.AgedMuteDefaultThresholdDays {
				t.Errorf("ThresholdDays=%d, want %d (fallback)", resp.ThresholdDays, insights.AgedMuteDefaultThresholdDays)
			}
		})
	}
}

// itoaForTest avoids importing strconv just for one test
// subtest label.
func itoaForTest(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestReader_ReadAgedMutes_ReaderErrorPropagates asserts the
// backend scan error is returned by the Reader wrapper. The
// HTTP layer maps any non-[ErrBackendUnavailable] error to a
// 500.
func TestReader_ReadAgedMutes_ReaderErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("backend exploded")
	am := &insights.AgedMutes{
		Reader: listAllOverridesFuncForTest(func(ctx context.Context) ([]insights.OverrideRecord, error) {
			return nil, sentinel
		}),
		Clock:     fixedAgedMuteClock{time.Now()},
		Threshold: 90 * 24 * time.Hour,
	}
	r := NewReader(nil, WithAgedMutes(am))

	_, err := r.ReadAgedMutes(context.Background(), nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("err=%v, want sentinel %v", err, sentinel)
	}
}

// listAllOverridesFuncForTest adapts a closure into an
// [insights.OverrideReader] without declaring a new struct
// per failure-mode test.
type listAllOverridesFuncForTest func(context.Context) ([]insights.OverrideRecord, error)

func (f listAllOverridesFuncForTest) ListAllOverrides(ctx context.Context) ([]insights.OverrideRecord, error) {
	return f(ctx)
}

// TestWithAgedMutes_NilOptionPermitted pins the
// "nil-as-no-op" option contract -- mirrors
// [WithMetricsBackend] / [WithInsightsFreshness]. A nil-valued
// option does NOT panic; the verb later returns
// [ErrBackendUnavailable] on use.
func TestWithAgedMutes_NilOptionPermitted(t *testing.T) {
	t.Parallel()
	r := NewReader(nil, WithAgedMutes(nil))
	_, err := r.ReadAgedMutes(context.Background(), nil)
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("err=%v, want ErrBackendUnavailable", err)
	}
}

// TestReader_ReadAgedMutes_NilStoreAdapterMapsToBackendUnavailable
// pins iter 2 evaluator item 3: when the composition root
// wires an [OverrideReaderFromStore] with a nil
// [steward.Store] (a real scaffold-mode bring-up bug), the
// Reader MUST map the adapter sentinel
// [ErrAgedMuteOverrideStoreUnavailable] to
// [ErrBackendUnavailable] so the HTTP layer emits 503 and
// the operator dashboard renders "unavailable" instead of
// the internal scaffold-mode error string.
func TestReader_ReadAgedMutes_NilStoreAdapterMapsToBackendUnavailable(t *testing.T) {
	t.Parallel()
	// Production adapter wired with a nil steward.Store --
	// the exact composition-root bug evaluator item 3
	// flagged.
	nilStoreAdapter := &OverrideReaderFromStore{Store: nil}
	am := insights.NewAgedMutes(nilStoreAdapter, nil)
	r := NewReader(nil, WithAgedMutes(am))

	_, err := r.ReadAgedMutes(context.Background(), nil)
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("err=%v, want ErrBackendUnavailable (per nil-store -> 503 convention)", err)
	}
	// Contract pin: the Reader returns the BARE
	// [ErrBackendUnavailable] sentinel -- it does NOT wrap
	// the underlying adapter sentinel into the chain. The
	// HTTP layer only branches on `errors.Is(err,
	// ErrBackendUnavailable)` to emit 503; an additional
	// wrap would force every HTTP-layer caller to also
	// unwrap the inner sentinel just to render the 503
	// body. If the contract changes to preserve the
	// underlying sentinel, assert
	// `errors.Is(err, ErrAgedMuteOverrideStoreUnavailable)`
	// here AND update [Reader.ReadAgedMutes] to use
	// `fmt.Errorf("%w: %w", ...)`.
	if errors.Is(err, ErrAgedMuteOverrideStoreUnavailable) {
		t.Errorf("err chain leaked ErrAgedMuteOverrideStoreUnavailable to the HTTP-facing surface: %v", err)
	}
}

// TestReader_ReadAgedMutes_NilOverrideReaderMapsToBackendUnavailable
// pins iter 2 evaluator item 3 for the OTHER nil-wiring
// flavour: a composition root that passed `nil` as the
// [insights.OverrideReader] argument to
// [insights.NewAgedMutes]. The projection returns
// [insights.ErrAgedMuteReaderUnavailable]; the Reader MUST
// remap that to [ErrBackendUnavailable] for the same 503
// HTTP contract.
func TestReader_ReadAgedMutes_NilOverrideReaderMapsToBackendUnavailable(t *testing.T) {
	t.Parallel()
	am := insights.NewAgedMutes(nil, nil)
	r := NewReader(nil, WithAgedMutes(am))

	_, err := r.ReadAgedMutes(context.Background(), nil)
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("err=%v, want ErrBackendUnavailable", err)
	}
}
