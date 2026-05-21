// -----------------------------------------------------------------------
// <copyright file="E2eBase.cs" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------
//
// docs/forge/e2e.md Stage 7.3 — auto-generated step-def base class
// for story 'code-intelligence:CLEAN-CODE'. Emitted ONCE per spawn (by the first
// qa-e2e workstream to fire); subsequent qa-e2e stages reuse this
// type. Edit by hand only when adding helpers that apply to every
// stage of the story; the generator overwrites this file on the
// next full Forge spawn.

namespace Forge.Tests.Stories.code_intelligence_CLEAN_CODE;

using System;

/// <summary>
/// Step-definition base class — derive every Reqnroll
/// <c>[Binding]</c> class for this story from <c>E2eBase</c>
/// and call <see cref="RequireEnv"/> in the constructor /
/// <c>BeforeScenario</c> hook to assert the env-vars
/// <c>appsettings.e2e.json</c> declares are populated.
/// </summary>
public abstract class E2eBase
{
    /// <summary>
    /// Read an env-var; throw <see cref="SetupSkippedException"/>
    /// when it's null or empty. The exception carries the var
    /// name so the test runner can surface it; locally the
    /// CI-fail-on-skipped flag is off so it bubbles up as a
    /// yellow skip, on CI the override config flips it red.
    /// </summary>
    protected static string RequireEnv(string name)
    {
        var value = Environment.GetEnvironmentVariable(name);
        if (string.IsNullOrEmpty(value))
        {
            throw new SetupSkippedException(name);
        }
        return value;
    }
}

/// <summary>
/// Thrown by <see cref="E2eBase.RequireEnv(string)"/> when a
/// required env-var is not populated. Carries the missing var
/// name as a property so the surrounding test runner /
/// reporter can render an actionable skip / failure message.
/// </summary>
public sealed class SetupSkippedException : Exception
{
    /// <summary>The env-var name that was unset.</summary>
    public string VarName { get; }

    public SetupSkippedException(string varName)
        : base($"E2E setup skipped: required env var '{varName}' is not set. Populate it via appsettings.e2e.json overrides or the CI secret store.")
    {
        this.VarName = varName;
    }
}
