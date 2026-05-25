# `services/clean-code` runbook

Operational guide for the clean-code service. Add a new
section here as each subsystem ships against the production
composition root (`cmd/clean-coded/main.go`).

## Policy Steward signing-key cache (Stage 5.1)

### What

The clean-code service signs every published policy bundle with
an **Ed25519** keypair. The active set of signing keys lives in
the `clean_code.policy_signing_keys` Postgres table; the matching
private keys live in the operator's KMS (envelope-encrypted under
a master AES-256-GCM key the operator injects via env var). The
24h **rotation overlap window** (tech-spec Sec 8.2 row 6) is the
key reliability mechanism -- when a new key is minted, the prior
key remains accepted for `policy_publish_overlap_min_seconds`
(default 86400 = 24h) so consumers cache catch up before signature
verification breaks.

### Configuration (env vars)

| Env var                              | Meaning                                                                                            | Required when             |
| ------------------------------------ | -------------------------------------------------------------------------------------------------- | ------------------------- |
| `CLEAN_CODE_KMS_PROVIDER`            | `"local"` (production, envelope KMS), `"in-memory"` (test-only), `""` (scaffold mode, no signing). | always                    |
| `CLEAN_CODE_KMS_MASTER_KEY_HEX`      | 64 lowercase hex chars = 32-byte AES-256 master key. Required when provider = `local`.             | `KMS_PROVIDER=local`      |
| `CLEAN_CODE_PG_URL`                  | PostgreSQL connection URL. Required when provider = `local`.                                       | `KMS_PROVIDER=local`      |
| `CLEAN_CODE_POLICY_PUBLISH_OVERLAP_SECONDS` | Rotation overlap in seconds. Defaults to 86400 (24h).                                       | never (defaulted)         |

Scaffold mode (`KMS_PROVIDER=""`) leaves the signing-key cache
unwired; `/readyz` reports `signing_key_cache` as **missing**
which keeps the listener at 503 by design.

### Read verb: `policy.keys.list_active`

The active signing-key inventory is exposed at:

```
GET /v1/policy/keys/list_active
```

Response: a **bare JSON array**:

```json
[
  {
    "key_id": "f4c1...-uuid",
    "fingerprint": "<64 lowercase hex>",
    "valid_from": "2025-01-01T00:00:00Z",
    "valid_until": "2026-01-02T00:00:00Z"
  }
]
```

Status codes:

| Code | Meaning |
| ---- | ------- |
| 200  | List emitted (may be `[]` during the brief startup window before the first key is minted). |
| 405  | Method other than `GET` / `HEAD`. |
| 503  | Signing-key cache not wired (scaffold mode) or no active key. The route is ALWAYS mounted -- scaffold mode never returns 404. This lets operators distinguish "verb exists, backing subsystem down" from "this build doesn't ship the verb". The contract is pinned by `TestRootMux_ScaffoldModeListActive503` in `cmd/clean-coded/routes_test.go`. |
| 500  | Unexpected backend error. |

### Rotation

Routine rotation is rate-limited by the overlap window: the
`Manager.Rotate` call refuses while the most recent key is still
inside its `valid_until` cooldown. Use `Manager.ForceRotate` (or
the equivalent `keys.compromise` runbook step) for the Sec 9.3
compromise response path -- that bypass is the one and only way
to mint a successor key before the overlap expires.

### Health check

The composition root registers `signing_key_cache` as a readiness
check. It probes the KMS via `Ping` and asserts at least one key
is loaded. A KMS outage or an empty cache flips `/readyz` to 503
WITHOUT crashing the process -- the rest of the service continues
running but no new policy publishes will succeed.

### Cache refresh

Every replica re-reads the `policy_signing_keys` table every
`5m` (constant `signingKeyCacheRefreshInterval` in
`cmd/clean-coded/main.go`). This is two orders of magnitude
faster than the 24h overlap window, so a sibling-replica
rotation always propagates well before the old key expires.

### Transport

The canonical transport for `policy.keys.list_active` (and every
other read verb this service grows in later stages) is
**HTTP/JSON v1**. The path prefix is `/v1/`; a future shape
change ships under `/v2/` so dashboards keep working through the
cutover.

