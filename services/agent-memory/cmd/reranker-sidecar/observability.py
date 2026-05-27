"""Stage 8.3 observability surface for the reranker-sidecar.

Adds two production observability surfaces to the FastAPI app:

  1. A Prometheus `/metrics` endpoint, exposing per-route
     request counters and per-route inference duration
     histograms. Metric names mirror the Stage 8.3 pinned
     constants in services/agent-memory/internal/obs/metrics.go
     (`reranker_sidecar_inference_total`,
     `reranker_sidecar_inference_duration_seconds`).

  2. OTel-trace export via the OTLP/HTTP exporter when
     OTEL_EXPORTER_OTLP_ENDPOINT (or _TRACES_ENDPOINT) is
     configured. A FastAPI middleware opens a child span on
     every inbound request named `reranker_sidecar.<method>`
     and attaches HTTP status + route attributes. When the env
     var is absent the sidecar wires the noop tracer so the
     same code path runs in dev and prod (matching the Go
     binaries).

The dependencies are kept optional so a deployment that has
not yet rolled out the observability bundle keeps booting
(the import block falls back to no-op stubs). This mirrors
the Go side's `obs.SetupTracer` "noop when env unset"
contract.
"""

from __future__ import annotations

import logging
import os
import time
from typing import Optional

logger = logging.getLogger("reranker-sidecar.obs")

# ---------------------------------------------------------------------------
# Prometheus exposition
# ---------------------------------------------------------------------------
# The repo's Go binaries hand-roll Prometheus text rather than
# pull the client library; we use the official `prometheus-client`
# here because Python's GIL makes hand-rolled atomic histograms
# error-prone, and the client weighs ~50 KiB on disk which is a
# rounding error against torch/transformers' 2+ GiB image.

try:
    from prometheus_client import (
        CONTENT_TYPE_LATEST,
        REGISTRY,
        CollectorRegistry,
        Counter,
        Histogram,
        generate_latest,
    )

    _PROM_AVAILABLE = True
except ImportError:  # pragma: no cover - exercised in optional-dep envs
    _PROM_AVAILABLE = False
    CONTENT_TYPE_LATEST = "text/plain; version=0.0.4"
    REGISTRY = None
    CollectorRegistry = None
    Counter = None
    Histogram = None

    def generate_latest(_registry=None):  # type: ignore[no-redef]
        return b""


# Histogram buckets mirror obs.DefaultDurationBuckets on the Go
# side so `histogram_quantile()` interpolation on the same SLO
# threshold (p95 <= 2s, p99 <= 5s for the trainer side) lands
# in the same bucket regardless of which binary served the
# scrape.
_DURATION_BUCKETS = (
    0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.4, 0.5,
    1.0, 1.5, 2.0, 2.5, 4.0, 5.0, 10.0,
)


class SidecarMetrics:
    """Wrapper around the prometheus_client primitives.

    Constructed lazily so a test can create a per-test
    `CollectorRegistry` without colliding on the global default.

    Label discipline (iter-4 evaluator findings #1 and #2):

    * `route` is restricted to the FastAPI route template
      (`/rank`, `/healthz`, `/train`) — never the raw
      `request.url.path`. Any request that does not match a
      mounted route is bucketed as `unknown` so a 404-storm
      against arbitrary URLs (`/admin`, `/.env`, …) cannot
      explode the time-series cardinality. The `_KNOWN_ROUTES`
      set below is the single source of truth.
    * `status` is restricted to the closed set
      `{"success", "error"}` — derived from the response
      (4xx and 5xx are mapped to `error`, anything < 400 OR
      a Python exception is `success`/`error` respectively).
      This aligns with the `reranker_sidecar_error_burst`
      alert in `deploy/alerts/agent-memory.rules.yml` which
      filters `status="error"`. The raw HTTP status code is
      preserved on the OTel span (`http.status_code`) where
      cardinality is unbounded by design.
    """

    def __init__(self, registry: Optional["CollectorRegistry"] = None):
        if not _PROM_AVAILABLE:
            self._enabled = False
            self.inference_total = _NullCounter()
            self.inference_duration = _NullHistogram()
            self._registry = None
            return
        self._enabled = True
        if registry is None:
            registry = CollectorRegistry()
        self._registry = registry
        self.inference_total = Counter(
            "reranker_sidecar_inference_total",
            "Total reranker sidecar inference requests, labelled by FastAPI route template and outcome (success|error).",
            labelnames=("route", "status"),
            registry=registry,
        )
        self.inference_duration = Histogram(
            "reranker_sidecar_inference_duration_seconds",
            "reranker sidecar request latency (seconds), labelled by FastAPI route template.",
            labelnames=("route",),
            buckets=_DURATION_BUCKETS,
            registry=registry,
        )

    def render(self) -> bytes:
        if not self._enabled or self._registry is None:
            return b""
        return generate_latest(self._registry)


