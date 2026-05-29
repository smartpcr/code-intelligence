package ast

// PowerShell parser — Stage 6.1 (story
// code-intelligence:AST-PARSER-FOR-ADDIT, phase
// powershell-parser, stage powershellParser subprocess
// implementation).
//
// Unlike the C / C++ / C# / Go / Rust parsers that ride
// smacker/go-tree-sitter grammar bindings, the official
// smacker module ships NO PowerShell grammar (architecture.md
// Section 6 enumerates the available bindings). The
// operator-pinned strategy is to follow the in-house
// `Ast.PowerShell` reference example, which uses the OFFICIAL
// PowerShell SDK rather than a community tree-sitter grammar:
// invoke `pwsh -NoProfile -NonInteractive -Command -` as a
// subprocess and feed the source via stdin, letting the SDK's
// `System.Management.Automation.Language.Parser` build the
// AST. The embedded extraction script walks
// `FunctionDefinitionAst`, `TypeDefinitionAst`, `ParamBlockAst`,
// `ScriptBlockAst`, `CommandAst`, `UsingStatementAst`,
// `InvokeMemberExpressionAst`, `MemberExpressionAst` and emits
// a single JSON envelope `{functions, types, imports}` (see
// `powershellExtractScript` below).
//
// This file is INTENTIONALLY free of `//go:build` tags. The
// subprocess approach has no compile-time dependency on `pwsh`,
// CGO, or any external library, so the same code compiles on
// `CGO_ENABLED=0` (the portable `make test` path) and
// `CGO_ENABLED=1` alike. Both `parsers_cgo.go` and
// `parsers_nocgo.go` register `NewPowerShellParser()` without
// guards — PowerShell is the one language in the v1 set that
// is build-tag agnostic.
//
// Runtime contract:
//
//   - `pwsh` not on PATH: every `Parse` call returns the
//     wrapped sentinel
//     `fmt.Errorf("powershell: %w (reason=pwsh_not_available)", ErrParserUnavailable)`.
//     The dispatcher's `errors.Is(err, ErrParserUnavailable)`
//     branch in `EmitFile` logs
//     `ast.dispatch.skip{reason="pwsh_not_available"}` at Info
//     level and the worker keeps draining its queue.
//
//   - `pwsh` is present but exits non-zero (genuine parse
//     error in the .ps1 source): the parser returns the
//     UN-WRAPPED subprocess error so `safeParse` falls through
//     to `ast.parse.error` — the same path tree-sitter parsers
//     use when a grammar rejects malformed input.
//
//   - `pwsh` hangs: a 10-second per-file timeout (configurable
//     via the `timeout` field for tests) bounds the subprocess
//     and surfaces a `context.DeadlineExceeded` error, again
//     un-wrapped so `ast.parse.error` fires instead of the
//     sentinel `ast.dispatch.skip` branch.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// defaultPowerShellTimeout bounds a single subprocess
// invocation. The 10 s ceiling comes from Section 6.4 of
// `architecture.md`: longer than worst-case `pwsh` startup
// plus extraction on a multi-thousand-line script, short
// enough that a stuck subprocess does not deadlock the worker.
const defaultPowerShellTimeout = 10 * time.Second

// powershellParser is the v1 PowerShell parser. The struct
// is deliberately exported only via `NewPowerShellParser`;
// tests in the same package construct `&powershellParser{
// pwshBin:""}` directly to drive the sentinel branch (see
// `parser_powershell_test.go`).
type powershellParser struct {
	// pwshBin is the absolute path to the `pwsh` binary, or
	// the empty string when `pwsh` is not on PATH. When
	// empty, every `Parse` call short-circuits to the
	// `ErrParserUnavailable` sentinel.
	pwshBin string

	// timeout caps a single subprocess invocation. Zero
	// means use `defaultPowerShellTimeout`. Exposed for
	// tests to drive the timeout branch with a tiny value
	// against a fake long-running binary.
	timeout time.Duration
}

