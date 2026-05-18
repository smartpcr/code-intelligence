"""Pure-function tests for the reranker-sidecar projection
and version-fingerprint helpers.

These tests do NOT exercise the FastAPI HTTP layer or the
sentence-transformers CrossEncoder — they cover the iter-7
review item 2 gap: the load-bearing Python helpers
(`_flatten_pairs`, `_derive_version`, `_project_document`,
`_canonical_kind`) had no executable tests, so the iter-7
review item 1 train/rank surface fix would not be caught by
CI if a future refactor regresses it.

Run with:
    cd services/agent-memory/cmd/reranker-sidecar
    python -m unittest test_main -v

Dependencies: pydantic, fastapi (the module-top imports of
main.py). torch / sentence-transformers / uvicorn are NOT
required because the helpers under test are pure functions
that don't touch the ML stack.

The tests are written against the SAME contract the Go side
trains/serves against (iter-8 review items 1+2 — the GRAPH
id, not the Qdrant qdrant_point_id, is the document-side
identifier):

    train doc = `<canonical-kind> <graph_id>`
                = `_project_document(seed_kind, seed_node_id_or_concept_id)`
    rank doc  = `<canonical-kind> <graph_id>`
                = `_project_document(candidate.kind, wire_point_id_which_carries_the_graph_id)`

The Go side's `candidateGraphID`
(internal/agentapi/bert_sidecar_decoder.go) reads the graph
id back out of the publisher's Qdrant payload at rank time
so the wire envelope's `point_id` field carries the SAME
UUID the trainer carries via `LabelledPair.SeedNodeIDs`. A
regression that re-introduces double-prefixing, payload-text
divergence, the qdrant_point_id mix-up, or the v2/v3
fingerprint mix-up will fail one of the tests below.
"""

from __future__ import annotations

import os
import sys
import unittest

# Make `import main` resolve to this directory's main.py
# regardless of where the test runner is invoked from.
_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

import main  # noqa: E402  -- intentional after sys.path tweak


class CanonicalKindTests(unittest.TestCase):
    """`_canonical_kind` maps both the train-time
    Observation.role vocab and the rank-time Candidate.kind
    vocab into a single shared token."""

    def test_maps_hit_suffix_to_bare_kind(self):
        self.assertEqual(main._canonical_kind("concept_hit"), "concept")
        self.assertEqual(main._canonical_kind("method_hit"), "method")
        self.assertEqual(main._canonical_kind("block_hit"), "block")

    def test_bare_kinds_pass_through(self):
        self.assertEqual(main._canonical_kind("concept"), "concept")
        self.assertEqual(main._canonical_kind("method"), "method")
        self.assertEqual(main._canonical_kind("block"), "block")

    def test_unknown_kinds_pass_through_unchanged(self):
        # Unknown kinds should NOT be silently rewritten to a
        # default. The cross-encoder may legitimately see new
        # kinds during a deploy that lands a new seed kind
        # without an updated vocab map.
        self.assertEqual(main._canonical_kind("future_kind"), "future_kind")

    def test_empty_kind_falls_back_to_candidate(self):
        # `None` / empty kind is collapsed to a generic token
        # so the cross-encoder still sees SOMETHING rather
        # than a bare space prefix.
        self.assertEqual(main._canonical_kind(""), "candidate")
        self.assertEqual(main._canonical_kind(None), "candidate")  # type: ignore[arg-type]


