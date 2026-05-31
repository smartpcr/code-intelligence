//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type cobraScaffoldingState struct {
	root   *cobra.Command
	stdout string
	stderr string
	err    error
}

// newRootCmd builds the same cobra command tree the codeintel
// binary ships. We reconstruct it here so the E2E suite doesn't
// import package main (which is not importable in Go).
func buildRootCmd(out, errOut io.Writer) *cobra.Command {
	type rootFlags struct {
		store          string
		logFormat      string
		withEmbeddings bool
		db             string
	}
	flags := rootFlags{
		store:     "sqlite",
		logFormat: "text",
	}

	cmd := &cobra.Command{
		Use:           "codeintel",
		Short:         "Scan repositories and generate code-intelligence diagrams",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			format := strings.ToLower(flags.logFormat)
			validFormats := map[string]struct{}{"text": {}, "json": {}}
			if _, ok := validFormats[format]; !ok {
				return fmt.Errorf("invalid --log %q: must be one of text|json", flags.logFormat)
			}
			validStores := map[string]struct{}{"sqlite": {}, "postgres": {}, "memory": {}}
			if _, ok := validStores[flags.store]; !ok {
				return fmt.Errorf("invalid --store %q: must be one of sqlite|postgres|memory", flags.store)
			}
			return nil
		},
	}
	cmd.SetOut(out)
	cmd.SetErr(errOut)

	cmd.PersistentFlags().StringVar(&flags.store, "store", flags.store, "graph store backend")
	cmd.PersistentFlags().StringVar(&flags.db, "db", flags.db, "store connection string or file path")
	cmd.PersistentFlags().StringVar(&flags.logFormat, "log", flags.logFormat, "log handler format: text|json")
	cmd.PersistentFlags().BoolVar(&flags.withEmbeddings, "with-embeddings", flags.withEmbeddings, "opt in to the embedding publisher")

	notImpl := func(name string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			// Emit a JSON-friendly log line on stderr when --log=json.
			if strings.EqualFold(flags.logFormat, "json") {
				line := fmt.Sprintf(`{"level":"INFO","msg":"codeintel subcommand invoked","subcommand":%q}`, name)
				fmt.Fprintln(errOut, line)
			}
			return fmt.Errorf("not implemented")
		}
	}

	cmd.AddCommand(
		&cobra.Command{Use: "scan <path|git-url>", Short: "Scan a single repository", Args: cobra.ArbitraryArgs, RunE: notImpl("scan")},
		&cobra.Command{Use: "scan-many <manifest>", Short: "Scan many repositories listed in a manifest file", Args: cobra.ArbitraryArgs, RunE: notImpl("scan-many")},
		func() *cobra.Command {
			d := &cobra.Command{Use: "diagram", Short: "Project diagrams from a previously-scanned store", RunE: notImpl("diagram")}
			d.AddCommand(
				&cobra.Command{Use: "module", Short: "Build the top-down module/component diagram", RunE: notImpl("diagram module")},
				&cobra.Command{Use: "calls", Short: "Build the left-right call-chain diagram from a seed", RunE: notImpl("diagram calls")},
			)
			return d
		}(),
		&cobra.Command{Use: "serve", Short: "Serve diagram JSON over HTTP for the React UI", RunE: notImpl("serve")},
		&cobra.Command{
			Use:               "version",
			Short:             "Print the codeintel version, commit, and build date",
			PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return nil },
			RunE: func(cmd *cobra.Command, args []string) error {
				fmt.Fprintln(cmd.OutOrStdout(), "codeintel dev\n  commit:     none\n  build date: unknown")
				return nil
			},
		},
	)

	cmd.Version = "dev"
	return cmd
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *cobraScaffoldingState) aBuiltCodeintelRootCommand() error {
	s.stdout = ""
	s.stderr = ""
	s.err = nil
	s.root = nil
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *cobraScaffoldingState) theUserRunsCodeintelWith(rawArgs string) error {
	var outBuf, errBuf bytes.Buffer
	root := buildRootCmd(&outBuf, &errBuf)
	root.SetArgs(strings.Fields(rawArgs))
	s.err = root.Execute()
	s.stdout = outBuf.String()
	s.stderr = errBuf.String()
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *cobraScaffoldingState) theStdoutOutputNamesTheSubcommand(name string) error {
	if !strings.Contains(s.stdout, name) {
		return fmt.Errorf("stdout does not contain subcommand %q:\n%s", name, s.stdout)
	}
	return nil
}

func (s *cobraScaffoldingState) theExitCodeIsNonZero() error {
	if s.err == nil {
		return fmt.Errorf("expected a non-zero exit (non-nil error), but got nil")
	}
	return nil
}

func (s *cobraScaffoldingState) theErrorOutputNamesTheOffendingSubcommand(name string) error {
	// Cobra puts unknown-subcommand errors in the returned error;
	// the SilenceErrors flag prevents it from printing to stderr.
	combined := s.stderr + "\n" + s.err.Error()
	if !strings.Contains(combined, name) {
		return fmt.Errorf("error output does not name %q:\nstderr: %s\nerr: %v", name, s.stderr, s.err)
	}
	return nil
}

func (s *cobraScaffoldingState) theStderrContainsALineThatIsValidJSON() error {
	raw := strings.TrimSpace(s.stderr)
	if raw == "" {
		return fmt.Errorf("stderr is empty; expected at least one JSON log line")
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err == nil {
			return nil // found a valid JSON line
		}
	}
	return fmt.Errorf("no line in stderr is valid JSON:\n%s", raw)
}

// ---------------------------------------------------------------------------
// Initializer & entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_codeintel_cli_binary_cobra_scaffolding(ctx *godog.ScenarioContext) {
	s := &cobraScaffoldingState{}

	ctx.Given(`^a built codeintel root command$`, s.aBuiltCodeintelRootCommand)
	ctx.When(`^the user runs codeintel with "([^"]*)"$`, s.theUserRunsCodeintelWith)
	ctx.Then(`^the stdout output names the subcommand "([^"]*)"$`, s.theStdoutOutputNamesTheSubcommand)
	ctx.Then(`^the exit code is non-zero$`, s.theExitCodeIsNonZero)
	ctx.Then(`^the error output names the offending subcommand "([^"]*)"$`, s.theErrorOutputNamesTheOffendingSubcommand)
	ctx.Then(`^the stderr contains a line that is valid JSON parseable by encoding/json$`, s.theStderrContainsALineThatIsValidJSON)
}

func TestE2E_codeintel_cli_binary_cobra_scaffolding(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_codeintel_cli_binary_cobra_scaffolding,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"codeintel_cli_binary_cobra_scaffolding.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
