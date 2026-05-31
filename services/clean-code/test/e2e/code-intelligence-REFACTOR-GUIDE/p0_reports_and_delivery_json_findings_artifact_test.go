//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p0_reports_and_delivery_json_findings_artifact_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// ---------------------------------------------------------------------------
// Per-scenario state
// ---------------------------------------------------------------------------

type jsonFindingsState struct {
	artifact    report.RunArtifact
	outputBytes []byte
	secondBytes []byte
	decoded     report.RunArtifact
	renderErr   error
}

func newJSONFindingsState() *jsonFindingsState {
	return &jsonFindingsState{}
}

// ---------------------------------------------------------------------------
// Deterministic UUIDs so byte-stability holds across renders.
// ---------------------------------------------------------------------------

var (
	jfPolicyID  = uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111"))
	jfRepoID    = uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222"))
	jfRunID     = uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333"))
	jfVerdictID = uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444"))
	jfFindingID = uuid.Must(uuid.FromString("55555555-5555-5555-5555-555555555555"))
)

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *jsonFindingsState) aFullyPopulatedRunArtifactForJSONRendering() error {
	s.artifact = report.RunArtifact{
		SchemaVersion: report.SchemaVersionCurrent,
		Context: repocontext.RepoContext{
			RootPath:   "/repos/json-e2e",
			RepoID:     jfRepoID,
			HeadSHA:    "cafe1234",
			ModulePath: "example.com/json-e2e",
			IsGitRepo:  true,
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: jfPolicyID,
			Name:            "cleanc-dev-policy",
			RefactorWeights: steward.RefactorWeights{
				EffortModelVersion: "fallback-2026.05",
			},
		},
		Files: []report.WalkedFileSummary{
			{Path: "main.go", Language: "go", SizeBytes: 128, ParseStatus: "parsed"},
		},
		DarkMetrics: []orchestrator.DarkMetric{
			{MetricKind: "cyclo", Language: "go"},
		},
		Run: rule_engine.EvaluationRun{
			EvaluationRunID: jfRunID,
			RepoID:          jfRepoID,
			SHA:             "cafe1234",
			PolicyVersionID: jfPolicyID,
		},
		Verdict: rule_engine.EvaluationVerdict{
			VerdictID:       jfVerdictID,
			EvaluationRunID: jfRunID,
			Verdict:         rule_engine.VerdictWarn,
		},
		Findings: []rule_engine.Finding{
			{
				FindingID:       jfFindingID,
				EvaluationRunID: jfRunID,
				RepoID:          jfRepoID,
				RuleID:          "solid.srp.lcom4",
				RuleVersion:     1,
				PolicyVersionID: jfPolicyID,
				ExplanationMD:   "Suggested refactor: split <Class> & extract.",
			},
		},
	}
	return nil
}

func (s *jsonFindingsState) aNonEmptyRunArtifactWithoutAnExplicitSchemaVersion() error {
	s.artifact = report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath:   "/repos/schema-stamp",
			RepoID:     jfRepoID,
			HeadSHA:    "abcd5678",
			ModulePath: "example.com/schema-stamp",
			IsGitRepo:  true,
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: jfPolicyID,
			Name:            "cleanc-dev-policy",
			RefactorWeights: steward.RefactorWeights{
				EffortModelVersion: "fallback-2026.05",
			},
		},
		Verdict: rule_engine.EvaluationVerdict{
			Verdict: rule_engine.VerdictPass,
		},
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *jsonFindingsState) jsonRenderRuns() error {
	var buf bytes.Buffer
	s.renderErr = (report.JSON{}).Render(context.Background(), s.artifact, &buf)
	s.outputBytes = buf.Bytes()
	return nil
}

func (s *jsonFindingsState) theOutputIsUnmarshalledBackIntoARunArtifact() error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	if err := json.Unmarshal(s.outputBytes, &s.decoded); err != nil {
		return fmt.Errorf("Unmarshal failed: %w\n---output---\n%s\n---", err, string(s.outputBytes))
	}
	return nil
}

