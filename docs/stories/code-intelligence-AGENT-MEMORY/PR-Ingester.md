# PR-History Ingester — Evolutionary Coupling as a Trace Substitute

> Status: **design proposal / research note** (not yet a contract)
> Generated: 2026-05-30
> Companion docs: [`architecture.md`](architecture.md), [`tech-spec.md`](tech-spec.md),
> [`implementation-plan.md`](implementation-plan.md), [`e2e-scenarios.md`](e2e-scenarios.md)
> Authority: per the repo's `README.md`, when docs and code disagree the docs
> win. This file is **analysis, not contract** — it does not redefine the
> Agent Memory architecture; it proposes a new background worker (the analog of
> the §3.3 Span Ingestor) and surveys how it maps onto the machinery already
> shipped.

## 0. Problem statement

Agent Memory's "intelligence over time" loop depends on a **dynamic layer** —
`observed_calls` edges and `TraceObservation` aggregates fed by OpenTelemetry
spans (`architecture.md` §3.3, §5.2.3). In practice **most repositories have no
trace/span data**: an agent working against a large OSS repo (the Kubernetes
engine, the Linux kernel, an arbitrary monorepo) gets only the **static call
graph** from the Repo Indexer. Without the dynamic/episodic signal, recall
quality and ranking are limited to static structure, and the learning loop has
nothing to consolidate.

**Observation:** every repository lacks spans, but **every repository has its
full change history.** Git/PR history is the one "trace" you always have. This
note proposes mining 5 years / 10k+ PRs as a substitute signal, and shows it is
a well-validated technique (*evolutionary / logical coupling*) that maps onto
the existing episodic→conceptual pipeline.

The driving question (verbatim from the requester): *if I train a repo by
looking back over PRs over the past 5 years (10k+ PRs), since a PR usually
covers a single story, would that help identify clustered files (a module for a
specific function) and offer a better understanding of a big repo?*

**Verdict: yes**, with caveats. This is *software architecture recovery via
evolutionary coupling*, and it is the right substitution for the missing trace
layer.

## 1. Why it works (research grounding)

*Evolutionary coupling* (a.k.a. *logical* / *change* coupling) is the implicit
relationship between artifacts that are frequently changed together over a
system's evolution. The established findings:

- If two artifacts appear in the same change-set they are assumed to co-change;
  mined across history this surfaces coupling that **static analysis cannot
  see** — config↔code, test↔impl, proto↔generated, interface↔all-implementers,
  "these two functions encode the same business rule."
- It is **language-agnostic** — it works identically on Go, C++, or kernel C
  with **zero parser coverage required**, unlike the structural layer whose
  quality is capped by AST-parser support.
- For architecture/module recovery specifically, adding evolutionary coupling to
  static-only recovery has been measured to **improve accuracy by up to ~40%**.
  Co-change clusters are "logical modules" that frequently **cross
  directory/package boundaries** — exactly the understanding a newcomer (human
  or agent) to a big repo lacks.

See §9 for sources.

## 2. Why PR-level is the right granularity

