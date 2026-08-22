package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/AiKeyLabs/aikey-data/collector-service/internal/shared"
	"github.com/AiKeyLabs/pkg/aikeytime"
)

type sqlODS struct{ db *shared.DB }

// NewSQLODSRepository creates an ODS repository backed by either
// PostgreSQL or SQLite — dialect differences are abstracted by
// shared.DB (see internal/shared/dbkit.go). Renamed from
// postgresODS/NewPostgresODSRepository 2026-04-24 because the file
// name implied PG-only but the implementation has always been
// dual-dialect via dbkit.
func NewSQLODSRepository(db *shared.DB) ODSRepository {
	return &sqlODS{db: db}
}

// ingest_received_at / collector_time are set explicitly from Go here
// (aikeytime.Now()) rather than relying on the SQL DEFAULT. Why: the
// v1.0.3-alpha SQLite migration's ADD COLUMN path does not preserve
// DEFAULT expressions, so an upgraded trial DB would leave these
// columns NULL if the write relied on the DEFAULT. Always binding from
// Go keeps upgraded and fresh installs behaviour-identical — the
// schema DEFAULT becomes redundant belt-and-braces. See bugfix
// 20260424 review finding #2.
// Column order: keep new columns at the tail (after app_slug) so
// insertion order matches the bind list in InsertEvent without
// shifting historic positions. v1.0.0-rc.5 added app_slug; v1.0.0-rc.6
// adds session_id for the Performance Top N sessions chart; v1.0.0-rc.7
// adds source_id + source_seq for delivery integrity (per-source sequence).
// v1.0.0-rc.11 adds virtual_key_alias (from wire `key_label`) closing the
// gap where DWD.virtual_key_alias had been permanently empty — see
// aikey-config-tool/.../v1_0_0_rc11_virtual_key_alias.go.
const odsColumns = `event_id, request_id, trace_id, proxy_instance_id, device_id,
    schema_version, source_type, source_version, client_version,
    proxy_config_version, proxy_loaded_control_seq,
    event_time, occurred_at, started_at, finished_at,
    ingest_received_at, collector_time,
    org_id, account_id, seat_id, account_status_snapshot,
    virtual_key_id, virtual_key_revision, virtual_key_hash,
    binding_id, credential_id, credential_revision,
    real_key_hash, credential_fingerprint, provider_account_fingerprint,
    provider_id, provider_code, protocol_type, route_source, oauth_identity,
    model, request_count,
    input_tokens, output_tokens, cached_input_tokens, cache_creation_input_tokens, reasoning_tokens, total_tokens,
    billable_amount, currency,
    request_status, http_status_code, error_code, error_message, upstream_request_id,
    raw_usage_json, raw_headers_json, ext_json, raw_event_json,
    app_slug, session_id,
    source_id, source_seq,
    region, endpoint_url,
    virtual_key_alias,
    content_hash, ingest_status, dwd_status,
    fallback_reason, fallback_attempt`

const odsPlaceholders = `?,?,?,?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,?,
    ?,?,
    ?,?,?,?,
    ?,?,?,
    ?,?,?,
    ?,?,?,
    ?,?,?,?,?,
    ?,?,
    ?,?,?,?,?,?,
    ?,?,
    ?,?,?,?,?,
    ?,?,?,?,
    ?,?,
    ?,?,
    ?,?,
    ?,
    ?,?,?,
    ?,?`

// InsertEvent persists one event. quarantined drives the disposition columns:
// false → ingest_status='accepted', dwd_status='pending' (normal flow, projector
// will bill it); true → ingest_status='quarantined', dwd_status='quarantined' so
// the projector's pending/retry filter SKIPS it (never projected → never billed),
// while the row + its suspect values are preserved for review. We bind these two
// columns explicitly (not relying on the schema DEFAULTs) so the quarantine
// disposition is set atomically with the insert — no insert-then-update race
// where the 5s projector could grab the row in between. content_hash is stored
// for forensics + duplicate-conflict detection (§6.2).
func (r *sqlODS) InsertEvent(ctx context.Context, e *UsageEvent, rawJSON []byte, quarantined bool) (inserted, conflict bool, err error) {
	inserted, conflict, _, err = insertEventOn(ctx, r.db, r.db, e, rawJSON, quarantined)
	return inserted, conflict, err
}