A gRPC/proto layer is **out-of-scope for Stage 5.1**. No
`*.proto` file or gRPC server exists in this service, the
dashboard / operator-CLI clients are already HTTP-based, and
shipping a second transport speculatively would create
wire-shape drift between two surfaces that have to be kept in
sync. If a future downstream consumer requires streaming or
strong-typed verbs, a `management-grpc-adapter` workstream
would land it -- with explicit regression tests pinning both
transports to the same wire shape. Until then, HTTP/JSON v1 is
the SOLE ratified contract.

### KMS backend

Stage 5.1 ships `LocalSealedKMS` (AES-256-GCM envelope encryption
of the Ed25519 seed under an operator-injected master key) as
the production KMS implementation. The `KMS` interface contract
is stable.

A managed-service KMS adapter (Azure Key Vault / AWS KMS /
HashiCorp Vault) is **out-of-scope for Stage 5.1** and is owned
by a future `policy-steward-kms-adapter` workstream once the
deployment-target vendor is selected by the operator. That
workstream will only need to land a new `KMS` implementation
plus its config wiring; Stage 5.1's manager, store, rotation,
overlap-window, evaluator integration, and read verb continue
to work unchanged because they depend on the interface, not the
implementation.

### Test isolation (live PostgreSQL)

When `CLEAN_CODE_PG_URL` is exported, two test suites in
different packages hit the same database:

| Suite                                                  | Owns schema             |
| ------------------------------------------------------ | ----------------------- |
| `internal/storage/migrate_test.go::TestRoundTrip_...`  | `clean_code` (canonical migration) |
| `internal/policy/keys/sql_store_test.go::TestSQLStore_...` | `clean_code_keys_test` (isolated) |
| `internal/policy/steward/sql_store_test.go::TestSQLStore_...` | `clean_code_steward_test` (isolated) |

The storage round-trip `DROP SCHEMA clean_code CASCADE`s on prep,
which is why both SQLStore live tests own distinct schemas. CI
lanes that set `CLEAN_CODE_PG_URL` can run all three packages in
parallel without interference.

## Policy Steward write verbs (Stage 5.2)

### What