// NewPowerShellParser returns the v1 PowerShell parser. The
// constructor probes `pwsh` on PATH via `exec.LookPath` once
// at construction time; absence is a steady-state condition
// (the host either has `pwsh` or it does not), so re-probing
// per `Parse` call would add no information.
func NewPowerShellParser() LanguageParser {
	bin, _ := exec.LookPath("pwsh")
	return &powershellParser{pwshBin: bin}
}

// Language returns the canonical lower-case language id.
// Matches the convention used by all other parsers in this
// package (`typescript`, `python`, `c`, `cpp`, `csharp`,
// `go`, `rust`, `powershell`).
func (*powershellParser) Language() string { return "powershell" }

// Extensions returns the v1 PowerShell file extensions the
// dispatcher routes to this parser. `.ps1` is the canonical
// script extension, `.psm1` script modules, `.psd1` module
// manifests. The SDK parser accepts all three.
func (*powershellParser) Extensions() []string {
	return []string{".ps1", ".psm1", ".psd1"}
}

// Parse implements LanguageParser. See `powershellParser`
// doc for the runtime contract — in particular the sentinel
// wrapping for `pwsh` absence vs the un-wrapped error for
// genuine parse failures or timeouts.
func (p *powershellParser) Parse(_ string, src []byte) (ParseResult, error) {
	if p.pwshBin == "" {
		return ParseResult{}, fmt.Errorf(
			"powershell: %w (reason=pwsh_not_available)",
			ErrParserUnavailable,
		)
	}

	timeout := p.timeout
	if timeout <= 0 {
		timeout = defaultPowerShellTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.pwshBin,
		"-NoProfile",
		"-NonInteractive",
		"-NoLogo",
		"-Command", powershellExtractScript,
	)
	cmd.Stdin = bytes.NewReader(src)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
			return ParseResult{}, fmt.Errorf(
				"powershell: subprocess timeout after %s: %w",
				timeout, ctxErr,
			)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return ParseResult{}, fmt.Errorf("powershell: %s", msg)
	}

	var env powershellEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		return ParseResult{}, fmt.Errorf(
			"powershell: decode envelope: %w (stderr=%q)",
			err, strings.TrimSpace(stderr.String()),
		)
	}

	return env.toParseResult(), nil
}

// powershellEnvelope mirrors the JSON document the embedded
// extraction script writes to stdout. Field names follow the
// PascalCase the script uses (PowerShell's
// `ConvertTo-Json` preserves hashtable key casing); we map
// them onto the camelCase Go envelope via the `json` tags.
type powershellEnvelope struct {
	Functions []psFunctionRecord `json:"functions"`
	Types     []psTypeRecord     `json:"types"`
	Imports   []psImportRecord   `json:"imports"`
}

type psFunctionRecord struct {
	Name            string                 `json:"Name"`
	Params          string                 `json:"Params"`
	StartLine       int                    `json:"StartLine"`
	EndLine         int                    `json:"EndLine"`
	BodyStartLine   int                    `json:"BodyStartLine"`
	BodyEndLine     int                    `json:"BodyEndLine"`
	BodyStartOffset int                    `json:"BodyStartOffset"`
	BodyEndOffset   int                    `json:"BodyEndOffset"`
	BodyText        string                 `json:"BodyText"`
	Calls           []string               `json:"Calls"`
	ReceiverCalls   []string               `json:"ReceiverCalls"`
	MemberAccesses  []psMemberAccessRecord `json:"MemberAccesses"`
}

type psTypeRecord struct {
	Name      string           `json:"Name"`
	Kind      string           `json:"Kind"`
	BaseTypes []string         `json:"BaseTypes"`
	Methods   []psMethodRecord `json:"Methods"`
	StartLine int              `json:"StartLine"`
	EndLine   int              `json:"EndLine"`
}