// BeginBatch opens one transaction for a whole ingest batch so the batch costs
// ONE commit+WAL-fsync instead of one per event (P0-4 batch rewrite). The
// returned writer runs the SAME per-event insert logic (per-row statements —
// NOT a multi-row INSERT — so intra-batch duplicate event_ids and the F2
// swallow guard keep their exact per-row semantics). Infra-level SQL failures
// are flagged via Failed() so the service can roll back and replay the batch
// on the per-event autocommit path (on PostgreSQL one failed statement aborts
// the whole transaction; per-event independence is preserved by that replay).
func (r *sqlODS) BeginBatch(ctx context.Context) (BatchEventWriter, error) {
	tx, err := r.db.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	return &odsBatchWriter{d: r.db, tx: tx}, nil
}

type odsBatchWriter struct {
	d      *shared.DB
	tx     *shared.Tx
	failed bool
}

func (b *odsBatchWriter) InsertEvent(ctx context.Context, e *UsageEvent, rawJSON []byte, quarantined bool) (bool, bool, error) {
	inserted, conflict, infra, err := insertEventOn(ctx, b.d, b.tx, e, rawJSON, quarantined)
	if infra {
		b.failed = true
	}
	return inserted, conflict, err
}

func (b *odsBatchWriter) Failed() bool    { return b.failed }
func (b *odsBatchWriter) Commit() error   { return b.tx.Commit() }
func (b *odsBatchWriter) Rollback() error { return b.tx.Rollback() }