A PR ≈ one story/intent, so its file-set is a *semantically coherent*
co-change unit — cleaner than raw commits (which include "fix typo", "address
review", WIP noise). A PR also carries three signals a raw commit does not:

1. **Free natural-language labels** — title, body, linked issue. This is the
   sleeper win: it directly addresses the embedding-quality problem (the
   `agent-api` query embedder is currently a zero-vector stub —
   `cmd/agent-api/main.go:921-930`). A co-change cluster becomes a `Concept`
   whose `name` / `description_md` / embedding are grounded in real human prose,
   so semantic `recall` works without relying on the stub embedder.
2. **Outcome signal** — merged vs. reverted vs. hotfixed-within-N-days. Genuine
   quality polarity, not just coupling (see §5).
3. **Ownership / expertise** — author + reviewers per cluster → "who understands
   this module."

## 3. How it maps onto Agent Memory

### 3.1 The pipeline is already structurally a co-change clustering engine

```
PR                       → Episode            (one unit of intentful work)
changed files / methods  → Observation rows   (node_hit on File / Method nodes;
                                                File IS a node, kind='file')
Consolidator             → groups Episodes by observation-set, crystallises
                           recurring sets into Concepts (arch §7.7)
Concept + ConceptSupport → the durable "module" with per-repo, per-node evidence
```

In principle you replay 10k PRs as synthetic Episodes and the existing
Consolidator distils the modules.

### 3.2 The catch — the Consolidator uses exact-set matching, not co-change mining

`internal/consolidator/signature.go:66` (`computeSignature`) groups Episodes by
an **exact-set SHA-256 over the full, sorted, deduped observation-fingerprint
set**. A Concept crystallises only when the *identical* file-set recurs ≥
threshold (default 10) times (`internal/consolidator/doc.go:67-75`).

Real co-change mining needs **frequent-itemset / association-rule mining** over
*subsets and pairs* — **support, confidence, and especially lift** — because two
files rarely co-change in the exact same company every time. **The built-in
Consolidator is therefore too brittle to be the co-change miner as-is.** The
miner must run separately and write Concepts directly.

### 3.3 Schema frictions (why this needs migrations, not just the front door)

- `Episode.kind` enum is closed `{agent, feedback, synthetic_positive}` and
  `context_id` is required unless `kind='feedback'`
  (`episode_context_id_required_unless_feedback_chk`). Synthetic "PR episodes"
  do not fit cleanly.
- `Edge.kind` and `Node.kind` enums are closed (`architecture.md` §5.2.1,
  §5.2.2) — there is **no `co_changes` edge kind**, so co-change cannot natively
  enter the structural graph that `expand` walks without a migration.

Co-change clusters fit best as **Concepts** (global namespace per G6,
append-only per G4, with `ConceptSupport` rows anchoring to per-repo Nodes).

## 4. Proposed design — the "Git-History Ingester"

Treat it as the architectural analog of the Span Ingester (§3.3): a background
worker that feeds the episodic/conceptual layer from **PR history** instead of
**OTel spans**.

### 4.1 Reuse (no change required)

- Structural File/Method graph — the Repo Indexer already builds it.
- `Concept` / `ConceptVersion` / `ConceptSupport` tables and the GraphWriter.
- The embedding/recall infrastructure and the Concept Promoter (§7.8).

### 4.2 Build new

1. **PR fetch + normalise.** Pull PRs (`gh pr list` / host API): changed paths,
   title, body, linked issue, merge state, revert/hotfix linkage, author,
   reviewers. Resolve each changed path → File node (and, where the diff hunks
   permit, Method/Block nodes) via the structural graph.
2. **Miner.** Build the PR→file matrix and run:
   - **Association-rule mining** (Apriori) for pairs and rules — emit
     `support`, `confidence`, `lift` per coupling.
   - **Community detection** (Louvain/Leiden) over the lift-weighted co-change
     graph for the *module clusters*.
3. **Writer.** Persist each cluster as a `Concept` (bypassing the exact-set
   Consolidator):
   - `name` / `description_md` seeded from the dominant PR titles/issues in the
     cluster (this is what makes the Concept embeddable for semantic recall).
   - `ConceptSupport` rows anchoring the Concept to its File/Method Nodes and
     `repo_id`.
   - `ConceptVersion.confidence` from rule confidence/lift;
     `support_count` = number of contributing PRs.
   - Hand off to the Concept Promoter for embedding + publish (§7.8) so clusters
     become first-class `recall` results.

### 4.3 Optional migration — `co_changes` edge kind

Add a `co_changes` edge kind so `expand(node, direction=co_changes)` returns
"files history says you will also need to touch," ranked by lift — the same
shape `observed_calls` uses today. Cleanest fit if you want change-impact at
query time rather than only via Concept recall.

## 5. Polarity from PR outcomes (real quality signal)

The Consolidator's own polarity table (`internal/consolidator/doc.go:77-82`)
maps `success → positive` and `failure/refused/degraded/human_corrected →
negative`. Mirror this on mined history:

- Merged-and-survived PRs → positive support.
- **Reverted / hotfixed-within-N-days PRs → negative support.**

A file cluster that recurs across many *fix/revert* PRs becomes a
**defect-hotspot Concept** — directly useful for incident work, and a signal
static analysis cannot provide.

## 6. What it buys the two target use-cases

- **PR review (highest value, lowest effort): co-change completeness checks.**
  "You modified `X`; history says `Y` co-changes with it in 82% of PRs
  (lift 6.4) and you didn't touch it — likely missing." The cluster map also
  gives the reviewing agent the *real* module boundaries for blast-radius
  reasoning, not the directory tree.
- **Feature authoring.** Recall the cluster + its exemplar PRs → "here is how
  this kind of change was structured before, and the full set of files it
  usually spans."
- **Incident investigation.** Fix-PR and revert co-change → defect hotspots and
  "files historically touched together to fix this area" — substitutes for the
  runtime localisation traces would otherwise give. Fused with the static call
  graph, it yields a usable "where does this symptom live" signal with no spans.

## 7. How not to get fooled (caveats)

- **Tangled / mega PRs** (one PR, many unrelated concerns) inflate spurious
  coupling → cap by file count; down-weight or split large PRs.
- **Mechanical PRs** — renames touching 500 files, formatter/lint sweeps,
  dependency bumps, vendored code, generated-file regen, bot PRs → exclude or
  they couple everything to everything.
- **Frequency bias** — hot files (a central `utils.go`) appear to couple with
  all → rank by **lift/confidence, never raw support**.
- **Temporal decay** — 5-year-old coupling may be dead after a refactor →
  weight recent PRs higher; a retired file's tombstone (G5) lets you expire
  stale clusters honestly.
- **Complementary, not a replacement** — co-change misses stable structural
  dependencies that simply never change together. The win is **fusing**
  evolutionary coupling with the static call graph, as the architecture-recovery
  literature does.

## 8. Net recommendation

Mining PR history is a strong, evidence-backed way to manufacture the
intelligence layer for repos with **no telemetry** — arguably *more* portable
than the OTel path because it needs no instrumentation and no parser coverage.
The cleanest implementation is a **dedicated PR miner that writes Concepts**
(plus an optional `co_changes` edge), **not** a reuse of the exact-set
Consolidator.

Suggested next steps:
1. Prototype the miner against a real repo's `gh pr list` export and inspect the
   clusters that fall out **before** committing to schema work.
2. If the clusters are useful, promote this note to a full story spec
   (`architecture.md` / `tech-spec.md` / `implementation-plan.md` set) under a
   new `code-intelligence:PR-INGESTER` story.

## 9. Sources

- Zimmermann et al., *Mining Version Histories to Guide Software Changes*
  (TSE 2005) — <https://thomas-zimmermann.com/publications/files/zimmermann-tse-2005.pdf>
- *On the Use of Evolutionary Coupling for Software Architecture Recovery*
  (IEEE) — <https://ieeexplore.ieee.org/document/9659761/>
- *Change Coupling Between Software Artifacts: Learning from Past Changes* —
  <https://www.researchgate.net/publication/283802354_Change_Coupling_Between_Software_Artifacts_Learning_from_Past_Changes>
- *The effect of evolutionary coupling on software defects: an industrial case
  study on a legacy system* —
  <https://www.researchgate.net/publication/266661758_The_effect_of_evolutionary_coupling_on_software_defects_An_industrial_case_study_on_a_legacy_system>
- Rolfsnes, *Improving History-Based Change Recommendation Systems for Software
  Evolution* (PhD thesis) —
  <https://evolveit.bitbucket.io/publications/rolfsnes_thesis/thomas_rolfsnes_phdthesis.pdf>
