"""
reranker-sidecar: BERT-class cross-encoder training service.

Stage 6.4 (impl-plan ┬º1110) mandates a "cross-encoder
BERT-class model (<= 200M params)" for the reranker. The Go
binary at services/agent-memory/cmd/reranker-trainer/ pulls
labelled positive/negative pairs from PostgreSQL and POSTs
them to this sidecar's /train endpoint. The sidecar fits the
cross-encoder, writes the artifact to disk, and returns a
TrainingOutput envelope the Go binary persists as a new
reranker_model row.

Why a separate Python process: the BERT cross-encoder lives
in the Python ML stack (torch + transformers +
sentence-transformers). Hosting it in-process from Go would
require either a CGo binding to libtorch (heavy maintenance
burden) or a Go re-implementation of the transformer
architecture (unmaintainable). The HTTP sidecar boundary is
the maintained seam.

Model: `cross-encoder/ms-marco-MiniLM-L-12-v2` (~33M params,
well under the 200M cap from ┬º6.4 step-3). The model is
loaded at startup and re-used across train calls so the
cold-start cost amortises.

Wire-up from the Go side:
    AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT=http://reranker-sidecar:8088
    AGENT_MEMORY_RERANKER_TRAINER_KIND=sidecar (default when ENDPOINT set)
    AGENT_MEMORY_RERANKER_TRAINER_TAG=ms-marco-MiniLM-L-12-v2

Health endpoint: GET /healthz returns 200 once the model has
loaded (the Go binary's deploy probe blocks on it).

Artifact storage & recall consumption: writes to
/var/lib/reranker-sidecar/models/<version>/ by default and
returns `file://<absolute-path>` as the ArtifactURI. The
agent-api recall path's PublishedReranker is wired with the
BertSidecarDecoder (services/agent-memory/internal/agentapi/
bert_sidecar_decoder.go) which recognises `file://` URIs and
POSTs candidates to THIS sidecar's `/rank` endpoint over the
SAME HTTP boundary, so the trained checkpoint is genuinely
loaded and consumed at recall time. See `/rank` below for
the per-recall request shape.

Idempotency contract: `/train` is idempotent on `version`.
A repeat call with byte-identical payload (and the same
configured model_name + max_epochs) produces the same
version AND the same artifact bytes. The endpoint
short-circuits when `<artifact_dir>/<version>/` already
exists rather than re-fitting, so a retry after a sidecar
restart returns the existing artifact URI without mutating
the on-disk weights. The Go-side `reranker_model.version`
UNIQUE-INSERT contract relies on this ΓÇö a publish retry
must not produce different bytes under the same handle.

Configuration env vars:
    RERANKER_SIDECAR_LISTEN_ADDR     bind address (default ":8088")
    RERANKER_SIDECAR_MODEL_NAME      HuggingFace model id
                                     (default cross-encoder/ms-marco-MiniLM-L-12-v2)
    RERANKER_SIDECAR_ARTIFACT_DIR    artifact output directory
                                     (default /var/lib/reranker-sidecar/models)
    RERANKER_SIDECAR_MAX_EPOCHS      training epochs (default 1)
    RERANKER_SIDECAR_MAX_PAIRS       cap on training pairs the
                                     sidecar will accept (default 100000
                                     -- guards against an unbounded
                                     pull from the trainer's PG side
                                     OOMing the GPU host)
"""

from __future__ import annotations

import hashlib
import logging
import os
import shutil
import tempfile
import threading
import time
from collections import OrderedDict
from dataclasses import dataclass
from pathlib import Path
from typing import List, Optional, Tuple

from fastapi import FastAPI, HTTPException
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

logger = logging.getLogger("reranker-sidecar")
logging.basicConfig(level=os.environ.get("RERANKER_SIDECAR_LOG_LEVEL", "INFO"))


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class SidecarConfig:
    """Runtime configuration read from the environment.

    Fields documented at module top.
    """

    listen_addr: str
    model_name: str
    artifact_dir: Path
    max_epochs: int
    max_pairs: int

    @classmethod
    def from_env(cls) -> "SidecarConfig":
        artifact_dir = Path(
            os.environ.get("RERANKER_SIDECAR_ARTIFACT_DIR", "/var/lib/reranker-sidecar/models")
        )
        return cls(
            listen_addr=os.environ.get("RERANKER_SIDECAR_LISTEN_ADDR", ":8088"),
            model_name=os.environ.get(
                "RERANKER_SIDECAR_MODEL_NAME", "cross-encoder/ms-marco-MiniLM-L-12-v2"
            ),
            artifact_dir=artifact_dir,
            max_epochs=int(os.environ.get("RERANKER_SIDECAR_MAX_EPOCHS", "1")),
            max_pairs=int(os.environ.get("RERANKER_SIDECAR_MAX_PAIRS", "100000")),
        )


# ---------------------------------------------------------------------------
# Wire format - mirrors the Go side's TrainingInput / TrainingOutput
# ---------------------------------------------------------------------------


class LabelledObservation(BaseModel):
    role: str = ""
    weight: float = 0.0