// insertEventOn is the single insert implementation, executing through either
// the autocommit pool (*shared.DB) or a batch transaction (*shared.Tx). infra
// reports an infrastructure-level SQL failure (statement exec / scan error) as
// opposed to the logical F2 "silently ignored" detection — inside a PostgreSQL
// transaction an infra failure poisons the tx, so the batch writer uses it to
// trigger rollback+replay, while the F2 detection (a successful statement +
// ErrNoRows verify) is safe to continue past.
func insertEventOn(ctx context.Context, d *shared.DB, ex shared.Execer, e *UsageEvent, rawJSON []byte, quarantined bool) (inserted, conflict, infra bool, err error) {
	ingestStatus, dwdStatus := "accepted", "pending"
	if quarantined {
		ingestStatus, dwdStatus = "quarantined", "quarantined"
	}
	// Dialect-aware conflict target: PostgreSQL's usage_event_ods is partitioned
	// by event_time (v1.0.1-alpha.4), so its dedup UNIQUE is
	// (org_id, event_id, event_time) — the partition key must be in the ON
	// CONFLICT inference. SQLite is not partitioned and uses INSERT OR IGNORE
	// (target ignored).
	//
	// CONTRACT (not just an assumption): a given event_id always carries the
	// SAME event_time across every (re)delivery — the proxy stamps event_time
	// once and resends the persisted event verbatim, and usagehash excludes
	// event_time. The dedup verify below (WHERE org_id,event_id,event_time)
	// relies on this. If this contract is ever broken (clock skew across proxy
	// instances, a client crafting a fresh event_time on resend), the SQLite
	// path will misread a true duplicate as a NOT NULL/CHECK rejection. See
	// workflow/CI/bugfix/2026-06-10-collector-dedup-event-time-misclassify.md.
	conflictTarget := "org_id, event_id"
	if d.Dialect == shared.DialectPostgres {
		conflictTarget = "org_id, event_id, event_time"
	}
	insertODS := d.InsertOrIgnoreOn("usage_event_ods", odsColumns, odsPlaceholders, conflictTarget)
	res, err := ex.ExecContext(ctx, insertODS,
		e.EventID, nullStr(e.RequestID), nullStr(e.TraceID), nullStr(e.ProxyInstanceID), nullStr(e.DeviceID),
		e.SchemaVersion, "local_proxy", nullStr(e.SourceVersion), nullStr(e.ClientVersion),
		nullStr(e.ProxyConfigVersion), e.ProxyLoadedControlSeq,
		d.BindMillis(e.EventTime), d.BindMillis(e.OccurredAt), d.BindMillisPtr(e.StartedAt), d.BindMillisPtr(e.FinishedAt),
		d.BindMillis(aikeytime.Now()), d.BindMillis(aikeytime.Now()),
		e.OrgID, nullStr(e.AccountID), nullStr(e.SeatID), nullStr(e.AccountStatusSnapshot),
		nullStr(e.VirtualKeyID), nullStr(e.VirtualKeyRevision), nullStr(e.VirtualKeyHash),
		nullStr(e.BindingID), nullStr(e.CredentialID), nullStr(e.CredentialRevision),
		nullStr(e.RealKeyHash), nullStr(e.CredentialFingerprint), nullStr(e.ProviderAccountFingerprint),
		nullStr(e.ProviderID), nullStr(e.ProviderCode), nullStr(e.ProtocolType), nullStr(e.RouteSource), nullStr(e.OAuthIdentity),
		nullStr(e.Model), e.RequestCount,
		e.InputTokens, e.OutputTokens, e.CachedInputTokens, e.CacheCreationInputTokens, e.ReasoningTokens, e.TotalTokens,
		nullStr(ptrStr(e.BillableAmount)), nullStr(e.Currency),
		e.RequestStatus, e.HTTPStatusCode, nullStr(e.ErrorCode), nullStr(e.ErrorMessage), nullStr(e.UpstreamRequestID),
		jsonbOrNull(e.RawUsageJSON), jsonbOrNull(e.RawHeadersJSON), jsonbOrNull(e.ExtJSON), rawJSON,
		nullStr(e.AppSlug),
		nullStr(e.SessionID),
		nullStr(e.SourceID), e.SourceSeq, // delivery integrity (v2); SourceSeq nil → NULL for v1 events
		nullStr(e.Region), nullStr(e.EndpointURL), // cost-pricing audit (v1.0.0-rc.8)
		nullStr(e.KeyLabel),                             // virtual_key_alias (v1.0.0-rc.11) — display label, carried into DWD by projector
		nullStr(e.ContentHash), ingestStatus, dwdStatus, // stage C: content fingerprint + disposition
		// 🔴 Upstream fallback attribution. `e.FallbackAttempt` is bound as the
		// pointer it is: a nil reaches the column as NULL ("the sender said
		// nothing"), which every count over this column has to tell apart from a
		// measured 1 ("the primary served it").
		nullStr(e.FallbackReason), e.FallbackAttempt,
	)
	if err != nil {
		return false, false, true, &TransientStorageError{Err: fmt.Errorf("insert ods event %s: %w", e.EventID, err)}
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, false, false, nil
	}
	// rowsAffected==0 from `INSERT OR IGNORE` / `ON CONFLICT DO NOTHING` is
	// historically misread as "duplicate". It also fires for *any* other
	// constraint violation that the IGNORE/DO NOTHING swallows (NOT NULL,
	// CHECK, FK, ...). Verify it's a genuine UNIQUE conflict before
	// reporting "duplicated"; otherwise surface a real error so the
	// caller can mark the event rejected with diagnostics rather than
	// returning HTTP 200 + duplicated:1 while data was silently dropped.
	// Discovered as F2 on 2026-04-27 during D-plan cross-version-jump
	// E2E: post-upgrade ODS NOT NULL on legacy_text columns swallowed
	// every fresh INSERT and reported them as duplicates. See
	// workflow/CI/bugfix/ for the regression record.
	//
	// Stage C piggyback: select the STORED content_hash on this same verify
	// query (zero extra round-trips) so the caller can detect a §6.2 conflict —
	// same event_id re-delivered with a DIFFERENT hash = corruption/tamper. We
	// keep the first-stored row (do NOT overwrite — never silently pick one);
	// the caller logs + counts the conflict.
	var storedHash sql.NullString
	// event_time in the WHERE prunes to the one partition on PostgreSQL (harmless
	// extra filter on SQLite). It matches the existing row because event_time is
	// deterministic per event, so it never hides a genuine duplicate.
	verr := ex.QueryRowContext(ctx,
		"SELECT content_hash FROM usage_event_ods WHERE org_id = ? AND event_id = ? AND event_time = ? LIMIT 1",
		e.OrgID, e.EventID, d.BindMillis(e.EventTime),
	).Scan(&storedHash)
	if verr == nil {
		conflict := e.ContentHash != "" && storedHash.Valid && storedHash.String != "" && storedHash.String != e.ContentHash
		return false, conflict, false, nil // genuine duplicate (conflict flagged when hashes differ)
	}
	if verr == sql.ErrNoRows {
		return false, false, false, fmt.Errorf("ods INSERT silently ignored (no UNIQUE conflict on org_id=%s event_id=%s) — likely NOT NULL/CHECK/FK violation. F2 root cause: NOT NULL on _legacy_text columns left over from v1.0.3 hook is hit when v1.0.4 collector binds only INTEGER columns. Run dbmigrate v1.0.4 hook upgrade to drop legacy NOT NULL", e.OrgID, e.EventID)
	}
	return false, false, true, &TransientStorageError{Err: fmt.Errorf("verify dedup for event_id=%s: %w", e.EventID, verr)}
}