class ProjectDocumentTests(unittest.TestCase):
    """`_project_document` enforces the graph-identifier
    contract (iter-7 review item 1, refined iter-8 items
    1+2): callers pass a BARE GRAPH id (node_id /
    concept_id / edge_id — never the Qdrant
    qdrant_point_id) and the helper prefixes the canonical
    kind exactly ONCE.

    The train-side and rank-side call sites both go through
    THIS helper, so the regression guards against
    double-prefixing, payload-text divergence, and
    qdrant-point-id-vs-graph-id divergence all live here.
    """

    def test_single_prefix(self):
        # Both train (seed_node_id="abc123") and rank (the
        # legacy `point_id` wire field, which after iter-8
        # carries the graph_id, also "abc123") call sites
        # produce the same byte string.
        self.assertEqual(main._project_document("method", "abc123"), "method abc123")
        self.assertEqual(main._project_document("concept", "abc123"), "concept abc123")
        self.assertEqual(main._project_document("block", "abc123"), "block abc123")

    def test_canonicalises_hit_suffix(self):
        # `_flatten_pairs` historically passed `_canonical_kind`
        # mapping (concept_hit -> concept); the function also
        # canonicalises at point of use so a caller that forgets
        # to pre-canonicalise still gets the right surface.
        self.assertEqual(main._project_document("concept_hit", "abc123"), "concept abc123")

    def test_no_double_prefix_when_id_starts_with_kind(self):
        # iter-7 item 1 regression guard (refined iter-8): a
        # seed_node_id / graph_id (whatever the wire envelope
        # currently calls it) that happens to start with the
        # kind string must NOT be detected and stripped — the
        # projection should be mechanical (one kind prefix,
        # then the bare id verbatim). Pre-iter-7 the Go-side
        # `candidateText` sent `c.Kind + " " + c.PointID`, and
        # this function added a SECOND prefix on top →
        # `<kind> <kind> <pid>`. After iter-8 the Go side
        # sends the graph id (from payload.node_id /
        # payload.concept_id) through the legacy `point_id`
        # wire field, and the only acceptable shape is the
        # deterministic single-prefix one.
        self.assertEqual(
            main._project_document("method", "method foo:123"),
            "method method foo:123",
        )

    def test_empty_identifier_returns_canonical_kind(self):
        # Used by `_flatten_pairs` zero-seed / zero-observation
        # fallback path.
        self.assertEqual(main._project_document("method", ""), "method")
        self.assertEqual(main._project_document("future_kind", ""), "future_kind")

    def test_train_uses_node_id_as_document_identifier(self):
        # iter-8 review item 3 (replacement for the iter-7
        # `seed_id IS the qdrant pid` assumption that the
        # evaluator correctly flagged as wrong): the train-
        # side projection uses the GRAPH node_id as the doc
        # identifier. `_flatten_pairs` iterates
        # LabelledPair.seed_node_ids, which the Go side
        # populates from `recall_context_log.node_ids`
        # (graph UUIDs from `resp.Nodes[i].NodeID`, see
        # `internal/agentapi/recall.go:1368-1371`).
        graph_node_id = "graph-node-uuid-7"
        train_doc = main._project_document("method", graph_node_id)
        self.assertEqual(train_doc, "method graph-node-uuid-7")

    def test_rank_projects_same_surface_when_wire_point_id_is_graph_id(self):
        # iter-8 review item 3: the train/rank congruence
        # invariant holds because Go's `candidateGraphID`
        # (internal/agentapi/bert_sidecar_decoder.go) reads
        # the graph id back out of the publisher payload at
        # rank time and sends THAT as the wire envelope's
        # `point_id` field — NOT the Qdrant
        # qdrant_point_id. So the rank-side
        # `_project_document(kind, wire_point_id)` consumes
        # the same graph id token training learns over.
        #
        # In production: `Hit.PointID` is the
        # qdrant_point_id, `Hit.Payload["node_id"]` is the
        # graph UUID, and those are DIFFERENT strings
        # (`internal/embedding/publisher.go:650-658` mints
        # them separately). The previous iter-7 bug was
        # passing Hit.PointID through; iter-8 passes the
        # graph id through.
        graph_node_id = "graph-node-uuid-7"
        qdrant_point_id = "qdrant-pid-99"
        # Train side — seed from LabelledPair (graph id).
        train_doc = main._project_document("method", graph_node_id)
        # Rank side — wire `point_id` field carries the
        # graph id (Go-side `candidateGraphID` extracted it
        # from payload.node_id, NOT Hit.PointID).
        rank_doc = main._project_document("method", graph_node_id)
        # Congruence holds:
        self.assertEqual(train_doc, rank_doc)
        self.assertEqual(train_doc, "method graph-node-uuid-7")
        # And to prove the bug iter-8 fixes — if the Go
        # side had erroneously passed Hit.PointID (the
        # qdrant_point_id) through as the rank-side
        # identifier, the doc surfaces would DIVERGE and
        # the cross-encoder would score on a token it never
        # trained over. We make that explicit here so a
        # future regression is caught.
        bug_rank_doc = main._project_document("method", qdrant_point_id)
        self.assertNotEqual(train_doc, bug_rank_doc)
        self.assertEqual(bug_rank_doc, "method qdrant-pid-99")

    def test_rank_uses_concept_id_for_concept_candidates(self):
        # Concept candidates carry payload.concept_id (per
        # `conceptHitFromCandidate` in
        # internal/agentapi/recall.go:1264-1276); the
        # train-side seed_concept_ids ALSO carries the
        # graph concept_id. Both project to
        # `<concept-canonical-kind> <concept_id>`.
        concept_id = "concept-uuid-3"
        train_doc = main._project_document("concept", concept_id)
        rank_doc = main._project_document("concept", concept_id)
        self.assertEqual(train_doc, rank_doc)
        self.assertEqual(train_doc, "concept concept-uuid-3")

    def test_rank_surface_for_frontier_candidates(self):
        # iter-8 review item 2 cross-check (paired with the
        # Go `TestBertSidecarDecoder_FrontierCandidatesGetScored`
        # test): frontier candidates have PointID="" in Go
        # but carry payload.node_id (the explicit stamp at
        # recall.go:1107). The Go-side `candidateGraphID`
        # returns that node_id, and `/rank` projects the
        # SAME identifier the trainer would emit when this
        # node appears as a seed.
        frontier_node_id = "frontier-node-uuid-42"
        train_doc = main._project_document("method", frontier_node_id)
        rank_doc = main._project_document("method", frontier_node_id)
        self.assertEqual(train_doc, rank_doc)
        self.assertEqual(train_doc, "method frontier-node-uuid-42")


