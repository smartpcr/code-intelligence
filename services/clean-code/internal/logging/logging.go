// Package logging is the clean-code service's slog wrapper.
//
// Stage 1.1 (implementation-plan.md) requires structured JSON logs
// plus request-id propagation across handler boundaries
// (architecture Sec 8 telemetry invariant).
//
// The contract this package exposes:
//
//   - New(cfg) builds a *slog.Logger whose root handler emits
//     line-delimited JSON to the supplied io.Writer.
//   - Every log call that takes a context.Context (i.e. uses
//     LogAttrs / InfoContext / etc.) automatically attaches the
//     request id stored in that context under the package-private
//     ctxKey, so downstream packages cannot misuse the key type to
//     collide with other ctx values.
//   - WithRequestID / FromContext provide the request-id
//     plumbing that HTTP middleware and grpc interceptors will
//     reach for in later stages.
//
// The handler is intentionally a thin adapter on top of
// `slog.JSONHandler`; replacing it with OTel-native logging is a
// later-stage swap that does not require rewriting the call sites
// here.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strings"
)

// ctxKey is a package-private type for context keys. Keeping the
// type unexported prevents other packages from manufacturing a
// colliding key.
type ctxKey int

const (
	// requestIDKey is the context.Context key the request-id is
	// stored under.
	requestIDKey ctxKey = iota
)

// AttrRequestID is the JSON field name a propagated request id
// surfaces under in the log record. Pinned as a constant so
// dashboard / alert queries can grep for it.
const AttrRequestID = "request_id"

// Config controls the slog logger's behaviour. The zero value is
// usable: it emits info-level JSON to stderr.
type Config struct {
	// Writer is the target for log records. nil -> os.Stderr.
	Writer io.Writer
	// Level is the slog.Leveler the handler should filter at.
	// nil -> slog.LevelInfo.
	Level slog.Leveler
	// AddSource attaches a `source` attribute (file:line) to
	// every record. Costs ~5% throughput; off by default.
	AddSource bool
	// ServiceName is the static `service.name` attribute attached
	// to every record. Empty -> "clean-code".
	ServiceName string
}

// ParseLevel maps an env-var-style level string to a slog.Level.
// Unknown strings fall back to info but surface no error so
// startup never blocks on a typo in the log-level pin -- the
// log line that fires moments later carries `level=info` and
// the operator can react.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New builds a *slog.Logger whose handler:
//
//   - emits line-delimited JSON to cfg.Writer
//   - filters at cfg.Level
//   - attaches `service.name` to every record
//   - attaches the request id from the context (if any) under
//     the `request_id` key
func New(cfg Config) *slog.Logger {
	w := cfg.Writer
	if w == nil {
		w = os.Stderr
	}
	level := cfg.Level
	if level == nil {
		level = slog.LevelInfo
	}
	service := cfg.ServiceName
	if service == "" {
		service = "clean-code"
	}

	base := slog.NewJSONHandler(w, &slog.HandlerOptions{
		AddSource: cfg.AddSource,
		Level:     level,
	})
	h := &requestIDHandler{Handler: base.WithAttrs([]slog.Attr{
		slog.String("service.name", service),
	})}
	return slog.New(h)
}

// requestIDHandler decorates the base slog.Handler with the
// request-id extraction step. We wrap rather than subclass so the
// JSONHandler's attribute encoding stays untouched; the only
// difference is that Handle reads the ctx and prepends the
// request_id attr when present.
type requestIDHandler struct {
	slog.Handler
}

func (h *requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := FromContext(ctx); id != "" {
		r.AddAttrs(slog.String(AttrRequestID, id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs / WithGroup must re-wrap so the request-id step
// survives across Logger.With and Logger.WithGroup calls.
func (h *requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &requestIDHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *requestIDHandler) WithGroup(name string) slog.Handler {
	return &requestIDHandler{Handler: h.Handler.WithGroup(name)}
}

// WithRequestID returns a new context carrying the supplied
// request id. The id is logged under AttrRequestID by any logger
// produced from New.
func WithRequestID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// FromContext returns the request id stored in ctx, or "" if
// none. A missing id is NOT an error -- handlers that mint ids on
// receipt are responsible for calling WithRequestID before any
// logging fires.
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// NewRequestID mints a fresh request id (16 hex chars). Useful
// for inbound HTTP middleware that has no upstream `X-Request-Id`
// header. Falls back to a static sentinel only if the system PRNG
// fails (vanishingly unlikely; we surface it so the operator can
// see it in /readyz output).
func NewRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "id-rand-unavailable"
	}
	return hex.EncodeToString(buf[:])
}