// advanceWatermarkScanLimit bounds how many source_seq rows AdvanceWatermark
// pulls per call. After a long gap is finally backfilled the connected run can
// be huge; we walk at most this many in one pass and let the next batch
// continue, keeping a single ingest call's latency bounded.
const advanceWatermarkScanLimit = 10000

// AdvanceWatermark — see ODSRepository.AdvanceWatermark. Zipper-advances the
// connected-no-gap high-water mark for one source and persists it.
func (r *sqlODS) AdvanceWatermark(ctx context.Context, orgID, sourceID string, clientAllocated int64) (int64, error) {
	// Current contiguous high-water for this source (0 if first time).
	var contiguous int64
	err := r.db.QueryRowContext(ctx,
		"SELECT contiguous_seq FROM usage_source_watermark WHERE org_id = ? AND source_id = ?",
		orgID, sourceID,
	).Scan(&contiguous)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("read watermark org=%s source=%s: %w", orgID, sourceID, err)
	}

	// Zipper advance: build the set of "present" seqs in (contiguous,
	// contiguous+limit] from BOTH the ODS (genuinely received) AND the known-loss
	// ledger (stage D1: a seq promoted to known-lost is "accounted for", so
	// contiguous advances PAST it — §6.3 "every seq is either in ODS or
	// known-lost, no third state"). Then walk from contiguous+1 while present.
	// Bounding to a window (not unbounded) keeps a single ingest call's latency
	// bounded after a long backfill; the next call continues. With an empty
	// ledger this behaves identically to the pre-D1 ODS-only zipper.
	hi := contiguous + advanceWatermarkScanLimit
	present, err := r.presentSeqSet(ctx, orgID, sourceID, contiguous, hi)
	if err != nil {
		return 0, err
	}
	for present[contiguous+1] {
		contiguous++
	}

	// max_seen_seq is the highest seq stored for this source (may sit ABOVE
	// contiguous when there is a gap — that span is the "suspected gap" region
	// B' gap detection inspects). Query it directly so a freshly-arrived
	// out-of-order seq raises max_seen even though it didn't advance contiguous.
	var maxSeen int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(source_seq), 0) FROM usage_event_ods WHERE org_id = ? AND source_id = ?",
		orgID, sourceID,
	).Scan(&maxSeen); err != nil {
		return 0, fmt.Errorf("max_seen org=%s source=%s: %w", orgID, sourceID, err)
	}

	// Persist contiguous + max_seen + last_event_at together (one row, one
	// statement). ON CONFLICT DO UPDATE syntax is identical on PG + SQLite.
	// NowMillis() is dialect-aware (PG NOW() / SQLite epoch-millis) so the
	// timestamp columns match their β-hybrid type; B' gap detection compares
	// updated_at against NowMillis() for staleness.
	//
	// Why MAX(excluded, existing) on max_seen + client_allocated: watermark
	// advance can run from any batch in any order; these columns must be
	// monotonic (never regress) so a late small-seq / stale-allocated batch can't
	// lower a high-water mark. clientAllocated is the batch's allocator
	// high-water (0 when the batch carried no allocated_seq); since seqs start at
	// 1, MAX(existing, 0) leaves it unchanged — 0 is a safe "no info" no-op.
	// last_event_at is set to "now" unconditionally — it tracks "last time ANY
	// event for this source landed", the source-silence signal.
	// Greatest() is dialect-safe: MAX(a,b) on SQLite, GREATEST(a,b) on Postgres
	// (PG's MAX is aggregate-only — a literal MAX(a,b) throws there, a dialect bug
	// SQLite-only tests miss; caught by PG live E2E on confirm-lost).
	now := r.db.NowMillis()
	maxSeenG := r.db.Greatest("usage_source_watermark.max_seen_seq", "excluded.max_seen_seq")
	clientAllocG := r.db.Greatest("usage_source_watermark.client_allocated_seq", "excluded.client_allocated_seq")
	upsert := fmt.Sprintf(
		`INSERT INTO usage_source_watermark (org_id, source_id, contiguous_seq, max_seen_seq, client_allocated_seq, last_event_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, %s, %s)
		 ON CONFLICT (org_id, source_id) DO UPDATE SET
		   contiguous_seq       = ?,
		   max_seen_seq         = %s,
		   client_allocated_seq = %s,
		   last_event_at        = %s,
		   updated_at           = %s`,
		now, now, maxSeenG, clientAllocG, now, now,
	)
	if _, err := r.db.ExecContext(ctx, upsert, orgID, sourceID, contiguous, maxSeen, clientAllocated, contiguous); err != nil {
		return 0, fmt.Errorf("upsert watermark org=%s source=%s: %w", orgID, sourceID, err)
	}
	return contiguous, nil
}

