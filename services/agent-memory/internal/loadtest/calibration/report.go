package calibration

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// Report is the calibration run's wire- and disk-portable
// result. The harness fills it in during Run() and the Render
// methods turn it into a markdown artifact the operator pastes
// into a §8.3 override story (or reviews in place at
// `docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md`).
//
// A Report is plain data: tests construct one directly and
// assert against [Report.RenderMarkdown] / [Report.WriteFile].
type Report struct {
	// ProfileName mirrors the LoadProfile.Name the harness
	// drove ("nominal", "smoke", "burst-1min"). Reviewers
	// reject an artifact stamped "smoke" as a release gate.
	ProfileName string

	// StartedAt / FinishedAt frame the wall-clock run window
	// so a reviewer can correlate the artifact against
	// concurrent operator activity (Grafana, OTel traces).
	StartedAt    time.Time
	FinishedAt   time.Time

	// PlannedDuration is what the operator asked for;
	// ActualDuration is what the harness measured between
	// StartedAt and the moment all scenarios drained.
	// Reporting both surfaces a "harness clamped early"
	// regression.
	PlannedDuration time.Duration
	ActualDuration  time.Duration

	// RepoID / SeededFixtureLOC mirror the Config fields so
	// the artifact records the fixture the numbers came
	// from (a calibration on a 5-file fixture is meaningless
	// against the §8.3 envelope).
	RepoID           string
	SeededFixtureLOC int

	// RandomSeed is the rng seed the harness used. Echoing
	// it lets an operator replay a flaky run by passing
	// `--seed <value>` back to the binary.
	RandomSeed int64

	// ErrorBudgetRatio is the cap copied from the profile so
	// the artifact is self-contained ("no verb exceeded the
	// 1 % budget").
	ErrorBudgetRatio float64

	// Verbs is one VerbResult per scenario the harness drove.
	// Ordering matches the LoadProfile.Verbs ordering for
	// stable diffs across runs.
	Verbs []VerbResult

	// LearningQuality carries the two §8.3 SLO measurements
	// (rank-of-correct-node + concept-hit fraction). Surface
	// is always populated; an empty LabeledQueries set
	// produces `Evaluated == 0` and the renderer prints "n/a".
	LearningQuality LearningQualityResult

	// Notes is a free-form list of operator-visible
	// observations the harness emits (e.g., "fell back to
	// synthetic context_id pool", "scenario X recorded N
	// dropped ticks"). Rendered as a bullet list at the
	// bottom of the artifact so the operator can scan for
	// caveats without re-running.
	Notes []string

	// BudgetBreaches is the closed set of verb names that
	// exceeded ErrorBudgetRatio. The harness binary returns
	// non-zero when this is non-empty; the Stage 8.4
	// acceptance scenario 1 says "no verb errored above the
	// 1 % budget".
	BudgetBreaches []string

	// Aborted is true when the caller cancelled the run
	// before the planned duration elapsed (SIGINT/SIGTERM
	// or explicit `context.CancelFunc`). When set, the
	// per-verb percentile/budget numbers are over a partial
	// sample window and must NOT be treated as a §8.3
	// calibration result. The harness binary returns a
	// distinct exit code so CI does not file an aborted
	// run as a passing baseline.
	Aborted bool

	// CompletionReason is a short, machine-readable label
	// describing why the run ended ("completed",
	// "aborted-context-cancelled", "aborted-deadline-exceeded"
	// when the caller's parent deadline fires before the
	// harness's planned-duration deadline). Always set.
	CompletionReason string

	// Provenance is the operator-supplied tag the harness
	// stamps onto the artifact's prominent top-of-file
	// banner. Mirrors [Config.Provenance]; rendered by
	// [Report.RenderMarkdown] as a callout block immediately
	// after the title so any reviewer can spot whether the
	// numbers came from a real deploy/local-stack calibration
	// or an in-process baseline.
	Provenance string
}

