# Multi-stage build for the clean-code service.
#
# Stage 1.1 (implementation-plan.md) ships the `clean-coded`
# binary which serves /healthz and /readyz. Future stages add
# the Agent / Evaluator gRPC surfaces, the Refactor Planner, etc.
# behind the same binary -- no Dockerfile churn required.
#
# The final image is distroless-style: it carries the static
# `clean-coded` binary plus CA roots and nothing else. No
# package manager, no shell.

# -------- builder stage ----------------------------------------
FROM golang:1.25-bookworm AS builder

# Pin compiler env so the binary is statically linked and
# reproducible. CGO_ENABLED=0 keeps libc out of the binary so the
# final stage can stay distroless.
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    GOFLAGS="-buildvcs=false"

WORKDIR /src

# Cache go.mod / go.sum download separately so a docs-only diff
# does not invalidate the dependency layer.
COPY go.mod ./
RUN go mod download

COPY . .

# Build the clean-coded binary. The version metadata is stamped
# via -ldflags so /healthz output reflects the deploy.
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN go build -trimpath \
        -ldflags "-s -w \
            -X github.com/smartpcr/code-intelligence/services/clean-code/internal/version.Version=${VERSION} \
            -X github.com/smartpcr/code-intelligence/services/clean-code/internal/version.Commit=${COMMIT} \
            -X github.com/smartpcr/code-intelligence/services/clean-code/internal/version.BuildTime=${BUILD_TIME}" \
        -o /out/clean-coded \
        ./cmd/clean-coded

# -------- runtime stage ----------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

USER nonroot:nonroot
WORKDIR /

COPY --from=builder /out/clean-coded /clean-coded

EXPOSE 8080 9090

ENTRYPOINT ["/clean-coded"]
