-- Fix: DWD event_id uniqueness must match ODS (org_id + event_id), not just event_id.
-- Without this, cross-org duplicate event_ids would silently drop the second DWD record.
--
-- PostgreSQL inline UNIQUE creates a constraint (not just an index), so we must
-- use ALTER TABLE DROP CONSTRAINT, not DROP INDEX.

-- Drop the old single-column unique constraint on event_id
ALTER TABLE usage_fact_dwd DROP CONSTRAINT IF EXISTS usage_fact_dwd_event_id_key;

-- Create the correct composite unique index matching ODS semantics
CREATE UNIQUE INDEX IF NOT EXISTS uq_dwd_org_event_id
    ON usage_fact_dwd (org_id, event_id);