class LabelledPair(BaseModel):
    episode_id: str = Field(alias="EpisodeID")
    episode_kind: str = Field(alias="EpisodeKind", default="")
    created_at: str = Field(alias="CreatedAt", default="")
    seed_node_ids: List[str] = Field(alias="SeedNodeIDs", default_factory=list)
    seed_edge_ids: List[str] = Field(alias="SeedEdgeIDs", default_factory=list)
    seed_concept_ids: List[str] = Field(alias="SeedConceptIDs", default_factory=list)
    observations: List[LabelledObservation] = Field(alias="Observations", default_factory=list)
    correction_actor: str = Field(alias="CorrectionActor", default="")
    # Optional natural-language recall query the trainer
    # pulls from RecallContextLog.query_text. When present,
    # this is the SAME string `/rank` receives in its `query`
    # field at recall time ΓÇö so the train-time and recall-
    # time `_project_query` outputs match by construction
    # (iter-4 review item 3). Empty when the labelled pair
    # was synthesized from a correction without a recall
    # query attached.
    recall_query: str = Field(alias="RecallQuery", default="")

    class Config:
        populate_by_name = True


class TrainingInput(BaseModel):
    window_start: str = Field(alias="WindowStart", default="")
    window_end: str = Field(alias="WindowEnd", default="")
    trainer_tag: str = Field(alias="TrainerTag", default="")
    positives: List[LabelledPair] = Field(alias="Positives", default_factory=list)
    negatives: List[LabelledPair] = Field(alias="Negatives", default_factory=list)

    class Config:
        populate_by_name = True


# ---------------------------------------------------------------------------
# Shared (query, document) projection
# ---------------------------------------------------------------------------
#
# CRITICAL invariant (iter-4 review item 3, refined iter-7
# item 1, refined iter-8 items 1+2): both `/train` (via
# _flatten_pairs) and `/rank` MUST run their inputs through
# the SAME projection functions defined below AND pass the
# GRAPH IDENTIFIER (graph node_id / concept_id / edge_id ΓÇö
# NOT the Qdrant qdrant_point_id) as the document side so
# the cross-encoder is fine-tuned and queried on byte-
# identical document surfaces. See `_project_document`'s
# docstring for the graph-identifier contract and the
# iteration history of how we landed on this shape.
#
# The projection is intentionally minimal: just enough
# structure to keep the (q, d) pair distinguishable while
# matching the data both contexts can produce without
# requiring the Go-side trainer to hydrate Qdrant payload
# text for every seed.


_CANONICAL_KIND = {
    # train-time Observation.role vocab -> canonical kind
    "concept_hit": "concept",
    "method_hit": "method",
    "block_hit": "block",
    # iter-7 review item 1 follow-up: also map the remaining
    # Observation.role vocab the Go-side LinearTrainer
    # actually emits (internal/rerankertrainer/linear_trainer.go
    # extractFeatures switch). Without these entries,
    # `_canonical_kind("node_hit")` would pass through
    # unchanged and the per-role kind-only fallback in
    # `_flatten_pairs` would produce `node_hit` as a doc
    # token instead of the canonical `method` ΓÇö which is what
    # the rank side actually receives via the Candidate.Kind
    # field.
    "node_hit": "method",
    "call_edge_hit": "edge",
    "edge_hit": "edge",
    # rank-time Candidate.Kind vocab -> already canonical
    "concept": "concept",
    "method": "method",
    "block": "block",
    # iter-7 review item 1 follow-up: edges are surfaced at
    # train time (SeedEdgeIDs hint `_flatten_pairs` uses) so
    # the canonical form must round-trip.
    "edge": "edge",
}


def _canonical_kind(kind: str) -> str:
    """Map the train-time `Observation.role` vocab
    (`concept_hit`/`method_hit`/`block_hit`) and the
    rank-time `Candidate.Kind` vocab (`concept`/`method`/
    `block`) into a single canonical token. Unknown kinds
    pass through unchanged so future kinds don't get
    silently rewritten to a default."""
    return _CANONICAL_KIND.get(kind, kind or "candidate")


def _project_query(
    recall_query: str,
    seed_node_ids: List[str],
    seed_edge_ids: List[str],
    seed_concept_ids: List[str],
    fallback_kind: str,
) -> str:
    """Build the query side of the cross-encoder pair.

    Priority:
      1. The natural-language recall query (from the agent's
         `agent.recall` request, or RecallContextLog at train
         time).
      2. The joined seed identifiers (caps the first 8 so a
         pathological 100-seed expansion doesn't blow past
         the model's max sequence length).
      3. The episode kind / candidate kind as a last-ditch
         token so the cross-encoder still sees SOMETHING.

    Used by BOTH `_flatten_pairs` and `/rank` so the
    train/rank projection is a single function.
    """
    if recall_query:
        return recall_query
    seeds = (seed_node_ids or []) + (seed_edge_ids or []) + (seed_concept_ids or [])
    if seeds:
        return " ".join(seeds[:8])
    return fallback_kind or "recall"


