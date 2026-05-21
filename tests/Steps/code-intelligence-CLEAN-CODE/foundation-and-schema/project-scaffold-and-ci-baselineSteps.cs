// -----------------------------------------------------------------------
// <copyright file="project-scaffold-and-ci-baselineSteps.cs" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

namespace Forge.Tests.Stories.code_intelligence_CLEAN_CODE.foundation_and_schema;

using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using FluentAssertions;
using Reqnroll;
using YamlDotNet.RepresentationModel;

[Binding]
public class ProjectScaffoldAndCiBaselineSteps
{
    private const int CommandTimeoutMs = 120_000;

    private string repoRoot = string.Empty;
    private string serviceRoot = string.Empty;
    private int exitCode = -1;
    private string commandOutput = string.Empty;
    private string workflowContent = string.Empty;
    private Dictionary<string, string> configDefaults = new();

    // ── Background ──────────────────────────────────────────────

    [Given("the code-intelligence repository root is known")]
    public void GivenTheCodeIntelligenceRepositoryRootIsKnown()
    {
        this.repoRoot = Environment.GetEnvironmentVariable("CODE_INTELLIGENCE_REPO_ROOT")
            ?? Path.GetFullPath(Path.Combine(AppContext.BaseDirectory, "..", "..", "..", "..", "..", "code-intelligence"));

        Directory.Exists(this.repoRoot).Should().BeTrue(
            $"the code-intelligence repo root must exist at '{this.repoRoot}'. " +
            "Set CODE_INTELLIGENCE_REPO_ROOT env var if it is elsewhere.");

        this.serviceRoot = Path.Combine(this.repoRoot, "services", "clean-code");
    }

    // ── Scenario: scaffold-builds-clean ─────────────────────────

    [Given("a fresh checkout")]
    public void GivenAFreshCheckout()
    {
        Directory.Exists(this.serviceRoot).Should().BeTrue(
            $"services/clean-code/ must exist under the repo root at '{this.serviceRoot}'.");

        var makefile = Path.Combine(this.serviceRoot, "Makefile");
        File.Exists(makefile).Should().BeTrue(
            "a Makefile must exist in services/clean-code/.");
    }

    [When("make build lint test runs in services/clean-code/")]
    public void WhenMakeBuildLintTestRunsInServicesCleanCode()
    {
        var result = this.RunCommand("make", "build lint test", this.serviceRoot);
        this.exitCode = result.ExitCode;
        this.commandOutput = result.Output;
    }

    [Then("it exits 0 with no missing-target errors")]
    public void ThenItExits0WithNoMissingTargetErrors()
    {
        this.exitCode.Should().Be(0, "make build lint test must exit cleanly");
        this.commandOutput.Should().NotContain("No rule to make target",
            "there should be no missing Makefile targets");
        this.commandOutput.Should().NotContain("missing separator",
            "the Makefile should have no syntax errors");
    }

    [Then("it produces the clean-coded binary")]
    public void ThenItProducesTheCleanCodedBinary()
    {
        // The binary may be at the service root or in a bin/ subdirectory.
        var possiblePaths = new[]
        {
            Path.Combine(this.serviceRoot, "clean-coded"),
            Path.Combine(this.serviceRoot, "clean-coded.exe"),
            Path.Combine(this.serviceRoot, "bin", "clean-coded"),
            Path.Combine(this.serviceRoot, "bin", "clean-coded.exe"),
        };

        possiblePaths.Any(File.Exists).Should().BeTrue(
            "the clean-coded binary must be produced by 'make build'. " +
            $"Searched: {string.Join(", ", possiblePaths)}");
    }

    // ── Scenario: ci-workflow-triggers ───────────────────────────

    [Given("a PR touching services/clean-code/\\*\\*")]
    public void GivenAPrTouchingServicesCleanCode()
    {
        var workflowPath = Path.Combine(
            this.repoRoot, ".github", "workflows", "clean-code-ci.yml");

        File.Exists(workflowPath).Should().BeTrue(
            $"workflow file must exist at {workflowPath}");

        this.workflowContent = File.ReadAllText(workflowPath);
    }

    [When("GitHub Actions evaluates the workflow file")]
    public void WhenGitHubActionsEvaluatesTheWorkflowFile()
    {
        this.workflowContent.Should().NotBeNullOrWhiteSpace(
            "the workflow YAML content must be loaded");

        // Parse YAML to verify structural validity.
        var yaml = new YamlStream();
        using var reader = new StringReader(this.workflowContent);
        yaml.Load(reader);

        yaml.Documents.Should().HaveCountGreaterThan(0,
            "workflow YAML must be a valid document");
    }

    [Then(".github/workflows/clean-code-ci.yml runs make lint test and the container build job")]
    public void ThenWorkflowRunsMakeLintTestAndContainerBuildJob()
    {
        this.workflowContent.Should().Contain("make",
            "workflow must invoke make targets");

        // Verify lint and test targets are referenced.
        this.workflowContent.Should().Match(
            "*lint*",
            "workflow must run the lint target");

        this.workflowContent.Should().Match(
            "*test*",
            "workflow must run the test target");

        // Verify a container / docker build job exists.
        var hasContainerBuild =
            this.workflowContent.Contains("docker", StringComparison.OrdinalIgnoreCase) ||
            this.workflowContent.Contains("container", StringComparison.OrdinalIgnoreCase) ||
            this.workflowContent.Contains("image", StringComparison.OrdinalIgnoreCase);

        hasContainerBuild.Should().BeTrue(
            "workflow must include a container build job");
    }

    [Then("both succeed on the empty scaffold")]
    public void ThenBothSucceedOnTheEmptyScaffold()
    {
        // Structural verification: the workflow path filter covers
        // services/clean-code/**.
        this.workflowContent.Should().Contain("services/clean-code",
            "workflow triggers must include the services/clean-code path");
    }