// presentSeqSet returns the set of seqs in (lo, hi] that count as "present" for
// contiguity: received in ODS OR recorded in the known-loss ledger (stage D1).
// Two bounded queries merged in memory. NULL source_seq rows (v1 events) are
// excluded by the `> ?` comparison.
func (r *sqlODS) presentSeqSet(ctx context.Context, orgID, sourceID string, lo, hi int64) (map[int64]bool, error) {
	present := make(map[int64]bool)
	odsRows, err := r.db.QueryContext(ctx,
		"SELECT source_seq FROM usage_event_ods WHERE org_id = ? AND source_id = ? AND source_seq > ? AND source_seq <= ?",
		orgID, sourceID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("scan ods seqs org=%s source=%s: %w", orgID, sourceID, err)
	}
	for odsRows.Next() {
		var seq int64
		if err := odsRows.Scan(&seq); err != nil {
			odsRows.Close()
			return nil, err
		}
		present[seq] = true
	}
	odsRows.Close()
	if err := odsRows.Err(); err != nil {
		return nil, err
	}
	ledRows, err := r.db.QueryContext(ctx,
		"SELECT seq FROM usage_known_loss_ledger WHERE org_id = ? AND source_id = ? AND seq > ? AND seq <= ?",
		orgID, sourceID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("scan ledger seqs org=%s source=%s: %w", orgID, sourceID, err)
	}
	for ledRows.Next() {
		var seq int64
		if err := ledRows.Scan(&seq); err != nil {
			ledRows.Close()
			return nil, err
		}
		present[seq] = true
	}
	ledRows.Close()
	return present, ledRows.Err()
}

// EnumerateMissingSeqs — see ODSRepository. Returns seqs in (lo, hi] absent from
// BOTH ODS and the ledger (genuinely-unaccounted seqs eligible for known-loss
// promotion), bounded to advanceWatermarkScanLimit per call.
func (r *sqlODS) EnumerateMissingSeqs(ctx context.Context, orgID, sourceID string, lo, hi int64) ([]int64, error) {
	if hi <= lo {
		return nil, nil
	}
	if hi-lo > advanceWatermarkScanLimit {
		hi = lo + advanceWatermarkScanLimit
	}
	present, err := r.presentSeqSet(ctx, orgID, sourceID, lo, hi)
	if err != nil {
		return nil, err
	}
	var missing []int64
	for seq := lo + 1; seq <= hi; seq++ {
		if !present[seq] {
			missing = append(missing, seq)
		}
	}
	return missing, nil
}

