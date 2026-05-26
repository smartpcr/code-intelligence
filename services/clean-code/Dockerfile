# syntax=docker/dockerfile:1
# Multi-service Dockerfile for clean-code services.
# Build arg SERVICE selects the binary to build (clean-code-indexer | clean-code-metric-ingestor).

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

EXPOSE 8080
ENTRYPOINT ["app"]