def _project_document(kind: str, identifier: str) -> str:
    """Build the document side of the cross-encoder pair.

    Format: `<canonical-kind> <identifier>` ΓÇö produced
    by prefixing the canonicalised kind exactly ONCE.

    INVARIANT (iter-7 review item 1, refined in iter-8
    review items 1+2): callers MUST pass a BARE GRAPH
    identifier in `identifier` ΓÇö never a string that
    already carries the kind prefix, and never the Qdrant
    `qdrant_point_id`. The graph-identifier contract is
    what makes `_flatten_pairs` (train) and `/rank`
    (inference) produce byte-identical document surfaces:

      - `_flatten_pairs` calls `_project_document(seed_kind,
        seed_id)` where `seed_id` is the bare graph
        node_id from `LabelledPair.seed_node_ids[i]` (or
        the bare concept_id from `seed_concept_ids`,
        edge_id from `seed_edge_ids`). The Go-side
        `internal/rerankertrainer/pairs.go` hydrates these
        from `recall_context_log.node_ids` which the recall
        handler populated with the GRAPH `resp.Nodes[i].NodeID`
        (see `buildContextLogInput` at
        `internal/agentapi/recall.go:1368-1371`).
      - `/rank` calls `_project_document(candidate.kind,
        candidate.point_id)` where `candidate.point_id` is
        the wire-envelope field name (back-compat) but the
        VALUE is the graph id. The Go-side
        `candidateGraphID` in
        `internal/agentapi/bert_sidecar_decoder.go` reads
        the graph id back out of the publisher's Qdrant
        payload (`payload["node_id"]` for
        method/block/frontier, `payload["concept_id"]` for
        concept) and uses that as the wire envelope's
        `point_id` field ΓÇö explicitly NOT the
        `Hit.PointID` which would be the
        `embedding_publish.qdrant_point_id` (a separately-
        minted UUID, see
        `internal/embedding/publisher.go:650-658`).

    Both surfaces collapse to `<canonical-kind> <graph_id>`.
    The cross-encoder is fine-tuned and queried on this
    SAME shape; without the graph-id contract the rank-
    time documents would diverge from training in two
    distinct ways:

      (a) pre-iter-7 / iter-5-style: `<kind> <payload_text>`
          at rank vs `<kind> <seed_id>` at train ΓÇö
          completely different token streams; or
      (b) iter-7-style (the bug iter-8 closes): `<kind>
          <qdrant_point_id>` at rank vs `<kind>
          <graph_node_id>` at train ΓÇö same surface SHAPE
          but different ID strings, because the publisher
          mints qdrant_point_ids separately from graph
          node_ids. The cross-encoder learned a
          per-node_id signal; asking it to score on
          qdrant_point_ids is asking it to score on tokens
          it never saw at train time.

    Why graph id rather than payload text: the labelled-
    pair pull layer (Go-side
    `internal/rerankertrainer/pairs.go`) does NOT fetch
    the Qdrant payload text for each seed. A future
    enhancement could add a `seed_texts` field to
    `LabelledPair`, hydrate it via the graph store, and
    have BOTH train and rank emit `<canonical-kind>
    <payload_text>` for stronger text-based learning;
    that is a follow-up workstream rather than an iter-8
    fix. The current graph-id surface is honest about
    what the operator labels actually supervise:
    `(query, graph_node_id) ΓåÆ relevant?`.

    Returns the bare canonical kind when identifier is
    empty (degenerate fallback used by `_flatten_pairs`
    when a pair has neither seeds nor observations).
    """
    k = _canonical_kind(kind)
    if identifier:
        return f"{k} {identifier}"
    return k


# ---------------------------------------------------------------------------
# Model store
#
# Iter-4 review items 1+2 closed two related bugs:
#
#   Item 1: `/rank` ignored `artifact_uri` and always scored
#           with the in-memory singleton, so published
#           `file://<version>` checkpoints were never actually
#           loaded across restarts or multiple versions.
#   Item 2: `/train` mutated the singleton in place, so a
#           repeat call with the same payload trained from
#           already-mutated weights ΓÇö different bytes under
#           the same version, breaking the `reranker_model.
#           version` UNIQUE-INSERT idempotency contract.
#
# The `ModelStore` is the structural cure for both:
#
#   - `make_fresh_model()` is called per `/train` and
#     returns a NEWLY-constructed CrossEncoder instance
#     (loaded from the HuggingFace cache the second time
#     on, so the cost is bounded). The fit happens against
#     this fresh instance, never against shared state.
#
#   - `load_checkpoint(uri)` is called per `/rank` and
#     returns the CrossEncoder loaded from the on-disk
#     artifact at `uri`. An LRU cache (capacity 4) keeps
#     the hot checkpoints in memory across requests
#     without unbounded growth.
#
# All loads still pay the 200M-param cap from ┬º6.4 step-3.
# ---------------------------------------------------------------------------


