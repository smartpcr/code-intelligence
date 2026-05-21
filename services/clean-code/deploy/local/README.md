# services/clean-code/deploy/local

Local-development dependency stack for the clean-code service.
Identical to the stack CI brings up in
`.github/workflows/clean-code-ci.yml` so a green CI run is
reproducible byte-for-byte on a developer laptop.

## Services

| Container               | Image                                              | Host port(s)        | Healthcheck                                |
| ----------------------- | -------------------------------------------------- | ------------------- | ------------------------------------------ |
| `postgres`              | built from `./postgres/Dockerfile`                 | `5432`              | `pg_isready -U clean_code -d clean_code`   |
| `otel`                  | `otel/opentelemetry-collector-contrib:0.112.0`     | `4317`,`4318`,`13133` | `:13133` health_check extension          |
| `prometheus`            | `prom/prometheus:v2.55.1`                          | `9091` (host)       | (Prometheus's own `/-/healthy` endpoint)   |
| `clean-coded`           | built from `../../Dockerfile`                      | `8080`, `9090`      | `/healthz` on `:8080`                      |

The Postgres image extends `postgres:16` to bake in `pgcrypto`. The
first-boot init script under `postgres/init/` creates the extension
on the seed database; Stage 1.2's migrations populate the
`clean_code` schema. `pg_partman` is added in Stage 1.3 when the
`MetricSample` partitioning lands.

## Quick start

```
make compose-up    # docker compose up -d --build
docker compose ps  # wait until rows say "running (healthy)"
make compose-down  # tear down + drop the postgres volume
```

## Connection details

| Service             | URL                                                            |
| ------------------- | -------------------------------------------------------------- |
| Postgres            | `postgres://clean_code:clean_code@localhost:5432/clean_code`   |
| clean-coded HTTP    | `http://localhost:8080/healthz` / `/readyz`                    |
| clean-coded metrics | `http://localhost:9090/metrics` (future stages export here)    |
| OTel OTLP gRPC      | `localhost:4317`                                               |
| OTel OTLP HTTP      | `http://localhost:4318`                                        |
| OTel health         | `http://localhost:13133`                                       |
| Prometheus          | `http://localhost:9091`                                        |

The credentials above are for local dev / CI only. Production
secrets live in the deploy/k8s overlay, not here.
