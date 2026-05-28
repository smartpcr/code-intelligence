//go:build windows

package isolation

import "os"

// exitSignalName is a no-op on Windows. There is no POSIX-style
// terminating signal on a normal Windows process exit; the
// exit code carries the relevant diagnostic. Returning "" keeps
// the [ParserCrashError.Signal] field empty on Windows.
func exitSignalName(_ *os.ProcessState) string { return "" }
