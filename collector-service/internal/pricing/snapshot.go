package pricing

import (
	"crypto/sha256"
	"encoding/hex"
)

// Snapshot is a content-addressed fingerprint of the three pricing source files
// that were loaded into the resolver. Every projected event records the
// SnapshotID, so the exact global pricing state at compute time stays provable
// even after the source files are edited or upgraded. See design §3.7.
//
// This struct holds only the content hashes. Runtime/registry fields
// (aikey_version, created_at, effective_from/until, notes) are stamped by the
// projector when it UPSERTs into the pricing_snapshots table — they are not the
// pricing package's concern.
type Snapshot struct {
	// SnapshotID is the first 16 hex chars of sha256 over the three file bodies
	// joined by a NUL separator. Identical content -> identical id (so unchanged
	// releases share a snapshot row); any byte change -> new id.
	SnapshotID string `json:"snapshot_id"`

	// Per-file full sha256 (64 hex chars). These let an auditor map the snapshot
	// back to exact file versions in git history.
	LiteLLMSHA256   string `json:"litellm_sha256"`
	HistorySHA256   string `json:"history_sha256"`
	OverridesSHA256 string `json:"overrides_sha256"`
}

// snapshotSep separates the three file bodies before computing the combined
// digest. A fixed NUL byte prevents boundary collisions: without it, the inputs
// ("ab","c") and ("a","bc") would concatenate to the same byte stream and hash
// identically, falsely reporting two different pricing states as one.
var snapshotSep = []byte{0x00}

// BuildSnapshot computes the per-file sha256 hashes and the combined SnapshotID
// from the raw bytes of the three pricing sources.
//
// It deliberately does NOT normalize, sort, or reformat the bytes: a formatting
// difference is itself a content change and must yield a different SnapshotID,
// otherwise the audit fingerprint would mask real edits (design §2.3).
func BuildSnapshot(litellm, history, overrides []byte) Snapshot {
	ll := sha256.Sum256(litellm)
	hi := sha256.Sum256(history)
	ov := sha256.Sum256(overrides)

	combined := sha256.New()
	combined.Write(litellm)
	combined.Write(snapshotSep)
	combined.Write(history)
	combined.Write(snapshotSep)
	combined.Write(overrides)

	return Snapshot{
		SnapshotID:      hex.EncodeToString(combined.Sum(nil))[:16],
		LiteLLMSHA256:   hex.EncodeToString(ll[:]),
		HistorySHA256:   hex.EncodeToString(hi[:]),
		OverridesSHA256: hex.EncodeToString(ov[:]),
	}
}
