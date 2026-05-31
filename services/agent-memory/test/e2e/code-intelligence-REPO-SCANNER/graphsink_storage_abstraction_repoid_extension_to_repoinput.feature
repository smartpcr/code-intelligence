@story-code-intelligence:REPO-SCANNER @phase-graphsink-storage-abstraction @stage-repoid-extension-to-repoinput @setup-compose
Feature: RepoID extension to RepoInput — deterministic repo identity

  The RepoInput struct gains a RepoID field so callers can pre-compute the
  repo row's primary key via fingerprint.RepoIDFromURL. EnsureRepoWithID
  honours this field; EnsureRepo ignores it and relies on gen_random_uuid().

  Scenario: ensurerepowithid-deterministic-insert
    Given an empty "repo" table and a non-zero "RepoInput.RepoID"
    When "EnsureRepoWithID" runs
    Then the row's "repo_id" equals the supplied UUID

  Scenario: ensurerepo-zero-id-uses-default
    Given a zero-value "RepoInput.RepoID"
    When "EnsureRepo" runs via the legacy path
    Then the row's "repo_id" is allocated by "gen_random_uuid()" and is non-zero

  Scenario: url-collision-returns-existing
    Given an existing row with URL "https://x/y" and "repo_id = A"
    When "EnsureRepoWithID" runs with the same URL and a different precomputed "repo_id = B"
    Then the returned "RepoRecord.RepoID" equals "A" and a structured log records the parity gap