class ModelStore:
    """Per-request CrossEncoder factory + per-URI LRU cache.

    The store owns no mutable scoring state; every /train
    gets a fresh instance, and every /rank either hits the
    LRU or pays the disk-read cost.
    """

    LRU_CAPACITY = 4

    def __init__(self, model_name: str, artifact_dir: Path) -> None:
        self._model_name = model_name
        self._artifact_dir = artifact_dir.resolve()
        self._lock = threading.Lock()
        self._ready = False
        # base_param_count caches the result of the ┬º6.4
        # step-3 200M cap check on the base model. Re-used
        # by /healthz so operators can confirm the loaded
        # backbone is in-bounds.
        self._base_param_count = 0
        # _cache maps absolute path -> CrossEncoder instance.
        # OrderedDict provides O(1) move-to-end for the LRU
        # eviction policy.
        self._cache: "OrderedDict[str, object]" = OrderedDict()
        # _pending maps absolute path -> threading.Event signalled
        # when the in-flight cold load for that key has finished
        # (either populating _cache or failing). Under load, two
        # concurrent /rank requests for the same cold artifact_uri
        # would otherwise both miss the cache, both pay the
        # ~200ms CrossEncoder(key) cost, and both insert -- wasting
        # CPU/RAM and transiently exceeding LRU_CAPACITY. The
        # sentinel collapses duplicate loads to one without
        # serialising loads across DIFFERENT keys (the actual
        # CrossEncoder() call still runs outside _cache_lock).
        self._pending: "dict[str, threading.Event]" = {}
        # _cache_lock guards _cache AND _pending. Reusing one lock
        # keeps the "is it cached / is a load in flight / claim
        # leadership" decision atomic in a single critical section.
        self._cache_lock = threading.Lock()

    def load_base(self) -> None:
        """Pre-warm the HuggingFace cache by instantiating
        the base model ONCE at startup. Subsequent
        `make_fresh_model()` calls re-instantiate but hit
        the warm HF cache so cold-start cost amortises to
        ~200ms instead of 30s+ (HF download).

        Also enforces the ┬º6.4 step-3 200M parameter cap on
        the base model. The cap-violation RuntimeError is
        raised OUTSIDE the introspection try/except so a
        broad `except` cannot swallow it (iter-3 review
        item 2 contract preserved).
        """
        with self._lock:
            if self._ready:
                return
            from sentence_transformers import CrossEncoder  # type: ignore

            logger.info("loading base cross-encoder %s", self._model_name)
            base = CrossEncoder(self._model_name)

            try:
                inner = getattr(base, "model", None)
                if inner is not None:
                    self._base_param_count = sum(p.numel() for p in inner.parameters())
                    logger.info(
                        "base model introspected; %s parameters", self._base_param_count
                    )
            except (AttributeError, TypeError, RuntimeError) as exc:
                logger.warning("could not introspect param count: %s", exc)
                self._base_param_count = 0

            if self._base_param_count > 200_000_000:
                raise RuntimeError(
                    f"base model {self._model_name} has {self._base_param_count} "
                    f"params, exceeds Stage 6.4 step-3 cap of 200M -- refusing to load"
                )

            # Drop the warm-up instance; the HF cache
            # remains populated so make_fresh_model() is fast.
            del base
            self._ready = True

    @property
    def ready(self) -> bool:
        return self._ready

    @property
    def base_param_count(self) -> int:
        return self._base_param_count

    @property
    def model_name(self) -> str:
        return self._model_name

    def make_fresh_model(self):  # type: ignore[no-untyped-def]
        """Return a NEW CrossEncoder instance for /train.

        Critical: this does NOT return the singleton. Each
        /train call gets its own instance, so a fit against
        this model cannot mutate state visible to other
        requests. Combined with the deterministic seed
        in `fit_model()`, two /train calls with byte-
        identical input produce byte-identical weights ΓÇö
        the idempotency contract the
        `reranker_model.version` UNIQUE-INSERT relies on
        (iter-4 review item 2).
        """
        from sentence_transformers import CrossEncoder  # type: ignore

        return CrossEncoder(self._model_name)

    def load_checkpoint(self, artifact_uri: str):  # type: ignore[no-untyped-def]
        """Load a trained checkpoint from a `file://` URI
        for /rank. URI MUST resolve to a directory under
        `self._artifact_dir` (defence-in-depth against a
        malformed request reading arbitrary host paths).

        Maintains an LRU cache of at most LRU_CAPACITY
        loaded models so warm checkpoints score
        cheaply (~5ms predict vs ~200ms cold load).
        Returns the CrossEncoder; raises HTTPException on
        validation failure so the FastAPI handler can
        return a 400/404 without further wrapping.

        Iter-4 review item 1: this is the load path that
        was missing ΓÇö without it, `/rank` could only ever
        score with the singleton, so published artifacts
        across restarts/versions were dead bytes.
        """
        if not artifact_uri.startswith("file://"):
            raise HTTPException(
                status_code=400,
                detail=f"artifact_uri must use file:// scheme, got {artifact_uri!r}",
            )
        raw_path = artifact_uri[len("file://") :]
        candidate = Path(raw_path).resolve()
        # Defence-in-depth: refuse paths that escape the
        # configured artifact directory. A malformed or
        # adversarial request must not be able to load an
        # arbitrary on-disk torch checkpoint as a "model".
        try:
            candidate.relative_to(self._artifact_dir)
        except ValueError:
            raise HTTPException(
                status_code=400,
                detail=(
                    f"artifact_uri {artifact_uri!r} resolves outside the configured "
                    f"artifact_dir {self._artifact_dir!s}"
                ),
            )
        if not candidate.exists() or not candidate.is_dir():
            raise HTTPException(
                status_code=404,
                detail=f"artifact {artifact_uri!r} not found on disk",
            )

        key = str(candidate)
        # Cache lookup + in-flight-load detection runs in one
        # critical section so two concurrent misses for the same
        # `key` cannot both decide to load. The first miss claims
        # leadership by inserting a fresh Event into _pending;
        # subsequent misses for the SAME key find that event and
        # wait on it instead of re-loading. Different keys still
        # proceed independently because the lock is released
        # before the CrossEncoder() call.
        while True:
            with self._cache_lock:
                cached = self._cache.get(key)
                if cached is not None:
                    # Hot path: move-to-end marks recently used.
                    self._cache.move_to_end(key)
                    return cached
                in_flight = self._pending.get(key)
                if in_flight is None:
                    # We are the loader for this key.
                    load_event = threading.Event()
                    self._pending[key] = load_event
                    we_load = True
                else:
                    # Another request is already loading this key.
                    load_event = in_flight
                    we_load = False
            if we_load:
                break
            # Wait for the in-flight load to finish, then re-check
            # the cache. Looping (rather than returning the leader's
            # model directly) keeps the LRU's move-to-end semantics
            # correct AND handles the case where the leader's load
            # raised -- waiters then naturally retry as fresh
            # leaders on the next iteration.
            load_event.wait()

        try:
            from sentence_transformers import CrossEncoder  # type: ignore

            logger.info("loading checkpoint %s", key)
            model = CrossEncoder(key)

            with self._cache_lock:
                self._cache[key] = model
                self._cache.move_to_end(key)
                while len(self._cache) > self.LRU_CAPACITY:
                    evicted_key, _ = self._cache.popitem(last=False)
                    logger.info("evicted checkpoint %s from LRU", evicted_key)
        finally:
            # ALWAYS clear the pending entry and wake waiters,
            # whether the load succeeded or raised. If it raised,
            # waiters loop back, find no cached entry and no
            # pending entry, and become leaders for a retry --
            # exactly what a transient model-load failure deserves.
            with self._cache_lock:
                self._pending.pop(key, None)
            load_event.set()
        return model


