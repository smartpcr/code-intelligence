package mgmtapi

// Unit tests for the GitLsRemoteResolver. We never spawn a
// real `git` binary — the resolver exposes a `runCmd` hook
// for tests so the test process can simulate every `git
// ls-remote` outcome (success, exit non-zero, garbage, empty,
// timeout) deterministically and quickly.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestGitLsRemoteResolver_success_returnsSHA(t *testing.T) {
	t.Parallel()
	const wantSHA = "abcdefabcdefabcdefabcdefabcdefabcdef1234"
	r := &GitLsRemoteResolver{
		runCmd: func(_ context.Context, _ string, _ []string, args ...string) ([]byte, []byte, error) {
			// Sanity check: the resolver issues the exact
			// command shape we promised in the comment.
			if got, want := args[0], "ls-remote"; got != want {
				t.Errorf("args[0] = %q, want %q", got, want)
			}
			out := wantSHA + "\trefs/heads/main\n"
			return []byte(out), nil, nil
		},
	}
	sha, err := r.Resolve(context.Background(), "https://git.example/acme/svc", "main")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sha != wantSHA {
		t.Errorf("sha = %q, want %q", sha, wantSHA)
	}
}

func TestGitLsRemoteResolver_branchPreferredOverTag(t *testing.T) {
	t.Parallel()
	const headSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const tagSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	r := &GitLsRemoteResolver{
		runCmd: func(_ context.Context, _ string, _ []string, _ ...string) ([]byte, []byte, error) {
			out := tagSHA + "\trefs/tags/release\n" +
				headSHA + "\trefs/heads/release\n"
			return []byte(out), nil, nil
		},
	}
	sha, err := r.Resolve(context.Background(), "https://git.example/svc", "release")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sha != headSHA {
		t.Errorf("sha = %q, want %q (head must beat tag)", sha, headSHA)
	}
}

func TestGitLsRemoteResolver_tagOnly_rejected(t *testing.T) {
	t.Parallel()
	// `mgmt.register(repo_url, default_branch)` resolves a
	// BRANCH tip, not a tag. If the operator-supplied name
	// only resolves to a tag on the remote (e.g. they typed
	// "v1.0.0" into default_branch), the resolver must
	// return UnknownRef — pinning the repo to a tag would
	// silently break webhook-driven re-ingest (a later push
	// to a branch with the same name would not move the
	// tag, and the registered SHA would never update).
	const tagSHA = "ccccccccccccccccccccccccccccccccccccccccdddddddddddddddddddddddd"
	r := &GitLsRemoteResolver{
		runCmd: func(_ context.Context, _ string, _ []string, args ...string) ([]byte, []byte, error) {
			// Defensive check: the resolver MUST NOT pass
			// --tags now that tags are not accepted; if a
			// future edit re-adds --tags the assertion below
			// catches it before garbage propagates.
			for _, a := range args {
				if a == "--tags" {
					t.Errorf("ls-remote args must not contain --tags, got %v", args)
				}
			}
			// Even though the resolver doesn't ask for
			// tags, an evil remote could still send a tag
			// line in response — we MUST ignore it.
			out := tagSHA + "\trefs/tags/v1.0.0\n"
			return []byte(out), nil, nil
		},
	}
	_, err := r.Resolve(context.Background(), "https://git.example/svc", "v1.0.0")
	if !errors.Is(err, ErrHeadResolverUnknownRef) {
		t.Fatalf("err = %v, want ErrHeadResolverUnknownRef (tag must not satisfy a branch lookup)", err)
	}
}

func TestGitLsRemoteResolver_branchNotFound_returnsUnknownRef(t *testing.T) {
	t.Parallel()
	r := &GitLsRemoteResolver{
		runCmd: func(_ context.Context, _ string, _ []string, _ ...string) ([]byte, []byte, error) {
			// git ls-remote exits 0 with empty stdout when the
			// remote responds but the ref doesn't exist.
			return []byte{}, nil, nil
		},
	}
	_, err := r.Resolve(context.Background(), "https://git.example/svc", "no-such-branch")
	if !errors.Is(err, ErrHeadResolverUnknownRef) {
		t.Fatalf("err = %v, want ErrHeadResolverUnknownRef", err)
	}
}

func TestGitLsRemoteResolver_gitExitsNonZero_returnsUnavailable(t *testing.T) {
	t.Parallel()
	r := &GitLsRemoteResolver{
		runCmd: func(_ context.Context, _ string, _ []string, _ ...string) ([]byte, []byte, error) {
			return nil, []byte("fatal: unable to access 'https://git.example/svc': could not resolve host"),
				fmt.Errorf("exit status 128")
		},
	}
	_, err := r.Resolve(context.Background(), "https://git.example/svc", "main")
	if !errors.Is(err, ErrHeadResolverUnavailable) {
		t.Fatalf("err = %v, want ErrHeadResolverUnavailable", err)
	}
	if !strings.Contains(err.Error(), "could not resolve host") {
		t.Errorf("err = %v, want substring 'could not resolve host'", err)
	}
}

func TestGitLsRemoteResolver_garbageOutput_returnsUnavailable(t *testing.T) {
	t.Parallel()
	r := &GitLsRemoteResolver{
		runCmd: func(_ context.Context, _ string, _ []string, _ ...string) ([]byte, []byte, error) {
			// Non-SHA value where the SHA should be.
			return []byte("not-a-sha\trefs/heads/main\n"), nil, nil
		},
	}
	_, err := r.Resolve(context.Background(), "https://git.example/svc", "main")
	if !errors.Is(err, ErrHeadResolverUnavailable) {
		t.Fatalf("err = %v, want ErrHeadResolverUnavailable", err)
	}
}

