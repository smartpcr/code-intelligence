# Multi-stage build for the Stage 8.2 partition-maintainer
# binary. Build context MUST be the agent-memory Go module
# root (`services/agent-memory/`) so the full module + vendor
# graph is available to `go build`. Compose wires this as
#
#   build:
#     context: ../..
#     dockerfile: cmd/partition-maintainer/Dockerfile
#
# from `services/agent-memory/deploy/local/docker-compose.yml`.

# ---- builder ----------------------------------------------------------------
FROM golang:1.25-alpine AS builder

# ca-certificates so the (eventual) outbound HTTPS calls
# (Prometheus scrape over TLS in non-local envs) work; git for
# go mod download in case GOPROXY needs it.
RUN apk add --no-cache ca-certificates git

WORKDIR /src

# Module-cache layer: copy only go.mod / go.sum first so a
# source-only edit does not invalidate the dep-download layer.
COPY go.mod go.sum ./
RUN go mod download

# Now copy the full module. Compose's `context: ../..` means
# the build context root IS `services/agent-memory/`.
COPY . .

# Build a statically-linked binary so the runtime stage can be
# scratch / distroless. `CGO_ENABLED=0` keeps lib/pq off the
# C-toolchain dependency (lib/pq is pure Go).
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath
RUN go build -ldflags="-s -w" -o /out/partition-maintainer ./cmd/partition-maintainer

# ---- runtime ----------------------------------------------------------------
# distroless/static is the canonical "no shell, no package
# manager, only ca-certs + tzdata" runtime. The maintainer
# needs nothing else at runtime. We DO copy in a tiny static
# busybox so the docker-compose HEALTHCHECK can probe
# `:8087/healthz` from inside the container -- distroless ships
# no wget/curl/sh, and the compose stack's existing pattern
# (see qdrant/Dockerfile and otel/Dockerfile) is to bake in a
# static busybox for HEALTHCHECK use only. The maintainer
# process itself never invokes /busybox.
FROM busybox:1.36.1-uclibc AS busybox

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/partition-maintainer /partition-maintainer
COPY --from=busybox /bin/busybox /busybox

# /healthz + /metrics are served on :8087 (the package
# `partitionmaintainer` default; overridable via
# AGENT_MEMORY_LISTEN_ADDR).
EXPOSE 8087

USER nonroot:nonroot

ENTRYPOINT ["/partition-maintainer"]