// VerbResult is the per-verb slice of a Report.
type VerbResult struct {
	Verb            string
	MetricName      string
	RequestedRPS    float64
	AchievedRPS     float64
	PlannedRequests int
	Sent            int
	Succeeded       int
	Failed          int
	DroppedTicks    int
	ErrorRatio      float64
	P50             time.Duration
	P95             time.Duration
	P99             time.Duration
	Min             time.Duration
	Max             time.Duration
	SLO95Seconds    float64
	SLO99Seconds    float64
	// SLO95Met is true when P95 <= SLO95Seconds.
	SLO95Met bool
	// SLO99Met is true when P99 <= SLO99Seconds.
	SLO99Met bool
	// BudgetMet is true when ErrorRatio <= profile.ErrorBudgetRatio.
	BudgetMet bool
}

// LearningQualityResult is the §8.3 learning-quality slice.
// Both metrics are reported even when LabeledQueries is empty
// (Evaluated == 0, both metrics are math.NaN, the renderer
// prints "n/a (no labelled queries supplied)").
type LearningQualityResult struct {
	K                            int
	Evaluated                    int
	MedianRankOfCorrectNodeAtK   float64
	ConceptHitFractionAtK        float64
	MaxMedianRankAtK             int
	MinConceptHitFractionAtK     float64
	RankMet                      bool
	ConceptHitMet                bool
	// SLOSource MUST be "labelled-query proxy" — the harness
	// has no Episode/Observation feedback loop. Surfaces the
	// proxy provenance to the artifact so a reviewer reads the
	// right number against the §8.3 contract.
	SLOSource string
}