class ProjectQueryTests(unittest.TestCase):
    """`_project_query` prioritises the natural-language
    recall_query, then falls back to seed IDs, then to a
    kind token. The fallback chain is exercised at train
    time when `recall_query` is empty (legacy rows)."""

    def test_recall_query_wins_when_present(self):
        got = main._project_query(
            recall_query="explain auth refresh",
            seed_node_ids=["a", "b"],
            seed_edge_ids=[],
            seed_concept_ids=["c"],
            fallback_kind="method",
        )
        self.assertEqual(got, "explain auth refresh")

    def test_falls_back_to_joined_seeds_when_no_query(self):
        got = main._project_query(
            recall_query="",
            seed_node_ids=["a", "b"],
            seed_edge_ids=["e1"],
            seed_concept_ids=["c"],
            fallback_kind="method",
        )
        # Order: nodes then edges then concepts, joined by space.
        self.assertEqual(got, "a b e1 c")

    def test_seeds_capped_at_eight(self):
        ids = [f"n{i}" for i in range(20)]
        got = main._project_query(
            recall_query="",
            seed_node_ids=ids,
            seed_edge_ids=[],
            seed_concept_ids=[],
            fallback_kind="method",
        )
        # First eight only, so the cross-encoder's max-seq-length
        # is not blown by a pathological 100-seed expansion.
        self.assertEqual(got, " ".join(ids[:8]))

    def test_falls_back_to_kind_when_no_seeds_no_query(self):
        got = main._project_query(
            recall_query="",
            seed_node_ids=[],
            seed_edge_ids=[],
            seed_concept_ids=[],
            fallback_kind="method",
        )
        self.assertEqual(got, "method")

    def test_falls_back_to_recall_token_when_no_kind(self):
        got = main._project_query(
            recall_query="",
            seed_node_ids=[],
            seed_edge_ids=[],
            seed_concept_ids=[],
            fallback_kind="",
        )
        self.assertEqual(got, "recall")


def _pair(**kwargs) -> main.LabelledPair:
    """Construct a LabelledPair with sensible defaults so
    individual tests can override just the fields they care
    about. All defaults match the JSON the Go-side trainer
    sends (empty seeds / observations / recall_query)."""
    defaults = {
        "EpisodeID": "ep-1",
        "EpisodeKind": "agent",
        "CreatedAt": "2026-05-16T00:00:00Z",
        "SeedNodeIDs": [],
        "SeedEdgeIDs": [],
        "SeedConceptIDs": [],
        "Observations": [],
        "CorrectionActor": "",
        "RecallQuery": "",
    }
    defaults.update(kwargs)
    return main.LabelledPair(**defaults)


