# Thin wrapper image that adds a static `busybox` binary to the
# upstream qdrant/qdrant image so docker compose HEALTHCHECK can
# probe `:6333/healthz` from inside the container. The base image
# is debian:12-slim with no wget/curl, so we ship our own static
# probe rather than depending on a shell utility being present.
#
# The wrapper is intentionally trivial: it adds exactly one file
# and changes nothing else about the upstream image, so security
# audits and version bumps continue to track upstream cleanly.

FROM busybox:1.36.1-uclibc AS busybox

FROM qdrant/qdrant:v1.12.4
# /busybox is a single static binary; we invoke it via its
# applet syntax in the compose HEALTHCHECK ("/busybox wget ...").
COPY --from=busybox /bin/busybox /busybox
