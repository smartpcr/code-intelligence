//go:build linux

package isolation

import (
	"fmt"
	"strconv"
	"syscall"
)

// applyChildMemoryLimit sets RLIMIT_AS for the current process
// from the env-var value. Called by [RunChild] before any
// allocation-heavy work so a runaway handler cannot outrun
// the cap.
//
// The child applies the limit on itself (rather than the
// parent applying it via SysProcAttr) because Go's stdlib
// `syscall.SysProcAttr` doesn't expose rlimit fields portably.
// The env-var-then-self-Setrlimit pattern works on Linux where
// `setrlimit(RLIMIT_AS, …)` is the canonical address-space cap.
//
// Architecture pin (Sec 9.2): production runs on Linux. macOS
// development uses the [other-platforms] no-op; Windows uses
// the [windows] no-op. Tests gate the real OOM-via-rlimit
// scenario on `runtime.GOOS == "linux"`.
func applyChildMemoryLimit(memEnv string) error {
	bytes, err := strconv.ParseUint(memEnv, 10, 64)
	if err != nil {
		return fmt.Errorf("isolation/child: parse mem limit %q: %w", memEnv, err)
	}
	if bytes == 0 {
		return nil
	}
	rl := &syscall.Rlimit{Cur: bytes, Max: bytes}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, rl); err != nil {
		return fmt.Errorf("isolation/child: setrlimit(RLIMIT_AS, %d): %w", bytes, err)
	}
	return nil
}