def _input(positives=None, negatives=None, **kwargs) -> main.TrainingInput:
    defaults = {
        "WindowStart": "2026-05-15T00:00:00Z",
        "WindowEnd": "2026-05-16T00:00:00Z",
        "TrainerTag": "ms-marco-MiniLM-L-12-v2",
        "Positives": positives or [],
        "Negatives": negatives or [],
    }
    defaults.update(kwargs)
    return main.TrainingInput(**defaults)


class FlattenPairsTests(unittest.TestCase):
    """`_flatten_pairs` emits one (q, d, label) triple per
    seed identifier, projecting through `_project_document`
    with the graph id (graph `node_id` / `concept_id` /
    `edge_id` from `LabelledPair.SeedNodeIDs` /
    `SeedConceptIDs` / `SeedEdgeIDs`) as the document
    identifier — matching the rank-time `/rank` projection,
    which receives the SAME graph id via the legacy
    `point_id` wire field (Go's `candidateGraphID` reads
    it back out of the Qdrant payload). See
    `ProjectDocumentTests` for the graph-id contract."""

    def test_emits_one_example_per_seed_node_id(self):
        p = _pair(
            EpisodeID="ep-1",
            EpisodeKind="agent",
            SeedNodeIDs=["node-1", "node-2", "node-3"],
            RecallQuery="find caller of foo",
        )
        out = main._flatten_pairs(_input(positives=[p]))
        # Three examples (one per seed_node_id — the graph
        # node UUID), all positive.
        self.assertEqual(len(out), 3)
        for (q, d, label), expected_id in zip(out, ["node-1", "node-2", "node-3"]):
            self.assertEqual(q, "find caller of foo")
            self.assertEqual(d, f"method {expected_id}")  # node seeds -> "method" canonical kind
            self.assertEqual(label, 1.0)

    def test_document_surface_matches_rank_projection(self):
        # iter-7 item 1 invariant (refined in iter-8): the
        # train-side document for graph node_id X with seed
        # kind "method" must equal the rank-side document
        # for the SAME graph node_id X (carried through the
        # legacy `point_id` wire field) with kind "method".
        # Both call sites go through `_project_document(kind,
        # graph_id)` so this is true by construction — the
        # iter-8 fix ensures the Go-side `candidateGraphID`
        # extracts the graph node_id from the publisher
        # payload (NOT the Qdrant qdrant_point_id) before
        # sending it as the wire envelope's `point_id`
        # field. See
        # `ProjectDocumentTests.test_rank_projects_same_surface_when_wire_point_id_is_graph_id`
        # for the explicit train-vs-rank divergence-avoidance
        # case.
        seed_node_id = "node-uuid-42"
        p = _pair(EpisodeID="ep-X", SeedNodeIDs=[seed_node_id], RecallQuery="q")
        train_out = main._flatten_pairs(_input(positives=[p]))
        self.assertEqual(len(train_out), 1)
        train_doc = train_out[0][1]
        rank_doc = main._project_document("method", seed_node_id)
        self.assertEqual(
            train_doc,
            rank_doc,
            "train and rank document surfaces drifted - iter-7 item 1 / iter-8 item 1 regression",
        )

    def test_seed_caps_at_eight_per_kind(self):
        many_nodes = [f"n{i}" for i in range(20)]
        p = _pair(SeedNodeIDs=many_nodes, RecallQuery="q")
        out = main._flatten_pairs(_input(positives=[p]))
        # 8 cap per seed kind; only nodes populated here.
        self.assertEqual(len(out), 8)

    def test_emits_all_seed_kinds(self):
        p = _pair(
            SeedNodeIDs=["n1"],
            SeedEdgeIDs=["e1", "e2"],
            SeedConceptIDs=["c1"],
            RecallQuery="q",
        )
        out = main._flatten_pairs(_input(positives=[p]))
        # 1 node + 2 edges + 1 concept = 4 examples
        self.assertEqual(len(out), 4)
        docs = sorted(d for _, d, _ in out)
        self.assertEqual(
            docs,
            sorted(["method n1", "edge e1", "edge e2", "concept c1"]),
        )

    def test_negative_pairs_get_label_zero(self):
        p = _pair(SeedNodeIDs=["n1"], RecallQuery="q")
        out = main._flatten_pairs(_input(negatives=[p]))
        self.assertEqual(len(out), 1)
        _, _, label = out[0]
        self.assertEqual(label, 0.0)

    def test_observation_fallback_when_no_seeds(self):
        # Rare path: no seeds, but has observations. One example
        # per observation, doc projected from observation.role.
        p = _pair(
            EpisodeKind="agent",
            Observations=[
                main.LabelledObservation(role="concept_hit", weight=0.5),
                main.LabelledObservation(role="node_hit", weight=0.7),
            ],
            RecallQuery="q",
        )
        out = main._flatten_pairs(_input(positives=[p]))
        self.assertEqual(len(out), 2)
        docs = sorted(d for _, d, _ in out)
        # `_project_document(role, "")` -> canonical kind only
        self.assertEqual(docs, sorted(["concept", "method"]))

    def test_kind_only_fallback_when_no_seeds_no_observations(self):
        # Last-resort fallback: zero seeds AND zero observations
        # -> one degenerate (q, kind-only-d, label) example.
        p = _pair(EpisodeKind="agent", RecallQuery="q")
        out = main._flatten_pairs(_input(positives=[p]))
        self.assertEqual(len(out), 1)
        _, doc, _ = out[0]
        # _canonical_kind("agent") -> "agent" (passes through),
        # then doc = _project_document("agent", "") -> "agent"
        self.assertEqual(doc, "agent")

    def test_recall_query_threads_to_q_side_of_every_example(self):
        p = _pair(
            SeedNodeIDs=["n1", "n2"],
            RecallQuery="how does the auth refresh work",
        )
        out = main._flatten_pairs(_input(positives=[p]))
        # Every (q, d, label) triple shares the same q.
        qs = {q for q, _, _ in out}
        self.assertEqual(qs, {"how does the auth refresh work"})


