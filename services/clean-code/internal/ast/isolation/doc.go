// Package isolation wraps the Stage 2.1 Tree-sitter parser
// fleet (`internal/ast/parser`) with crash-isolation
// machinery so a misbehaving grammar (OOM, segfault,
// runtime panic) does NOT take the Metric Ingestor process
// down with it. The package also provides the
// mgmt.set_mode(repo_id, mode) drain coordinator that lets
// in-flight scans complete under the OLD mode before new
// scans pick up the NEW mode.
//
// # Architecture pin (Sec 9.2)
//
//	"Run tree-sitter parses in a subprocess worker pool with
//	 crash isolation even in embedded mode -- 'embedded'
//	 means in-pod, not in-thread. Worker crashes are counted
//	 as `parse_panics_total` metric; threshold triggers an
//	 automatic flip to `linked` mode via the Management
//	 surface."
//
// # Brief (implementation-plan Stage 9.3)
//
//   - Wrap the parser fleet in a per-language subprocess pool
//     with rlimit-bounded memory and a hard timeout.
//   - On `mgmt.set_mode(repo_id, mode)` between `embedded`
//     and `linked`, drain in-flight scans for the repo before
//     flipping; new scans pick up the new mode.
//   - Cover OOM-in-subprocess + mode-flip-drain in this
//     package's tests.
//
// # Components
//
//   - [Mode] / [ModeCoordinator] / [ScanToken] -- the per-repo
//     admission + drain primitive used by the management
//     surface and the scan dispatcher.
//   - [Worker] / [Pool] / [SubprocessConfig] -- the
//     per-language subprocess pool that wraps actual parser
//     execution. The default [Worker] is an `exec.Cmd`-backed
//     worker that re-execs the host binary as a child process
//     and applies an `RLIMIT_AS` cap (on Unix; documented
//     no-op on Windows). Tests inject a fake [Worker] via
//     [WorkerFactory].
//   - [WrapParser] -- the adapter that routes an existing
//     `parser.Parser` (Stage 2.1) through the isolation pool
//     so the seam is real and observable end-to-end.
//
// # Error mapping
//
// All failure modes map to typed sentinels carried by
// [*ParserCrashError]:
//
//   - [ErrParserOOM]     -- subprocess exceeded its memory
//     budget. Distinguished from a generic crash by exit
//     status (SIGKILL with rlimit applied) or stderr text
//     ("out of memory", "runtime: out of memory", "killed").
//   - [ErrParserTimeout] -- subprocess exceeded the hard
//     wall-clock budget; ctx.DeadlineExceeded distinguishes
//     this from caller-cancelled.
//   - [ErrParserCrash]   -- any other abnormal exit. Stderr
//     and exit code are preserved on [*ParserCrashError] for
//     operator diagnosis.
//
// Callers use `errors.Is(err, ErrParserOOM)` etc.
//
// # Wiring scope
//
// This package ships the seam and its tests. Wiring the
// Metric Ingestor's scan loop to call
// [ModeCoordinator.BeginScan]/[ModeCoordinator.EndScan]
// around every scan is the Stage 10.x integration follow-up;
// the brief test file [subprocess_test.go] proves the
// primitive in isolation.
package isolation
