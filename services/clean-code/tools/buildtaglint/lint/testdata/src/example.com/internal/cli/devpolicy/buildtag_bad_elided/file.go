// Regression guard for iter-5 evaluator item #1: elided
// composite literals inside a typed outer literal (slice
// or map) still construct steward.PolicyVersion values
// with nil Signature. The analyzer must flag each inner
// literal even though it carries no syntactic Type node
// (the type is supplied by the outer container's element
// type and recorded in pass.TypesInfo.Types[cl].Type).
//
// No `//go:build !prod` constraint -> diagnostics expected.
package buildtag_bad_elided

import "example.com/policy/steward"

func Slice() []steward.PolicyVersion {
	return []steward.PolicyVersion{
		{ // want `no-production-build-tag-bypass`
			Name:      "a",
			Signature: nil,
		},
		{ // want `no-production-build-tag-bypass`
			Name:      "b",
			Signature: nil,
		},
	}
}

func Map() map[string]steward.PolicyVersion {
	return map[string]steward.PolicyVersion{
		"x": { // want `no-production-build-tag-bypass`
			Name:      "x",
			Signature: nil,
		},
	}
}

func Array() [1]steward.PolicyVersion {
	return [1]steward.PolicyVersion{
		{ // want `no-production-build-tag-bypass`
			Name:      "z",
			Signature: nil,
		},
	}
}
