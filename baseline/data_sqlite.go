package baseline

// dataSQLite returns the per-statement DDL for the data component on
// SQLite. Sourced from the data-tables portion of the trial server's
// consolidated baseline.
//
// Note on timestamp types: usage_event_ods / usage_fact_dwd timestamp
// columns are INTEGER (Unix millis) from the start in the SQLite
// baseline. v1.0.3-alpha's hook short-circuits on a fresh install
// because columnType() returns "INTEGER" (see migrateColumn103 in
// v1_0_3_alpha.go). On already-installed DBs that pre-date the
// baseline rewrite, v1.0.3 still does the rename+add+backfill.
//
// Note on uq_dwd_org_event_id: the SQLite baseline includes the
// composite (org_id, event_id) unique index from the start (the legacy
// SQLite never had the broken `UNIQUE (event_id)` form because it was
// hand-translated post-fix). v1.0.2-alpha's CREATE UNIQUE INDEX IF NOT
// EXISTS is therefore a no-op on fresh installs.
func dataSQLite() []string {
	return []string{
		// --- ODS: raw usage events --------------------------------------------
		`CREATE TABLE IF NOT EXISTS usage_event_ods (
			ods_id                    INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id                  TEXT NOT NULL,
			request_id                TEXT,
			trace_id                  TEXT,
			proxy_instance_id         TEXT,
			device_id                 TEXT,
			schema_version            INTEGER NOT NULL DEFAULT 1,
			source_type               TEXT NOT NULL DEFAULT 'local_proxy',
			source_version            TEXT,
			client_version            TEXT,
			proxy_config_version      TEXT,
			proxy_loaded_control_seq  INTEGER,
			event_time                INTEGER NOT NULL,
			occurred_at               INTEGER NOT NULL,
			started_at                INTEGER,
			finished_at               INTEGER,
			ingest_received_at        INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER) * 1000),
			collector_time            INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER) * 1000),
			org_id                    TEXT NOT NULL,
			account_id                TEXT,
			seat_id                   TEXT,
			account_status_snapshot   TEXT,
			virtual_key_id            TEXT,
			virtual_key_revision      TEXT,
			virtual_key_hash          TEXT,
			virtual_key_alias         TEXT,
			binding_id                TEXT,
			credential_id             TEXT,
			credential_revision       TEXT,
			real_key_hash             TEXT,
			credential_fingerprint    TEXT,
			provider_account_fingerprint TEXT,
			provider_id               TEXT,
			provider_code             TEXT,
			protocol_type             TEXT,
			route_source              TEXT,
			oauth_identity            TEXT,
			model                     TEXT,
			request_count             INTEGER NOT NULL DEFAULT 1,
			input_tokens              INTEGER,
			output_tokens             INTEGER,
			cached_input_tokens       INTEGER,
			cache_creation_input_tokens INTEGER,
			reasoning_tokens          INTEGER,
			total_tokens              INTEGER,
			billable_amount           REAL,
			currency                  TEXT,
			request_status            TEXT NOT NULL,
			http_status_code          INTEGER,
			error_code                TEXT,
			error_message             TEXT,
			upstream_request_id       TEXT,
			raw_usage_json            TEXT,
			raw_headers_json          TEXT,
			ext_json                  TEXT,
			raw_event_json            TEXT NOT NULL,
			ingest_status             TEXT NOT NULL DEFAULT 'accepted',
			dwd_status                TEXT NOT NULL DEFAULT 'pending',
			dwd_retry_count           INTEGER NOT NULL DEFAULT 0,
			dwd_next_retry_at         INTEGER,
			dwd_last_error_code       TEXT,
			dwd_last_error_msg        TEXT,
			anomaly_type              TEXT,
			anomaly_reason            TEXT,
			UNIQUE (org_id, event_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_event_time ON usage_event_ods (event_time)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_org_time   ON usage_event_ods (org_id, event_time)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_seat_time  ON usage_event_ods (seat_id, event_time)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_vk_time    ON usage_event_ods (virtual_key_id, event_time)`,
		`CREATE INDEX IF NOT EXISTS idx_ods_dwd_scan   ON usage_event_ods (dwd_status, dwd_next_retry_at, ods_id)`,

		// --- DWD: standardised usage fact -------------------------------------
		`CREATE TABLE IF NOT EXISTS usage_fact_dwd (
			dwd_id                        INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id                      TEXT NOT NULL,
			ods_id                        INTEGER NOT NULL,
			occurred_at                   INTEGER NOT NULL,
			event_time                    INTEGER NOT NULL,
			usage_date                    TEXT NOT NULL,
			org_id                        TEXT,
			account_id                    TEXT,
			seat_id                       TEXT,
			virtual_key_id                TEXT,
			virtual_key_revision          TEXT,
			virtual_key_alias             TEXT,
			virtual_key_hash              TEXT,
			binding_id                    TEXT,
			binding_alias                 TEXT,
			credential_id                 TEXT,
			credential_revision           TEXT,
			real_key_hash                 TEXT,
			credential_fingerprint        TEXT,
			provider_account_fingerprint  TEXT,
			provider_id                   TEXT,
			provider_code                 TEXT,
			provider_display_name         TEXT,
			protocol_type                 TEXT,
			route_source                  TEXT,
			model                         TEXT,
			request_count                 INTEGER NOT NULL,
			input_tokens                  INTEGER NOT NULL DEFAULT 0,
			output_tokens                 INTEGER NOT NULL DEFAULT 0,
			cached_input_tokens           INTEGER NOT NULL DEFAULT 0,
			cache_creation_input_tokens   INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens              INTEGER NOT NULL DEFAULT 0,
			total_tokens                  INTEGER NOT NULL DEFAULT 0,
			billable_amount               REAL,
			currency                      TEXT,
			request_status                TEXT NOT NULL,
			http_status_code              INTEGER,
			upstream_request_id           TEXT,
			completion_source             TEXT NOT NULL,
			quality_status                TEXT NOT NULL,
			validation_code               TEXT,
			validation_message            TEXT,
			anomaly_type                  TEXT,
			anomaly_reason                TEXT,
			billing_scope                 TEXT NOT NULL,
			user_usage_scope              TEXT NOT NULL,
			control_event_id              TEXT,
			control_event_revision        TEXT,
			projector_version             TEXT NOT NULL,
			projected_at                  INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER) * 1000),
			UNIQUE (ods_id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_dwd_org_event_id ON usage_fact_dwd (org_id, event_id)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_org_date      ON usage_fact_dwd (org_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_seat_date     ON usage_fact_dwd (seat_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_account_date  ON usage_fact_dwd (account_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_protocol_date ON usage_fact_dwd (protocol_type, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_provider_date ON usage_fact_dwd (provider_id, usage_date)`,
		`CREATE INDEX IF NOT EXISTS idx_dwd_anomaly_date  ON usage_fact_dwd (anomaly_type, usage_date)`,

		// --- Projector cursor / checkpoint ------------------------------------
		`CREATE TABLE IF NOT EXISTS usage_dwd_projector_tasks (
			task_name              TEXT PRIMARY KEY,
			last_scanned_ods_id    INTEGER NOT NULL DEFAULT 0,
			last_scanned_at        TEXT,
			last_success_at        TEXT,
			last_error_at          TEXT,
			last_error_message     TEXT,
			updated_at             TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`INSERT OR IGNORE INTO usage_dwd_projector_tasks (task_name) VALUES ('default')`,
	}
}
