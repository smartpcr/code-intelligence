// Package version exposes build-time identifiers for the
// clean-code service. The values are overridden at link time by
// the CI release pipeline; the defaults below are what an
// unstamped local `go build` produces and are intentionally
// human-recognisable.
//
// Stage 1.1 (implementation-plan.md) requires a `/healthz` body
// that returns the running binary's `version`, `commit`, and
// `build_time` so the operator can correlate a misbehaving pod
// against a specific deploy.
package version

// These vars are populated via `-ldflags "-X ..."` at release time.
// They are vars (not consts) so the linker can rewrite them.
//
// The exported names match the implementation-plan.md Stage 1.1
// contract verbatim: `Version`, `Commit`, `BuildTime`. The JSON
// payload of `/healthz` keys these as `version`, `commit`,
// `build_time` (snake_case) for parity with the agent-memory
// service's wire format.
var (
	// Version is the semantic version of the service binary.
	Version = "0.0.0-dev"
	// Commit is the git SHA the binary was built from.
	Commit = "unknown"
	// BuildTime is the RFC3339 UTC timestamp of the build.
	// (implementation-plan.md Stage 1.1 line 53 names this
	// field `BuildTime`; mirroring that name keeps the
	// contract searchable across docs + code.)
	BuildTime = "unknown"
)
