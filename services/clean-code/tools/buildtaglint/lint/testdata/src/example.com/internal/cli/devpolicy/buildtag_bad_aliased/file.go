// Aliased import of steward -> selector reads `stewardv1.PolicyVersion`
// instead of `steward.PolicyVersion`. The analyzer MUST still fire
// because type resolution sees the underlying package path.
// Regression guard for evaluator iter 1, item #4.
package buildtag_bad_aliased

import stewardv1 "example.com/policy/steward"

func New() stewardv1.PolicyVersion {
	return stewardv1.PolicyVersion{ // want `no-production-build-tag-bypass`
		Name: "x",
	}
}
