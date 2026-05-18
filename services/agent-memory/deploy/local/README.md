# services/agent-memory/deploy/local

Local-development dependency stack for the agent-memory service.
Identical to the stack CI brings up in
`.github/workflows/agent-memory-ci.yml` so a green CI run is
reproducible byte-for-byte on a developer laptop.

## Services

| Container | Image                                           | Host port | Healthcheck                                     |
| --------- | ----------------------------------------------- | --------- | ----------------------------------------------- |
| `postgres`| built locally from `./postgres/Dockerfile`      | `5432`    | `pg_isready -U agent_memory -d agent_memory`    |
| `qdrant`  | `qdrant/qdrant:v1.12.4`                         | `6333`    | HTTP GET `/healthz`                             |
| `otel`    | `otel/opentelemetry-collector-contrib:0.112.0`  | `4317`,`4318`,`13133` | HTTP GET `:13133`                  |
| `partition-maintainer` | built locally from `../../cmd/partition-maintainer/Dockerfile` | `8087` | HTTP GET `/healthz` (Stage 8.2) |
| `prometheus` | `prom/prometheus:v2.55.1`                    | `9090`    | scrapes `partition-maintainer:8087/metrics`     |

The Postgres image extends `postgres:16` to bake in
`postgresql-16-partman` so the `pg_partman` extension is available
without a runtime install. `pgcrypto` ships with the base image.
The first-boot init script in `postgres/init/` creates both
extensions on the seed database.

## Quick start

```
docker compose up -d --build
docker compose ps      # wait until all three rows say "running (healthy)"
docker compose down -v # tear down + drop the postgres volume
```

## Healthcheck wait helper

CI polls `docker inspect -f '{{.State.Health.Status}}' <container>`
in a `while` loop (deadline 60 s) for each of
`agent_memory_postgres`, `agent_memory_qdrant`,
`agent_memory_otel`, and `agent_memory_partition_maintainer`,
then re-probes each public endpoint from the runner host
(`pg_isready`, `curl /healthz`, `curl :13133`, `curl :8087/healthz`).
The Prometheus container does not advertise a healthcheck of its
own (operators rely on Prometheus's own `/-/ready` if needed) and
is brought up `depends_on: partition-maintainer (healthy)` so the
scrape target is live the moment Prometheus starts. The exact
loop is in
[`../../../.github/workflows/agent-memory-ci.yml`](../../../../.github/workflows/agent-memory-ci.yml);
copy/paste it for local scripts.

## Connection details

| Service  | URL                                                       |
| -------- | --------------------------------------------------------- |
| Postgres | `postgres://agent_memory:agent_memory@localhost:5432/agent_memory` |
| Qdrant   | `http://localhost:6333`                                   |
| OTel OTLP gRPC | `localhost:4317`                                    |
| OTel OTLP HTTP | `http://localhost:4318`                             |
| OTel health    | `http://localhost:13133`                            |
| partition-maintainer | `http://localhost:8087/healthz`, `http://localhost:8087/metrics` |
| Prometheus     | `http://localhost:9090` (UI + `/api/v1/alerts`)     |

The credentials above are for local dev / CI only. Production
secrets live in the deploy/k8s overlay, not here.
