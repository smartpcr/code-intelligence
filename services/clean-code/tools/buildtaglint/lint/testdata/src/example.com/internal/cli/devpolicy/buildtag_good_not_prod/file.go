//go:build !prod

// Correct construction: unsigned PolicyVersion gated to !prod.
// Mirrors the real internal/cli/devpolicy/unsigned_dev.go shape.
package buildtag_good_not_prod

import "example.com/policy/steward"

func New() steward.PolicyVersion {
	return steward.PolicyVersion{
		Name:      "x",
		Signature: nil,
	}
}
