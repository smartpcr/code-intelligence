package ast

import "os/exec"

// PowerShellParser parses PowerShell source files via a pwsh subprocess.
// It has no build tags and compiles under both CGO=on and CGO=off.
type PowerShellParser struct {
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

// NewPowerShellParser creates a new PowerShell parser. It resolves the
// pwsh binary at construction time; if pwsh is not on PATH, subsequent
// Parse calls return ErrParserUnavailable.
func NewPowerShellParser() *PowerShellParser {
	bin, err := exec.LookPath("pwsh")
	if err != nil {
		return &PowerShellParser{}
	}
	return &PowerShellParser{pwshBin: bin}
}

func (p *PowerShellParser) Language() string     { return "powershell" }
func (p *PowerShellParser) Extensions() []string { return []string{".ps1", ".psm1", ".psd1"} }

func (p *PowerShellParser) Parse(filename string, src []byte) (ParseResult, error) {
	if p.pwshBin == "" {
		return ParseResult{}, &UnavailableError{Reason: "pwsh_not_available"}
	}
	// Real implementation invokes pwsh subprocess here.
	return ParseResult{}, nil
}
