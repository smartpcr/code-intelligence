// No build constraint, but Signature is non-nil -> the
// constructed PolicyVersion is signed and out of scope for
// the rule. The analyzer MUST NOT fire.
package buildtag_good_signed

import "example.com/policy/steward"

func sign() []byte { return []byte("sig") }

func New() steward.PolicyVersion {
	return steward.PolicyVersion{
		Name:      "x",
		Signature: sign(),
	}
}
