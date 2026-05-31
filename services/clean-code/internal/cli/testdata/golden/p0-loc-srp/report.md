# Clean-code report

- **Repo path:** /fixtures/loc-srp
- **Head SHA:** working-copy
- **Policy:** policy_id=2b8b672e-2c5c-5891-ad9a-394921e95273 version=dev
- **Active parser fleet:** go, java, python, typescript
- **Dark metrics:** 5

## Dark Metrics

- metric dark: cognitive_complexity (go) — missing: decision_blocks
- metric dark: cyclo (go) — missing: decision_blocks
- metric dark: fan_in (go) — missing: call_edges
- metric dark: fan_out (go) — missing: call_edges
- metric dark: lcom4 (go) — missing: call_edges, field_accesses

## Verdict

Verdict: warn
## Findings

- solid.srp.interface_width_high [warn]
- solid.srp.loc_high [warn]


## Diagnostics

- **Effort source:** fallback
- **Prompts emitted:** 0
