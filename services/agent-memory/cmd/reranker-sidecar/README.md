# reranker-sidecar

BERT-class cross-encoder training service for Stage 6.4 of
the AGENT-MEMORY story. Mirrors the contract declared by the
Go-side `reranker-trainer` binary and satisfies the
`implementation-plan.md` §6.4 step-3 mandate of "Train a
cross-encoder BERT-class model (≤ 200M params)".

## Why a sidecar

The BERT cross-encoder lives in the Python ML stack
(`torch`, `transformers`, `sentence-transformers`). Embedding
it in the Go trainer binary would require either CGo bindings
to libtorch or a Go re-implementation of the transformer
architecture — both unmaintainable at this scope. The HTTP
sidecar boundary is the maintained seam.

## Model

Default: `cross-encoder/ms-marco-MiniLM-L-12-v2`
- ~33M parameters (well under the 200M cap)
- Pre-trained on MS-MARCO passage ranking
- Suitable for the §6.4 "rank candidate Nodes for a recall
  Episode" objective with one epoch of fine-tuning per
  nightly run

Override via `RERANKER_SIDECAR_MODEL_NAME=<huggingface-id>`.
The container enforces the 200M cap at load time — over-cap
models fail fast.

## Wire-up

Set on the Go-side `reranker-trainer` binary:

```sh
AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT=http://reranker-sidecar:8088
# KIND defaults to "sidecar" when ENDPOINT is set.
AGENT_MEMORY_RERANKER_TRAINER_TAG=ms-marco-MiniLM-L-12-v2
```

## HTTP surface

### `GET /healthz`

- `503` while model is loading (returns body
  `{"status":"loading","model_name":"…"}`)
- `200` once loaded with body
  `{"status":"ready","model_name":"…","param_count":N}`

The Go trainer binary's deploy probe SHOULD block on this
endpoint before the first nightly tick.

### `POST /train`

Request body (matches `rerankertrainer.TrainingInput`):

```json
{
  "WindowStart": "2025-08-01T00:00:00Z",
  "WindowEnd":   "2025-10-30T00:00:00Z",
  "TrainerTag":  "ms-marco-MiniLM-L-12-v2",
  "Positives": [
    {
      "EpisodeID": "ep-001",
      "EpisodeKind": "agent",
      "CreatedAt": "2025-10-15T00:00:00Z",
      "SeedNodeIDs": ["node-a", "node-b"],
      "SeedEdgeIDs": [],
      "SeedConceptIDs": [],
      "Observations": [{"role": "node_hit", "weight": 1.0}],
      "CorrectionActor": ""
    }
  ],
  "Negatives": [...]
}
```

Response body (matches the strict `rerankertrainer.TrainingOutput`
contract — every field below is REQUIRED):

```json
{
  "version": "abc123def456",
  "artifact_uri": "file:///var/lib/reranker-sidecar/models/abc123def456",
  "trained_at": "",
  "metrics": {
    "train_loss": 0.482,
    "eval_ndcg@k": 0.871,
    "rank-of-correct-node@k=20": 3.0,
    "fit_seconds": 12.4,
    "positives": 200.0,
    "negatives": 200.0
  },
  "publish_status": "published"
}
```

The Go side REJECTS any response missing `version`,
`artifact_uri`, `publish_status`, or any of the three
required metrics (`train_loss`, `eval_ndcg@k`,
`rank-of-correct-node@k=20`) — see
`internal/rerankertrainer/sidecar_trainer.go:validateSidecarOutput`.

## Configuration

| Env var                          | Default                                            | Purpose                          |
|----------------------------------|----------------------------------------------------|----------------------------------|
| `RERANKER_SIDECAR_LISTEN_ADDR`   | `:8088`                                            | Bind address                     |
| `RERANKER_SIDECAR_MODEL_NAME`    | `cross-encoder/ms-marco-MiniLM-L-12-v2`            | HuggingFace model id             |
| `RERANKER_SIDECAR_ARTIFACT_DIR`  | `/var/lib/reranker-sidecar/models`                 | Output dir for saved checkpoints |
| `RERANKER_SIDECAR_MAX_EPOCHS`    | `1`                                                | Training epochs per request      |
| `RERANKER_SIDECAR_MAX_PAIRS`     | `100000`                                           | Reject request if pair-count exceeds (OOM guard) |