def fit_model(  # type: ignore[no-untyped-def]
    model, pairs: List[Tuple[str, str, float]], epochs: int, seed: int
) -> dict:
    """Fit `model` (a sentence_transformers.CrossEncoder)
    against (query, document, label) triples. Returns the
    ┬º6.4 required metric set:
        - train_loss
        - eval_ndcg@k
        - rank-of-correct-node@k=20

    DETERMINISM CONTRACT (iter-3 review item 3 +
    iter-4 review item 2): two calls with the same `model`-
    initial-state, same `pairs`, same `epochs`, same `seed`
    MUST produce byte-identical model weights. This is the
    idempotency the `reranker_model.version` UNIQUE-INSERT
    relies on. Enforced by:
      1. Caller passes a FRESH model from
         `ModelStore.make_fresh_model()` ΓÇö no shared state.
      2. Seeding python.random / numpy / torch / cuDNN with
         `seed` BEFORE the first fit step.
      3. `shuffle=False` on the DataLoader.
    """
    import random
    import numpy as np
    import torch
    from sentence_transformers import InputExample  # type: ignore
    from torch.utils.data import DataLoader  # type: ignore

    random.seed(seed)
    np.random.seed(seed & 0xFFFFFFFF)
    torch.manual_seed(seed)
    if torch.cuda.is_available():
        torch.backends.cudnn.deterministic = True
        torch.backends.cudnn.benchmark = False

    examples = [InputExample(texts=[q, d], label=lbl) for q, d, lbl in pairs]
    loader = DataLoader(examples, shuffle=False, batch_size=16)
    model.fit(
        train_dataloader=loader,
        epochs=epochs,
        show_progress_bar=False,
    )

    positives = [(q, d, lbl) for q, d, lbl in pairs if lbl > 0.5]
    if not positives:
        return {
            "train_loss": 0.0,
            "eval_ndcg@k": 0.0,
            "rank-of-correct-node@k=20": 0.0,
        }
    scores = model.predict([(q, d) for q, d, _ in pairs])
    ranked = sorted(
        ((s, lbl) for s, (_, _, lbl) in zip(scores, pairs)),
        key=lambda x: x[0],
        reverse=True,
    )
    rank_of_correct = next(
        (i + 1 for i, (_, lbl) in enumerate(ranked[:20]) if lbl > 0.5), 21
    )
    import math

    dcg = sum(
        (1.0 if lbl > 0.5 else 0.0) / math.log2(i + 2)
        for i, (_, lbl) in enumerate(ranked[:20])
    )
    idcg = sum(1.0 / math.log2(i + 2) for i in range(min(len(positives), 20)))
    ndcg = dcg / idcg if idcg > 0 else 0.0

    # Cross-entropy proxy for train_loss (CrossEncoder.fit
    # doesn't expose the loss curve).
    loss = 0.0
    n = 0
    for s, (_, _, lbl) in zip(scores, pairs):
        p = 1.0 / (1.0 + math.exp(-float(s)))
        p = max(min(p, 1 - 1e-9), 1e-9)
        loss += -(lbl * math.log(p) + (1 - lbl) * math.log(1 - p))
        n += 1
    train_loss = loss / n if n else 0.0
    return {
        "train_loss": float(train_loss),
        "eval_ndcg@k": float(ndcg),
        "rank-of-correct-node@k=20": float(rank_of_correct),
    }


def save_model(model, dest_dir: Path) -> None:  # type: ignore[no-untyped-def]
    """Save a fitted model to dest_dir atomically: write
    to a sibling staging dir, then rename. Avoids leaving
    a half-written checkpoint when a save crashes mid-flight
    (which would later be silently loaded by /rank)."""
    dest_dir = dest_dir.resolve()
    dest_dir.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=".staging-", dir=str(dest_dir.parent)))
    try:
        model.save(str(staging))
        if dest_dir.exists():
            shutil.rmtree(dest_dir)
        staging.rename(dest_dir)
    except Exception:
        if staging.exists():
            shutil.rmtree(staging, ignore_errors=True)
        raise


# ---------------------------------------------------------------------------
# FastAPI surface
# ---------------------------------------------------------------------------