func (s *jsonFindingsState) jsonRenderRunsTwiceOnTheSameArtifact() error {
	var buf1 bytes.Buffer
	if err := (report.JSON{}).Render(context.Background(), s.artifact, &buf1); err != nil {
		return fmt.Errorf("first render failed: %w", err)
	}
	s.outputBytes = buf1.Bytes()

	var buf2 bytes.Buffer
	if err := (report.JSON{}).Render(context.Background(), s.artifact, &buf2); err != nil {
		return fmt.Errorf("second render failed: %w", err)
	}
	s.secondBytes = buf2.Bytes()
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *jsonFindingsState) allFieldsOfTheUnmarshalledArtifactMatchTheOriginal() error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	if !reflect.DeepEqual(s.artifact, s.decoded) {
		return fmt.Errorf("round-trip mismatch:\nwant: %#v\n got: %#v", s.artifact, s.decoded)
	}
	return nil
}

func (s *jsonFindingsState) theTwoJSONOutputsAreByteIdentical() error {
	if !bytes.Equal(s.outputBytes, s.secondBytes) {
		return fmt.Errorf("re-render produced different bytes:\nfirst  (%d bytes): %s\nsecond (%d bytes): %s",
			len(s.outputBytes), string(s.outputBytes),
			len(s.secondBytes), string(s.secondBytes))
	}
	return nil
}

func (s *jsonFindingsState) theOutputJSONContainsASchemaVersionFieldSetTo(expected string) error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	// Decode into a generic map and check the field value
	// directly — avoids false positives from substring matching.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(s.outputBytes, &doc); err != nil {
		return fmt.Errorf("Unmarshal to map failed: %w", err)
	}
	raw, ok := doc["schemaVersion"]
	if !ok {
		return fmt.Errorf("output JSON has no top-level \"schemaVersion\" key\n---output---\n%s\n---",
			string(s.outputBytes))
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("schemaVersion is not a string: %w (raw: %s)", err, string(raw))
	}
	if got != expected {
		return fmt.Errorf("schemaVersion = %q; want %q", got, expected)
	}
	// Cross-check against the exported constant.
	if got != report.SchemaVersionCurrent {
		return fmt.Errorf("schemaVersion %q does not match report.SchemaVersionCurrent (%q)",
			got, report.SchemaVersionCurrent)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_p0_reports_and_delivery_json_findings_artifact(ctx *godog.ScenarioContext) {
	s := newJSONFindingsState()

	// Given
	ctx.Step(
		`^a fully-populated RunArtifact for JSON rendering$`,
		s.aFullyPopulatedRunArtifactForJSONRendering,
	)
	ctx.Step(
		`^a non-empty RunArtifact without an explicit schema version$`,
		s.aNonEmptyRunArtifactWithoutAnExplicitSchemaVersion,
	)

	// When
	ctx.Step(`^JSON\.Render runs$`, s.jsonRenderRuns)
	ctx.Step(
		`^the output is unmarshalled back into a RunArtifact$`,
		s.theOutputIsUnmarshalledBackIntoARunArtifact,
	)
	ctx.Step(
		`^JSON\.Render runs twice on the same artifact$`,
		s.jsonRenderRunsTwiceOnTheSameArtifact,
	)

	// Then
	ctx.Step(
		`^all fields of the unmarshalled artifact match the original$`,
		s.allFieldsOfTheUnmarshalledArtifactMatchTheOriginal,
	)
	ctx.Step(
		`^the two JSON outputs are byte-identical$`,
		s.theTwoJSONOutputsAreByteIdentical,
	)
	ctx.Step(
		`^the output JSON contains a schemaVersion field set to "([^"]*)"$`,
		s.theOutputJSONContainsASchemaVersionFieldSetTo,
	)
}

func TestE2E_p0_reports_and_delivery_json_findings_artifact(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p0_reports_and_delivery_json_findings_artifact,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p0_reports_and_delivery_json_findings_artifact.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