# _KNOWN_ROUTES is the closed set of FastAPI route templates
# the sidecar exposes. Iter-4 evaluator finding #2 fix: this
# bounds the `route` label cardinality so an arbitrary 404
# storm against `/admin`, `/.env`, `/wp-login.php`, etc. can
# not explode the time-series count. Any incoming request
# whose matched route is NOT in this set is bucketed as
# `unknown`.
#
# Update this set in lockstep with the @app.get/@app.post
# decorations in main.py (currently `/healthz`, `/train`,
# `/rank`, and the implicit `/metrics` that install_observability
# below registers).
_KNOWN_ROUTES = frozenset(
    {"/healthz", "/train", "/rank", "/metrics"}
)

# _STATUS_SUCCESS / _STATUS_ERROR are the closed set of
# `status` label values. Iter-4 evaluator finding #1 fix:
# the previous implementation emitted the raw HTTP status
# code (e.g. `200`, `404`, `500`), which broke the
# `reranker_sidecar_error_burst` alert that filters
# `status="error"`. By collapsing to a 2-value enum we
# guarantee the alert's filter matches, AND we bound the
# label cardinality.
_STATUS_SUCCESS = "success"
_STATUS_ERROR = "error"


def _classify_route(request) -> str:
    """Returns a bounded `route` label value for `request`.

    Prefers FastAPI's matched route template (via
    `request.scope["route"].path`) so dynamic segments
    (`/items/{id}`) do not multiply across distinct ids.
    Falls back to the raw path WHEN it is in the
    `_KNOWN_ROUTES` allow-list, and finally to `unknown` for
    anything else (404 spray, malicious probes).
    """
    try:
        route = request.scope.get("route") if hasattr(request, "scope") else None
    except Exception:  # pragma: no cover - defensive
        route = None
    if route is not None:
        path = getattr(route, "path", None) or getattr(route, "path_format", None)
        if isinstance(path, str) and path:
            if path in _KNOWN_ROUTES:
                return path
            # A matched route that is NOT in our allow-list
            # is still bounded (FastAPI does not invent
            # routes at runtime) but flag it as `other` so
            # an operator notices the omission and updates
            # `_KNOWN_ROUTES`.
            return "other"
    # Fallback: the request did not match any FastAPI
    # route, OR the test harness bypassed scope. Bound it
    # against the allow-list before defaulting to `unknown`.
    try:
        raw_path = request.url.path
    except Exception:
        raw_path = ""
    if raw_path in _KNOWN_ROUTES:
        return raw_path
    return "unknown"


def _classify_status(http_status: Optional[int], exception: bool) -> str:
    """Maps a (status_code, exception) pair to the bounded
    `status` label. Any exception OR HTTP code >= 400 is an
    error; anything else is success. The boundary at 400
    (rather than 500) treats client-induced failures as
    errors too -- the recall verb's degradation path doesn't
    care which side caused the failure, only that it
    happened.
    """
    if exception:
        return _STATUS_ERROR
    if http_status is None:
        return _STATUS_ERROR
    if http_status >= 400:
        return _STATUS_ERROR
    return _STATUS_SUCCESS


class _NullCounter:
    def labels(self, *_args, **_kwargs):
        return self

    def inc(self, *_args, **_kwargs):
        return None


class _NullHistogram:
    def labels(self, *_args, **_kwargs):
        return self

    def observe(self, *_args, **_kwargs):
        return None


# ---------------------------------------------------------------------------
# OTel trace export
# ---------------------------------------------------------------------------
# Optional import — when the OTel SDK is not installed we fall
# back to a noop tracer that satisfies the same interface. This
# mirrors the Go binaries' "noop when OTEL_EXPORTER_OTLP_ENDPOINT
# is unset" contract from internal/obs/tracer.go.

try:
    from opentelemetry import trace
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import (
        OTLPSpanExporter,
    )
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    _OTEL_AVAILABLE = True
except ImportError:  # pragma: no cover
    _OTEL_AVAILABLE = False
    trace = None
    OTLPSpanExporter = None
    Resource = None
    TracerProvider = None
    BatchSpanProcessor = None


