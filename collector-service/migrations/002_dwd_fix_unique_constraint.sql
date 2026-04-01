-- Fix: DWD event_id uniqueness must match ODS (org_id + event_id), not just event_id.
-- Without this, cross-org duplicate event_ids would silently drop the second DWD record.

-- Drop the old single-column unique constraint on event_id
DROP INDEX IF EXISTS usage_fact_dwd_event_id_key;

-- Create the correct composite unique constraint matching ODS semantics
CREATE UNIQUE INDEX IF NOT EXISTS uq_dwd_org_event_id
    ON usage_fact_dwd (org_id, event_id);
