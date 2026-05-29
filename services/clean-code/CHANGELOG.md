# Changelog: `services/clean-code`

All notable changes to the clean-code service are recorded here.

## v1.0 — Initial release

### Public API surface

- **Schema**: `clean_code`
- **Verdict values**: `pass`, `warn`, `block`
- **Override**: the `mgmt.override` verb accepts `scope`, `rule_id`,
  and `mute` fields. Overrides are permanent until explicitly
  reversed by a subsequent `mgmt.override` call with `mute=false`.
  There is no expiry field on the override record.

### Canonical verbs

| Verb                   | Category   |
|------------------------|------------|
| `mgmt.register_repo`  | management |
| `mgmt.retract_sample` | management |
| `mgmt.rescan`         | management |
| `mgmt.override`       | management |
| `policy.publish`      | policy     |
| `policy.activate`     | policy     |
| `eval.gate`           | evaluation |

### Metric kinds

Two categories of metric_kind are supported:

- **foundation (6)**: `cyclomatic_complexity`, `cognitive_complexity`,
  `duplication_ratio`, `function_length`, `parameter_count`,
  `nesting_depth`
- **system (6)**: `coupling_between_objects`, `afferent_coupling`,
  `efferent_coupling`, `instability_index`, `lack_of_cohesion`,
  `dependency_cycle_count`

```json
{
  "metric_kind_counts": {
    "foundation": 6,
    "system": 6
  }
}
```

### Breaking changes

None — this is the initial release.
