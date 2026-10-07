// models_state_test.go — the JSON view must not publish timestamps it never had.
//
// Rule under test (user-reported): before the first successful apply,
// `models/state` answered `applied_at: "0001-01-01T00:00:00Z"` — the Go zero
// time formatted as if it were a real moment. The HTML view already guarded the
// same field with `!appliedAt.IsZero()`; the JSON view did not, so a consumer
// (dsh-cliproxyapi-status) rendered a bogus clock instead of "—".
//
// The field has to be ABSENT rather than zero-valued, because "no apply yet" and
// "applied at year 1" are not distinguishable once the string is written.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// countdownState is the smallest modelsState that resolveModels accepts: one
// model, cheap, so the payload carries a real `kept` row.
type countdownState struct {
	GeneratedAt string `json:"generated_at"`
	Protected   string `json:"protected"`
	Models      []struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Credits *float64 `json:"credits"`
	} `json:"models"`
}

func writeState(t *testing.T, dir string, credits float64) string {
	t.Helper()
	state := countdownState{GeneratedAt: "2026-09-29T04:58:27Z", Protected: "keep-me"}
	state.Models = append(state.Models, struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Credits *float64 `json:"credits"`
	}{ID: "model-a", Name: "Model A", Credits: &credits})
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "models-state.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// resetModelsCache clears the process-global apply bookkeeping the payload reads.
func resetModelsCache(t *testing.T) {
	t.Helper()
	modelsCache.mu.Lock()
	previousApply, previousError, previousAt := modelsCache.lastApply, modelsCache.lastError, modelsCache.appliedAt
	modelsCache.lastApply, modelsCache.lastError, modelsCache.appliedAt = "", "", time.Time{}
	modelsCache.mu.Unlock()
	t.Cleanup(func() {
		modelsCache.mu.Lock()
		modelsCache.lastApply, modelsCache.lastError, modelsCache.appliedAt = previousApply, previousError, previousAt
		modelsCache.mu.Unlock()
	})
}

// payloadField pulls one key out of the payload map as a string.
func payloadField(t *testing.T, payload any, key string) (string, bool) {
	t.Helper()
	m, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want map[string]any", payload)
	}
	value, present := m[key]
	if !present {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("%s is %T, want string", key, value)
	}
	return text, true
}

// Before the first apply the payload must omit `applied_at` entirely, so no
// consumer can mistake the zero time for a real moment.
func TestModelsStateOmitsAppliedAtBeforeFirstApply(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{
		ModelsStatePath: writeState(t, dir, 0.05),
		SelectionPath:   filepath.Join(dir, "models-selection.json"),
		DSHPatchPath:    filepath.Join(dir, "cordis.patch.yml"),
	}
	resetModelsCache(t)

	payload := modelsStatePayload(cfg)

	appliedAt, present := payloadField(t, payload, "applied_at")
	if present && appliedAt != "" {
		t.Fatalf("applied_at = %q before any apply, want absent or empty", appliedAt)
	}
	if appliedAt == "0001-01-01T00:00:00Z" {
		t.Fatal("zero time leaked into applied_at; a consumer cannot tell it from a real apply")
	}
}

// After a real apply the field must carry that moment, so the guard above cannot
// have been written by simply deleting the key.
func TestModelsStateReportsAppliedAtAfterApply(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{
		ModelsStatePath: writeState(t, dir, 0.05),
		SelectionPath:   filepath.Join(dir, "models-selection.json"),
		DSHPatchPath:    filepath.Join(dir, "cordis.patch.yml"),
	}
	resetModelsCache(t)

	applied := time.Date(2026, 9, 29, 12, 58, 37, 0, time.Local)
	modelsCache.mu.Lock()
	modelsCache.appliedAt = applied
	modelsCache.lastApply = "wrote 3 models"
	modelsCache.lastError = ""
	modelsCache.mu.Unlock()

	payload := modelsStatePayload(cfg)
	appliedAt, present := payloadField(t, payload, "applied_at")
	if !present {
		t.Fatal("applied_at missing after a real apply")
	}
	if appliedAt != applied.Format(time.RFC3339) {
		t.Fatalf("applied_at = %q, want %q", appliedAt, applied.Format(time.RFC3339))
	}
}
