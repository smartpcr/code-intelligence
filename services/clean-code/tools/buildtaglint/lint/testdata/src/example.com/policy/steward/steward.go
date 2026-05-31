// Fake steward package for analysistest fixtures. The
// canonical type name and Signature field shape match the
// real internal/policy/steward.PolicyVersion. The fixture
// import path ends in `/internal/policy/steward` so it
// satisfies the analyzer's suffix match.
package steward

// PolicyVersion is the fixture twin of the real type.
type PolicyVersion struct {
	Name      string
	Signature []byte
}