def make_app(config: SidecarConfig) -> FastAPI:
    """Constructs the FastAPI app for the configured sidecar.
    Factored out as a function so unit tests can spin up the
    app with an injected config without touching real env."""
    app = FastAPI(title="reranker-sidecar", version="0.1.0")
    config.artifact_dir.mkdir(parents=True, exist_ok=True)
    store = ModelStore(config.model_name, config.artifact_dir)

    @app.on_event("startup")
    def _startup() -> None:
        # Load the base model in a background thread so
        # /healthz can come up immediately and return 503
        # until the base is loaded (the HF cache is then
        # warm for make_fresh_model() calls).
        def _loader() -> None:
            try:
                store.load_base()
            except Exception:
                logger.exception("base model load failed")

        threading.Thread(target=_loader, daemon=True).start()

    @app.get("/healthz")
    def healthz() -> JSONResponse:
        if not store.ready:
            return JSONResponse(
                {"status": "loading", "model_name": store.model_name},
                status_code=503,
            )
        return JSONResponse(
            {
                "status": "ready",
                "model_name": store.model_name,
                "param_count": store.base_param_count,
            }
        )

    @app.post("/train")
    def train(payload: TrainingInput) -> JSONResponse:
        if not store.ready:
            raise HTTPException(status_code=503, detail="model still loading")
        if len(payload.positives) + len(payload.negatives) > config.max_pairs:
            raise HTTPException(
                status_code=413,
                detail=(
                    f"input has {len(payload.positives) + len(payload.negatives)} pairs; "
                    f"max is {config.max_pairs}"
                ),
            )
        pairs = _flatten_pairs(payload)
        if not pairs:
            raise HTTPException(status_code=400, detail="no labelled pairs in request")

        version = _derive_version(payload, config.model_name, config.max_epochs)
        dest = config.artifact_dir / version

        # IDEMPOTENCY SHORT-CIRCUIT (iter-4 review item 2):
        # If a checkpoint at this version already exists on
        # disk, skip the fit and return the existing URI.
        # This makes /train idempotent end-to-end even when
        # the upstream Go-side trainer retries: the on-disk
        # weights are NOT mutated, and the response is the
        # same as the original successful call. Combined
        # with `make_fresh_model()` for genuine first-time
        # calls, two calls with the same payload guarantee
        # the same artifact bytes.
        if dest.exists() and dest.is_dir() and any(dest.iterdir()):
            logger.info(
                "train: short-circuit -- version %s already exists at %s",
                version,
                dest,
            )
            artifact_uri = f"file://{dest.absolute()}"
            return JSONResponse(
                {
                    "version": version,
                    "artifact_uri": artifact_uri,
                    "trained_at": "",
                    "metrics": {
                        "fit_seconds": 0.0,
                        "positives": float(len(payload.positives)),
                        "negatives": float(len(payload.negatives)),
                        "epochs": float(config.max_epochs),
                        "train_loss": 0.0,
                        "eval_ndcg@k": 0.0,
                        "rank-of-correct-node@k=20": 0.0,
                        "idempotent_replay": 1.0,
                    },
                    "publish_status": "published",
                }
            )

        # Fresh model per train so weights start identical
        # (iter-4 review item 2). Combined with the
        # version-derived seed below, two calls with the
        # same payload produce byte-identical weights.
        fresh = store.make_fresh_model()
        seed = _seed_from_version(version)

        started = time.time()
        metrics = fit_model(fresh, pairs, config.max_epochs, seed=seed)
        elapsed = time.time() - started
        metrics["fit_seconds"] = elapsed
        metrics["positives"] = float(len(payload.positives))
        metrics["negatives"] = float(len(payload.negatives))
        metrics["epochs"] = float(config.max_epochs)
        metrics["idempotent_replay"] = 0.0

        save_model(fresh, dest)

        artifact_uri = f"file://{dest.absolute()}"
        return JSONResponse(
            {
                "version": version,
                "artifact_uri": artifact_uri,
                "trained_at": "",
                "metrics": metrics,
                "publish_status": "published",
            }
        )

    @app.post("/rank")
    def rank(payload: dict) -> JSONResponse:
        """Score the supplied candidates against the
        cross-encoder loaded from `artifact_uri` (iter-4
        review item 1: the published checkpoint is genuinely
        loaded and consumed at recall time).

        Request shape (mirror in
        services/agent-memory/internal/agentapi/bert_sidecar_decoder.go):
            {
              "artifact_uri": "file:///path/to/<version>",
              "query": "the natural-language recall query",
              "candidates": [
                {"point_id": "...", "kind": "method", "text": "...",
                 "score": 0.42, "structural_distance": 0}
              ]
            }
        Response shape:
            {"scored": [{"point_id": "...", "score": 0.91}, ...]}

        The (query, document) projection uses the SAME
        helpers (`_project_query`, `_project_document`) as
        the train-time `_flatten_pairs`, AND both sides
        pass a BARE GRAPH ID as the document identifier
        (iter-8 review items 1+2 contract) so the cross-
        encoder is asked to score on the SAME
        `<canonical-kind> <graph_id>` surface it was
        fine-tuned against.

        IMPORTANT (iter-8 review item 1): the wire field
        named `point_id` in the request envelope below is a
        BACK-COMPAT name, not a Qdrant `qdrant_point_id`.
        Go's `candidateGraphID` in
        internal/agentapi/bert_sidecar_decoder.go reads the
        graph id back out of the publisher's Qdrant payload
        (`payload["node_id"]` for method/block/frontier,
        `payload["concept_id"]` for concept) and sends THAT
        as the wire envelope's `point_id` value ΓÇö the
        sidecar never talks to Qdrant and only uses this
        field as a unique key. Training carries the SAME
        graph id via `LabelledPair.SeedNodeIDs` /
        `LabelledPair.SeedConceptIDs` (hydrated from
        `recall_context_log.node_ids` /
        `recall_context_log.concept_ids` ΓÇö see
        `internal/rerankertrainer/pairs.go`). The `text`
        field in this envelope is informational/diagnostic
        only (Go currently sets it equal to the graph id;
        a future text-aware trainer would consume it).
        """
        if not store.ready:
            raise HTTPException(status_code=503, detail="model still loading")
        artifact_uri = payload.get("artifact_uri")
        if not isinstance(artifact_uri, str) or not artifact_uri:
            raise HTTPException(status_code=400, detail="artifact_uri: required string")
        candidates = payload.get("candidates")
        if not isinstance(candidates, list) or not candidates:
            raise HTTPException(status_code=400, detail="candidates: required non-empty list")

        # Load the checkpoint at `artifact_uri` (LRU cache).
        # Raises HTTPException(400/404) on validation failure.
        model = store.load_checkpoint(artifact_uri)

        recall_query = str(payload.get("query") or "")
        pairs = []
        ids = []
        for c in candidates:
            if not isinstance(c, dict):
                raise HTTPException(status_code=400, detail="candidates[i]: object required")
            pid = c.get("point_id")
            if not isinstance(pid, str) or pid == "":
                raise HTTPException(status_code=400, detail="candidates[i].point_id: required string")
            # SHARED projection (iter-4 item 3 + iter-7 item 1
            # + iter-8 item 1): the SAME _project_query /
            # _project_document helpers `_flatten_pairs` uses
            # at train time.
            #
            # iter-8 NOTE: the wire `point_id` here carries
            # the GRAPH id (graph `node_id` for
            # method/block/frontier, `concept_id` for concept)
            # ΓÇö NOT the Qdrant `qdrant_point_id`. Go's
            # `candidateGraphID` in
            # internal/agentapi/bert_sidecar_decoder.go reads
            # the graph id back out of the publisher payload
            # and uses it as the request `point_id` field
            # value, matching the SAME id training carries via
            # `LabelledPair.SeedNodeIDs` /
            # `LabelledPair.SeedConceptIDs`. Both surfaces
            # therefore project from `(canonical_kind,
            # graph_id)` and `_project_document` produces
            # byte-identical document strings at train and
            # rank time. The `text` field in this envelope is
            # informational/diagnostic only (Go currently sets
            # it equal to the graph id too; a future
            # text-aware trainer would consume it).
            kind = c.get("kind") or ""
            query_str = _project_query(
                recall_query=recall_query,
                seed_node_ids=[],
                seed_edge_ids=[],
                seed_concept_ids=[],
                fallback_kind=kind,
            )
            doc_str = _project_document(kind, pid)
            pairs.append((query_str, doc_str))
            ids.append(pid)

        scores = model.predict(pairs)
        scored = [
            {"point_id": pid, "score": float(score)}
            for pid, score in zip(ids, scores)
        ]
        return JSONResponse({"scored": scored})

    return app


