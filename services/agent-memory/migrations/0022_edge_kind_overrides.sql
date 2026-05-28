-- Migration 0022: Add 'overrides' to edge_kind enum.
-- This allows Pass 2d to store trait→impl override relationships
-- as graph edges rather than only in attrs_json.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_enum
        WHERE enumlabel = 'overrides'
        AND enumtypid = (SELECT oid FROM pg_type WHERE typname = 'edge_kind')
    ) THEN
        ALTER TYPE edge_kind ADD VALUE 'overrides';
    END IF;
END
$$;