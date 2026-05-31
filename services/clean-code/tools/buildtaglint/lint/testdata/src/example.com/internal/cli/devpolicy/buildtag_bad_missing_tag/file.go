// No build constraint -> file would compile into prod.
// Constructs an unsigned steward.PolicyVersion -> rule fires.
package buildtag_bad_missing_tag

import "example.com/policy/steward"

func New() steward.PolicyVersion {
	return steward.PolicyVersion{ // want `no-production-build-tag-bypass`
		Name:      "x",
		Signature: nil,
	}
}
