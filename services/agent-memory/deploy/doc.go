// Package deploy holds validation tests for the operator-facing
// artifacts in `deploy/`. The tests are deliberately co-located
// with the artifact files so a `go test ./deploy/...` run
// catches:
//
//   - dashboard panel targets referencing a metric NAME that no
//     in-tree binary actually emits (typo / rename drift);
//   - alert-rule expressions referencing a metric NAME that no
//     in-tree binary actually emits (same drift);
//   - the §8.3 acceptance assertion that a `recall_p95_breach`
//     rule exists with the documented threshold and a traffic
//     guard so it cannot page on a single slow probe.
//
// These tests guard the implementation-plan.md Stage 8.3 test
// scenarios:
//
//   - "dashboard renders with seeded data" → JSON parses, every
//     panel target's PromQL expression resolves to a metric
//     name registered in `internal/obs/metrics.go` (or one of
//     the externally-owned names the brief lists -- e.g.
//     `agent_memory_degraded_total` from Stage 8.1 which
//     pre-dated the obs package).
//   - "alert rule fires on synthetic SLO breach" → YAML parses
//     and `recall_p95_breach` is present with the documented
//     1.5s threshold and a `> N` traffic-gate clause.
package deploy
