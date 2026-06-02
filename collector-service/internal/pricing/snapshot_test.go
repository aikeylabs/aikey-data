package pricing

import "testing"

// TestBuildSnapshot_Stable: identical input must yield an identical, well-formed
// snapshot_id (so an unchanged release reuses the same pricing_snapshots row).
func TestBuildSnapshot_Stable(t *testing.T) {
	ll := []byte(`{"claude-3-5-sonnet":{"input":1.5e-6}}`)
	hi := []byte("history: []\n")
	ov := []byte("overrides: []\n")

	a := BuildSnapshot(ll, hi, ov)
	b := BuildSnapshot(ll, hi, ov)

	if a.SnapshotID != b.SnapshotID {
		t.Fatalf("same input must yield same snapshot_id: %q != %q", a.SnapshotID, b.SnapshotID)
	}
	if len(a.SnapshotID) != 16 {
		t.Fatalf("snapshot_id must be 16 hex chars, got %d: %q", len(a.SnapshotID), a.SnapshotID)
	}
	if len(a.LiteLLMSHA256) != 64 || len(a.HistorySHA256) != 64 || len(a.OverridesSHA256) != 64 {
		t.Fatalf("per-file sha256 must each be 64 hex chars, got %d/%d/%d",
			len(a.LiteLLMSHA256), len(a.HistorySHA256), len(a.OverridesSHA256))
	}
}

// TestBuildSnapshot_Sensitive: a one-byte change in ANY of the three files must
// change the snapshot_id (otherwise the audit fingerprint would mask edits).
func TestBuildSnapshot_Sensitive(t *testing.T) {
	base := BuildSnapshot([]byte("a"), []byte("b"), []byte("c"))

	cases := map[string]Snapshot{
		"litellm changed":   BuildSnapshot([]byte("a!"), []byte("b"), []byte("c")),
		"history changed":   BuildSnapshot([]byte("a"), []byte("b!"), []byte("c")),
		"overrides changed": BuildSnapshot([]byte("a"), []byte("b"), []byte("c!")),
	}
	for name, got := range cases {
		if got.SnapshotID == base.SnapshotID {
			t.Errorf("%s: snapshot_id must change when a file changes (both %q)", name, got.SnapshotID)
		}
	}
}

// TestBuildSnapshot_BoundarySep guards the NUL separator: without it,
// ("ab","c",..) and ("a","bc",..) would hash to the same stream and collide.
func TestBuildSnapshot_BoundarySep(t *testing.T) {
	x := BuildSnapshot([]byte("ab"), []byte("c"), []byte("d"))
	y := BuildSnapshot([]byte("a"), []byte("bc"), []byte("d"))
	if x.SnapshotID == y.SnapshotID {
		t.Fatalf("boundary collision: NUL separator failed to distinguish ('ab','c') from ('a','bc')")
	}
}