The Policy Steward owns the three canonical `policy.*` write
verbs (tech-spec Sec 8.5 lines 963-970 + architecture Sec 6.5).
All three are append-only -- no row is ever UPDATEd or DELETEd
-- and all three require an active signing key in the
[Stage 5.1 cache](#policy-steward-signing-key-cache-stage-5-1)
before they will write.

**Signing scope is narrow**: only `policy.publish` produces a
signed row. The PolicyVersion table has a `signature bytea NOT
NULL` column carrying an Ed25519 signature over the canonical
JSON of `(rule_refs, threshold_refs, refactor_weights)`.
`policy.activate` and `policy.publish_rulepack` REQUIRE that a
signing key be loaded (so the service is in a state where
signed writes work end-to-end) but do NOT write a signature
column of their own -- the `policy_activation` and `rule_pack`
/ `rule` tables have no signature column.

| Verb                      | Writes signature? | Append-only? |
| ------------------------- | ----------------- | ------------ |
| `policy.publish`          | yes (`policy_version.signature`) | yes |
| `policy.activate`         | no                | yes |
| `policy.publish_rulepack` | no                | yes (pack + rules, transactionally) |

### Write verbs

| Verb                          | URL                                  | Status table |
| ----------------------------- | ------------------------------------ | ------------ |
| `policy.publish`              | `POST /v1/policy/publish`            | 200 / 400 / 405 / 500 / 503 |
| `policy.activate`             | `POST /v1/policy/activate`           | 200 / 400 / 405 / 500 / 503 |
| `policy.publish_rulepack`     | `POST /v1/policy/publish_rulepack`   | 200 / 400 / 405 / 409 / 500 / 503 |

Banned historical-draft verbs return **501 Not Implemented**:

- `POST /v1/policy/rulepack/add`     -> `{error:"unimplemented_verb", verb:"policy.rulepack.add"}`
- `POST /v1/policy/rulepack/remove`  -> `{error:"unimplemented_verb", verb:"policy.rulepack.remove"}`
- `POST /v1/policy/override`         -> `{error:"unimplemented_verb", verb:"policy.override"}`

### `policy.publish` body

```json
{
  "name": "default-v3",
  "rule_refs": [{"rule_id": "solid.srp.lcom4_high", "version": 1}],
  "threshold_refs": [],
  "refactor_weights": {
    "alpha": 0.4, "beta": 0.3, "gamma": 0.2, "delta": 0.1,
    "effort_model_version": "v1.0",
    "window_days": 90
  }
}
```

Response: the full `PolicyVersion` row including
`policy_version_id`, `signature`, and `created_at`.

**rule_refs / threshold_refs FK contract**: each `rule_refs`
entry MUST reference an existing `(rule_id, version)` pair
registered via a prior `policy.publish_rulepack` call. Each
`threshold_refs` entry MUST reference an existing
`threshold_id` row in `clean_code.threshold`. The migration
keeps these references inside a JSONB document (not as proper
SQL FKs) so the Policy Steward enforces them at write time --
an unknown ref returns **400 Bad Request** with the offending
`rule_id`/`version` (or `threshold_id`) in the body. The
steward refuses BEFORE spending signing material, so a
rejected request leaves no signature on the audit trail.
Duplicate refs within the same payload also return 400.

### `policy.activate` body

```json
{
  "policy_version_id": "f4c1...-uuid",
  "activated_by": "alice@example"
}
```

The body MUST NOT contain a `scope` field -- v1 activation is
global per deployment (architecture Sec 5.3.4 single-tenant
pin). The HTTP handler decodes with `DisallowUnknownFields`, so
a body carrying `scope` is rejected with 400 and a body that
mentions `scope` in its error message; clients can self-correct
without dashboard support.

### `policy.publish_rulepack` body

```json
{
  "pack_id": "solid.srp",
  "version": 1,
  "display_name": "Single Responsibility",
  "description_md": "SOLID SRP rulepack.",
  "rules": [
    {
      "rule_id": "solid.srp.lcom4_high",
      "version": 1,
      "predicate_dsl": "metric_kind == 'lcom4' AND value > 0.7",
      "severity_default": "block",
      "description_md": "High LCOM4 -- methods share little state."
    }
  ]
}
```

Response: `{rule_pack, rules}`. The pack + every rule row is
appended in a **single transaction** -- a mid-batch failure
rolls back both the pack and any earlier rules so an append-
only store never carries a partial pack. A re-publish of the
same `(pack_id, version)` returns 409 -- this is the append-
only contract. None of the rows carry a signature column;
`policy.publish_rulepack` is unsigned (only `policy.publish`
signs).

### Signing-key precondition

All three verbs refuse when no signing key is active
(`ErrNoActiveSigningKey` -> 503). The signing key must be
wired via `CLEAN_CODE_KMS_PROVIDER=local` (or the in-memory
provider for development). See "Policy Steward signing-key
cache (Stage 5.1)" above.

### Evaluator pickup

A future `eval.gate` call resolves the active policy via the
canonical lookup: read the latest `policy_activation` row,
dereference its `policy_version_id`, verify the row's
`signature`. The same path is exposed in code via
`Steward.ActivePolicyVersion(ctx)` for integration tests.
After `policy.activate(pvB)` runs, this query returns `pvB`
even if `pvA` was activated first -- latest-row-wins by
`created_at, activation_id DESC` (architecture Sec 5.3.4).

### Storage backend

When `CLEAN_CODE_PG_URL` is set, the steward writes rows to
the canonical `clean_code.{policy_version, policy_activation,
rule_pack, rule}` tables (migration 0003). Otherwise the
composition root falls back to the in-memory store -- rows
are lost on process restart. The development warning log line
`policy steward backed by in-memory store` signals which mode
is active.


## `mgmt.override` write verb (Stage 5.3)

### What

`mgmt.override` is the operator **mute / unmute kill switch**
for individual rules at a chosen scope (architecture Sec 1.5.1
row 5 + Sec 6.3 line 1357). The verb appends one `override` row
per call; unmute is a fresh INSERT with `mute=false`. The
evaluator (Stage 5.7) consults `MAX(created_at) WHERE rule_id
= $1 AND scope_filter matches the candidate scope` so the
**latest matching row wins**.

### How `scope_filter matches the candidate scope`

The evaluator carries a `CandidateScope{repo_id, scope_kind,
signature}` and asks the steward for the latest override that
matches it. "Match" means:

1. `scope_filter.repo_id` == `candidate.repo_id` (exact)
2. `scope_filter.scope_kind` == `candidate.scope_kind` (exact)
3. `scope_filter.scope_signature_glob` matches
   `candidate.signature` under simple glob vocab:
   - `*` matches any sequence of characters (including empty,
     and across `.` / `/`).
   - `?` matches exactly one character.
   - Every other rune is literal (no `[...]` classes, no
     backslash escapes).
   The pattern is anchored end-to-end -- `com.example.legacy.*`
   matches `com.example.legacy.Foo` and `com.example.legacy.a.b`
   but NOT `com.other.legacy.X`.

The read is implemented in `Store.LatestMatchingOverride`. The
SQL path pre-filters with a JSONB extractor (`scope_filter->>
'repo_id'` and `scope_filter->>'scope_kind'`) so only the rows
in the candidate's `(repo_id, scope_kind)` partition are
streamed; the glob match is applied in Go in descending
`(created_at, override_id)` order, stopping at the first hit.
Crucially the query does **not** carry a `LIMIT`: a newer row
under a non-matching glob must not hide an older row under a
matching glob.

### Wire shape

```
POST /v1/mgmt/override
Content-Type: application/json
X-OIDC-Subject: <caller OIDC sub>

{
  "rule_id": "solid.srp.lcom4_high",
  "scope_filter": {
    "repo_id": "repo-a",
    "scope_kind": "class",
    "scope_signature_glob": "com.example.legacy.*"
  },
  "mute":   true,
  "reason": "legacy code; planned refactor in Q3"
}
```

Response (HTTP 200) carries **only** the new override id --
matching the architecture `mgmt.override(...) -> OverrideId`
return type:

```json
{ "override_id": "f4c1...-uuid" }
```

### Required invariants

- The body **MUST NOT** contain `expires_at` (v1 mute lifecycle
  has no TTL -- tech-spec Sec 10A pin). `DisallowUnknownFields`
  on the decoder returns **400** if you try.
- The body **MUST NOT** contain `actor_id`. The OIDC subject is
  sourced exclusively from the `X-OIDC-Subject` header. Bodies
  carrying `actor_id` are rejected with **400**; missing or
  empty `X-OIDC-Subject` returns **401**.
- `scope_filter.scope_kind` must be one of the canonical seven
  values: `repo, package, file, class, interface, method,
  block`. Anything else -> 400.
- `scope_filter.repo_id` and `scope_filter.scope_signature_glob`
  MUST be non-empty (use `"*"` for the repo-wide wildcard --
  the empty string is rejected). 400 on miss.
- `reason` MUST be non-empty (after `TrimSpace`) when
  `mute=true`. Empty / whitespace-only reasons return 400 at
  the handler; the SQL CHECK constraint
  `override_reason_required_when_muted` provides defence in
  depth at the database. `reason` MAY be empty on unmute.
- `rule_id` MUST reference a rule that has been registered via
  `policy.publish_rulepack`. The verb performs a logical FK
  check; an unknown rule returns 400.

### Status codes

| Code | Meaning |
| ---- | ------- |
| 200  | Override row appended; body is `{override_id}`. Note: in scaffold mode (`CLEAN_CODE_KMS_PROVIDER` unset) the Policy Steward is still wired with a null-object signer, so `mgmt.override` continues to serve 200 -- see the scaffold-mode matrix in "No signing-key precondition (kill-switch contract)" below. |
| 400  | Malformed JSON, unknown field (including `expires_at` / `actor_id`), invalid `scope_kind`, empty `reason` on mute, or unknown `rule_id`. |
| 401  | Missing or empty `X-OIDC-Subject` header. |
| 405  | Any method other than POST. |
| 500  | Internal error; opaque body. |
| 503  | The Policy Steward is genuinely unreachable (the composition root failed to construct a writer, or the route was mounted against a nil writer). Under the Stage 5.3 + iter 3 composition root in `cmd/clean-coded/main.go`, `mgmt.override` does NOT 503 simply because the signing-key cache is unwired -- that case is wired explicitly to keep the kill switch operable. |

### Trust boundary

`X-OIDC-Subject` is set by the **auth gateway** after token
validation. In any deployment where `clean-coded` is directly
reachable from untrusted clients, the gateway MUST strip the
header at the edge and re-inject the validated `sub` claim --
otherwise a caller can spoof the actor. clean-coded does not
re-validate the bearer token here because the gateway already
did.

### No signing-key precondition (kill-switch contract)

Unlike `policy.publish`, `policy.activate`, and
`policy.publish_rulepack`, `mgmt.override` **does not** require
an active signing key. The override row carries no signature
column, and the operator must be able to silence a noisy or
broken rule even during a signing-key outage -- that is the
worst time to deny an emergency mute. If the steward verbs
above are returning 503 because the signing-key cache is
unwired, `mgmt.override` still serves 200.

The composition root encodes this contract by building the
Policy Steward + write verbs UNCONDITIONALLY (Stage 5.3 +
iter 3) -- not gated on `cfg.KMSProvider != ""`. When KMS is
unset the steward is constructed with a **null-object signer**
(`noActiveSigner`) so `Steward.Override` proceeds while the
Stage 5.2 signing verbs surface `ErrNoActiveSigningKey` via
the existing `len(ListActive()) == 0` branch. Pinned by
`TestRootMux_ScaffoldModeOverrideMounted_200` and
`TestBuildPolicyWriter_ScaffoldModeProducesWriter` (both in
`cmd/clean-coded/routes_test.go`) plus
`TestSteward_PublishRefusesWhenSignerNil` (in
`internal/policy/steward/steward_test.go`).

#### Scaffold-mode status-code matrix

When the composition root runs with `CLEAN_CODE_KMS_PROVIDER`
empty (scaffold mode, signing-key cache unwired):

| Verb                              | Path                              | Scaffold mode | Reason |
| --------------------------------- | --------------------------------- | ------------- | ------ |
| `mgmt.override` (mute / unmute)   | `POST /v1/mgmt/override`          | **200** for a valid request against a registered rule | Kill-switch contract: the verb must remain operable during a signing-key outage. |
| `policy.keys.list_active` (read)  | `GET /v1/policy/keys/list_active` | **503**       | The verb is mounted (not 404), but the signing-key cache is unwired. |
| `policy.publish`                  | `POST /v1/policy/publish`         | **503**       | Refuses with `ErrNoActiveSigningKey` -- the null-object signer reports an empty active-key set. |
| `policy.activate`                 | `POST /v1/policy/activate`        | **503**       | Same precondition as `policy.publish`. |
| `policy.publish_rulepack`         | `POST /v1/policy/publish_rulepack`| **503**       | Same precondition. |
| `policy.rulepack.add` / `.remove` | (legacy paths)                    | **501**       | Banned-verb canonical response (not part of the v1 surface). |
| `policy.override` (legacy path)   | `POST /v1/policy/override`        | **501**       | Renamed; see "Historical: `policy.override`" below. |

### Append-only / unmute flow

To unmute, POST again with the **same** `scope_filter` and
`mute=false`:

```
POST /v1/mgmt/override
X-OIDC-Subject: bob@example.com
{
  "rule_id": "solid.srp.lcom4_high",
  "scope_filter": { "repo_id": "repo-a", "scope_kind": "class",
                    "scope_signature_glob": "com.example.legacy.*" },
  "mute":   false,
  "reason": ""
}
```

The evaluator's latest-row read now sees `mute=false` for that
`(rule_id, scope_filter)` pair. The previous `mute=true` row
remains in the table for audit -- no UPDATE / DELETE primitives
exist on the Store interface.

### No automatic expiry

A row planted years ago remains the active mute until an
operator unmutes it. There is no scheduled "expire old
overrides" job in v1. Aged-mute hygiene is surfaced via the
`mgmt.insights.aged_mutes` read verb (a future stage); v1
operators audit by querying the `override` table directly:

```sql
SELECT rule_id, scope_filter, mute, created_at
  FROM clean_code.override
  WHERE created_at < now() - interval '180 days'
  ORDER BY created_at DESC;
```

### Historical: `policy.override`

The draft surface listed a `policy.override` verb. The canonical
name is `mgmt.override`; the legacy path is still mounted at
`/v1/policy/override` and returns **501 Not Implemented** with
a body identifying the rename so operators scripting against an
older draft contract learn of the change without getting a
404.

## Predicate DSL evaluator (Stage 5.4)

### What

`internal/policy/dsl/` is the parser + evaluator for the
**predicate DSL** that lives on each `Rule.predicate_dsl` text
column (architecture Sec 5.3.1 line 1101). Each rule's
predicate is a boolean function over a `MetricSample` row; the
Rule Engine (Stage 5.7) compiles the predicate once per
`(policy_version_id, source)` pair, then re-evaluates it
against every active sample for every SHA on the hot path.

The package ships **no HTTP verb and no background goroutine**.
It is a pure-Go library consumed in-process by the Rule Engine,
so there is no readiness check, no env var, and no migration to
roll out -- a binary that includes the DSL package is the
deployment unit.

### Grammar

```text
predicate      ::= or_expr
or_expr        ::= and_expr ( "OR" and_expr )*
and_expr       ::= not_expr ( "AND" not_expr )*
not_expr       ::= "NOT" not_expr | atom
atom           ::= "(" predicate ")"
                |  threshold_call
                |  comparison
                |  bool_literal
threshold_call ::= "threshold" "(" string_literal ")"
comparison     ::= operand cmp_op operand
cmp_op         ::= "==" | "!=" | ">" | ">=" | "<" | "<="
operand        ::= field | string_literal | number_literal | bool_literal
field          ::= "metric_kind" | "scope_kind" | "value"
                |  "pack" | "source" | "degraded"
```

Precedence: `NOT` binds tightest, then `AND`, then `OR`.
Parentheses override. Keywords `AND`/`OR`/`NOT` are
case-insensitive (SQL convention); field names and string
literals are case-sensitive (they match the database ENUM
labels verbatim).

Lexer details:

- String literals are single-quoted (`'lcom4'`). Supported
  escapes inside the string body: `\\`, `\'`, `\n`, `\t`.
- Number literals: `'-'? [0-9]+ ( '.' [0-9]+ )?`. Scientific
  notation is **not** supported in v1. Leading `-` is
  consumed only when immediately followed by a digit, so
  inline negative comparisons (`value < -0.5`) parse the
  same way they would against a `velocity_trend` Threshold
  row that stored a negative reading.
- Comments: `#`-prefixed to end of line. Useful when a
  `predicate_dsl` string is embedded in a YAML rulepack and
  the author wants to annotate the rule.

### Canonical closed sets

The parser rejects any string literal on either side of a
comparison whose field is one of `metric_kind`, `scope_kind`,
`pack`, `source` and whose value is not in the canonical set.
This is the **canon-guard** (the
`dsl-rejects-unknown-metric-kind` Stage 5.4 test scenario).
The canonical sets are pinned in
`services/clean-code/internal/policy/dsl/sample.go`:

| Field         | Canonical values                                                                                                                                                                                                                                                                                          |
| ------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `metric_kind` | base: `cyclo`, `cognitive_complexity`, `loc`, `cycle_member`, `duplication_ratio`, `modification_count_in_window`; solid: `lcom4`, `fan_in`, `fan_out`, `depth_of_inheritance`, `interface_width`, `coupling_between_objects`; system: `xrepo_dep_depth`, `arch_debt_ratio`, `velocity_trend`, `arch_fitness`, `blast_radius`, `xservice_test_reliability`, `knowledge_index`; ingested: `coverage_line_ratio`, `coverage_branch_ratio`, `pass_first_try_ratio` |
| `scope_kind`  | `repo`, `package`, `file`, `class`, `interface`, `method`, `block`                                                                                                                                                                                                                                       |
| `pack`        | `base`, `solid`, `system`, `ingested`                                                                                                                                                                                                                                                                    |
| `source`      | `computed`, `derived`, `ingested`                                                                                                                                                                                                                                                                        |

**Legacy aliases are NEVER written and are rejected:**
`coverage_line` and `coverage_branch` (use the `_ratio`
suffixed canonical names); `lines_of_code` (use `loc`).
Implementation-plan line 31 pins the negative clause.

### `threshold('<uuid>')` triple-check

A `threshold('<uuid>')` atom references a `Threshold` row from
the Policy / rules sub-store (architecture Sec 5.3.5) by its
`threshold_id` UUID. The UUID **MUST** be in the active
`PolicyVersion.ThresholdRefs` set -- the
"application-layer FK" contract from migration 0003 line 462
("the FK target is enforced by the writer, not by SQL"). The
DSL `Bind` step enforces this at compile time so the hot path
never errors on unresolved refs.

A bound `threshold(t)` atom evaluates `true` iff ALL of:

1. `sample.metric_kind == t.MetricKind`
2. `sample.scope_kind == t.ScopeKind`
3. `sample.value <op> t.Value` per `t.Op`

A `metric_kind` or `scope_kind` mismatch returns `false`
(not an error) -- a rule that fires on `lcom4` simply does
not match a sample for `fan_in`. A missing `sample.value`
(e.g. a degraded sample with `HasValue=false`) returns
`false` rather than erroring; the Rule Engine separately
records the `degraded` flag.

`Bind` also rejects a resolver that returns a `Threshold`
whose `ThresholdID` does not match the requested UUID --
Threshold rows are immutable (architecture G3) and uniquely
keyed by their own `threshold_id`, so any mismatch indicates
an upstream bug.

### Cache shape

`Cache` memoises compiled `*Predicate` instances per
`(policy_version_id, source string)` pair. The Stage 5.4
brief: "Cache parsed predicates per `policy_version_id` so
re-evaluation is hot-path cheap."

- **Hot path:** `RWMutex.RLock` + two map lookups + a
  closed-channel receive. Single-digit nanoseconds.
- **Miss path:** install a per-entry placeholder under the
  cache mutex, then RELEASE the mutex before calling
  `Compile`. The actual parse + `ThresholdResolver.Lookup`
  runs WITHOUT the cache mutex held.
  - Concurrent compiles on DIFFERENT keys never block each
    other (a slow resolver on one `(policy, source)` does
    NOT stall hits or compiles for another).
  - Concurrent compiles on the SAME `(policy, source)`
    de-duplicate via the placeholder's `ready` channel
    (the singleflight property): `Compile` runs at most
    once per key and every caller returns the same
    `*Predicate` (or the same error).
- **`Invalidate`:** drops every entry for a policy version.
  This is a **memory-reclamation hint**, not a hard
  correctness boundary: a `GetOrCompile` that races with
  `Invalidate` may re-install an entry for the invalidated
  policy after `Invalidate` returns. This is acceptable
  because `PolicyVersion` rows are immutable (G5), so a
  re-compiled "retired" policy version is semantically
  equivalent.
- **Errors are cached.** A predicate that fails the
  canon-guard returns the same `ErrSemantic` on every
  subsequent `GetOrCompile` -- we do not retry a
  deterministic parse failure on each Rule Engine cycle.
- **Panics in `Compile` do not cascade.** If `Compile`
  panics, the entry stores a synthetic
  `internal: compile panicked: <value>` error and closes
  `ready`; concurrent waiters and future lookups observe
  an error rather than re-panicking. The panic still
  propagates out of the original caller's frame so the bug
  surfaces loudly in its stack trace.

### Error shapes

Every parse / bind failure returns a structured `*dsl.Error`
with `Kind`, `Pos`, `Msg`, and (optionally) `Cause`. The
canonical wire format is:

```text
dsl: <kind>: <line>:<column>: <message>
```

The `Kind` is one of:

| Kind            | When                                                                                                                                                                                                                              |
| --------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `dsl.ErrLex`    | Tokenizer failure: bare `=`, unterminated string literal, illegal character, unsupported escape, dangling `.` in a number.                                                                                                        |
| `dsl.ErrParse`  | Syntactic failure: unexpected token, missing closing paren, trailing junk after the predicate, `threshold()` with no argument or a non-string argument, `AND` / `OR` with no RHS.                                                  |
| `dsl.ErrSemantic` | Closed-set canon-guard failure: a `metric_kind` / `scope_kind` / `pack` / `source` literal that is not in the canonical set; an unknown field name in an operand position.                                                       |
| `dsl.ErrType`   | Type-unification failure: a comparison whose operands have different static types (`value == 'foo'`), or an ordering operator on a non-numeric operand (`metric_kind > 'cyclo'`), or `degraded == 1`.                              |
| `dsl.ErrBind`   | `Bind`-time failure: `threshold('<uuid>')` argument is not a valid UUID, or its UUID is not in the resolver's set (wraps `dsl.ErrUnknownThreshold`); the resolver returned a Threshold whose `ThresholdID` does not match the requested UUID; a resolver-returned Threshold fails the closed-set re-validation. |

Callers branch with `errors.Is(err, dsl.ErrParse)` etc. When a
`Bind` failure wraps an underlying cause (e.g.
`ErrUnknownThreshold`), `errors.Is` matches both targets via
the Go 1.20+ multi-target Unwrap protocol.

The `Pos` field is **never zero** on a parse / bind failure --
the Stage 5.4 implementation-plan line 500 acceptance
criterion ("rejection of malformed ones with line/column
error messages") is enforced by `TestParser_RejectsMalformed`
and `TestParser_PositionPointsAtOffendingToken`.

### Operator triage

Common rule-engine error surfaces and how to recover:

| Error message snippet                                | Likely cause                                                                                                                                                                                                          | Fix                                                                                                                                                                |
| ---------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `unknown metric_kind "lines_of_code"`                | The rule pack used a non-canonical alias. The canonical name is `loc`.                                                                                                                                                | Re-publish the rulepack with `metric_kind == 'loc'`. The list of canonical names is printed in the error message.                                                  |
| `unknown metric_kind "coverage_line"` (or `_branch`) | Legacy coverage-metric name. The v1 canon uses the `_ratio` suffix.                                                                                                                                                   | Re-publish with `coverage_line_ratio` / `coverage_branch_ratio`. The bare names are NEVER written by the Metric Ingestor (implementation-plan line 31).            |
| `unknown scope_kind "function"`                      | The rule pack used a non-canonical scope kind. The canonical set is `repo, package, file, class, interface, method, block`.                                                                                           | Re-publish with the correct scope kind (probably `method`).                                                                                                        |
| `type mismatch in comparison: number == string`      | A `value == '...'` comparison; the `value` field is numeric.                                                                                                                                                          | Quote-strip the literal (`value == 0`) or compare against a `string` field.                                                                                        |
| `ordering operator > requires numeric operands`      | Using `<`, `<=`, `>`, `>=` on a string field (`metric_kind > 'cyclo'`).                                                                                                                                               | Use `==` / `!=` for closed-set string fields.                                                                                                                      |
| `threshold('<uuid>') ...not a valid UUID`            | The string argument is not a UUID (typo or stale templating).                                                                                                                                                         | Re-publish the rulepack with a correctly-formatted UUID. Threshold rows are queried out of the Policy / rules sub-store.                                           |
| `threshold('<uuid>') threshold_id not registered`    | The rule references a threshold UUID that is not in the active `PolicyVersion.ThresholdRefs`. The application-layer FK enforced by the Steward at publish time -- but a runtime drift indicates a stale policy view.   | Confirm the Steward holds the threshold row (the publish that registered it succeeded) and the resolver wired to `Compile` includes it. Re-activate the policy if needed. |
| `threshold('<uuid>') resolver returned mismatched threshold_id` | A hand-rolled `ThresholdResolver` returned a row keyed by a different `threshold_id`. Threshold rows are immutable and uniquely keyed (architecture G3) -- silently binding the wrong row would gate the wrong predicate. | Triage the resolver implementation; the canonical `dsl.MapResolver` cannot trigger this because it keys by `threshold_id` and the row carries the same value.       |
| `unterminated string literal`                        | A `'` was opened but never closed before EOF or a newline.                                                                                                                                                            | Close the string. Newlines inside literals are rejected -- use `\n` if the literal genuinely contains a newline.                                                   |
| `expected '==' but got '=...'`                       | A bare `=` was used (typical paste from SQL `WHERE` clause).                                                                                                                                                          | Use `==` for equality. Boolean negation is `NOT`, not `!`.                                                                                                         |

A predicate that fails to compile leaves its rule **inactive**
in the Rule Engine -- the rule does not block any decision
because it cannot be evaluated. Operators should treat a
parse / bind error as a **deployment-blocking** condition
because the safety-rule failed open. The Rule Engine surfaces
the failure via its own emit channel (Stage 5.7); the DSL
package itself does not log.

### When to invalidate

Stage 5.4 callers do **not** need to invalidate the cache on
every rule edit. Each `PolicyVersion` row is immutable
(architecture G5); a new edit produces a new
`policy_version_id` and lands in a fresh inner map. The
existing entries for the prior policy version stay valid
until the operator explicitly retires that policy version,
at which point `Cache.Invalidate(policy_version_id)` reclaims
the inner map's memory. `Invalidate` does not stall in-flight
compiles; it returns immediately and the freshly-installed
inner map for any subsequent compile shadows the dropped one.

### Verification: running the suite

```bash
cd services/clean-code
go test ./internal/policy/dsl/... -race -count=1
```

The `-race` flag is required: `TestCache_HotPathIsConcurrent`,
`TestCache_ConcurrentDistinctSources`,
`TestCache_SlowMissDoesNotStallUnrelatedHits`, and
`TestCache_SingleFlightSameKey` exercise the cache's
concurrency contract and require the race detector to catch
a regression that re-introduces a data race.