type psMethodRecord struct {
	Name            string                 `json:"Name"`
	Params          string                 `json:"Params"`
	StartLine       int                    `json:"StartLine"`
	EndLine         int                    `json:"EndLine"`
	BodyStartLine   int                    `json:"BodyStartLine"`
	BodyEndLine     int                    `json:"BodyEndLine"`
	BodyStartOffset int                    `json:"BodyStartOffset"`
	BodyEndOffset   int                    `json:"BodyEndOffset"`
	BodyText        string                 `json:"BodyText"`
	Modifiers       []string               `json:"Modifiers"`
	Calls           []string               `json:"Calls"`
	ReceiverCalls   []string               `json:"ReceiverCalls"`
	MemberAccesses  []psMemberAccessRecord `json:"MemberAccesses"`
}

type psImportRecord struct {
	Module     string `json:"Module"`
	ModuleKind string `json:"ModuleKind"`
	CmdletVerb string `json:"CmdletVerb"`
	Line       int    `json:"Line"`
}

type psMemberAccessRecord struct {
	Name    string `json:"Name"`
	IsWrite bool   `json:"IsWrite"`
}

// toParseResult flattens the JSON envelope onto the
// language-agnostic `ParseResult` the dispatcher consumes.
// All language-specific richness (cmdlet verb, module kind,
// modifiers) flows onto `LangMeta` so downstream
// `attrs_json` can preserve it without polluting the shared
// envelope.
func (e powershellEnvelope) toParseResult() ParseResult {
	var res ParseResult

	for _, t := range e.Types {
		extends, implements := splitPowerShellBaseTypes(t.BaseTypes)
		res.Classes = append(res.Classes, ClassDecl{
			QualifiedName: t.Name,
			Kind:          normalisePowerShellKind(t.Kind),
			Extends:       extends,
			Implements:    implements,
			StartLine:     t.StartLine,
			EndLine:       t.EndLine,
		})
		for _, m := range t.Methods {
			res.Methods = append(res.Methods, methodFromPSMethod(t.Name, m))
		}
	}

	for _, f := range e.Functions {
		res.Methods = append(res.Methods, methodFromPSFunction(f))
	}

	for _, imp := range e.Imports {
		res.Imports = append(res.Imports, Import{
			Path:   imp.Module,
			Module: imp.Module,
			Line:   imp.Line,
			LangMeta: map[string]any{
				"module_kind": imp.ModuleKind,
				"cmdlet_verb": imp.CmdletVerb,
			},
		})
	}

	return res
}

// methodFromPSMethod converts a PowerShell class member
// (FunctionMemberAst) record into the dispatcher's
// `MethodDecl`. The QualifiedName is `Class.Method` so the
// existing same-file resolution path (`<EnclosingClass>.<callee>`
// in dispatcher Pass 2b) matches `$this.X(...)` receiver
// calls without any PowerShell-specific resolver code.
func methodFromPSMethod(className string, m psMethodRecord) MethodDecl {
	body, bodyStart, bodyEnd := stripPowerShellBraces(m.BodyText, m.BodyStartOffset, m.BodyEndOffset)
	mods := append([]string(nil), m.Modifiers...)
	return MethodDecl{
		QualifiedName:  className + "." + m.Name,
		EnclosingClass: className,
		ParamSignature: m.Params,
		BodySource:     body,
		StartLine:      m.StartLine,
		EndLine:        m.EndLine,
		BodyStartLine:  m.BodyStartLine,
		BodyEndLine:    m.BodyEndLine,
		BodyStartByte:  bodyStart,
		BodyEndByte:    bodyEnd,
		Calls:          dedupeStrings(m.Calls),
		ReceiverCalls:  dedupeStrings(m.ReceiverCalls),
		MemberAccesses: convertPSMemberAccesses(m.MemberAccesses),
		Modifiers:      mods,
	}
}

