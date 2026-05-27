package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Default HTTP server timeouts. Values mirror the conservative
// stdlib `http.Server` recommendation; production wiring can
// override via [ServerConfig]. The gateway terminates the
// request after these elapse so a slow downstream verb
// handler cannot keep a socket pinned indefinitely.
const (
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 90 * time.Second
)

// ServerConfig bundles the gateway's wiring. The composition
// root constructs one of these and passes it to [NewServer].
type ServerConfig struct {
	// Addr is the listen address (e.g. ":8080"). Empty
	// defers to the stdlib default (":http").
	Addr string

	// Authenticator verifies the bearer token. REQUIRED.
	Authenticator Authenticator

	// Registry holds the verb table. REQUIRED. Composition
	// root populates the registry before [Server.ListenAndServe]
	// returns. The Server holds a reference, so additional
	// Register calls AFTER serve-start are visible to the
	// handler (the registry's RWMutex makes the read side
	// safe), but the canonical pattern is register-then-serve.
	Registry *VerbRegistry

	// Tracer receives one span per request. Nil installs
	// [NoopTracer] -- the gateway still functions, span
	// emission silently drops. Production wiring SHOULD
	// pass [SlogTracer] or an OTel-backed Tracer.
	Tracer Tracer

	// Logger receives structured log entries for every
	// 4xx / 5xx response and every internal-error path.
	// Nil falls back to [slog.Default].
	Logger *slog.Logger

	// ReadHeaderTimeout / ReadTimeout / WriteTimeout /
	// IdleTimeout pin the [http.Server] timeouts. Zero
	// values fall back to the `Default*` constants above.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// BaseContext is the optional hook the stdlib
	// `http.Server.BaseContext` consumes. Nil installs a
	// background context. Composition roots typically pass
	// a cancellable parent so a shutdown signal propagates
	// into every active request.
	BaseContext func(net.Listener) context.Context
}

// Server is the gateway's HTTP server. Constructed once at
// startup, served with [Server.ListenAndServe] / [Server.Serve],
// and torn down with [Server.Shutdown].
type Server struct {
	handler  *GatewayHandler
	registry *VerbRegistry
	logger   *slog.Logger
	http     *http.Server
}

// NewServer constructs a [Server] from `cfg`. PANICS on any
// wiring error (nil Authenticator / nil Registry); failing
// loudly at startup beats silently degraded behaviour at
// runtime.
func NewServer(cfg ServerConfig) *Server {
	if cfg.Authenticator == nil {
		panic("api.NewServer: ServerConfig.Authenticator is nil")
	}
	if cfg.Registry == nil {
		panic("api.NewServer: ServerConfig.Registry is nil")
	}
	tracer := cfg.Tracer
	if tracer == nil {
		tracer = NoopTracer{}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	handler := NewGatewayHandler(cfg.Authenticator, cfg.Registry, tracer, logger)
	readHeaderTimeout := cfg.ReadHeaderTimeout
	if readHeaderTimeout == 0 {
		readHeaderTimeout = DefaultReadHeaderTimeout
	}
	readTimeout := cfg.ReadTimeout
	if readTimeout == 0 {
		readTimeout = DefaultReadTimeout
	}
	writeTimeout := cfg.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = DefaultWriteTimeout
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = DefaultIdleTimeout
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext:       cfg.BaseContext,
	}
	return &Server{
		handler:  handler,
		registry: cfg.Registry,
		logger:   logger,
		http:     srv,
	}
}

// Handler returns the underlying [GatewayHandler]. Useful for
// mounting the gateway under a parent mux (e.g. when the
// composition root composes the gateway with a separate
// `/healthz` handler).
func (s *Server) Handler() http.Handler { return s.handler }

// Registry returns the gateway's verb registry. The
// composition root calls Register on the returned registry to
// add verbs.
func (s *Server) Registry() *VerbRegistry { return s.registry }

// HTTPServer exposes the underlying stdlib server. Used by
// tests that need to drive the gateway via [httptest.Server]
// or by composition roots that want to swap the listener.
func (s *Server) HTTPServer() *http.Server { return s.http }

// ListenAndServe binds the configured Addr and serves until
// the listener returns an error other than [http.ErrServerClosed].
// Composition roots typically run this in a goroutine and
// invoke [Server.Shutdown] on a cancel signal.
func (s *Server) ListenAndServe() error {
	s.logger.Info("gateway: listening", slog.String("addr", s.http.Addr))
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("gateway: ListenAndServe: %w", err)
	}
	return nil
}

// Serve takes a pre-bound listener (used by tests that pin
// the listener address before serving).
func (s *Server) Serve(ln net.Listener) error {
	s.logger.Info("gateway: serving", slog.String("addr", ln.Addr().String()))
	if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("gateway: Serve: %w", err)
	}
	return nil
}

// Shutdown gracefully drains active requests then closes the
// listener. The passed context bounds the drain.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("gateway: shutting down")
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("gateway: Shutdown: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// statusWriter -- transparent http.ResponseWriter wrapper that
// records the status code without buffering the response
// body. The wrapper forwards every method to the inner writer
// directly so streaming semantics (chunked transfer, flush)
// are preserved.
// ---------------------------------------------------------------------------

// statusWriter wraps an http.ResponseWriter to capture the
// status code emitted by the downstream verb handler. The
// gateway uses the captured status to:
//
//   - stamp `http.status_code` on the span;
//   - decide whether the panic-recover branch needs to emit
//     its own 500 (a downstream handler that already wrote
//     headers cannot have a status downgraded).
//
// The wrapper is transparent: Write / WriteHeader / Header
// all forward to the underlying writer with no buffering.
// Streaming verb handlers (server-sent events, chunked
// responses) continue to work unmodified.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func newStatusWriter(w http.ResponseWriter) *statusWriter {
	return &statusWriter{ResponseWriter: w}
}

// WriteHeader captures the status and forwards. Defends
// against double-WriteHeader by ignoring the second call --
// matches the stdlib's own behaviour of logging a warning on
// the second call but not emitting a malformed response.
func (s *statusWriter) WriteHeader(code int) {
	if s.written {
		return
	}
	s.status = code
	s.written = true
	s.ResponseWriter.WriteHeader(code)
}

// Write forwards to the underlying writer. If the downstream
// handler skipped WriteHeader (the stdlib default-200 path),
// we still record 200 so the span carries the right status.
func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.written {
		s.status = http.StatusOK
		s.written = true
	}
	return s.ResponseWriter.Write(b)
}

// Status returns the captured status (defaults to 200 when no
// status was explicitly written).
func (s *statusWriter) Status() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// HeaderWritten reports whether the downstream handler has
// already started the response. The panic-recover branch
// uses this to decide whether emitting its own 500 is still
// possible.
func (s *statusWriter) HeaderWritten() bool { return s.written }

// Flush forwards to the underlying writer when it supports
// [http.Flusher]. Without this method an `http.ResponseWriter`
// wrapper would break SSE / chunked streaming because the
// downstream handler would observe a non-Flusher writer.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer when it supports
// [http.Hijacker]. Verb handlers that upgrade the connection
// (websocket, HTTP/2-to-raw-TCP) need this transparency.
func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errHijackNotSupported
}

// errHijackNotSupported is returned by [statusWriter.Hijack]
// when the underlying writer does not implement
// [http.Hijacker]. Pinned as a sentinel so a caller can
// `errors.Is`-match it.
var errHijackNotSupported = errors.New("api: underlying ResponseWriter does not support Hijack")
