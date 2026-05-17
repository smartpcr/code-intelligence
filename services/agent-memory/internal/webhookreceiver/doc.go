// Package webhookreceiver implements Stage 3.5 of the agent-memory
// pipeline per `implementation-plan.md` §3.5 and `architecture.md`
// §3.1 / §4.6: an HTTP handler that authenticates inbound push /
// merge webhooks from any configured git host, writes one
// `repo_event` audit row, and enqueues one `ingest_jobs(mode=delta)`
// row that the Stage 3.4 delta worker picks up.
//
// Trust boundary
// --------------
// The Webhook Receiver is the FIRST trust boundary on the static
// ingestion path. Every other component downstream (the Repo
// Indexer, the Embedding publisher, the retirement service) trusts
// that the (repo_id, from_sha, to_sha) tuple in `repo_event` /
// `ingest_jobs` was authenticated by THIS handler. Tech-spec §9.12
// pins HMAC-SHA256 as the authentication primitive; an attacker
// without the per-repo secret cannot enqueue a fake delta job.
//
// Secret lookup
// -------------
// The per-repo secret lives in `repo_webhook_secret` (migration
// 0018). The Webhook Receiver and `mgmt.register` (Stage 7.1)
// are the only two writers; the read-only `agent_memory_ro` role
// is explicitly REVOKEd in 0018 so recall / mgmt.read.* paths
// cannot inadvertently leak it. The handler does NOT cache the
// secret in memory across requests: a per-request SELECT is
// trivially cheap (PK lookup) and avoids a stale-cache window
// after secret rotation.
//
// Request shape
// -------------
//
//	POST  /webhook/{repo_id}
//	Headers:
//	    X-Hub-Signature-256: sha256=<lowercase-hex>
//	    Content-Type:        application/json
//	Body (JSON):
//	    { "kind":     "push" | "merge",
//	      "from_sha": "<git sha>",
//	      "to_sha":   "<git sha>" }
//
// `repo_id` is a UUID in the URL so the handler can look up the
// per-repo HMAC secret BEFORE trusting any field in the body.
//
// `kind` is restricted to `push|merge` -- the two values from the
// `repo_event_kind` ENUM that originate from a git host. The
// other two values (`register`, `manual`) come from `mgmt.*`
// verbs and MUST NOT be accepted here.
//
// Response codes
// --------------
//
//	202 Accepted -- HMAC verified, RepoEvent row written,
//	                ingest_jobs row enqueued. Body carries
//	                `{event_id, job_id}` as JSON.
//	400 Bad Request -- HMAC verified BUT body is malformed or
//	                   carries an unsupported `kind`.
//	401 Unauthorized -- repo_id unknown OR missing signature
//	                    header OR HMAC mismatch. Same response
//	                    body for all three cases so the
//	                    discriminator is not leaked.
//	405 Method Not Allowed -- non-POST request to the route.
//	413 Payload Too Large -- body exceeds the configured cap
//	                         (default 1 MiB), enforced by
//	                         http.MaxBytesReader before any
//	                         verification work runs.
//	500 Internal Server Error -- database failure AFTER
//	                             signature verification.
//
// Idempotency
// -----------
// The Stage 1.2 ingest_jobs_dedupe_uidx UNIQUE on
// (repo_id, mode, COALESCE(from_sha, empty_string), to_sha)
// makes duplicate webhook deliveries deduplicate at the queue
// layer. The handler still appends a fresh repo_event row per
// call (the table is an audit log, not a deduped queue) so
// operators can count receipts.
package webhookreceiver
