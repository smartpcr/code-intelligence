package webhook_test

import (
	"bytes"
	"log/slog"
	"sync"
)

// safeBuffer is a thread-safe wrapper around bytes.Buffer used
// for the slog handler in tests (slog writes from internal
// goroutines on the in-flight watchdog path).
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestLogger returns a JSON-handler-backed [*slog.Logger]
// whose output is captured in the returned buffer. Tests
// assert on String() against the buffer's accumulated lines.
//
// The handler is set to LevelDebug so Warn / Info / Debug
// lines are all captured; tests can grep for the specific
// level / message they care about.
func newTestLogger() (*safeBuffer, *slog.Logger) {
	buf := &safeBuffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return buf, slog.New(h)
}