// Percentiles computes p50/p95/p99 (in seconds) from raw
// sample latencies. The slice may be mutated (sorted in
// place) — callers that need to preserve order should pass a
// copy. Empty input returns zeroes.
//
// Percentile indexing is the nearest-rank method (Excel-style
// PERCENTILE.INC equivalent for these three points), defined
// here as `idx = ceil(p * n) - 1`. This matches the
// definition used by `histogram_quantile()` for cumulative
// histograms with infinitely many buckets and produces stable
// percentiles even at very small N (e.g. 10 samples ⇒
// p95 = sample[ceil(0.95*10)-1] = sample[9]).
func Percentiles(latencies []time.Duration) (p50, p95, p99 time.Duration) {
	n := len(latencies)
	if n == 0 {
		return 0, 0, 0
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return latencies[percentileIndex(n, 0.50)],
		latencies[percentileIndex(n, 0.95)],
		latencies[percentileIndex(n, 0.99)]
}

func percentileIndex(n int, p float64) int {
	if n <= 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return idx
}

// AggregateVerb computes the VerbResult percentiles, ratios,
// and SLO/budget flags from raw sample data. budgetRatio is
// the profile-wide error-budget cap (0.01 = 1 %); the harness
// also tracks BudgetBreaches at the Report level.
//
// Two duration inputs:
//   - plannedDuration is the operator-requested window. Used
//     to compute PlannedRequests (the §8.3 expected arrival
//     count) so a drain-extended ActualDuration does NOT
//     inflate the denominator and silently shrink the
//     reported planned-vs-sent gap.
//   - actualDuration is the wall-clock window the harness
//     observed (StartedAt → FinishedAt, post-drain). Used to
//     compute AchievedRPS so the "I sent N requests over T
//     seconds" rate is honest.
func AggregateVerb(verb reliability.VerbProfile, latencies []time.Duration, succeeded, failed, dropped int, plannedDuration, actualDuration time.Duration, budgetRatio float64) VerbResult {
	sent := succeeded + failed
	var errorRatio float64
	if sent > 0 {
		errorRatio = float64(failed) / float64(sent)
	}
	var achieved float64
	if actualDuration > 0 {
		achieved = float64(sent) / actualDuration.Seconds()
	}
	p50, p95, p99 := Percentiles(latencies)
	var minVal, maxVal time.Duration
	if len(latencies) > 0 {
		// Percentiles() left latencies sorted ascending.
		minVal = latencies[0]
		maxVal = latencies[len(latencies)-1]
	}
	return VerbResult{
		Verb:            verb.Verb,
		MetricName:      verb.MetricName,
		RequestedRPS:    verb.SustainedRPS,
		AchievedRPS:     achieved,
		PlannedRequests: verb.PlannedRequests(plannedDuration),
		Sent:            sent,
		Succeeded:       succeeded,
		Failed:          failed,
		DroppedTicks:    dropped,
		ErrorRatio:      errorRatio,
		P50:             p50,
		P95:             p95,
		P99:             p99,
		Min:             minVal,
		Max:             maxVal,
		SLO95Seconds:    verb.SLO95Seconds,
		SLO99Seconds:    verb.SLO99Seconds,
		SLO95Met:        p95.Seconds() <= verb.SLO95Seconds,
		SLO99Met:        p99.Seconds() <= verb.SLO99Seconds,
		BudgetMet:       errorRatio <= budgetRatio,
	}
}

// AggregateLearningQuality computes the §8.3 learning-quality
// summary from per-recall measurements. ranks is the sorted-
// later (we sort internally) slice of rank-of-correct-node
// integers (1-based; 0 or > K means "not found in top K");
// conceptHits is the slice of bool "did any expected concept
// appear in the top K" flags.
func AggregateLearningQuality(targets reliability.LearningQualityTargets, ranks []int, conceptHits []bool) LearningQualityResult {
	res := LearningQualityResult{
		K:                            targets.K,
		MaxMedianRankAtK:             targets.MaxMedianRankAtK,
		MinConceptHitFractionAtK:     targets.MinConceptHitFractionAtK,
		MedianRankOfCorrectNodeAtK:   math.NaN(),
		ConceptHitFractionAtK:        math.NaN(),
		SLOSource:                    "labelled-query proxy",
	}
	if len(ranks) == 0 && len(conceptHits) == 0 {
		return res
	}
	res.Evaluated = max(len(ranks), len(conceptHits))
	if len(ranks) > 0 {
		// Treat 0 / out-of-K as the worst possible rank
		// (K + 1) so a missed query does not silently
		// improve the median.
		bucket := make([]int, len(ranks))
		for i, r := range ranks {
			if r <= 0 || r > targets.K {
				bucket[i] = targets.K + 1
			} else {
				bucket[i] = r
			}
		}
		sort.Ints(bucket)
		median := float64(bucket[len(bucket)/2])
		if len(bucket)%2 == 0 {
			median = (float64(bucket[len(bucket)/2-1]) + float64(bucket[len(bucket)/2])) / 2
		}
		res.MedianRankOfCorrectNodeAtK = median
		res.RankMet = median <= float64(targets.MaxMedianRankAtK)
	}
	if len(conceptHits) > 0 {
		hits := 0
		for _, h := range conceptHits {
			if h {
				hits++
			}
		}
		frac := float64(hits) / float64(len(conceptHits))
		res.ConceptHitFractionAtK = frac
		res.ConceptHitMet = frac >= targets.MinConceptHitFractionAtK
	}
	return res
}

// RenderMarkdown produces the artifact body. The output:
//   - opens with a YAML-style front-matter so a downstream
//     parser can ingest the headline numbers without a markdown
//     parser;
//   - lists every verb in a single table (one row per verb);
//   - calls out budget breaches in a "Status" line so a CI
//     reviewer spots a regression without reading the table;
//   - reports learning-quality SLOs with the proxy provenance
//     label per §8.3;
//   - closes with operator notes.
//
// The output is intentionally stable across runs (timestamps
// aside) so a diff between two calibration artifacts is
// meaningful.
func (r Report) RenderMarkdown() string {
	var b strings.Builder
	b.WriteString("# Load-test calibration — iter 1\n\n")

	// Provenance banner — rendered FIRST so a reviewer
	// scanning the artifact cannot miss the distinction
	// between an in-process stub baseline and a real
	// deploy/local-stack §8.3 calibration. Empty Provenance
	// suppresses the banner entirely.
	if strings.TrimSpace(r.Provenance) != "" {
		b.WriteString("> ⚠ **Provenance:** ")
		b.WriteString(r.Provenance)
		b.WriteString("\n>\n")
		b.WriteString("> See `docs/code-intelligence/agent-memory/load-test-calibration.md`\n")
		b.WriteString("> for the operator workflow that distinguishes shape-baseline\n")
		b.WriteString("> artifacts (CI / PR refresh) from §8.3 production-seal\n")
		b.WriteString("> calibrations (deploy/local stack, seeded 200 k LOC fixture,\n")
		b.WriteString("> 30-minute window).\n")
		// When the banner flags a non-production-seal run
		// (in-process baseline, synthetic seed, or smoke
		// preflight), surface a pointer to the
		// operator-action-items tracker so the gap between
		// "engineering-complete in-process artifact" and the
		// Stage 8.4 production-seal acceptance criterion is
		// visible from the artifact itself, not just from
		// the implementation plan. The substring match is
		// deliberately broad — any non-empty provenance that
		// is NOT explicitly stamped DEPLOY/LOCAL or
		// PRODUCTION SEAL is treated as pending.
		if isPendingProductionSeal(r.Provenance) {
			b.WriteString(">\n")
			b.WriteString("> **Production-seal artifact pending.** This run does NOT\n")
			b.WriteString("> satisfy the Stage 8.4 acceptance criterion at\n")
			b.WriteString("> `docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md:1479-1486`.\n")
			b.WriteString("> The operator action that produces the production-seal\n")
			b.WriteString("> artifact is tracked in\n")
			b.WriteString("> `docs/stories/code-intelligence-AGENT-MEMORY/operator-action-items.md`.\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("> **Generator.** This file is written by the `loadtest-harness` binary\n")
	b.WriteString("> (`services/agent-memory/cmd/loadtest-harness`). It is the **Stage 8.4**\n")
	b.WriteString("> calibration artifact described in\n")
	b.WriteString("> `docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md` §8.4.\n")
	b.WriteString("> The values below are **informational** — the operator pins post-\n")
	b.WriteString("> calibration SLO numbers into tech-spec.md §8.3 via the §8.3 override\n")
	b.WriteString("> route, not by editing this file.\n\n")

	// Front matter.
	b.WriteString("```yaml\n")
	fmt.Fprintf(&b, "profile: %s\n", yamlString(r.ProfileName))
	fmt.Fprintf(&b, "started_at: %s\n", r.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "finished_at: %s\n", r.FinishedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "planned_duration: %s\n", r.PlannedDuration)
	fmt.Fprintf(&b, "actual_duration: %s\n", r.ActualDuration)
	fmt.Fprintf(&b, "repo_id: %s\n", yamlString(r.RepoID))
	fmt.Fprintf(&b, "seeded_fixture_loc: %d\n", r.SeededFixtureLOC)
	fmt.Fprintf(&b, "random_seed: %d\n", r.RandomSeed)
	fmt.Fprintf(&b, "error_budget_ratio: %g\n", r.ErrorBudgetRatio)
	fmt.Fprintf(&b, "budget_breaches: %d\n", len(r.BudgetBreaches))
	fmt.Fprintf(&b, "aborted: %t\n", r.Aborted)
	if r.CompletionReason != "" {
		fmt.Fprintf(&b, "completion_reason: %s\n", yamlString(r.CompletionReason))
	}
	if strings.TrimSpace(r.Provenance) != "" {
		fmt.Fprintf(&b, "provenance: %s\n", yamlString(r.Provenance))
	}
	b.WriteString("```\n\n")

	// Status line — scanner-friendly.
	status := "PASS"
	if len(r.BudgetBreaches) > 0 {
		status = "FAIL"
	}
	if r.Aborted {
		status = "ABORTED"
	}
	fmt.Fprintf(&b, "**Status:** %s — %s\n\n", status, statusReason(r))

	// Verb table.
	b.WriteString("## Per-verb percentiles\n\n")
	b.WriteString("| Verb | Requested RPS | Achieved RPS | Sent | Failed | Err % | p50 | p95 | p99 | SLO p95 | SLO p99 | SLO p95 met | SLO p99 met | Budget met |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | :---: | :---: | :---: |\n")
	for _, v := range r.Verbs {
		fmt.Fprintf(&b,
			"| `%s` | %.3f | %.3f | %d | %d | %.3f | %s | %s | %s | %.3fs | %.3fs | %s | %s | %s |\n",
			v.Verb,
			v.RequestedRPS, v.AchievedRPS,
			v.Sent, v.Failed,
			v.ErrorRatio*100,
			fmtMs(v.P50), fmtMs(v.P95), fmtMs(v.P99),
			v.SLO95Seconds, v.SLO99Seconds,
			yesNo(v.SLO95Met), yesNo(v.SLO99Met), yesNo(v.BudgetMet),
		)
	}
	b.WriteString("\n")

	// Open-loop hygiene table — dropped ticks visible.
	hasDropped := false
	for _, v := range r.Verbs {
		if v.DroppedTicks > 0 {
			hasDropped = true
			break
		}
	}
	if hasDropped {
		b.WriteString("## Open-loop scheduler hygiene\n\n")
		b.WriteString("Dropped ticks indicate the scenario could not keep up with the requested arrival rate within the per-verb in-flight cap. Re-run with `--max-inflight` raised or investigate downstream saturation.\n\n")
		b.WriteString("| Verb | Planned | Sent | Dropped |\n| --- | ---: | ---: | ---: |\n")
		for _, v := range r.Verbs {
			fmt.Fprintf(&b, "| `%s` | %d | %d | %d |\n", v.Verb, v.PlannedRequests, v.Sent, v.DroppedTicks)
		}
		b.WriteString("\n")
	}

	// Learning-quality.
	b.WriteString("## Learning-quality SLOs\n\n")
	b.WriteString("Source: **")
	if r.LearningQuality.SLOSource != "" {
		b.WriteString(r.LearningQuality.SLOSource)
	} else {
		b.WriteString("labelled-query proxy")
	}
	b.WriteString("** (§8.3's contract definition is a post-hoc join over\n")
	b.WriteString("`Observation` × `RecallContextLog`; the harness measures the proxy on the\n")
	b.WriteString("recall response payload).\n\n")
	fmt.Fprintf(&b, "- **K =** %d\n", r.LearningQuality.K)
	fmt.Fprintf(&b, "- **Labelled queries evaluated:** %d\n", r.LearningQuality.Evaluated)
	fmt.Fprintf(&b, "- **`rank_of_correct_node_at_k20`:** %s  (SLO ≤ %d, met: %s)\n",
		fmtFloat(r.LearningQuality.MedianRankOfCorrectNodeAtK),
		r.LearningQuality.MaxMedianRankAtK,
		yesNo(r.LearningQuality.RankMet))
	fmt.Fprintf(&b, "- **`concept_hit_fraction_at_k20`:** %s  (SLO ≥ %.2f, met: %s)\n",
		fmtFloat(r.LearningQuality.ConceptHitFractionAtK),
		r.LearningQuality.MinConceptHitFractionAtK,
		yesNo(r.LearningQuality.ConceptHitMet))
	b.WriteString("\n")

	// Notes.
	if len(r.Notes) > 0 {
		b.WriteString("## Operator notes\n\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n")
	}

	// Breach list.
	if len(r.BudgetBreaches) > 0 {
		b.WriteString("## Error-budget breaches\n\n")
		for _, v := range r.BudgetBreaches {
			fmt.Fprintf(&b, "- `%s` exceeded the %g error-budget ratio.\n", v, r.ErrorBudgetRatio)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// statusReason produces a single-line summary of the run
// outcome for the artifact's "Status" line.
func statusReason(r Report) string {
	if r.Aborted {
		return fmt.Sprintf("run aborted before planned duration elapsed (reason: %s); partial sample window only", r.CompletionReason)
	}
	if len(r.BudgetBreaches) == 0 {
		return fmt.Sprintf("no verb exceeded the %g error budget across %d verbs", r.ErrorBudgetRatio, len(r.Verbs))
	}
	return fmt.Sprintf("%d verb(s) exceeded the %g error budget: %s",
		len(r.BudgetBreaches), r.ErrorBudgetRatio, strings.Join(r.BudgetBreaches, ", "))
}

// fmtMs renders a duration to the artifact in milliseconds
// with three decimal places. Zero renders as "0ms".
func fmtMs(d time.Duration) string {
	if d == 0 {
		return "0ms"
	}
	return fmt.Sprintf("%.3fms", float64(d.Microseconds())/1000)
}

// fmtFloat renders a float that may be NaN; NaN becomes
// "n/a (no labelled queries supplied)" so the artifact text is
// self-explanatory.
func fmtFloat(v float64) string {
	if math.IsNaN(v) {
		return "n/a (no labelled queries supplied)"
	}
	return fmt.Sprintf("%.4f", v)
}

func yesNo(b bool) string {
	if b {
		return "✅"
	}
	return "❌"
}

// yamlString returns "value" or '""' when the value is empty so
// the YAML front matter stays parseable.
func yamlString(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, ":#{}[]\n\"'") {
		// Quote-escape: replace " with \" and wrap.
		escaped := strings.ReplaceAll(v, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return v
}

// isPendingProductionSeal returns true when the provenance
// string indicates the run is NOT a deploy/local-stack §8.3
// production-seal calibration — i.e. an in-process stub
// baseline, a synthetic seed, or a smoke preflight. Any such
// artifact carries an inline pointer to the
// operator-action-items tracker so a reviewer who reads the
// committed artifact can see the production-seal gap is
// structurally tracked (not silently outstanding).
//
// The detection is intentionally a positive match on the
// non-seal vocabulary the harness and the seed-fixture-200k
// Makefile target stamp, with a fallback positive-match on the
// deploy/local-stack vocabulary so an unrecognised provenance
// string defaults to "treat as pending" (fail-safe).
func isPendingProductionSeal(provenance string) bool {
	upper := strings.ToUpper(provenance)
	// Explicit non-seal stamps the harness / seed targets emit.
	nonSealMarkers := []string{
		"IN-PROCESS STUB BASELINE",
		"IN-PROCESS BASELINE",
		"SYNTHETIC SEED",
		"SMOKE",
		"PREFLIGHT",
		"DEV PREFLIGHT",
		"NOT THE §8.3 PRODUCTION SEAL",
		"NOT THE PRODUCTION SEAL",
	}
	for _, m := range nonSealMarkers {
		if strings.Contains(upper, strings.ToUpper(m)) {
			return true
		}
	}
	// Explicit production-seal stamps the operator emits via
	// the loadtest-calibration target's PROVENANCE variable.
	sealMarkers := []string{
		"DEPLOY/LOCAL STACK",
		"DEPLOY-LOCAL STACK",
		"PRODUCTION SEAL",
		"PRODUCTION-SEAL",
		"NOMINAL CALIBRATION",
	}
	for _, m := range sealMarkers {
		if strings.Contains(upper, strings.ToUpper(m)) {
			return false
		}
	}
	// Fail-safe: unrecognised provenance → treat as pending so
	// the artifact errs on the side of surfacing the operator
	// tracker rather than silently presenting an unverified
	// artifact as a production seal.
	return true
}

// WriteFile renders the markdown and persists it via the
// injected writer. The harness binary wires this to os.WriteFile;
// tests wire an in-memory map.
func (r Report) WriteFile(path string, writeFile func(string, []byte, os.FileMode) error) error {
	if writeFile == nil {
		return errors.New("calibration: WriteFile requires a non-nil writer")
	}
	body := []byte(r.RenderMarkdown())
	if err := writeFile(path, body, 0o644); err != nil {
		return fmt.Errorf("calibration: write artifact %q: %w", path, err)
	}
	return nil
}

// WriteTo streams the markdown body to w (an io.Writer).
// Useful when the artifact destination is not a local file
// (CI uploads it to an object store, etc).
func (r Report) WriteTo(w io.Writer) (int64, error) {
	body := r.RenderMarkdown()
	n, err := io.WriteString(w, body)
	return int64(n), err
}