func TestGitLsRemoteResolver_emptyBranch_returnsUnknownRef(t *testing.T) {
	t.Parallel()
	r := &GitLsRemoteResolver{}
	_, err := r.Resolve(context.Background(), "https://git.example/svc", "   ")
	if !errors.Is(err, ErrHeadResolverUnknownRef) {
		t.Fatalf("err = %v, want ErrHeadResolverUnknownRef", err)
	}
}

func TestGitLsRemoteResolver_emptyRepoURL_returnsUnavailable(t *testing.T) {
	t.Parallel()
	r := &GitLsRemoteResolver{}
	_, err := r.Resolve(context.Background(), "  ", "main")
	if !errors.Is(err, ErrHeadResolverUnavailable) {
		t.Fatalf("err = %v, want ErrHeadResolverUnavailable", err)
	}
}

func TestParseLsRemote_caseFolded(t *testing.T) {
	t.Parallel()
	// `git ls-remote` always emits lower-case SHAs in
	// practice, but we lowercase defensively in the parser
	// since the dedupe index is case-sensitive.
	out := []byte("ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEF1234\trefs/heads/main\n")
	sha, err := parseLsRemote(out, "main")
	if err != nil {
		t.Fatalf("parseLsRemote: %v", err)
	}
	if sha != "abcdefabcdefabcdefabcdefabcdefabcdef1234" {
		t.Errorf("sha = %q, want lower-cased", sha)
	}
}

func TestParseLsRemote_multipleRefs_picksBranch(t *testing.T) {
	t.Parallel()
	out := []byte(
		"1111111111111111111111111111111111111111\trefs/heads/feature/x\n" +
			"2222222222222222222222222222222222222222\trefs/heads/main\n" +
			"3333333333333333333333333333333333333333\trefs/tags/v1.0\n",
	)
	sha, err := parseLsRemote(out, "main")
	if err != nil {
		t.Fatalf("parseLsRemote: %v", err)
	}
	if sha != "2222222222222222222222222222222222222222" {
		t.Errorf("sha = %q, want main's SHA", sha)
	}
}

func TestParseLsRemote_branchAndTagWithSameName_picksBranch(t *testing.T) {
	t.Parallel()
	// Repo has both refs/heads/release and refs/tags/release.
	// The resolver must return the branch SHA. Even if a
	// future maintainer accidentally re-introduces `--tags`
	// on the ls-remote args, this test guarantees the branch
	// still wins (and that the tag-only path is gone, since
	// TestGitLsRemoteResolver_tagOnly_rejected pins the
	// other direction).
	out := []byte(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\trefs/tags/release\n" +
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\trefs/heads/release\n",
	)
	sha, err := parseLsRemote(out, "release")
	if err != nil {
		t.Fatalf("parseLsRemote: %v", err)
	}
	if sha != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("sha = %q, want refs/heads/release SHA", sha)
	}
}

func TestRunGitLsRemote_inheritsParentEnv(t *testing.T) {
	// Cannot t.Parallel() — t.Setenv would panic. The test
	// itself is fast (one shell-out) so this is fine.
	// Item 2 (iter 3): the production execution path must
	// inherit os.Environ() so the spawned `git` finds HOME,
	// SSH_AUTH_SOCK, credential helpers, proxy vars, and any
	// other operator-configured env. We assert this by
	// shelling out to `cmd` / `sh` (every CI has one) and
	// echoing a marker env var that we set on this process.
	//
	// We do NOT need a real git binary for this test — we're
	// asserting the env-merging contract of runGitLsRemote,
	// not the resolver itself. The function takes a binary
	// name as its first non-context argument, so we point it
	// at the platform-native shell.
	const marker = "AGENT_MEMORY_TEST_PARENT_ENV_MARKER"
	t.Setenv(marker, "yes-please")

	bin, args := shellEcho(marker)
	stdout, stderr, err := runGitLsRemote(context.Background(), bin, nil, args)
	if err != nil {
		t.Fatalf("runGitLsRemote: %v (stderr=%s)", err, string(stderr))
	}
	got := strings.TrimSpace(string(stdout))
	if !strings.Contains(got, "yes-please") {
		t.Errorf("stdout = %q, want it to contain %q (parent env was not inherited)", got, "yes-please")
	}
}

func TestRunGitLsRemote_userEnvOverridesInherited(t *testing.T) {
	// Cannot t.Parallel() — t.Setenv would panic.
	// Last-wins semantics: when the caller supplies an env
	// entry with the same key as one in os.Environ(), the
	// child should see the caller's value. This is how the
	// composition root in cmd/mgmt-api forces
	// GIT_TERMINAL_PROMPT=0 even when the operator's shell
	// had it set to "1".
	const marker = "AGENT_MEMORY_TEST_OVERRIDE_MARKER"
	t.Setenv(marker, "inherited-value")

	bin, args := shellEcho(marker)
	stdout, _, err := runGitLsRemote(context.Background(), bin, []string{marker + "=overridden"}, args)
	if err != nil {
		t.Fatalf("runGitLsRemote: %v", err)
	}
	got := strings.TrimSpace(string(stdout))
	if !strings.Contains(got, "overridden") {
		t.Errorf("stdout = %q, want it to contain %q (caller env did not override)", got, "overridden")
	}
}

// shellEcho returns (bin, args) that will echo the value of
// the named environment variable to stdout. Portable across
// Windows (cmd.exe) and Unix (sh). The caller passes the
// raw variable name (no $ prefix).
func shellEcho(envVarName string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "echo %" + envVarName + "%"}
	}
	return "sh", []string{"-c", "echo $" + envVarName}
}