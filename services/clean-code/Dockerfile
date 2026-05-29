# syntax=docker/dockerfile:1
# Multi-service Dockerfile for clean-code services.
# Build arg SERVICE selects the binary to build (clean-code-indexer | clean-code-metric-ingestor | clean-code-refactor-planner).

FROM golang:1.25-alpine AS builder

ARG SERVICE=clean-code-metric-ingestor

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /app ./cmd/${SERVICE}

FROM alpine:3.20

ARG SERVICE=clean-code-metric-ingestor
ARG ROLE=""

ENV SERVICE_NAME=${SERVICE}
ENV SERVICE_ROLE=${ROLE}

COPY --from=builder /app /usr/local/bin/app

# Stage 9.3 iter-9: ship the ML effort-model artefact alongside
# the binary so the refactor-planner's `file:///models/...`
# URI resolves at runtime. The artefact is a small placeholder
# (the v0 estimator is a deterministic FNV hash of the contents
# folded with the task identifiers); a future Stage 10.x will
# replace the loader + artefact with a real ONNX runtime. The
# COPY is unconditional so the metric-ingestor image carries the
# file too -- harmless extra bytes that simplify the Dockerfile.
COPY cmd/clean-code-refactor-planner/effort_model.onnx /models/effort_model.onnx

EXPOSE 8080
ENTRYPOINT ["app"]