// RecordKnownLoss — see ODSRepository. Appends seqs to the known-loss ledger
// (idempotent per (org, source, seq)) then re-advances the watermark so
// contiguous_seq zips past the now-accounted-for seqs (the ledger-aware zipper).
// Returns the new contiguous_seq. Per-seq INSERT loop is fine: known-loss is rare
// and the projector hands bounded enumerated sets.
func (r *sqlODS) RecordKnownLoss(ctx context.Context, orgID, sourceID string, seqs []int64, reason string) (int64, error) {
	insert := r.db.InsertOrIgnoreOn("usage_known_loss_ledger",
		"org_id, source_id, seq, reason", "?,?,?,?", "org_id, source_id, seq")
	for _, seq := range seqs {
		if _, err := r.db.ExecContext(ctx, insert, orgID, sourceID, seq, reason); err != nil {
			return 0, fmt.Errorf("record known loss org=%s source=%s seq=%d: %w", orgID, sourceID, seq, err)
		}
	}
	// Re-advance so contiguous moves past the ledgered seqs. clientAllocated=0 is
	// a no-op on that column (MAX guard); we're only here to zip contiguity.
	return r.AdvanceWatermark(ctx, orgID, sourceID, 0)
}

// ApplyStreamFloor — see ODSRepository.
//
// Advances contiguous_seq past the terminated span in the SAME statement that
// records the floor, so the two can never disagree: a reader that sees
// contiguous jump can always find the floor that authorised it.
func (r *sqlODS) ApplyStreamFloor(ctx context.Context, orgID, sourceID string, floor int64) (int64, bool, error) {
	if floor <= 0 {
		return 0, false, fmt.Errorf("stream floor must be positive, got %d", floor)
	}
	// The guard `stream_floor_seq < ?` is the idempotency: a replayed
	// declaration matches zero rows and changes nothing.
	res, err := r.db.ExecContext(ctx,
		`UPDATE usage_source_watermark
		    SET stream_floor_seq = ?,
		        contiguous_seq   = CASE WHEN contiguous_seq < ? THEN ? ELSE contiguous_seq END
		  WHERE org_id = ? AND source_id = ? AND stream_floor_seq < ?`,
		floor, floor, floor, orgID, sourceID, floor)
	if err != nil {
		return 0, false, fmt.Errorf("apply stream floor org=%s source=%s floor=%d: %w", orgID, sourceID, floor, err)
	}
	n, _ := res.RowsAffected()
	changed := n > 0
	if n == 0 {
		// Either already at/above this floor (idempotent replay), or the row
		// does not exist yet because this lane has never delivered an event.
		// The second case must still be recorded, or the first event to arrive
		// starts a watermark at zero and the terminated span reappears as a gap.
		if insRes, ierr := r.db.ExecContext(ctx, r.db.InsertOrIgnoreOn(
			"usage_source_watermark",
			"org_id, source_id, contiguous_seq, max_seen_seq, client_allocated_seq, stream_floor_seq",
			"?,?,?,?,?,?", "org_id, source_id"),
			orgID, sourceID, floor, 0, 0, floor); ierr != nil {
			return 0, false, fmt.Errorf("seed watermark for stream floor org=%s source=%s: %w", orgID, sourceID, ierr)
		} else if ins, _ := insRes.RowsAffected(); ins > 0 {
			// Seeding a lane that had never delivered IS a state change — the
			// audit WARN must fire for it. Reporting it as a replay (the first
			// version of this did, because `changed` only tracked the UPDATE)
			// would hide the very first switch of every new lane.
			changed = true
		}
	}
	var contiguous int64
	if err := r.db.QueryRowContext(ctx,
		"SELECT contiguous_seq FROM usage_source_watermark WHERE org_id = ? AND source_id = ?",
		orgID, sourceID).Scan(&contiguous); err != nil {
		return 0, false, fmt.Errorf("read watermark after stream floor: %w", err)
	}
	return contiguous, changed, nil
}

