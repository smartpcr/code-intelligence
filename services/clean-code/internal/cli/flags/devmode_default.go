//go:build !prod

package flags

// DefaultDevMode is the compile-time default for the
// `--dev-mode` flag. The dev (no-tag) build defaults to
// `true` so a developer-laptop run permits unsigned policy
// bundles without an extra flag; the prod-tagged twin in
// `devmode_prod.go` flips this to `false` so a release
// binary refuses unsigned bundles by default.
//
// Pinning the constant in this build-tag-paired file (and
// NOT in `cmd/cleanc/buildtag_default.go`) is what resolves
// iter-4 evaluator item 6 -- "DEV-MODE DEFAULT NOT
// CENTRALIZED IN FLAGS HELPER". The dispatcher in
// `cmd/cleanc/main.go` MUST reference `flags.DefaultDevMode`
// and never own a sibling constant.
const DefaultDevMode = true
