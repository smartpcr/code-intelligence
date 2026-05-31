@story-code-intelligence:REPO-SCANNER @phase-local-materializer-and-sha-synthesis @stage-localdirmaterializer @setup-inline
Feature: LocalDirMaterializer SHA synthesis and walk excludes

  The LocalDirMaterializer prepares a Workspace from a local filesystem
  directory. SHA resolution follows a three-tier precedence:
    1. operator-supplied SHA (verbatim),
    2. `git rev-parse HEAD` when `.git/` is present,
    3. `fingerprint.MTimeTreeSHA(rootDir, defaultExcludeDirs)` otherwise.
  Walk must skip default-excluded directories such as `.git/` and
  `node_modules/`.

  Scenario: local-non-git-sha
    Given a temporary directory without ".git/"
    When Materialize runs with an empty sha
    Then Workspace.SHA equals MTimeTreeSHA of the directory with defaultExcludeDirs

  Scenario: local-git-sha
    Given a temporary directory that is a git checkout with at least one commit
    When Materialize runs with an empty sha
    Then Workspace.SHA equals the output of "git rev-parse HEAD" in that directory

  Scenario: operator-sha-override
    Given a temporary directory that is a git checkout with at least one commit
    And an operator-supplied sha "feedfacecafebabefeedfacecafebabe"
    When Materialize runs with the operator-supplied sha
    Then Workspace.SHA equals the operator-supplied sha

  Scenario: walk-excludes-applied
    Given a temporary directory containing "node_modules/" and ".git/" with files inside
    When Workspace.Walk runs on the materialized workspace
    Then no WalkFile originates inside "node_modules/" or ".git/"