// methodFromPSFunction converts a top-level
// `FunctionDefinitionAst` record into the dispatcher's
// `MethodDecl`. Free functions have `EnclosingClass=""` and
// no modifiers (per the architecture-doc table).
func methodFromPSFunction(f psFunctionRecord) MethodDecl {
	body, bodyStart, bodyEnd := stripPowerShellBraces(f.BodyText, f.BodyStartOffset, f.BodyEndOffset)
	return MethodDecl{
		QualifiedName:  f.Name,
		EnclosingClass: "",
		ParamSignature: f.Params,
		BodySource:     body,
		StartLine:      f.StartLine,
		EndLine:        f.EndLine,
		BodyStartLine:  f.BodyStartLine,
		BodyEndLine:    f.BodyEndLine,
		BodyStartByte:  bodyStart,
		BodyEndByte:    bodyEnd,
		Calls:          dedupeStrings(f.Calls),
		ReceiverCalls:  dedupeStrings(f.ReceiverCalls),
		MemberAccesses: convertPSMemberAccesses(f.MemberAccesses),
	}
}

// stripPowerShellBraces trims the leading `{` and trailing
// `}` from a ScriptBlockAst extent. The PowerShell SDK
// surfaces the body extent INCLUDING the braces, while every
// other parser in this package (TS / Python / tree-sitter
// languages) stores `BodySource` without them so the block
// subdivider (block.go) sees only logical lines. The byte
// offsets shift accordingly: BodyStartByte points at the
// first interior byte, BodyEndByte at the last interior byte
// inclusive (per parser_treesitter.go convention). Returns
// the original text and offsets unchanged when the extent
// does not actually start with `{` (defensive).
func stripPowerShellBraces(text string, startOff, endOff int) (string, int, int) {
	trimmed := text
	bodyStart := startOff
	bodyEnd := endOff - 1
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		trimmed = trimmed[1 : len(trimmed)-1]
		bodyStart++
		bodyEnd -= 1
	}
	if bodyStart < 0 {
		bodyStart = 0
	}
	if bodyEnd < bodyStart {
		bodyEnd = bodyStart
	}
	return trimmed, bodyStart, bodyEnd
}

// normalisePowerShellKind maps the script-side kind label
// ("class" / "enum" / "interface") onto the canonical Kind
// strings the dispatcher writes into `attrs_json["decl_kind"]`.
// Returns "class" for unknown labels so a future PowerShell
// SDK release that adds new TypeDefinitionAst flavours does
// not silently drop the type.
func normalisePowerShellKind(k string) string {
	switch k {
	case "enum", "interface", "class":
		return k
	default:
		return "class"
	}
}

// splitPowerShellBaseTypes partitions a TypeDefinitionAst's
// `BaseTypes` list into the `Extends` (first non-interface
// entry) and `Implements` (remaining entries) buckets the
// shared `ClassDecl` exposes. v1 has no way to distinguish
// an interface from a class at parse time (the PowerShell
// SDK reports both as `TypeConstraintAst`); the convention
// matches the C# reference example which writes the first
// base type to the inheritance slot and the rest to the
// interface list.
func splitPowerShellBaseTypes(bases []string) (extends, implements []string) {
	for i, b := range bases {
		if i == 0 {
			extends = []string{b}
			continue
		}
		implements = append(implements, b)
	}
	return extends, implements
}

func convertPSMemberAccesses(in []psMemberAccessRecord) []MemberAccess {
	if len(in) == 0 {
		return nil
	}
	out := make([]MemberAccess, 0, len(in))
	for _, m := range in {
		out = append(out, MemberAccess{Name: m.Name, IsWrite: m.IsWrite})
	}
	return out
}

