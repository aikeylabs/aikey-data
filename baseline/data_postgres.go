package baseline

// dataPostgres returns the per-statement DDL for the data component on
// PostgreSQL.
//
// Note on uq_dwd_org_event_id: PostgreSQL deployments shipped with a
// single-column UNIQUE (event_id) for a brief period; that did not match
// the collector-service projector's ON CONFLICT (org_id, event_id)
// target (PG 42P10) so DWD projection failed. The composite
// CREATE UNIQUE INDEX added below is the fix (bugfix 2026-05-11-pg-dwd-
// unique-missing). Pre-GA deployments needed a one-time idempotent re-run
// of the same statement to recover.
func dataPostgres() []string {
	return []string{
		// --- ODS: raw usage events --------------------------------------------
		//
		// PARTITIONED BY RANGE (event_time) — monthly (v1.0.1-alpha.4). ODS is the
		// larger firehose; same rationale as usage_fact_dwd (pruning + cheap
		// retention). Partition key is event_time (the only time column every read
		// path filters on). PK and the dedup UNIQUE gain event_time (PostgreSQL
		// requires the partition key in them). Dedup stays equivalent: event_time
		// is stamped once by the proxy and never changes across retransmits, so a
		// given (org_id, event_id) always lands on the same event_time. SQLite
		// (Personal/Trial) is NOT partitioned — see data_sqlite.go.
		`CREATE TABLE IF NOT EXISTS usage_event_ods (
			ods_id                    BIGSERIAL,
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
			event_time                TIMESTAMPTZ NOT NULL,
			occurred_at               TIMESTAMPTZ NOT NULL,
			started_at                TIMESTAMPTZ,
			finished_at               TIMESTAMPTZ,
			ingest_received_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			collector_time            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			org_id                    VARCHAR(64) NOT NULL,
			account_id                VARCHAR(64),
			seat_id                   VARCHAR(64),
			account_status_snapshot   VARCHAR(32),
			virtual_key_id            VARCHAR(64),
			virtual_key_revision      VARCHAR(64),
			virtual_key_hash          VARCHAR(128),
			binding_id                VARCHAR(64),
			credential_id             VARCHAR(64),
			credential_revision       VARCHAR(64),
			real_key_hash             VARCHAR(128),
			credential_fingerprint    VARCHAR(128),
			provider_account_fingerprint VARCHAR(128),
			provider_id               VARCHAR(64),
			provider_code             VARCHAR(64),
			protocol_type             VARCHAR(32),
			route_source              VARCHAR(32),
			oauth_identity            VARCHAR(255),
			model                     VARCHAR(255),
			request_count             INTEGER NOT NULL DEFAULT 1,
			input_tokens              BIGINT,
			output_tokens             BIGINT,
			cached_input_tokens       BIGINT,
			cache_creation_input_tokens BIGINT,
			reasoning_tokens          BIGINT,
			total_tokens              BIGINT,
			billable_amount           NUMERIC(20,8),
			currency                  VARCHAR(16),
			request_status            VARCHAR(32) NOT NULL,
			http_status_code          INTEGER,
			error_code                VARCHAR(64),
			error_message             VARCHAR(1024),
			upstream_request_id       VARCHAR(128),
			raw_usage_json            JSONB,
			raw_headers_json          JSONB,
			ext_json                  JSONB,
			raw_event_json            JSONB NOT NULL,
			ingest_status             VARCHAR(32) NOT NULL DEFAULT 'accepted',
			dwd_status                VARCHAR(32) NOT NULL DEFAULT 'pending',
			dwd_retry_count           INTEGER NOT NULL DEFAULT 0,
			dwd_next_retry_at         TIMESTAMPTZ,
			dwd_last_error_code       VARCHAR(64),
			dwd_last_error_msg        VARCHAR(1024),
			anomaly_type              VARCHAR(64),
			anomaly_reason            VARCHAR(1024),
			-- event_time appended to PK + the dedup UNIQUE: PostgreSQL requires the
			-- partition key in unique constraints. Dedup-equivalent — see the
			-- partitioning note on the CREATE TABLE above.
			PRIMARY KEY (ods_id, event_time),
			UNIQUE (org_id, event_id, event_time)
		) PARTITION BY RANGE (event_time)`,
		// Default partition so inserts never fail before EnsureMonthlyPartitions
		// (collector startup) creates the current/next month.
		`CREATE TABLE IF NOT EXISTS usage_event_ods_default PARTITION OF usage_event_ods DEFAULT`,
		`CREATE INDEX IF NOT EXISTS idx_ods_event_time ON usage_event_ods (event_time)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_org_time   ON usage_event_ods (org_id, event_time)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_seat_time  ON usage_event_ods (seat_id, event_time)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_vk_time    ON usage_event_ods (virtual_key_id, event_time)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_dwd_scan   ON usage_event_ods (dwd_status, dwd_next_retry_at, ods_id)`,

		// --- DWD: standardised usage fact -------------------------------------
		//
		// PARTITIONED BY RANGE (usage_date) — monthly (v1.0.1-alpha.4, enterprise
		// usage-audit). Why partition: the audit page reads the last 3 days and
		// the audit export can span a year; range-partitioning on usage_date gives
		// partition pruning (bounded per-query cost) + cheap retention (DETACH +
		// DROP an old month, no big DELETE). Why usage_date is the key: every read
		// path filters on it, so pruning actually fires. PostgreSQL requires the
		// partition key in the PK and every UNIQUE constraint, so dwd_id/event_id/
		// ods_id all gain usage_date below. This is dedup-equivalent to the old
		// keys because usage_date = date(event_time) and event_time is stamped once
		// at event creation and never changes across retransmits — so a given
		// (org_id, event_id) always lands on the same usage_date. SQLite (Personal/
		// Trial) is NOT partitioned — see data_sqlite.go (low volume, single table).
		`CREATE TABLE IF NOT EXISTS usage_fact_dwd (
			dwd_id                        BIGSERIAL,
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
			cache_creation_input_tokens   BIGINT NOT NULL DEFAULT 0,
			reasoning_tokens              BIGINT NOT NULL DEFAULT 0,
			total_tokens                  BIGINT NOT NULL DEFAULT 0,
			billable_amount               NUMERIC(20,8),
			currency                      VARCHAR(16),
			request_status                VARCHAR(32) NOT NULL,
			http_status_code              INTEGER,
			upstream_request_id           VARCHAR(128),
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
			-- usage_date appended to PK + every UNIQUE: PostgreSQL requires the
			-- partition key in unique constraints. Dedup-equivalent — see the
			-- partitioning note on the CREATE TABLE above.
			PRIMARY KEY (dwd_id, usage_date),
			UNIQUE (event_id, usage_date),
			UNIQUE (ods_id, usage_date)
		) PARTITION BY RANGE (usage_date)`,
		// Default partition catches any row whose month has no dedicated
		// partition yet, so inserts never fail before EnsureMonthlyPartitions
		// (collector startup) has created the current/next month. Monthly
		// partitions are created at runtime, not here (time-dependent).
		`CREATE TABLE IF NOT EXISTS usage_fact_dwd_default PARTITION OF usage_fact_dwd DEFAULT`,
		// 2026-05-11 fix: composite UNIQUE matching collector-service's projector
		// ON CONFLICT target. v1.0.1-alpha.4: + usage_date for the partition key.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_dwd_org_event_id ON usage_fact_dwd (org_id, event_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_org_date      ON usage_fact_dwd (org_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_seat_date     ON usage_fact_dwd (seat_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_account_date  ON usage_fact_dwd (account_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_protocol_date ON usage_fact_dwd (protocol_type, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_provider_date ON usage_fact_dwd (provider_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_anomaly_date  ON usage_fact_dwd (anomaly_type, usage_date)`,

		// --- Projector cursor / checkpoint ------------------------------------
		`CREATE TABLE IF NOT EXISTS usage_dwd_projector_tasks (
			task_name              VARCHAR(64) PRIMARY KEY,
			last_scanned_ods_id    BIGINT NOT NULL DEFAULT 0,
			last_scanned_at        TIMESTAMPTZ,
			last_success_at        TIMESTAMPTZ,
			last_error_at          TIMESTAMPTZ,
			last_error_message     VARCHAR(1024),
			updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`INSERT INTO usage_dwd_projector_tasks (task_name) VALUES ('default')
		 ON CONFLICT (task_name) DO NOTHING`,
	}
}
