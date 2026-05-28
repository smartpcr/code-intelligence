-- Migration 0022: Add 'overrides' to edge_kind enum.
-- This allows Pass 2d to store trait→impl override relationships
-- as graph edges rather than only in attrs_json.

ALTER TYPE edge_kind ADD VALUE IF NOT EXISTS 'overrides';