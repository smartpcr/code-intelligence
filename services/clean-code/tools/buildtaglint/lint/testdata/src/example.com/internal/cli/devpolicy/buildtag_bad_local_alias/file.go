// Regression guard for iter-4 evaluator item #1: a local
// type alias `type PV = steward.PolicyVersion` hides the
// `steward.PolicyVersion` selector entirely. The analyzer
// must still flag the unsigned construction because the
// resolved type is identical to the steward type
// (Go type aliases create no new type identity).
//
// No `//go:build !prod` constraint -> diagnostic expected.
package buildtag_bad_local_alias

import "example.com/policy/steward"

type PV = steward.PolicyVersion

func New() PV {
	return PV{ // want `no-production-build-tag-bypass`
		Name:      "x",
		Signature: nil,
	}
}