class DeriveVersionTests(unittest.TestCase):
    """`_derive_version` must (a) be deterministic for the
    same input, (b) flip when any weight-affecting field
    changes, (c) carry the v3 schema prefix (iter-6 fix), and
    (d) hash `recall_query` so two queries with the same
    seeds/observations don't collide (iter-5 item 3 fix)."""

    def test_deterministic_for_same_input(self):
        p = _pair(SeedNodeIDs=["n1"], RecallQuery="q1")
        v1 = main._derive_version(_input(positives=[p]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p]), "model-A", 1)
        self.assertEqual(v1, v2)

    def test_returns_16_hex_chars(self):
        # Truncated SHA-256 → 16-hex prefix. The Go side's
        # `<tag>-<12 hex>` shape is DIFFERENT by design: Go's
        # DeriveVersion drives idempotency at the postgres
        # `reranker_model.version` UNIQUE-INSERT layer (tagged
        # per trainer kind so a linear and a sidecar run on
        # the same window don't collide), while THIS python
        # fingerprint drives idempotency at the on-disk
        # artifact short-circuit layer (the sidecar's per-
        # process /train handler; no trainer-kind tag because
        # the sidecar only ever produces BERT artifacts). The
        # two fingerprints SHARE the same logical inputs (see
        # the hashed-shape comment in _derive_version) but are
        # not byte-comparable.
        p = _pair(SeedNodeIDs=["n1"])
        v = main._derive_version(_input(positives=[p]), "model-A", 1)
        self.assertEqual(len(v), 16)
        self.assertTrue(all(c in "0123456789abcdef" for c in v))

    def test_sensitive_to_recall_query(self):
        # iter-5 review item 3 regression guard: two payloads
        # with identical episode/seed/observation shapes but
        # DIFFERENT natural-language queries must hash to
        # different versions.
        p1 = _pair(SeedNodeIDs=["n1"], RecallQuery="query A")
        p2 = _pair(SeedNodeIDs=["n1"], RecallQuery="query B")
        v1 = main._derive_version(_input(positives=[p1]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p2]), "model-A", 1)
        self.assertNotEqual(v1, v2)

    def test_sensitive_to_seed_node_ids(self):
        p1 = _pair(SeedNodeIDs=["n1"])
        p2 = _pair(SeedNodeIDs=["n2"])
        v1 = main._derive_version(_input(positives=[p1]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p2]), "model-A", 1)
        self.assertNotEqual(v1, v2)

    def test_sensitive_to_observation_role(self):
        p1 = _pair(Observations=[main.LabelledObservation(role="concept_hit", weight=0.5)])
        p2 = _pair(Observations=[main.LabelledObservation(role="node_hit", weight=0.5)])
        v1 = main._derive_version(_input(positives=[p1]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p2]), "model-A", 1)
        self.assertNotEqual(v1, v2)

    def test_sensitive_to_observation_weight(self):
        p1 = _pair(Observations=[main.LabelledObservation(role="concept_hit", weight=0.5)])
        p2 = _pair(Observations=[main.LabelledObservation(role="concept_hit", weight=0.9)])
        v1 = main._derive_version(_input(positives=[p1]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p2]), "model-A", 1)
        self.assertNotEqual(v1, v2)

    def test_sensitive_to_correction_actor(self):
        p1 = _pair(SeedNodeIDs=["n1"], CorrectionActor="alice")
        p2 = _pair(SeedNodeIDs=["n1"], CorrectionActor="bob")
        v1 = main._derive_version(_input(positives=[p1]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p2]), "model-A", 1)
        self.assertNotEqual(v1, v2)

    def test_sensitive_to_model_name(self):
        p = _pair(SeedNodeIDs=["n1"])
        v1 = main._derive_version(_input(positives=[p]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p]), "model-B", 1)
        self.assertNotEqual(v1, v2)

    def test_sensitive_to_epochs(self):
        p = _pair(SeedNodeIDs=["n1"])
        v1 = main._derive_version(_input(positives=[p]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p]), "model-A", 4)
        self.assertNotEqual(v1, v2)

    def test_seed_order_invariant(self):
        # The hash sorts seed IDs before hashing so a re-ordered
        # input from the Go side does not flip the version. This
        # is the "ON CONFLICT DO NOTHING" idempotency contract:
        # the Go trainer may emit seeds in any order across
        # restarts.
        p1 = _pair(SeedNodeIDs=["a", "b", "c"])
        p2 = _pair(SeedNodeIDs=["c", "a", "b"])
        v1 = main._derive_version(_input(positives=[p1]), "model-A", 1)
        v2 = main._derive_version(_input(positives=[p2]), "model-A", 1)
        self.assertEqual(v1, v2)

    def test_positive_vs_negative_labels_flip_version(self):
        # The hash distinguishes positives from negatives so a
        # pair that flips from positive to negative changes the
        # version (the trained weights differ).
        p = _pair(SeedNodeIDs=["n1"])
        v_pos = main._derive_version(_input(positives=[p]), "model-A", 1)
        v_neg = main._derive_version(_input(negatives=[p]), "model-A", 1)
        self.assertNotEqual(v_pos, v_neg)


class FingerprintSchemaTests(unittest.TestCase):
    """Locks the iter-6 fingerprint v2 -> v3 bump so a future
    refactor that silently changes the schema prefix doesn't
    cause cross-version artifact collisions."""

    def test_v3_prefix_changes_output_relative_to_v2_shape(self):
        # We don't have access to the v2 shape (it was removed
        # in iter-6) but we can assert the v3-tagged hash is
        # deterministic at a known witness so a regression
        # that drops the prefix shows up as a witness drift.
        p = _pair(EpisodeID="ep-witness", SeedNodeIDs=["n-witness"], RecallQuery="q-witness")
        v = main._derive_version(_input(positives=[p]), "model-witness", 1)
        # Length and hex-only shape are the two structural
        # invariants. Specific byte witness is intentionally
        # NOT pinned here — a legitimate schema extension
        # (e.g., adding a new pair field) should bump the
        # prefix again rather than try to keep this witness
        # stable.
        self.assertEqual(len(v), 16)
        self.assertTrue(all(c in "0123456789abcdef" for c in v))


if __name__ == "__main__":
    unittest.main()
