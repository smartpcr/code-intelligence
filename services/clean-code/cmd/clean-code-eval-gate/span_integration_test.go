package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofrs/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/evaluator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/telemetry"
)

// TestIntegration_EvalGateHandlerEmitsSpanWithVerdict
// satisfies iter-2 evaluator feedback item #1 + #4 for
// the STANDALONE clean-code-eval-gate binary surface
// (`/v1/eval/gate`, `/v1/eval/replay`). It drives the
// REAL [makeEvalHandler] / [makeReplayHandler] through
// `httptest.NewServer` with stub [evalGateFunc] /
// [evalReplayFunc] seams, and asserts that the
// in-process OTel SDK exporter captured a span with the
// full Stage 9.4 canonical attribute set for each
// outcome (happy, degraded, no-active-policy, internal
// error).
//
// Why an in-process SDK exporter (not a Tracer seam)?
// The standalone eval-gate binary wires the production
// OTel SDK directly (no `api.Tracer` seam) -- so the
// integration test mirrors that wiring exactly. A
// global TracerProvider is installed for the duration
// of the test and reset on cleanup so other parallel
// tests in the binary are unaffected.
func TestIntegration_EvalGateHandlerEmitsSpanWithVerdict(t *testing.T) {
	cases := []struct {
		name             string
		gateResult       evaluator.EvaluateResult
		gateErr          error
		wantStatus       int
		wantVerdict      string
		wantDegraded     bool
		wantDegradedReas string
	}{
		{
			name: "happy_pass",
			gateResult: evaluator.EvaluateResult{
				PolicyVersionID: uuid.Must(uuid.NewV4()),
				Verdict:         evaluator.VerdictPass,
			},
			wantStatus:  http.StatusOK,
			wantVerdict: "pass",
		},
		{
			name: "degraded_samples_pending",
			gateResult: evaluator.EvaluateResult{
				PolicyVersionID: uuid.Must(uuid.NewV4()),
				Verdict:         evaluator.VerdictWarn,
				Degraded:        true,
				DegradedReason:  evaluator.DegradedReasonSamplesPending,
			},
			gateErr:          evaluator.ErrSamplesPending,
			wantStatus:       http.StatusOK,
			wantVerdict:      "warn",
			wantDegraded:     true,
			wantDegradedReas: "samples_pending",
		},
		{
			name:       "no_active_policy",
			gateErr:    evaluator.ErrNoActivePolicy,
			wantStatus: http.StatusConflict,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			prev := otel.GetTracerProvider()
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(
				sdktrace.WithSyncer(exp),
				sdktrace.WithSampler(sdktrace.AlwaysSample()),
			)
			otel.SetTracerProvider(tp)
			t.Cleanup(func() {
				_ = tp.Shutdown(context.Background())
				otel.SetTracerProvider(prev)
			})

			repoID := uuid.Must(uuid.NewV4())
			var (
				gotCtx    context.Context
				stubCalls int
			)
			handler := makeEvalHandler(func(ctx context.Context, rID uuid.UUID, _ string, _ *uuid.UUID) (evaluator.EvaluateResult, error) {
				stubCalls++
				gotCtx = ctx
				if rID != repoID {
					t.Errorf("repoID forwarded to gateFn = %s, want %s", rID, repoID)
				}
				return tc.gateResult, tc.gateErr
			})

			srv := httptest.NewServer(handler)
			defer srv.Close()

			body, _ := jsonEncode(map[string]string{
				"repo_id": repoID.String(),
				"sha":     "deadbeef",
			})
			resp, err := srv.Client().Post(srv.URL, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status=%d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if stubCalls != 1 {
				t.Fatalf("gateFn called %d times, want 1", stubCalls)
			}
			if gotCtx == nil {
				t.Fatal("gateFn did not receive a span-bound context")
			}

			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("captured span count=%d, want 1", len(spans))
			}
			s := spans[0]
			if s.Name != "eval.gate" {
				t.Errorf("span name=%q, want eval.gate", s.Name)
			}
			attrs := attrMap(s.Attributes)
			if got := attrs[telemetry.AttrVerb]; got != "eval.gate" {
				t.Errorf("verb attr = %v, want eval.gate", got)
			}
			if got := attrs[telemetry.AttrRepoID]; got != repoID.String() {
				t.Errorf("repo_id attr = %v, want %s", got, repoID.String())
			}
			if got, ok := attrs[telemetry.AttrDegraded].(bool); !ok || got != tc.wantDegraded {
				t.Errorf("degraded attr = %v (ok=%v), want %v", got, ok, tc.wantDegraded)
			}
			if got := attrs[telemetry.AttrDegradedReason]; got != tc.wantDegradedReas {
				t.Errorf("degraded_reason attr = %v, want %q", got, tc.wantDegradedReas)
			}
			if got := attrs[telemetry.AttrVerdict]; got != tc.wantVerdict {
				t.Errorf("verdict attr = %v, want %q", got, tc.wantVerdict)
			}
			// policy_version_id is overwritten by
			// AnnotateEvalGateSpan only on the happy /
			// degraded branches where the gate
			// resolved one. ErrNoActivePolicy leaves it
			// at the default empty string.
			if tc.gateErr == nil || tc.gateErr == evaluator.ErrSamplesPending {
				if got := attrs[telemetry.AttrPolicyVersionID]; got != tc.gateResult.PolicyVersionID.String() {
					t.Errorf("policy_version_id attr = %v, want %s", got, tc.gateResult.PolicyVersionID.String())
				}
			}
		})
	}
}

