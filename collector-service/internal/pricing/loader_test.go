package pricing

import "testing"

// Load builds a working resolver from the embedded files and the litellm subset
// resolves to the expected rates.
func TestLoad_EmbeddedFiles(t *testing.T) {
	r, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if r.Snapshot().SnapshotID == "" {
		t.Error("snapshot_id must be set after Load")
	}

	// claude opus from the embedded litellm subset, with full cache rates.
	up, err := r.Lookup("anthropic", "claude-opus-4-20250514", 1, 100, "")
	if err != nil {
		t.Fatalf("expected claude-opus hit: %v", err)
	}
	if up.InputPerToken != 0.000015 || up.OutputPerToken != 0.000075 {
		t.Errorf("opus rates wrong: in=%v out=%v", up.InputPerToken, up.OutputPerToken)
	}
	if up.CacheReadInputPerToken != 0.0000015 || up.CacheCreationInputPerToken != 0.00001875 {
		t.Errorf("opus cache rates wrong: read=%v creation=%v", up.CacheReadInputPerToken, up.CacheCreationInputPerToken)
	}
	if up.Source != SourceLiteLLM {
		t.Errorf("source = %q, want litellm", up.Source)
	}
}

// reasoning rate defaults to the output rate when LiteLLM has no separate field.
func TestLoad_ReasoningDefaultsToOutput(t *testing.T) {
	r, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	up, err := r.Lookup("openai", "o3", 1, 100, "")
	if err != nil {
		t.Fatalf("expected o3 hit: %v", err)
	}
	if up.ReasoningPerToken != up.OutputPerToken {
		t.Errorf("reasoning %v must default to output %v", up.ReasoningPerToken, up.OutputPerToken)
	}
	if up.ReasoningPerToken != 0.000008 {
		t.Errorf("o3 reasoning = %v, want 0.000008 (=output)", up.ReasoningPerToken)
	}
}

// the _meta block (no litellm_provider) must NOT become a queryable model.
func TestLoad_MetaSkipped(t *testing.T) {
	r, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup("", "_meta", 1, 1, ""); err != ErrUnknownModel {
		t.Errorf("_meta must be skipped, got err=%v", err)
	}
}

// malformed litellm JSON must fail (fail-fast), not silently yield empty table.
func TestLoad_FailFastOnBadJSON(t *testing.T) {
	_, err := LoadFrom([]byte("{ not json"), historyJSON, overridesJSON)
	if err == nil {
		t.Fatal("expected parse error on malformed litellm JSON, got nil")
	}
}

// empty overlays (the shipped skeletons) parse fine and contribute nothing.
func TestLoad_EmptyOverlaysOK(t *testing.T) {
	r, err := LoadFrom(litellmJSON, []byte(`{"schema_version":1,"entries":[]}`), []byte(`{"schema_version":1,"entries":[]}`))
	if err != nil {
		t.Fatalf("empty overlays should load: %v", err)
	}
	// override lookup falls through to litellm since overrides are empty.
	up, err := r.Lookup("anthropic", "claude-opus-4-20250514", 1, 1, "org_anything")
	if err != nil || up.Source != SourceLiteLLM {
		t.Errorf("empty override must fall through to litellm; src=%v err=%v", up.Source, err)
	}
}
