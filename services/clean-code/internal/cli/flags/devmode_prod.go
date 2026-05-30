//go:build prod

package flags

// DefaultDevMode is the compile-time default for the
// `--dev-mode` flag under `-tags prod`. The release binary
// must NOT permit unsigned policy bundles by default, so
// the constant is `false`. An operator can still pass
// `--dev-mode=true` explicitly, but the audit trail then
// shows the override on the command line.
//
// Paired with `devmode_default.go` (`//go:build !prod`)
// which sets the constant to `true` for developer builds.
const DefaultDevMode = false
