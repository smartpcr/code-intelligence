package mgmtapi

// Production [HeadResolver] backed by `git ls-remote`. Used
// by cmd/mgmt-api when AGENT_MEMORY_HEAD_RESOLVER=git-ls-remote
// (the default in production). The dev-only StaticHeadResolver
// remains opt-in for local docker-compose stacks where a real
// git remote is not necessarily reachable.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultGitTimeout is the per-call timeout the git resolver
// applies if Resolve is not given a context with a shorter
// deadline. 15s is comfortably above the round-trip a healthy
// git host takes and below the operator-visible "stalled"
// threshold of 30s.
const DefaultGitTimeout = 15 * time.Second

// GitLsRemoteResolver implements [HeadResolver] by invoking
// `git ls-remote --refs --heads -- <repo_url> <branch>` and
// parsing the resulting SHA out of the matching `refs/heads/…`
// line.
//
// The resolver does NOT clone or fetch the repository. It
// performs the minimum-cost remote handshake that returns a
// SHA without writing to disk. For the typical OIDC-protected
// GitHub / GitLab / Bitbucket / Azure DevOps remote, that
// handshake takes ~150ms and consumes no on-disk state.
//
// Branch-only resolution
// ----------------------
// `mgmt.register(repo_url, default_branch)` and
// `mgmt.ingest({sha: ""})` both expect to resolve a BRANCH
// tip. The resolver therefore only honours `refs/heads/<name>`
// matches; if the operator-supplied name only resolves to a
// tag (or to nothing at all on the remote) the resolver
// returns [ErrHeadResolverUnknownRef]. This guards against an
// operator typing `v1.0.0` into `default_branch` and the
// system silently pinning the repo to a tag — which a later
// webhook push to the same name on a branch would not move.
//
// Process environment
// -------------------
// The resolver INHERITS the parent process's environment so
// the spawned `git` finds HOME, SSH_AUTH_SOCK, the configured
// credential helper, HTTPS_PROXY, GIT_SSL_CAINFO, and any
// other operator-configured env. User-supplied Env entries
// are appended LAST so they override the inherited value for
// the same key (Go's exec package honours last-wins).
//
// Configuration:
//   - GitPath: absolute path to the `git` binary. Empty means
//     `git` (resolved on PATH at execution time).
//   - Timeout: per-call timeout. Zero means DefaultGitTimeout.
//   - Env: extra env vars APPENDED to the inherited
//     environment. Typical uses: GIT_TERMINAL_PROMPT=0 and
//     GIT_ASKPASS=/bin/echo to make git fail-fast instead of
//     hanging on a missing-credentials prompt. The composition
//     root in cmd/mgmt-api adds GIT_TERMINAL_PROMPT=0 by
//     default.
type GitLsRemoteResolver struct {
	GitPath string
	Timeout time.Duration
	Env     []string

	// runCmd is the injection point for tests. Production
	// leaves this nil and the resolver uses os/exec directly.
	// Tests inject a fake to avoid spawning a real git
	// process.
	runCmd func(ctx context.Context, name string, env []string, args ...string) (stdout, stderr []byte, err error)
}

