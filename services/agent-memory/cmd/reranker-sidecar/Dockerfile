# reranker-sidecar: BERT-class cross-encoder training service
# Stage 6.4 step-3 reference implementation.
#
# Builds a self-contained container hosting
# sentence-transformers' cross-encoder/ms-marco-MiniLM-L-12-v2
# (~33M params, well under the 200M cap). The Go binary at
# services/agent-memory/cmd/reranker-trainer/ POSTs labelled
# pair batches to /train and persists the returned
# version+artifact_uri as a reranker_model row.

FROM python:3.11-slim AS base

# torch needs libgomp1 + a C toolchain headers for ARM
# wheels; keep the runtime image lean by purging build deps
# after install.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        libgomp1 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Install Python deps first so the layer caches across code
# edits.
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

# Warm the HuggingFace cache so the first /train call is not
# blocked on a 200MB+ download. The model is fetched and
# cached under /root/.cache/huggingface during build.
ENV TRANSFORMERS_OFFLINE=0
RUN python -c "from sentence_transformers import CrossEncoder; \
    CrossEncoder('cross-encoder/ms-marco-MiniLM-L-12-v2')"

COPY main.py ./

# Default artifact dir; mount a volume here in production so
# saved cross-encoder checkpoints survive container restarts.
RUN mkdir -p /var/lib/reranker-sidecar/models
VOLUME ["/var/lib/reranker-sidecar/models"]

EXPOSE 8088

# Readiness probe target: /healthz returns 503 until the
# model has loaded into memory, then 200.
HEALTHCHECK --interval=10s --timeout=3s --start-period=120s --retries=3 \
    CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:8088/healthz').status == 200 else 1)"

CMD ["python", "-m", "main"]
