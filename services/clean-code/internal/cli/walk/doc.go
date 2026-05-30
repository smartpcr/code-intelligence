// Package walk implements the L1 filesystem walker that turns
// a local repo root into a deterministic stream of
// [WalkedFile] rows the parser registry can consume.
//
// The walker is the only CLI component with filesystem side
// effects. It owns:
//
//   - Recursive directory traversal of the supplied root path
//     via [filepath.WalkDir] (lexicographic per-directory
//     ordering is inherited; the walker adds no further sort).
//   - A hard-coded skip-directory baseline ([DefaultSkipDirs])
//     covering generated / vendored output that is never source.
//   - `.gitignore` and `.git/info/exclude` honouring via the
//     `github.com/go-git/go-git/v5/plumbing/format/gitignore`
//     matcher (the host-OS `~/.config/git/ignore` is
//     deliberately NOT consulted so two runs on the same tree
//     produce byte-identical artifacts regardless of host).
//   - A 2 MiB per-file size cap ([MaxFileSizeBytes]); oversize
//     files emit [SkipReasonSizeCap] without their bytes being
//     read.
//   - Extension-based language filtering via
//     [parser.DetectLanguage]; files outside the v1 pinned
//     set emit [SkipReasonUnsupportedLanguage] without a stat
//     or read.
//   - A best-effort symlink-loop guard that detects a symlinked
//     directory whose resolved target is an ancestor of the
//     current path and surfaces it as [SkipReasonSymlinkLoop].
//     [filepath.WalkDir] does NOT follow symlinks, so generic
//     symlink following is out of scope for v1; only true
//     ancestor cycles are reported.
//
// # Channel contract
//
// [Walker.Walk] returns three independent channels (files,
// skips, errs). All THREE MUST be drained concurrently by the
// consumer (typically the orchestrator); failure to drain any
// one will eventually block the walker once its small internal
// buffer fills. To cancel mid-walk the consumer cancels the
// supplied [context.Context]; the walker stops at the next
// per-entry boundary and closes all three channels.
//
// Anchors: REFACTOR-GUIDE `architecture.md` Sec 3.1 (Repo
// Walker), Sec 4.2 (WalkedFile), Sec 5.1 (Walker interface);
// `tech-spec.md` Sec 4.2 (filesystem walker), Sec 8.3 (size
// cap), Sec 8.6 (exit code 2 maps to [ErrRootNotFound]),
// constraints C8 (read-error non-fatal) and C11 (deterministic
// ordering); `implementation-plan.md` Stage 2.1 (this file).
package walk
