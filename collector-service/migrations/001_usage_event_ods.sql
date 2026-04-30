-- ============================================================================
-- LEGACY / NOT EXECUTED — kept for historical reference only.
--
-- Authoritative DDL now lives in:
--     aikey-config-tool/pkg/dbmigrate/versions/v_*.go
-- and is applied by every service at boot via versions.UpgradeTo(...).
--
-- Per CLAUDE.md "Schema 治理": "Do not add new *.sql files to service
-- migrations/ dirs." This file pre-dates the D plan registry consolidation
-- (Stage 2.5 fixture pipeline). It is NOT loaded at runtime; new columns
-- (e.g. v1.0.4 oauth_identity, v1.0.5 cache_creation_input_tokens) are
-- added via the registry, not by editing this file.
--
-- For the current schema state on a live database, dump the table or read
-- the post-v1.0.4 canonical CREATE TABLE in v1_0_4_alpha.go (odsRebuildCreateSQL
-- / dwdRebuildCreateSQL).
-- ============================================================================

-- ODS: raw usage events received from Local Proxy
CREATE TABLE IF NOT EXISTS usage_event_ods (
    ods_id                    BIGSERIAL PRIMARY KEY,
    event_id                  VARCHAR(64) NOT NULL,
    request_id                VARCHAR(64),
    trace_id                  VARCHAR(64),
    proxy_instance_id         VARCHAR(64),
    device_id                 VARCHAR(128),

    schema_version            INTEGER NOT NULL DEFAULT 1,
    source_type               VARCHAR(32) NOT NULL DEFAULT 'local_proxy',
    source_version            VARCHAR(64),
    client_version            VARCHAR(64),
    proxy_config_version      VARCHAR(64),
    proxy_loaded_control_seq  BIGINT,

    -- timestamps
    event_time                TIMESTAMPTZ NOT NULL,       -- local client time (D4)
    occurred_at               TIMESTAMPTZ NOT NULL,
    started_at                TIMESTAMPTZ,
    finished_at               TIMESTAMPTZ,
    ingest_received_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    collector_time            TIMESTAMPTZ NOT NULL DEFAULT NOW(),  -- server time (D4)

    -- ownership
    org_id                    VARCHAR(64) NOT NULL,
    account_id                VARCHAR(64),
    seat_id                   VARCHAR(64),
    account_status_snapshot   VARCHAR(32),

    -- routing: virtual key + binding + credential
    virtual_key_id            VARCHAR(64),
    virtual_key_revision      VARCHAR(64),
    virtual_key_hash          VARCHAR(128),
    binding_id                VARCHAR(64),
    credential_id             VARCHAR(64),
    credential_revision       VARCHAR(64),
    real_key_hash             VARCHAR(128),
    credential_fingerprint    VARCHAR(128),
    provider_account_fingerprint VARCHAR(128),

    -- provider / protocol
    provider_id               VARCHAR(64),
    provider_code             VARCHAR(64),
    protocol_type             VARCHAR(32),
    route_source              VARCHAR(32),
    -- Email / display name for OAuth-managed provider accounts. Mirrors
    -- the collector ingest INSERT order in
    -- aikey-data/collector-service/internal/ingest/repository_sql.go.
    -- Nullable: only OAuth-sourced events fill this; static API key
    -- credentials leave it NULL.
    oauth_identity            VARCHAR(255),

    -- usage
    model                     VARCHAR(255),
    request_count             INTEGER NOT NULL DEFAULT 1,
    input_tokens              BIGINT,
    output_tokens             BIGINT,
    cached_input_tokens       BIGINT,
    reasoning_tokens          BIGINT,
    total_tokens              BIGINT,
    billable_amount           NUMERIC(20,8),
    currency                  VARCHAR(16),

    -- result
    request_status            VARCHAR(32) NOT NULL,
    http_status_code          INTEGER,
    error_code                VARCHAR(64),
    error_message             VARCHAR(1024),
    upstream_request_id       VARCHAR(128),

    -- raw payload
    raw_usage_json            JSONB,
    raw_headers_json          JSONB,
    ext_json                  JSONB,
    raw_event_json            JSONB NOT NULL,

    -- processing state
    ingest_status             VARCHAR(32) NOT NULL DEFAULT 'accepted',
    dwd_status                VARCHAR(32) NOT NULL DEFAULT 'pending',
    dwd_retry_count           INTEGER NOT NULL DEFAULT 0,
    dwd_next_retry_at         TIMESTAMPTZ,
    dwd_last_error_code       VARCHAR(64),
    dwd_last_error_msg        VARCHAR(1024),
    anomaly_type              VARCHAR(64),
    anomaly_reason            VARCHAR(1024),

    -- idempotency: org_id + event_id (D7)
    UNIQUE (org_id, event_id)
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_ods_event_time      ON usage_event_ods (event_time);
CREATE INDEX IF NOT EXISTS idx_ods_org_time         ON usage_event_ods (org_id, event_time);
CREATE INDEX IF NOT EXISTS idx_ods_seat_time        ON usage_event_ods (seat_id, event_time);
CREATE INDEX IF NOT EXISTS idx_ods_vk_time          ON usage_event_ods (virtual_key_id, event_time);
CREATE INDEX IF NOT EXISTS idx_ods_dwd_scan         ON usage_event_ods (dwd_status, dwd_next_retry_at, ods_id);


-- DWD: standardised usage fact after control-plane enrichment
CREATE TABLE IF NOT EXISTS usage_fact_dwd (
    dwd_id                        BIGSERIAL PRIMARY KEY,
    event_id                      VARCHAR(64) NOT NULL,
    ods_id                        BIGINT NOT NULL,
    occurred_at                   TIMESTAMPTZ NOT NULL,
    event_time                    TIMESTAMPTZ NOT NULL,
    usage_date                    DATE NOT NULL,

    org_id                        VARCHAR(64),
    account_id                    VARCHAR(64),
    seat_id                       VARCHAR(64),

    virtual_key_id                VARCHAR(64),
    virtual_key_revision          VARCHAR(64),
    virtual_key_alias             VARCHAR(255),
    virtual_key_hash              VARCHAR(128),

    binding_id                    VARCHAR(64),
    binding_alias                 VARCHAR(255),

    credential_id                 VARCHAR(64),
    credential_revision           VARCHAR(64),
    real_key_hash                 VARCHAR(128),
    credential_fingerprint        VARCHAR(128),
    provider_account_fingerprint  VARCHAR(128),

    provider_id                   VARCHAR(64),
    provider_code                 VARCHAR(64),
    provider_display_name         VARCHAR(255),
    protocol_type                 VARCHAR(32),

    route_source                  VARCHAR(32),
    model                         VARCHAR(255),

    request_count                 INTEGER NOT NULL,
    input_tokens                  BIGINT NOT NULL DEFAULT 0,
    output_tokens                 BIGINT NOT NULL DEFAULT 0,
    cached_input_tokens           BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens              BIGINT NOT NULL DEFAULT 0,
    total_tokens                  BIGINT NOT NULL DEFAULT 0,
    billable_amount               NUMERIC(20,8),
    currency                      VARCHAR(16),

    request_status                VARCHAR(32) NOT NULL,
    http_status_code              INTEGER,
    upstream_request_id           VARCHAR(128),

    -- data quality & anomaly (D8: MVP only valid / late_report_abnormal_charge / pending_review)
    completion_source             VARCHAR(64) NOT NULL,
    quality_status                VARCHAR(32) NOT NULL,
    validation_code               VARCHAR(64),
    validation_message            VARCHAR(1024),
    anomaly_type                  VARCHAR(64),
    anomaly_reason                VARCHAR(1024),
    billing_scope                 VARCHAR(32) NOT NULL,
    user_usage_scope              VARCHAR(32) NOT NULL,

    control_event_id              VARCHAR(64),
    control_event_revision        VARCHAR(64),

    projector_version             VARCHAR(64) NOT NULL,
    projected_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (event_id),
    UNIQUE (ods_id)
);

CREATE INDEX IF NOT EXISTS idx_dwd_org_date       ON usage_fact_dwd (org_id, usage_date);
CREATE INDEX IF NOT EXISTS idx_dwd_seat_date      ON usage_fact_dwd (seat_id, usage_date);
CREATE INDEX IF NOT EXISTS idx_dwd_account_date   ON usage_fact_dwd (account_id, usage_date);
CREATE INDEX IF NOT EXISTS idx_dwd_protocol_date  ON usage_fact_dwd (protocol_type, usage_date);
CREATE INDEX IF NOT EXISTS idx_dwd_provider_date  ON usage_fact_dwd (provider_id, usage_date);
CREATE INDEX IF NOT EXISTS idx_dwd_anomaly_date   ON usage_fact_dwd (anomaly_type, usage_date);


-- Projector cursor / checkpoint
CREATE TABLE IF NOT EXISTS usage_dwd_projector_tasks (
    task_name              VARCHAR(64) PRIMARY KEY,
    last_scanned_ods_id    BIGINT NOT NULL DEFAULT 0,
    last_scanned_at        TIMESTAMPTZ,
    last_success_at        TIMESTAMPTZ,
    last_error_at          TIMESTAMPTZ,
    last_error_message     VARCHAR(1024),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Seed the default projector task row
INSERT INTO usage_dwd_projector_tasks (task_name) VALUES ('default')
ON CONFLICT (task_name) DO NOTHING;