// Resolve implements [HeadResolver]. Returns the SHA of the
// branch tip on the remote, or one of:
//
//   - ErrHeadResolverUnknownRef when the remote responds but
//     does not contain a branch matching the requested name
//     (the wire equivalent of "branch not found"). A tag with
//     the same name does NOT count — only branches. Handler
//     maps this to 400.
//   - ErrHeadResolverUnavailable for any other failure
//     (network, git binary missing, git error, garbage
//     output). Handler maps to 502.
//
// The returned SHA is normalised to lower-case to match the
// dedupe constraint on `ingest_jobs`.
func (r *GitLsRemoteResolver) Resolve(ctx context.Context, repoURL, defaultBranch string) (string, error) {
	branch, err := normalizeRef(defaultBranch)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrHeadResolverUnknownRef, err)
	}
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return "", fmt.Errorf("%w: repo_url required", ErrHeadResolverUnavailable)
	}

	name := r.GitPath
	if name == "" {
		name = "git"
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultGitTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// --refs filters out the `^{}` annotated-tag-peel lines.
	// --heads scopes the output to branches only; refs/tags/*
	// / refs/pull/* / refs/notes/* etc never appear. Passing
	// the branch name as a refspec further reduces network
	// bytes — the remote only sends matching refs.
	//
	// SECURITY: the `--` separator is REQUIRED here. Even
	// though exec.CommandContext avoids shell expansion, git
	// itself interprets any positional argument that starts
	// with `-` as a flag — so a `repoURL` like
	// `--upload-pack=cmd` (defeating the scheme allowlist
	// via some future URL form) or a `branch` like
	// `--upload-pack=cmd` (normalizeRef only trims
	// whitespace) would otherwise be parsed as options and
	// could trigger remote-code execution on the SSH path or
	// other unintended git behaviour. The `--` terminator
	// forces git to treat every subsequent token as a
	// positional argument. This is standard git-safety
	// practice for any user-supplied repo or refspec.
	args := []string{"ls-remote", "--refs", "--heads", "--", repoURL, branch}

	var stdout, stderr []byte
	if r.runCmd != nil {
		stdout, stderr, err = r.runCmd(rctx, name, r.Env, args...)
	} else {
		stdout, stderr, err = runGitLsRemote(rctx, name, r.Env, args)
	}
	if err != nil {
		// Distinguish "remote unreachable" / "binary missing"
		// from "remote reachable but ref not found". git
		// exits 0 with empty output when the ref doesn't
		// exist, so any non-zero exit AND any os/exec error
		// is a resolver outage.
		return "", fmt.Errorf("%w: git ls-remote: %v: %s",
			ErrHeadResolverUnavailable, err, strings.TrimSpace(string(stderr)))
	}
	sha, err := parseLsRemote(stdout, branch)
	if err != nil {
		return "", err
	}
	if !IsHexGitSHA(sha) {
		return "", fmt.Errorf("%w: git ls-remote produced non-SHA %q",
			ErrHeadResolverUnavailable, sha)
	}
	return sha, nil
}

// runGitLsRemote is the production execution path. Captures
// stdout / stderr, runs synchronously, and returns the buffers
// along with any os/exec error.
//
// The parent process's environment is INHERITED so the spawned
// git finds HOME / SSH_AUTH_SOCK / credential helpers / proxy
// / CA-related env. User-supplied env entries are APPENDED so
// they override on a per-key basis (Go's exec package honours
// last-wins for Env).
func runGitLsRemote(ctx context.Context, name string, env []string, args []string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	merged := append([]string(nil), os.Environ()...)
	if len(env) > 0 {
		merged = append(merged, env...)
	}
	cmd.Env = merged
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	runErr := cmd.Run()
	return so.Bytes(), se.Bytes(), runErr
}

// parseLsRemote scans the output of `git ls-remote ...` for
// the SHA matching `refs/heads/<branch>`. A `refs/tags/<branch>`
// match is deliberately NOT honored: `mgmt.register(repo_url,
// default_branch)` resolves a BRANCH tip, not a tag. If the
// caller wants a tagged revision they must supply the SHA
// explicitly on `mgmt.ingest`.
//
// `git ls-remote` output lines are SHA<TAB>refname, one per
// matching ref. Empty stdout means the remote was reachable
// but contained no matching ref — that's ErrHeadResolverUnknownRef.
func parseLsRemote(out []byte, branch string) (string, error) {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("%w: branch %q not found on remote", ErrHeadResolverUnknownRef, branch)
	}
	refHead := "refs/heads/" + branch

	var headSHA string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// git ls-remote uses a literal tab; some shells /
		// CRLF terminals can mangle that, so we accept any
		// run of whitespace via strings.Fields.
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha, ref := strings.ToLower(fields[0]), fields[1]
		if ref == refHead {
			headSHA = sha
		}
	}
	if headSHA == "" {
		return "", fmt.Errorf("%w: branch %q not found on remote", ErrHeadResolverUnknownRef, branch)
	}
	return headSHA, nil
}

// ensure the resolver exposes the documented errors so type
// assertions in handler.go still compile if the sentinels are
// ever renamed without a callsite update.
var _ = []error{ErrHeadResolverUnavailable, ErrHeadResolverUnknownRef}
