//go:build !windows

package isolation

import (
	"os"
	"syscall"
)

// exitSignalName extracts the terminating signal from a
// finished `*os.ProcessState`. Unix-specific: uses
// `syscall.WaitStatus`. Returns the empty string when the
// process exited normally with a code (i.e., was not
// signal-terminated).
func exitSignalName(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return ""
	}
	if ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