// dedupeStrings preserves insertion order while removing
// duplicates. The extraction script already de-dupes within
// a single body, but we re-dedupe defensively because the
// dispatcher contracts (e.g. multimap collision rules) treat
// duplicate entries as ambiguity signals.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// powershellExtractScript is the embedded extraction script.
// It runs under `pwsh -NoProfile -NonInteractive -Command -`
// with the .ps1 source piped to stdin. The script writes a
// single JSON envelope `{functions, types, imports}` to
// stdout and exits 0 on success, exits 2 on a fatal parse
// error after writing the message to stderr.
//
// Implementation notes:
//
//   - The script is held in a Go raw-string literal, so it
//     MUST NOT contain any back-tick characters (PowerShell's
//     escape / line-continuation char). All multi-line
//     conditionals use parentheses for grouping, all string
//     escapes use double-up (`""`) inside double-quoted
//     strings, and `param([type]$Var)` uses the apostrophe-
//     plus-dollar form rather than back-tick escapes.
//
//   - `ModuleNotFoundDuringParse` and the assembly-load
//     equivalents are FILTERED from the parse-error list:
//     `using module Foo` and `using assembly Bar` resolve
//     their targets at parse time, but an indexer has no
//     execution context — these are not syntax failures and
//     must not abort extraction.
//
//   - Each helper function returns its result *unwrapped*
//     (i.e. `return $result`, not `return ,$result`); every
//     call site re-wraps with `@(...)` so single-element and
//     empty arrays survive PowerShell's pipeline unrolling
//     without collapsing to scalar / null.
const powershellExtractScript = `$ErrorActionPreference = 'Stop'

$src = [System.Console]::In.ReadToEnd()

$tokens = $null
$errs = $null
$ast = [System.Management.Automation.Language.Parser]::ParseInput($src, [ref]$tokens, [ref]$errs)
if ($errs -and $errs.Count -gt 0) {
    $fatal = @()
    foreach ($e in $errs) {
        switch ($e.ErrorId) {
            'ModuleNotFoundDuringParse' { continue }
            'ErrorLoadingAssembly' { continue }
            'CouldNotLoadAssemblyDuringParse' { continue }
            default { $fatal += $e }
        }
    }
    if ($fatal.Count -gt 0) {
        $msgs = @()
        foreach ($e in $fatal) {
            $msgs += ($e.Message + ' at line ' + $e.Extent.StartLineNumber)
        }
        [System.Console]::Error.WriteLine('powershell parse error: ' + ($msgs -join '; '))
        exit 2
    }
}

function PS-FormatParams {
    param($paramAsts)
    if ($null -eq $paramAsts -or $paramAsts.Count -eq 0) { return '' }
    $parts = @()
    foreach ($p in $paramAsts) {
        $name = ''
        if ($p.Name -and $p.Name.VariablePath) { $name = $p.Name.VariablePath.UserPath }
        $typeStr = ''
        if ($p.StaticType -and $p.StaticType.Name -and $p.StaticType.Name -ne 'Object') {
            $typeStr = $p.StaticType.Name
        }
        if ([string]::IsNullOrEmpty($typeStr)) {
            $parts += ('$' + $name)
        }
        else {
            $parts += ('[' + $typeStr + ']$' + $name)
        }
    }
    return ($parts -join ', ')
}

function PS-ExtractCalls {
    param($body)
    $result = @()
    if ($null -eq $body) { return $result }
    $seen = @{}
    $cmds = $body.FindAll({ param($n) $n -is [System.Management.Automation.Language.CommandAst] }, $true)
    foreach ($c in $cmds) {
        if ($c.InvocationOperator -eq [System.Management.Automation.Language.TokenKind]::Dot) { continue }
        $name = $c.GetCommandName()
        if ([string]::IsNullOrEmpty($name)) { continue }
        if ($seen.ContainsKey($name)) { continue }
        $seen[$name] = $true
        $result += $name
    }
    return $result
}

function PS-ExtractReceiverCalls {
    param($body)
    $result = @()
    if ($null -eq $body) { return $result }
    $seen = @{}
    $invs = $body.FindAll({ param($n) $n -is [System.Management.Automation.Language.InvokeMemberExpressionAst] }, $true)
    foreach ($inv in $invs) {
        $expr = $inv.Expression
        if ($expr -isnot [System.Management.Automation.Language.VariableExpressionAst]) { continue }
        if (-not $expr.VariablePath) { continue }
        if ($expr.VariablePath.UserPath -ne 'this') { continue }
        if ($inv.Member -isnot [System.Management.Automation.Language.StringConstantExpressionAst]) { continue }
        $name = $inv.Member.Value
        if ($seen.ContainsKey($name)) { continue }
        $seen[$name] = $true
        $result += $name
    }
    return $result
}

function PS-ExtractMemberAccesses {
    param($body)
    $result = @()
    if ($null -eq $body) { return $result }
    $writes = @{}
    $assigns = $body.FindAll({ param($n) $n -is [System.Management.Automation.Language.AssignmentStatementAst] }, $true)
    foreach ($a in $assigns) {
        $lhs = $a.Left
        if ($lhs -isnot [System.Management.Automation.Language.MemberExpressionAst]) { continue }
        if ($lhs -is [System.Management.Automation.Language.InvokeMemberExpressionAst]) { continue }
        $expr = $lhs.Expression
        if ($expr -isnot [System.Management.Automation.Language.VariableExpressionAst]) { continue }
        if (-not $expr.VariablePath) { continue }
        if ($expr.VariablePath.UserPath -ne 'this') { continue }
        if ($lhs.Member -isnot [System.Management.Automation.Language.StringConstantExpressionAst]) { continue }
        $writes[$lhs.Member.Value] = $true
    }
    $seen = @{}
    $mems = $body.FindAll({ param($n) $n -is [System.Management.Automation.Language.MemberExpressionAst] }, $true)
    foreach ($m in $mems) {
        if ($m -is [System.Management.Automation.Language.InvokeMemberExpressionAst]) { continue }
        $expr = $m.Expression
        if ($expr -isnot [System.Management.Automation.Language.VariableExpressionAst]) { continue }
        if (-not $expr.VariablePath) { continue }
        if ($expr.VariablePath.UserPath -ne 'this') { continue }
        if ($m.Member -isnot [System.Management.Automation.Language.StringConstantExpressionAst]) { continue }
        $name = $m.Member.Value
        if ($seen.ContainsKey($name)) { continue }
        $seen[$name] = $true
        $result += @{ Name = $name; IsWrite = [bool]($writes[$name]) }
    }
    return $result
}

function PS-IsInsideTypeDefinition {
    param($node)
    $p = $node.Parent
    while ($null -ne $p) {
        if ($p -is [System.Management.Automation.Language.TypeDefinitionAst]) { return $true }
        $p = $p.Parent
    }
    return $false
}

$types = @()
$typeAsts = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.TypeDefinitionAst] }, $true)
foreach ($t in $typeAsts) {
    $bases = @()
    if ($t.BaseTypes) {
        foreach ($b in $t.BaseTypes) {
            if ($b.TypeName) { $bases += $b.TypeName.FullName }
        }
    }
    $methods = @()
    foreach ($m in $t.Members) {
        if ($m -isnot [System.Management.Automation.Language.FunctionMemberAst]) { continue }
        $params = PS-FormatParams $m.Parameters
        $mods = @()
        if ($m.IsStatic) { $mods += 'static' }
        if ($m.IsHidden) { $mods += 'hidden' }
        $body = $m.Body
        $bodyExtent = if ($body) { $body.Extent } else { $m.Extent }
        $bodyText = if ($body) { $body.Extent.Text } else { '' }
        $methods += @{
            Name            = $m.Name
            Params          = $params
            StartLine       = $m.Extent.StartLineNumber
            EndLine         = $m.Extent.EndLineNumber
            BodyStartLine   = $bodyExtent.StartLineNumber
            BodyEndLine     = $bodyExtent.EndLineNumber
            BodyStartOffset = $bodyExtent.StartOffset
            BodyEndOffset   = $bodyExtent.EndOffset
            BodyText        = $bodyText
            Modifiers       = @($mods)
            Calls           = @(PS-ExtractCalls $body)
            ReceiverCalls   = @(PS-ExtractReceiverCalls $body)
            MemberAccesses  = @(PS-ExtractMemberAccesses $body)
        }
    }
    $kind = 'class'
    if ($t.IsEnum) { $kind = 'enum' }
    elseif ($t.IsInterface) { $kind = 'interface' }
    $types += @{
        Name      = $t.Name
        Kind      = $kind
        BaseTypes = @($bases)
        Methods   = @($methods)
        StartLine = $t.Extent.StartLineNumber
        EndLine   = $t.Extent.EndLineNumber
    }
}

$functions = @()
$funcAsts = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)
foreach ($f in $funcAsts) {
    if ($f -is [System.Management.Automation.Language.FunctionMemberAst]) { continue }
    if (PS-IsInsideTypeDefinition $f) { continue }
    $params = ''
    if ($f.Parameters -and $f.Parameters.Count -gt 0) {
        $params = PS-FormatParams $f.Parameters
    }
    elseif ($f.Body -and $f.Body.ParamBlock -and $f.Body.ParamBlock.Parameters) {
        $params = PS-FormatParams $f.Body.ParamBlock.Parameters
    }
    $body = $f.Body
    $bodyExtent = if ($body) { $body.Extent } else { $f.Extent }
    $bodyText = if ($body) { $body.Extent.Text } else { '' }
    $functions += @{
        Name            = $f.Name
        Params          = $params
        StartLine       = $f.Extent.StartLineNumber
        EndLine         = $f.Extent.EndLineNumber
        BodyStartLine   = $bodyExtent.StartLineNumber
        BodyEndLine     = $bodyExtent.EndLineNumber
        BodyStartOffset = $bodyExtent.StartOffset
        BodyEndOffset   = $bodyExtent.EndOffset
        BodyText        = $bodyText
        Calls           = @(PS-ExtractCalls $body)
        ReceiverCalls   = @()
        MemberAccesses  = @()
    }
}

$imports = @()
$cmdAsts = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.CommandAst] }, $true)
foreach ($cmd in $cmdAsts) {
    if ($cmd.InvocationOperator -eq [System.Management.Automation.Language.TokenKind]::Dot) {
        if ($cmd.CommandElements -and $cmd.CommandElements.Count -ge 1) {
            $first = $cmd.CommandElements[0]
            if ($first -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
                $imports += @{
                    Module     = $first.Value
                    ModuleKind = 'dot_source'
                    CmdletVerb = '.'
                    Line       = $cmd.Extent.StartLineNumber
                }
            }
        }
        continue
    }
    $cmdName = $cmd.GetCommandName()
    if ($cmdName -eq 'Import-Module' -and $cmd.CommandElements -and $cmd.CommandElements.Count -ge 2) {
        $second = $cmd.CommandElements[1]
        if ($second -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $imports += @{
                Module     = $second.Value
                ModuleKind = 'Import-Module'
                CmdletVerb = 'Import'
                Line       = $cmd.Extent.StartLineNumber
            }
        }
    }
}

$usingAsts = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.UsingStatementAst] }, $true)
foreach ($u in $usingAsts) {
    $modKind = ''
    if ($u.UsingStatementKind -eq [System.Management.Automation.Language.UsingStatementKind]::Module) {
        $modKind = 'using_module'
    }
    elseif ($u.UsingStatementKind -eq [System.Management.Automation.Language.UsingStatementKind]::Namespace) {
        $modKind = 'using_namespace'
    }
    else { continue }
    $modName = ''
    if ($u.Name) { $modName = $u.Name.Value }
    elseif ($u.ModuleSpecification) { $modName = $u.ModuleSpecification.Extent.Text }
    if ([string]::IsNullOrEmpty($modName)) { continue }
    $imports += @{
        Module     = $modName
        ModuleKind = $modKind
        CmdletVerb = 'using'
        Line       = $u.Extent.StartLineNumber
    }
}

$envelope = @{
    functions = @($functions)
    types     = @($types)
    imports   = @($imports)
}
$json = $envelope | ConvertTo-Json -Depth 12 -Compress
[System.Console]::Out.WriteLine($json)
`
