-- 0001_enums.sql
--
-- Stage 1.2 step 1 (implementation-plan.md): create every named ENUM
-- listed in tech-spec §8.7.1. Each ENUM's members are exactly the
-- closed set defined in architecture.md §5 (or §8.2 for
-- degraded_reason). New members MUST be added in a dedicated
-- migration so the closed-set invariant in architecture.md remains
-- auditable.
--
-- This migration is dependency-free and applies before any table
-- migration so subsequent migrations can reference the types in
-- their column declarations.

-- migrate:up
BEGIN;

-- architecture.md §5.2.1 Node.kind
CREATE TYPE node_kind AS ENUM (
    'repo',
    'package',
    'file',
    'class',
    'method',
    'block'
);

-- architecture.md §5.2.2 Edge.kind
CREATE TYPE edge_kind AS ENUM (
    'contains',
    'imports',
    'static_calls',
    'observed_calls',
    'extends',
    'implements',
    'reads',
    'writes',
    'renamed_to'
);

-- architecture.md §5.3.1 Episode.kind
CREATE TYPE episode_kind AS ENUM (
    'agent',
    'feedback',
    'synthetic_positive'
);

-- architecture.md §5.3.1 Episode.outcome (also EpisodeUpdate.new_outcome)
CREATE TYPE outcome AS ENUM (
    'success',
    'failure',
    'refused',
    'degraded',
    'human_corrected'
);

-- architecture.md §3.7 Block.kind discriminator (carried as
-- Node.attrs_json discriminator today; promoted to a first-class
-- typed column when Stage 3 lands the AST dispatcher).
CREATE TYPE block_kind AS ENUM (
    'entry',
    'branch',
    'loop_body',
    'exception',
    'exit'
);

-- architecture.md §5.5.2 ConceptVersion.confidence_band
CREATE TYPE concept_band AS ENUM (
    'low',
    'medium',
    'high'
);

-- architecture.md §5.5.2 ConceptVersion.producer
CREATE TYPE producer AS ENUM (
    'consolidator',
    'promoter'
);

-- architecture.md §5.5.3 ConceptSupport.polarity
CREATE TYPE polarity AS ENUM (
    'positive',
    'negative'
);

-- architecture.md §5.3.2 EpisodeUpdate.actor
CREATE TYPE actor AS ENUM (
    'operator',
    'consolidator',
    'system'
);

-- architecture.md §5.3.3 Observation.role
CREATE TYPE observation_role AS ENUM (
    'node_hit',
    'edge_hit',
    'call_edge_hit',
    'concept_hit',
    'degraded_recall_context'
);

-- architecture.md §5.6 RepoEvent.kind (closed set: push|merge|register|manual)
CREATE TYPE repo_event_kind AS ENUM (
    'push',
    'merge',
    'register',
    'manual'
);

-- architecture.md §5.4.1 RecallContextLog.verb
CREATE TYPE verb AS ENUM (
    'recall',
    'expand',
    'summarize'
);

-- architecture.md §8.2 closed degraded-reason set. tech-spec
-- §8.7.1 promotes this to an ENUM so the writer cannot emit a
-- novel reason string and bypass the closed-set invariant.
CREATE TYPE degraded_reason AS ENUM (
    'episodic_log_unavailable',
    'graph_store_unavailable',
    'embedding_index_unavailable',
    'reranker_model_stale',
    'span_ingestor_backpressure',
    'consolidator_backpressure'
);

COMMIT;

-- migrate:down
BEGIN;

DROP TYPE IF EXISTS degraded_reason;
DROP TYPE IF EXISTS verb;
DROP TYPE IF EXISTS repo_event_kind;
DROP TYPE IF EXISTS observation_role;
DROP TYPE IF EXISTS actor;
DROP TYPE IF EXISTS polarity;
DROP TYPE IF EXISTS producer;
DROP TYPE IF EXISTS concept_band;
DROP TYPE IF EXISTS block_kind;
DROP TYPE IF EXISTS outcome;
DROP TYPE IF EXISTS episode_kind;
DROP TYPE IF EXISTS edge_kind;
DROP TYPE IF EXISTS node_kind;

COMMIT;
