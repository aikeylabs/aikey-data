// Package baseline holds the v1.0.0 baseline DDL for the aikey-data
// **observability data** component (ODS / DWD / projector tasks).
//
// # Scope is intentionally narrow
//
// Only the data component is exposed here. The data tables
// (usage_event_ods, usage_fact_dwd, usage_dwd_projector_tasks) are part
// of the observability layer that already ships in every install — the
// aikey-proxy writes to ODS, the collector projects ODS → DWD, and the
// query-service reads from DWD. The DDL is therefore equivalent
// information to what any user can already inspect via
// `sqlite3 <db> .schema`, and publishing it as code lets public
// consumers (query-service tests, future user-facing tooling) work
// against the real schema without a private dependency.
//
// The control component (organizations, seats, managed credentials,
// virtual keys, bindings, control events, login sessions, etc.) is
// **not** exposed here. That tier is the enterprise edition's
// commercial ER model; its DDL lives only in the private
// aikey-config-tool repository.
//
// # Consumers
//
//   - aikey-config-tool (private): wraps these DDL lists into Migration
//     entries registered under "1.0.0" in
//     pkg/dbmigrate/versions/v1_0_0_baseline.go.
//   - aikey-data/query-service (public): tests bootstrap an in-memory
//     SQLite using DDLFor(ComponentData, DialectSQLite) so that
//     repository unit tests run against the real schema (per the
//     project's test-fixture-real-schema principle).
//   - aikey-data/collector-service (public): planned follow-up — will
//     replace its current aikey-config-tool import with this package to
//     remove the public→private dependency leak.
//
// # Idempotency
//
// Every CREATE TABLE / CREATE INDEX uses IF NOT EXISTS so a re-run on an
// already-installed DB is a benign no-op. The projector_tasks seed row
// uses INSERT OR IGNORE / ON CONFLICT DO NOTHING for the same guarantee.
//
// # Stability
//
// This package only exposes v1.0.0 DDL. Future schema changes belong in
// aikey-config-tool's migration registry as new Version entries — they
// do NOT mutate the strings here. If a v1.0.0 row needs to be patched
// for a security fix, mirror the patch into both repos and document it
// in the migration history.
package baseline

// Component identifies which logical service owns a schema slice.
// Only ComponentData is supported in this public package by design —
// see the package doc for the rationale (control component stays
// private). The constant matches aikey-config-tool's
// dbmigrate.Component naming so call sites that bridge the two layers
// stay readable.
type Component string

const (
	ComponentData Component = "data"
)

// Dialect identifies the SQL flavor. The two values match
// aikey-config-tool's dbmigrate.Dialect constants.
type Dialect string

const (
	DialectPostgres Dialect = "postgres"
	DialectSQLite   Dialect = "sqlite"
)

// DDLFor returns the per-statement v1.0.0 baseline DDL for the data
// component in the requested dialect. Returns nil for any
// unsupported pair (e.g. asking for a hypothetical ComponentControl,
// which deliberately is not exported here — see package doc).
//
// Statement order matters: indexes follow their parent table; seed
// INSERTs follow the table they target. Callers should execute the
// returned slice in order.
func DDLFor(c Component, d Dialect) []string {
	switch {
	case c == ComponentData && d == DialectPostgres:
		return dataPostgres()
	case c == ComponentData && d == DialectSQLite:
		return dataSQLite()
	default:
		return nil
	}
}