def _flatten_pairs(payload: TrainingInput) -> List[Tuple[str, str, float]]:
    """Translate the LabelledPair envelope into
    (query, document, label) triples the CrossEncoder
    consumes.

    Uses the SAME _project_query / _project_document helpers
    as the /rank endpoint AND emits one training example per
    seed identifier so the train-time AND rank-time documents
    BOTH project from `(canonical_kind, graph_id)` ΓÇö the
    cross-encoder is fine-tuned and queried on the SAME
    `<canonical-kind> <graph_id>` surface shape (iter-8
    review item 1).

    The `graph_id` here is the GRAPH node identifier (UUID),
    NOT the Qdrant `qdrant_point_id`:

      - At train time the graph id comes straight from
        `LabelledPair.SeedNodeIDs` /
        `LabelledPair.SeedConceptIDs` /
        `LabelledPair.SeedEdgeIDs`, hydrated by
        `internal/rerankertrainer/pairs.go` from
        `recall_context_log.{node_ids, concept_ids,
        edge_ids}` (which the recall handler populated with
        `resp.Nodes[i].NodeID` etc. ΓÇö see
        `internal/agentapi/recall.go:buildContextLogInput`).
      - At rank time the same graph id comes from the
        wire envelope's `point_id` field (back-compat name)
        which Go's `candidateGraphID` extracts from
        `Candidate.Payload["node_id"]` /
        `Candidate.Payload["concept_id"]` ΓÇö the publisher
        always writes these onto every Qdrant row.

    Both surfaces therefore project from
    `(canonical_kind, graph_id)` via the same
    `_project_document` helper.

    Iteration history for the contract this docstring
    encodes:
      - iter-5/6: projection helpers introduced; train
        projected `(episode_kind, joined observation
        roles)` while rank projected `(kind, candidate
        text or point_id)` ΓÇö mismatched surfaces.
      - iter-7: Go-side `candidateText` reduced to bare
        `c.PointID` (the Qdrant qdrant_point_id) so the
        token shape matched, but the IDs DIDN'T ΓÇö
        production trains on graph node UUIDs while
        ranking carried qdrant point UUIDs.
      - iter-8 (current): Go-side `candidateGraphID`
        reads the graph node UUID back out of the Qdrant
        payload at rank time so the rank-side wire
        `point_id` carries the SAME UUID training carries
        via `SeedNodeIDs`. Both surfaces collapse to the
        same bytes by construction.

    Emission rule, in order of preference:
      1. ONE training example per seed_node_id +
         seed_concept_id + seed_edge_id (up to 8 each).
         Document = `_project_document(<canonical-kind>,
         <graph_id>)` ΓÇö the EXACT shape `/rank` produces
         from `_project_document(candidate.kind,
         wire_point_id_which_is_the_graph_id)`. This is
         the dominant path: any labelled pair that came
         from a real recall has seeds, and the model
         learns the same `(query, kind, graph_id)` ΓåÆ
         score mapping at train and rank time.
      2. Fallback when the pair has zero seeds (rare ΓÇö
         degraded recalls, manually-injected pairs): one
         training example per observation, document =
         `_project_document(observation.role, "")` (the
         degenerate kind-only surface). Still uses the
         shared projection function so the trained weights
         apply to the same surface format.
      3. Last-resort fallback when zero seeds AND zero
         observations: one degenerate example with
         `_project_document(episode_kind, "")` so the pair
         still contributes a (q, kind-only-d, label) gradient.
    """
    out: List[Tuple[str, str, float]] = []
    for label_val, pairs in ((1.0, payload.positives), (0.0, payload.negatives)):
        for pair in pairs:
            q = _project_query(
                recall_query=pair.recall_query,
                seed_node_ids=pair.seed_node_ids,
                seed_edge_ids=pair.seed_edge_ids,
                seed_concept_ids=pair.seed_concept_ids,
                fallback_kind=pair.episode_kind,
            )
            emitted = 0
            for ids, seed_kind in (
                (pair.seed_node_ids, "method"),
                (pair.seed_edge_ids, "edge"),
                (pair.seed_concept_ids, "concept"),
            ):
                for sid in (ids or [])[:8]:
                    d = _project_document(seed_kind, sid)
                    out.append((q, d, label_val))
                    emitted += 1
            if emitted == 0:
                for obs in (pair.observations or [])[:8]:
                    if not obs.role:
                        continue
                    d = _project_document(obs.role, "")
                    out.append((q, d, label_val))
                    emitted += 1
            if emitted == 0:
                d = _project_document(pair.episode_kind, "")
                out.append((q, d, label_val))
    return out