// GapSeqs — see ODSRepository. Returns the source's unaccounted seqs (absent from
// ODS and the ledger) in (contiguous, max(max_seen, client_allocated)], bounded
// to `limit`. truncated is true when the gap span exceeds the limit (client
// re-runs reconcile for the rest). Stage D3 (client-confirmed reconciliation).
func (r *sqlODS) GapSeqs(ctx context.Context, orgID, sourceID string, limit int64) (seqs []int64, truncated bool, err error) {
	var contiguous, maxSeen, clientAllocated int64
	werr := r.db.QueryRowContext(ctx,
		"SELECT contiguous_seq, max_seen_seq, client_allocated_seq FROM usage_source_watermark WHERE org_id = ? AND source_id = ?",
		orgID, sourceID,
	).Scan(&contiguous, &maxSeen, &clientAllocated)
	if werr == sql.ErrNoRows {
		return nil, false, nil // unknown source → no gaps
	}
	if werr != nil {
		return nil, false, fmt.Errorf("read watermark org=%s source=%s: %w", orgID, sourceID, werr)
	}
	hi := maxSeen
	if clientAllocated > hi {
		hi = clientAllocated
	}
	if limit <= 0 || limit > advanceWatermarkScanLimit {
		limit = advanceWatermarkScanLimit
	}
	if hi-contiguous > limit {
		hi = contiguous + limit
		truncated = true
	}
	missing, err := r.EnumerateMissingSeqs(ctx, orgID, sourceID, contiguous, hi)
	return missing, truncated, err
}

// ConfirmLost — see ODSRepository. The client has declared `seqs` unrecoverable
// (not in its WAL). We GUARD against marking received seqs lost: only seqs
// genuinely absent from BOTH ODS and the ledger are ledgered (reason). Returns
// the count actually promoted + the new contiguous. Stage D3.
func (r *sqlODS) ConfirmLost(ctx context.Context, orgID, sourceID string, seqs []int64, reason string) (promoted int, contiguous int64, err error) {
	if len(seqs) == 0 {
		c, e := r.AdvanceWatermark(ctx, orgID, sourceID, 0)
		return 0, c, e
	}
	lo, hi := seqs[0], seqs[0]
	for _, s := range seqs {
		if s < lo {
			lo = s
		}
		if s > hi {
			hi = s
		}
	}
	// Upper bound: a seq the client never allocated and the server never saw is
	// NOT a real loss — reject it so a buggy/malicious client can't inflate
	// known_loss (and advance contiguous) with fabricated above-allocated seqs.
	// max(max_seen, client_allocated) is the same ceiling GapSeqs enumerates to.
	var maxSeen, clientAllocated int64
	if werr := r.db.QueryRowContext(ctx,
		"SELECT max_seen_seq, client_allocated_seq FROM usage_source_watermark WHERE org_id = ? AND source_id = ?",
		orgID, sourceID,
	).Scan(&maxSeen, &clientAllocated); werr != nil && werr != sql.ErrNoRows {
		return 0, 0, fmt.Errorf("read watermark org=%s source=%s: %w", orgID, sourceID, werr)
	}
	ceiling := maxSeen
	if clientAllocated > ceiling {
		ceiling = clientAllocated
	}
	// present in [lo, hi] = (lo-1, hi]; a seq already received or ledgered is NOT lost.
	present, err := r.presentSeqSet(ctx, orgID, sourceID, lo-1, hi)
	if err != nil {
		return 0, 0, err
	}
	absent := make([]int64, 0, len(seqs))
	for _, s := range seqs {
		if s > 0 && s <= ceiling && !present[s] {
			absent = append(absent, s)
		}
	}
	if len(absent) == 0 {
		c, e := r.AdvanceWatermark(ctx, orgID, sourceID, 0)
		return 0, c, e
	}
	c, err := r.RecordKnownLoss(ctx, orgID, sourceID, absent, reason)
	return len(absent), c, err
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullTime kept for backward compatibility with any callers that still bind
// *time.Time via sql.NullTime. New code goes through shared.DB.BindMillis.
// Unused placeholder import below ensures aikeytime is wired in.
var _ = aikeytime.Millis(0)

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func jsonbOrNull(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
