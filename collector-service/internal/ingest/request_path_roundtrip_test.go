package ingest

import (
	"encoding/json"
	"testing"
)

// TestUsageEvent_RequestPathRoundTrip pins the 2026-07-15 lesson learned in
// live E2E: usage_event_ods.raw_event_json is json.Marshal(UsageEvent) — NOT
// the verbatim wire bytes — so any additive wire field that isn't declared on
// this struct is silently dropped before storage. The proxy started sending
// `request_path` and the first live run stored raw events WITHOUT it because
// the field was missing here. This round-trip test fails if the field (or a
// future rename) breaks the wire→struct→raw_event_json chain again.
func TestUsageEvent_RequestPathRoundTrip(t *testing.T) {
	wire := []byte(`{"event_id":"evt-rp","org_id":"org1","event_time":1783928037595,` +
		`"occurred_at":1783928037595,"request_status":"error","schema_version":1,` +
		`"request_path":"/openai/v1/models"}`)
	var e UsageEvent
	if err := json.Unmarshal(wire, &e); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if e.RequestPath != "/openai/v1/models" {
		t.Fatalf("RequestPath = %q, want /openai/v1/models", e.RequestPath)
	}
	stored, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal for raw_event_json: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(stored, &back); err != nil {
		t.Fatal(err)
	}
	if got, _ := back["request_path"].(string); got != "/openai/v1/models" {
		t.Errorf("raw_event_json dropped request_path: %s", stored)
	}
}