def _derive_version(payload: TrainingInput, model_name: str, epochs: int) -> str:
    """Deterministic version fingerprint over (model name,
    epochs, ALL payload fields that affect the trained
    weights). MUST be stable across calls with the same
    input so the Go side's ON CONFLICT idempotency contract
    holds when the sidecar is restarted between requests --
    AND the trained weights MUST be byte-identical for two
    calls that hash to the same version, otherwise the
    UNIQUE-version contract conflates different weights
    under the same handle.

    Iter-3 review item 3 expanded the hash to cover model
    config + per-pair episode_id/episode_kind/seeds/
    observations/actor. Iter-5 review item 3 closes the
    remaining gap: `recall_query` is now hashed too so once
    the Go side populates it (iter-6 item 1+2), two
    training calls with the SAME episode/seed/observation
    shape but DIFFERENT natural-language queries cannot
    collide under the same `reranker_model.version`. The
    fingerprint bumps from `v2` to `v3` to make the schema
    change visible ΓÇö pre-iter-6 versions and iter-6+
    versions will never collide even on identical episode
    surfaces.

    Hashed shape (per pair, in canonical order):
      - label sign (+ for positive, - for negative)
      - episode_id, episode_kind
      - recall_query (the natural-language query)
      - sorted seed_node_ids / seed_edge_ids / seed_concept_ids
      - per observation: role + weight (rounded to 6 decimals
        to absorb float jitter from upstream serialisation)
      - correction_actor
    """
    hasher = hashlib.sha256()
    hasher.update(b"v3|")
    hasher.update(model_name.encode("utf-8"))
    hasher.update(b"|epochs=")
    hasher.update(str(int(epochs)).encode("ascii"))
    hasher.update(b"|")
    for label, pairs in (("+", payload.positives), ("-", payload.negatives)):
        for pair in pairs:
            hasher.update(label.encode("utf-8"))
            hasher.update(b":")
            hasher.update(pair.episode_id.encode("utf-8"))
            hasher.update(b"|kind=")
            hasher.update(pair.episode_kind.encode("utf-8"))
            # Iter-5 review item 3: recall_query MUST be in
            # the hash. Without it, two training calls with
            # different natural-language queries collide
            # under the same version once Go starts
            # populating recall_query.
            hasher.update(b"|q=")
            hasher.update((pair.recall_query or "").encode("utf-8"))
            hasher.update(b"|nodes=")
            for nid in sorted(pair.seed_node_ids):
                hasher.update(nid.encode("utf-8"))
                hasher.update(b",")
            hasher.update(b"|edges=")
            for eid in sorted(pair.seed_edge_ids):
                hasher.update(eid.encode("utf-8"))
                hasher.update(b",")
            hasher.update(b"|concepts=")
            for cid in sorted(pair.seed_concept_ids):
                hasher.update(cid.encode("utf-8"))
                hasher.update(b",")
            hasher.update(b"|obs=")
            for obs in pair.observations:
                hasher.update(obs.role.encode("utf-8"))
                hasher.update(b":")
                hasher.update(f"{obs.weight:.6f}".encode("ascii"))
                hasher.update(b",")
            hasher.update(b"|actor=")
            hasher.update(pair.correction_actor.encode("utf-8"))
            hasher.update(b"\x00")
    return hasher.hexdigest()[:16]


def _seed_from_version(version: str) -> int:
    """Derive a 64-bit int seed from the version string.
    Used to seed python.random / numpy / torch so the
    fit() call is deterministic for a given version."""
    digest = hashlib.sha256(version.encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "big", signed=False)


# ---------------------------------------------------------------------------
# Entrypoint
# ---------------------------------------------------------------------------


def main() -> None:
    import uvicorn

    config = SidecarConfig.from_env()
    app = make_app(config)
    host, _, port = config.listen_addr.lstrip(":").partition(":")
    if not port:
        port = host or "8088"
        host = "0.0.0.0"
    uvicorn.run(app, host=host or "0.0.0.0", port=int(port))


if __name__ == "__main__":
    main()
