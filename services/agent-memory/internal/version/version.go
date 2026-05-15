// Package version exposes build-time identifiers for the
// agent-memory service. The values are overridden at link time by
// the CI release pipeline; the defaults below are what an
// unstamped local `go build` produces and are intentionally
// human-recognisable.
//
// Keeping a single non-empty Go package in the tree from day one
// lets `make build`, `make test`, and `make lint` exit 0 on the
// otherwise-empty scaffold (Stage 1.1, implementation-plan.md).
package version

// These vars are populated via `-ldflags "-X ..."` at release time.
// They are vars (not consts) so the linker can rewrite them.
var (
	// Version is the semantic version of the service binary.
	Version = "0.0.0-dev"
	// Commit is the git SHA the binary was built from.
	Commit = "unknown"
	// BuildDate is the RFC3339 UTC timestamp of the build.
	BuildDate = "unknown"
)