## Local dev

```sh
cd services/agent-memory/cmd/reranker-sidecar
pip install -r requirements.txt
python -m main
# In another shell
curl http://localhost:8088/healthz
```

## Docker

```sh
docker build -t reranker-sidecar services/agent-memory/cmd/reranker-sidecar
docker run --rm -p 8088:8088 -v rerank-models:/var/lib/reranker-sidecar/models reranker-sidecar
```

The model is pre-warmed at image-build time so the first
`/train` call is not blocked on a 200MB+ HuggingFace
download.

## Artifact format & recall consumption

The sidecar saves a sentence-transformers `CrossEncoder` to
disk at `<artifact_dir>/<version>/` and returns
`file://<absolute-path>` as the ArtifactURI. The agent-api
recall path's `PublishedReranker` is wired with the
`BertSidecarDecoder` (`services/agent-memory/internal/agentapi/
bert_sidecar_decoder.go`) which recognises `file://` URIs and
POSTs candidate scoring requests back to THIS sidecar's
`/rank` endpoint over the SAME HTTP boundary used by `/train`.
The trained checkpoint is genuinely loaded (LRU-cached by URI
inside the sidecar's `ModelStore`) and consumed at recall
time — there is no "fall back to V0" path on `file://`
artifacts when the sidecar is reachable.

When the inference endpoint is unset on the agent-api side
(`AGENT_MEMORY_RERANKER_INFERENCE_ENDPOINT` not configured),
the `BertSidecarDecoder` is **not appended** to the chain
(see the conditional at `services/agent-memory/cmd/agent-api/
main.go` immediately before the `NewPublishedReranker(...)`
call). The chain still recognises `data:` URIs through
`LinearWeightsDecoder`, so the linear trainer's inlined
weights keep working. Unrecognised `file://` URIs cause
`MultiArtifactDecoder.Decode` to return `(nil, false, nil)`
(the same shape `DisabledArtifactDecoder` returns), and the
wrapping `PublishedReranker` cleanly falls back to the inner
V0 cold-start scorer — a deliberate "no broken deployments
when the sidecar isn't wired" guard. In this configuration
the trainer-side artifact is still persisted and the latest
version is still advertised on the recall response envelope
through the `rankWithVersion` shim; only candidate-level
reranking falls back. See the inline composition at
`services/agent-memory/cmd/agent-api/main.go` (search for
`NewPublishedReranker`), the wrapper contract at
`services/agent-memory/internal/agentapi/
published_reranker.go`, the chain at
`services/agent-memory/internal/agentapi/
multi_artifact_decoder.go`, and the sidecar decoder at
`services/agent-memory/internal/agentapi/
bert_sidecar_decoder.go`.

## In-process unit tests

The pure-Python helpers — `_canonical_kind`,
`_project_query`, `_project_document`, `_flatten_pairs`,
`_derive_version`, and the v3 fingerprint schema — are
covered by `test_main.py` (37 stdlib `unittest` tests, no
pytest dependency, no torch / sentence-transformers /
uvicorn requirement). Run with:

```
cd services/agent-memory/cmd/reranker-sidecar
python -m unittest test_main
```

The HTTP shape is additionally contract-tested from the Go
side: `internal/rerankertrainer/sidecar_trainer_test.go`
exercises every accept/reject branch via
`httptest.NewServer`, and
`internal/agentapi/bert_sidecar_decoder_test.go` covers the
`/rank` wire envelope including the iter-8 graph-id
contract, frontier-candidate scoring, and the
scorable/unscorable partition.

The sidecar's own ML pipeline is intentionally a thin
wrapper around `sentence-transformers.CrossEncoder.fit`; an
integration test that actually trains a model takes minutes
and pulls a 200MB+ HuggingFace asset, so we keep the live
ML test out of CI and validate the wrapper through the
Go-side contract tests plus the in-process helper tests
described above.
