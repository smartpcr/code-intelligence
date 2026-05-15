# Thin wrapper image that adds a static `busybox` binary to the
# upstream otel/opentelemetry-collector-contrib image so docker
# compose HEALTHCHECK can probe the `:13133` health_check extension
# from inside the container. The upstream image is distroless --
# no shell, no wget, no curl -- so we ship our own static probe.
#
# busybox runs as the non-root user the upstream image uses
# (10001:10001) because COPY defaults to 0755 perms.

FROM busybox:1.36.1-uclibc AS busybox

FROM otel/opentelemetry-collector-contrib:0.112.0
COPY --from=busybox /bin/busybox /busybox
