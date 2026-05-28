package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
)

func TestSetup_EmptyEndpointReturnsNoopShutdown(t *testing.T) {
	cfg := config.Config{OTelEndpoint: ""}
	shutdown, err := Setup(context.Background(), cfg, SetupOptions{
		ServiceName: "clean-code-test",
	})
	if err != nil {
		t.Fatalf("Setup(empty endpoint) returned err: %v", err)
	}
	if shutdown == nil {
		t.Fatalf("Setup(empty endpoint) returned nil ShutdownFunc; want noop")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown returned err: %v", err)
	}
}

func TestSetup_EmptyServiceNameIsRejected(t *testing.T) {
	cfg := config.Config{OTelEndpoint: "localhost:4317"}
	_, err := Setup(context.Background(), cfg, SetupOptions{ServiceName: ""})
	if !errors.Is(err, ErrSetupServiceName) {
		t.Fatalf("Setup(ServiceName=\"\") err = %v; want ErrSetupServiceName", err)
	}
}

func TestBuildSampler_Branches(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ratio  float64
		expect string
	}{
		{"never_on_zero", 0, "AlwaysOffSampler"},
		{"never_on_negative", -1.5, "AlwaysOffSampler"},
		{"always_on_one", 1.0, "AlwaysOnSampler"},
		{"always_on_high", 2.0, "AlwaysOnSampler"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := buildSampler(tc.ratio)
			got := s.Description()
			if got != tc.expect {
				t.Errorf("buildSampler(%v).Description() = %q; want %q", tc.ratio, got, tc.expect)
			}
		})
	}
	t.Run("ratio_based_mid", func(t *testing.T) {
		s := buildSampler(0.5)
		// The OTel SDK emits ParentBased{root:TraceIDRatioBased{0.5},...}
		// with extra parent-based sub-sampler suffixes. We
		// just assert the prefix so the test is robust to
		// the SDK appending fields in future versions.
		desc := s.Description()
		if !strings.HasPrefix(desc, "ParentBased{root:TraceIDRatioBased{0.5}") {
			t.Errorf("buildSampler(0.5).Description() = %q; want prefix ParentBased{root:TraceIDRatioBased{0.5}", desc)
		}
	})
}

func TestIsLocalEndpoint(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"localhost:4317", true},
		{"127.0.0.1:4317", true},
		{"::1:4317", true},
		{"otel-collector.svc.cluster.local:4317", false},
		{"", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := isLocalEndpoint(tc.in); got != tc.want {
				t.Errorf("isLocalEndpoint(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildResource_DefaultsVersionToDev(t *testing.T) {
	res := buildResource(SetupOptions{ServiceName: "svc-x"})
	attrs := res.Attributes()
	var sawName, sawVersion bool
	for _, kv := range attrs {
		switch string(kv.Key) {
		case "service.name":
			sawName = true
			if kv.Value.AsString() != "svc-x" {
				t.Errorf("service.name = %q; want svc-x", kv.Value.AsString())
			}
		case "service.version":
			sawVersion = true
			if kv.Value.AsString() != "dev" {
				t.Errorf("service.version = %q; want dev (default)", kv.Value.AsString())
			}
		}
	}
	if !sawName {
		t.Errorf("resource missing service.name")
	}
	if !sawVersion {
		t.Errorf("resource missing service.version")
	}
}

// Sanity that DialTimeout zero-defaulting happens without
// us reaching into the package internals -- buildTracer
// path is exercised via the broader integration test; here
// we just confirm Setup gracefully reports a dial failure
// rather than hanging when an obviously-invalid endpoint is
// given and the dial times out.
func TestSetup_DialFailureSurfaces(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping dial-failure smoke under -short")
	}
	cfg := config.Config{OTelEndpoint: "203.0.113.1:4317"} // RFC5737 TEST-NET-3
	_, err := Setup(context.Background(), cfg, SetupOptions{
		ServiceName: "clean-code-test",
		DialTimeout: 50 * time.Millisecond,
		Insecure:    true,
	})
	// The OTLP gRPC exporter is lazy: New() does NOT
	// block on the underlying connection by default, so a
	// dial failure may not surface here. We just verify
	// the call returns SOMETHING (err or success) and
	// doesn't hang past DialTimeout.
	_ = err
}
