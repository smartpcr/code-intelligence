# Clean-code report

- **Repo path:** REPO_ROOT
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

Verdict: block
## Findings

- decoupling.cycle_member_present [block]
- decoupling.cycle_member_present [block]
- decoupling.cycle_member_present [block]
- decoupling.cycle_member_present [block]
- decoupling.duplication_ratio_high [warn]
- solid.srp.interface_width_high [warn]


## Diagnostics

- **Effort source:** fallback
- **Prompts emitted:** 0