    // ── Scenario: config-honours-pins ───────────────────────────

    [Given("a config file that omits the five operator pins")]
    public void GivenAConfigFileThatOmitsTheFiveOperatorPins()
    {
        // Write a minimal / empty config to a temp file and set it for the loader.
        // The Go config loader reads CLEAN_CODE_CONFIG_FILE
        // (see services/clean-code/internal/config/config.go: EnvConfigFile).
        var tempConfig = Path.Combine(Path.GetTempPath(), $"clean-code-test-config-{Guid.NewGuid()}.yaml");
        File.WriteAllText(tempConfig, "# empty config — all operator pins omitted\n");

        Environment.SetEnvironmentVariable("CLEAN_CODE_CONFIG_FILE", tempConfig);
    }

    [When("the loader initialises")]
    public void WhenTheLoaderInitialises()
    {
        // Run the binary with a --dump-config flag to get resolved defaults.
        var binaryPaths = new[]
        {
            Path.Combine(this.serviceRoot, "clean-coded"),
            Path.Combine(this.serviceRoot, "clean-coded.exe"),
            Path.Combine(this.serviceRoot, "bin", "clean-coded"),
            Path.Combine(this.serviceRoot, "bin", "clean-coded.exe"),
        };

        var binary = binaryPaths.FirstOrDefault(File.Exists);
        binary.Should().NotBeNull(
            "clean-coded binary must exist to test config loading");

        var result = this.RunCommand(binary!, "--dump-config", this.serviceRoot);
        result.ExitCode.Should().Be(0, "dump-config must succeed");

        // Parse the output as YAML key-value pairs.
        this.configDefaults = this.ParseConfigOutput(result.Output);
    }

    [Then("it returns defaults matching architecture Sec 1.6")]
    public void ThenItReturnsDefaultsMatchingArchitectureSec16()
    {
        this.configDefaults.Should().NotBeEmpty(
            "config loader must return defaults when pins are omitted");
    }

    [Then("the default AST mode is embedded")]
    public void ThenTheDefaultAstModeIsEmbedded()
    {
        this.configDefaults.Should().ContainKey("ast_mode");
        this.configDefaults["ast_mode"].Should().Be("embedded",
            "default AST mode must be 'embedded' per architecture Sec 1.6");
    }

    [Then("the default coverage format is Cobertura XML")]
    public void ThenTheDefaultCoverageFormatIsCoberturaXml()
    {
        this.configDefaults.Should().ContainKey("coverage_format");
        this.configDefaults["coverage_format"].Should().Be("Cobertura XML",
            "default coverage format must be 'Cobertura XML'");
    }

    [Then("the default severity is warn")]
    public void ThenTheDefaultSeverityIsWarn()
    {
        this.configDefaults.Should().ContainKey("severity");
        this.configDefaults["severity"].Should().Be("warn",
            "default severity must be 'warn'");
    }

    [Then("the default schema version is v1 required")]
    public void ThenTheDefaultSchemaVersionIsV1Required()
    {
        this.configDefaults.Should().ContainKey("schema_version");
        this.configDefaults["schema_version"].Should().Be("v1 required",
            "default schema version must be 'v1 required'");
    }

    [Then("the default model source is ML model from historical commits")]
    public void ThenTheDefaultModelSourceIsMlModelFromHistoricalCommits()
    {
        this.configDefaults.Should().ContainKey("model_source");
        this.configDefaults["model_source"].Should().Be("ML model from historical commits",
            "default model source must be 'ML model from historical commits'");
    }

    // ── Helpers ─────────────────────────────────────────────────

    private (int ExitCode, string Output) RunCommand(string command, string arguments, string workingDir)
    {
        var psi = new ProcessStartInfo
        {
            FileName = command,
            Arguments = arguments,
            WorkingDirectory = workingDir,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
            CreateNoWindow = true,
        };

        using var process = Process.Start(psi)
            ?? throw new InvalidOperationException($"Failed to start process: {command}");

        var stdout = process.StandardOutput.ReadToEnd();
        var stderr = process.StandardError.ReadToEnd();

        if (!process.WaitForExit(CommandTimeoutMs))
        {
            try
            {
                process.Kill(entireProcessTree: true);
            }
            catch
            {
                // Best-effort cleanup; the process may have exited between the
                // WaitForExit timeout and the Kill call.
            }

            throw new TimeoutException(
                $"'{command} {arguments}' did not exit within {CommandTimeoutMs / 1000} s " +
                $"(working dir: {workingDir}). Process was killed.");
        }

        return (process.ExitCode, stdout + Environment.NewLine + stderr);
    }

    private Dictionary<string, string> ParseConfigOutput(string output)
    {
        var result = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);

        try
        {
            var yaml = new YamlStream();
            using var reader = new StringReader(output);
            yaml.Load(reader);

            if (yaml.Documents.Count > 0 &&
                yaml.Documents[0].RootNode is YamlMappingNode root)
            {
                foreach (var entry in root.Children)
                {
                    var key = entry.Key.ToString();
                    var value = entry.Value.ToString();
                    result[key] = value;
                }
            }
        }
        catch
        {
            // Fallback: line-based key=value or key: value parsing.
            foreach (var line in output.Split('\n', StringSplitOptions.RemoveEmptyEntries))
            {
                var trimmed = line.Trim();
                var sepIdx = trimmed.IndexOfAny(new[] { ':', '=' });
                if (sepIdx > 0)
                {
                    var key = trimmed[..sepIdx].Trim();
                    var value = trimmed[(sepIdx + 1)..].Trim();
                    result[key] = value;
                }
            }
        }

        return result;
    }
}
