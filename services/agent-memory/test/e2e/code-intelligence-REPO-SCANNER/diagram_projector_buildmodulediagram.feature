@story-code-intelligence:REPO-SCANNER @phase-diagram-projector @stage-buildmodulediagram @setup-inline
Feature: BuildModuleDiagram projector

  The module-diagram projector reads a code-intelligence graph from a
  SQLite store and emits a Diagram envelope whose shape depends on the
  requested granularity. At granularity=package, file Nodes are hidden
  and imports edges are rolled up to pkg->pkg with a weight count. At
  granularity=file, file Nodes appear and per-file imports edges are
  preserved.

  The E2E proof path builds the codeintel CLI binary with CGO, exercises
  `codeintel scan` against the polyglot fixture to produce a scan-smoke.db,
  then runs `codeintel diagram module --db scan-smoke.db` to verify the
  full CLI pipeline produces valid diagram JSON. Each scenario also
  provisions a separate polyglot.db with specific graph shapes via the
  graphsink.Sink API and exercises BuildModuleDiagram directly to assert
  exact node/edge counts.

  Scenario: module-tree-shape
    Given the codeintel CLI binary compiles and its scan and diagram subcommands are registered
    And a SQLite polyglot.db with 1 repo, 2 packages, and 5 files
    When BuildModuleDiagram runs at granularity "package"
    Then the diagram has 3 nodes and 2 contains edges

  Scenario: imports-rollup-weight
    Given the codeintel CLI binary compiles and its scan and diagram subcommands are registered
    And a SQLite polyglot.db where 3 files in pkg/a each import 2 files in pkg/b
    When the projector runs at granularity "package"
    Then exactly one imports edge "pkg:a -> pkg:b" with weight 6 is emitted

  Scenario: granularity-file
    Given the codeintel CLI binary compiles and its scan and diagram subcommands are registered
    And a SQLite polyglot.db with imports at granularity "file"
    When the projector runs at granularity "file"
    Then file Nodes appear and per-file imports edges are NOT rolled up
