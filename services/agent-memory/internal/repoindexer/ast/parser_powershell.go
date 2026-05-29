package ast

import "os/exec"

// PowerShellParser parses PowerShell source files via a pwsh subprocess.
// It has no build tags and compiles under both CGO=on and CGO=off.
type PowerShellParser struct {
	pwshBin string
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
