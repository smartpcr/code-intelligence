# Clean-code report

- **Repo path:** /fixtures/typescript
- **Head SHA:** working-copy
- **Policy:** policy_id=2b8b672e-2c5c-5891-ad9a-394921e95273 version=dev
- **Active parser fleet:** go, java, python, typescript
- **Dark metrics:** 5

## Dark Metrics

- metric dark: cognitive_complexity (typescript) — missing: decision_blocks
- metric dark: cyclo (typescript) — missing: decision_blocks
- metric dark: fan_in (typescript) — missing: call_edges
- metric dark: fan_out (typescript) — missing: call_edges
- metric dark: lcom4 (typescript) — missing: call_edges, field_accesses

## Verdict

Verdict: block
## Findings

- decoupling.cycle_member_present [block]
- solid.srp.interface_width_high [warn]
- decoupling.cycle_member_present [block]
- decoupling.duplication_ratio_high [warn]
- decoupling.cycle_member_present [block]
- decoupling.cycle_member_present [block]
- solid.isp.interface_width_high [warn]


## Diagnostics

- **Effort source:** fallback
- **Prompts emitted:** 0
