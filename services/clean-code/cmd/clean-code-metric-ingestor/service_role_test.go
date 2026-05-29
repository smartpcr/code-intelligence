package main

import (
	"strings"
	"testing"
)

// TestValidateServiceRole_AcceptsRecognisedValues pins the
// SERVICE_ROLE allow-list. The metric-ingestor binary mounts
// both the Stage 3.1 ingestor routes and the Stage 3.4 mgmt
// write verbs, so empty, "metric-ingestor", "mgmt-surface", and
// "management" are all valid (the routes do not change based on
// the label; it's purely an observability hint + a typo-fence
// against the operator-rejected refactor-planner role).
func TestValidateServiceRole_AcceptsRecognisedValues(t *testing.T) {
	cases := []string{
		"",
		"metric-ingestor",
		"metric_ingestor",
		"metricingestor",
		"MetricIngestor",
		"mgmt-surface",
		"mgmt_surface",
		"mgmtsurface",
		"management",
		"  metric-ingestor  ",
	}
	for _, role := range cases {
		role := role
		t.Run(role, func(t *testing.T) {
			if err := validateServiceRole(role); err != nil {
				t.Fatalf("validateServiceRole(%q) returned error: %v", role, err)
			}
		})
	}
}

// TestValidateServiceRole_RejectsRefactorPlanner is the
// operator-extended role-dispatch contract: when the deploy
// accidentally pins SERVICE_ROLE to refactor-planner, the
// metric-ingestor binary MUST refuse to start and MUST point
// the operator at the correct SERVICE build arg rather than
// silently no-op on hot_spot / refactor_plan writes.
func TestValidateServiceRole_RejectsRefactorPlanner(t *testing.T) {
	cases := []string{"refactor-planner", "refactor_planner", "RefactorPlanner", "REFACTOR-PLANNER"}
	for _, role := range cases {
		role := role
		t.Run(role, func(t *testing.T) {
			err := validateServiceRole(role)
			if err == nil {
				t.Fatalf("validateServiceRole(%q) returned nil; want refactor-planner rejection", role)
			}
			msg := err.Error()
			if !strings.Contains(msg, "SERVICE=clean-code-refactor-planner") {
				t.Fatalf("error %q does not point to SERVICE=clean-code-refactor-planner", msg)
			}
		})
	}
}

// TestValidateServiceRole_RejectsUnknown ensures typos like
// SERVICE_ROLE=worker fail loudly with an enumeration of the
// allowed values rather than booting in a surprising mode.
func TestValidateServiceRole_RejectsUnknown(t *testing.T) {
	err := validateServiceRole("worker")
	if err == nil {
		t.Fatalf("validateServiceRole(\"worker\") returned nil; want rejection")
	}
	if !strings.Contains(err.Error(), "not recognised") {
		t.Fatalf("error %q does not mention not-recognised", err.Error())
	}
}
