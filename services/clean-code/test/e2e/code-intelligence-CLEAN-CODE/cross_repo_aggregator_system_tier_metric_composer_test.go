//go:build e2e

package e2e

import (
	"testing"

	"github.com/cucumber/godog"
)

// requireEnv returns the value of the named environment variable,
// calling t.Skip when unset or empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := lookupEnv(name)
	if !ok {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// TestE2E_cross_repo_aggregator_system_tier_metric_composer is the
// godog test entrypoint for this stage.
func TestE2E_cross_repo_aggregator_system_tier_metric_composer(t *testing.T) {
	svcRoot, err := serviceRoot()
	if err != nil {
		t.Fatalf("serviceRoot: %v", err)
	}
	if !composerPackageExists(svcRoot) {
		t.Skip("aggregator package not present; impl branch not landed")
	}

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_cross_repo_aggregator_system_tier_metric_composer,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"cross_repo_aggregator_system_tier_metric_composer.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("godog suite failed")
	}
}