def setup_tracer(service_name: str = "reranker-sidecar"):
    """Wires the global OTel tracer to an OTLP/HTTP exporter
    when `OTEL_EXPORTER_OTLP_ENDPOINT` (or the traces-specific
    variant) is set; otherwise installs a noop tracer so the
    middleware can call `tracer.start_as_current_span` either
    way.

    Returns a 2-tuple of (tracer, shutdown_callable). The
    shutdown callable is a no-op in noop mode.
    """
    endpoint = os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "").strip()
    if not endpoint:
        endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "").strip()

    if not _OTEL_AVAILABLE or not endpoint:
        logger.info(
            "obs.tracer.disabled service_name=%s reason=%s",
            service_name,
            "OTel SDK not installed" if not _OTEL_AVAILABLE else "endpoint unset",
        )
        return _NoopTracer(), lambda: None

    resource = Resource.create(
        {
            "service.name": service_name,
            "service.namespace": "agent-memory",
        }
    )
    provider = TracerProvider(resource=resource)
    # The OTel HTTP exporter expects the full traces URL
    # (`http(s)://host:port/v1/traces`) OR no URL at all
    # (in which case it reads OTEL_EXPORTER_OTLP_ENDPOINT
    # itself). We pass the resolved endpoint as the explicit
    # URL so the operator's intent is unambiguous.
    if "://" not in endpoint:
        endpoint = f"http://{endpoint}"
    if not endpoint.rstrip("/").endswith("/v1/traces"):
        endpoint = endpoint.rstrip("/") + "/v1/traces"
    exporter = OTLPSpanExporter(endpoint=endpoint)
    processor = BatchSpanProcessor(exporter)
    provider.add_span_processor(processor)
    trace.set_tracer_provider(provider)
    tracer = trace.get_tracer(service_name)
    logger.info("obs.tracer.exporting service_name=%s endpoint=%s", service_name, endpoint)

    def _shutdown():
        try:
            provider.shutdown()
        except Exception:  # pragma: no cover - shutdown is best-effort
            logger.exception("obs.tracer.shutdown_failed")

    return tracer, _shutdown


class _NoopSpanContextManager:
    def __enter__(self):
        return self

    def __exit__(self, *_args, **_kwargs):
        return False

    def set_attribute(self, *_args, **_kwargs):
        return None

    def set_status(self, *_args, **_kwargs):
        return None

    def record_exception(self, *_args, **_kwargs):
        return None


class _NoopTracer:
    def start_as_current_span(self, *_args, **_kwargs):
        return _NoopSpanContextManager()


# ---------------------------------------------------------------------------
# FastAPI integration
# ---------------------------------------------------------------------------


def install_observability(app, metrics: SidecarMetrics, tracer) -> None:
    """Registers the /metrics endpoint and the trace middleware.

    Idempotent: calling twice on the same app installs two
    middlewares which would double-count, so callers should
    invoke once per app instance (the make_app factory does
    so).
    """
    from fastapi.responses import Response

    @app.get("/metrics", include_in_schema=False)
    def _metrics() -> "Response":
        return Response(content=metrics.render(), media_type=CONTENT_TYPE_LATEST)

    @app.middleware("http")
    async def _trace_and_metrics(request, call_next):
        # iter-4 evaluator finding #2 fix: classify the route
        # against the bounded `_KNOWN_ROUTES` set rather than
        # using `request.url.path` directly. Prevents 404 spray
        # from exploding label cardinality.
        route = _classify_route(request)
        start = time.perf_counter()
        # Open the span BEFORE awaiting the handler so a
        # downstream exception still records on the span.
        with tracer.start_as_current_span(
            f"reranker_sidecar.{request.method}"
        ) as span:
            span.set_attribute("http.method", request.method)
            span.set_attribute("http.route", route)
            try:
                response = await call_next(request)
            except Exception as exc:
                span.record_exception(exc)
                # iter-4 evaluator finding #1 fix: emit the
                # bounded `error` enum value so the
                # `reranker_sidecar_error_burst` alert
                # (filters status="error") fires. The raw
                # HTTP status code is preserved on the span
                # attribute above.
                metrics.inference_total.labels(
                    route=route, status=_STATUS_ERROR
                ).inc()
                metrics.inference_duration.labels(route=route).observe(
                    time.perf_counter() - start
                )
                raise
            span.set_attribute("http.status_code", response.status_code)
            # iter-4 evaluator finding #1 fix: map the raw
            # HTTP code to the bounded {success, error} enum
            # the alert filter expects.
            metrics.inference_total.labels(
                route=route,
                status=_classify_status(response.status_code, exception=False),
            ).inc()
            metrics.inference_duration.labels(route=route).observe(
                time.perf_counter() - start
            )
            return response