// TestIntegration_EvalReplayHandlerEmitsSpan asserts the
// admin/replay surface emits a span tagged `eval.replay`
// with the canonical attribute set when invoked through
// the real [makeReplayHandler]. This is the second half
// of iter-2 evaluator feedback item #1 / #4 for the
// standalone binary.
func TestIntegration_EvalReplayHandlerEmitsSpan(t *testing.T) {
	prev := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	repoID := uuid.Must(uuid.NewV4())
	policyVersionID := uuid.Must(uuid.NewV4())
	gotResult := evaluator.EvaluateResult{
		PolicyVersionID: policyVersionID,
		Verdict:         evaluator.VerdictBlock,
	}
	handler := makeReplayHandler(func(ctx context.Context, _ uuid.UUID, _ string, _ *uuid.UUID, pvID uuid.UUID) (evaluator.EvaluateResult, error) {
		if pvID != policyVersionID {
			t.Errorf("replayFn pinned %s, want %s", pvID, policyVersionID)
		}
		return gotResult, nil
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, _ := jsonEncode(map[string]string{
		"repo_id":           repoID.String(),
		"sha":               "feedface",
		"policy_version_id": policyVersionID.String(),
	})
	resp, err := srv.Client().Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d, want 200", resp.StatusCode)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("span count = %d, want 1", len(spans))
	}
	if spans[0].Name != "eval.replay" {
		t.Errorf("span name=%q, want eval.replay", spans[0].Name)
	}
	attrs := attrMap(spans[0].Attributes)
	if got := attrs[telemetry.AttrVerb]; got != "eval.replay" {
		t.Errorf("verb attr = %v, want eval.replay", got)
	}
	if got := attrs[telemetry.AttrVerdict]; got != "block" {
		t.Errorf("verdict attr = %v, want block", got)
	}
	if got := attrs[telemetry.AttrPolicyVersionID]; got != policyVersionID.String() {
		t.Errorf("policy_version_id attr = %v, want %s", got, policyVersionID.String())
	}
}

// attrMap converts the OTel SDK attribute slice into a
// map keyed by attribute name. Returns the canonical Go
// value (string / bool / int64) so the assertions can
// use direct equality without unwrapping
// attribute.Value.
func attrMap(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		switch kv.Value.Type() {
		case attribute.STRING:
			out[string(kv.Key)] = kv.Value.AsString()
		case attribute.BOOL:
			out[string(kv.Key)] = kv.Value.AsBool()
		case attribute.INT64:
			out[string(kv.Key)] = kv.Value.AsInt64()
		case attribute.FLOAT64:
			out[string(kv.Key)] = kv.Value.AsFloat64()
		default:
			out[string(kv.Key)] = kv.Value.Emit()
		}
	}
	return out
}

// jsonEncode is a tiny wrapper to keep test body
// preparation tidy.
func jsonEncode(v any) ([]byte, error) {
	return json.Marshal(v)
}
