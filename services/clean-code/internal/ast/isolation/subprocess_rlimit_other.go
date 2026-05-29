//go:build !linux

package isolation

// applyChildMemoryLimit is a no-op on every platform other
// than Linux. macOS lacks `RLIMIT_AS` (it has `RLIMIT_DATA`
// but its semantics are weaker for Go's mmap-heavy allocator);
// Windows lacks `setrlimit(2)` entirely. The architecture
// (Sec 9.2) pins Linux production; tests gate the real OOM-
// via-rlimit scenario on `runtime.GOOS == "linux"`.
//
// On non-Linux platforms the child still runs the parser
// handler under the [SubprocessConfig.Timeout] enforced by
// `exec.CommandContext`; OOM scenarios are exercised via a
// fake [Worker] that surfaces the typed [ErrParserOOM]
// directly (proving the host-side error mapping without
// depending on a real OOM kill).
func applyChildMemoryLimit(_ string) error { return nil }
