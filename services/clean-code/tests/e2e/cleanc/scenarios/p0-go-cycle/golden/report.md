# Clean-code report

- **Repo path:** /NORMALIZED_ROOT/internal/cli/testdata/fixtures/go
- **Head SHA:** working-copy
- **Policy:** policy_id=XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX version=dev
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
