// efforts_test.go — the DSH region must carry the reasoning levels the catalog
// reports, and nothing when it reports none.
//
// Rule under test (user-reported): the WorkBuddy models had no reasoning-level
// selector in dsh. The cause is on the dsh side — pi-ai resolves an absent
// `reasoningEfforts` from its installed catalog, which describes none of these
// ids, so every one of them resolved as non-reasoning. The levels therefore have
// to be written into the generated region.
//
// The other half of the rule matters as much: a model with no selectable level
// must get NO `reasoningEfforts` key. `hy3` and `deepseek-v4.1-flash` report a
// fixed `effort: high` and ignore every level sent to them, so advertising
// low/medium/high would promise a choice the upstream discards.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// entryModel builds the smallest resolvedModel that renderModelBlock accepts.
func entryModel(id string, efforts []string) resolvedModel {
	return resolvedModel{Entry: modelEntry{ID: id, Name: id, Efforts: efforts}}
}

// blockFor renders one model's lines as a single string.
func blockFor(id string, efforts []string) string {
	return strings.Join(renderModelBlock([]resolvedModel{entryModel(id, efforts)}, "  "), "\n")
}

// TestRenderEffortsWritesEveryDeclaredLevel pins the mapping the pi-ai schema
// reads: each key is an offered level and its value is the wire spelling sent
// with the request.
func TestRenderEffortsWritesEveryDeclaredLevel(t *testing.T) {
	block := blockFor("glm-5.3-flash", []string{"low", "high", "max"})
	for _, want := range []string{
		"  reasoningEfforts:",
		"    low: low",
		"    high: high",
		"    max: max",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block is missing %q\n%s", want, block)
		}
	}
	// The catalog reports these three and no others; a level it never listed
	// must not appear, because declaring it promises dispatch will send it.
	for _, unwanted := range []string{"medium", "minimal", "xhigh"} {
		if strings.Contains(block, unwanted) {
			t.Errorf("block declares undeclared level %q\n%s", unwanted, block)
		}
	}
}

// TestRenderEffortsOmittedWhenModelOffersNoLevel is the half that keeps a fixed
// -effort model honest: absent means dsh shows no selector instead of a
// selector whose values are silently ignored.
func TestRenderEffortsOmittedWhenModelOffersNoLevel(t *testing.T) {
	for _, efforts := range [][]string{nil, {}} {
		block := blockFor("hy3", efforts)
		if strings.Contains(block, "reasoningEfforts") {
			t.Errorf("efforts %#v produced a reasoningEfforts key\n%s", efforts, block)
		}
	}

	// A single fixed level is still declared: it is the one true choice.
	block := blockFor("hy3", []string{"high"})
	if !strings.Contains(block, "  reasoningEfforts:") || !strings.Contains(block, "    high: high") {
		t.Errorf("a fixed-effort model must declare its one level\n%s", block)
	}
}

// TestRenderEffortsSitsBetweenMaxTokensAndInput keeps the key ordering stable:
// the region is regenerated wholesale on every apply, so an unstable order would
// churn the user's patch file.
func TestRenderEffortsSitsBetweenMaxTokensAndInput(t *testing.T) {
	model := entryModel("glm-5.3-flash", []string{"low", "high"})
	model.Entry.ContextWindow = 1_000_000
	model.Entry.MaxTokens = 131_072
	block := strings.Join(renderModelBlock([]resolvedModel{model}, "  "), "\n")
	maxTokens := strings.Index(block, "maxTokens:")
	efforts := strings.Index(block, "reasoningEfforts:")
	input := strings.Index(block, "  input:")
	if maxTokens < 0 || efforts < 0 || input < 0 {
		t.Fatalf("expected all three keys\n%s", block)
	}
	if !(maxTokens < efforts && efforts < input) {
		t.Errorf("want maxTokens < reasoningEfforts < input, got %d/%d/%d\n%s", maxTokens, efforts, input, block)
	}
}

// TestRenderEffortsIndentFollowsRegion guards the marker-region indent, which
// the writer derives from the existing start-marker line rather than a constant.
func TestRenderEffortsIndentFollowsRegion(t *testing.T) {
	lines := renderModelBlock([]resolvedModel{entryModel("glm-5.3-flash", []string{"low"})}, "        ")
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "\n          reasoningEfforts:") {
		t.Errorf("reasoningEfforts did not follow the region indent\n%s", joined)
	}
	if !strings.Contains(joined, "\n            low: low") {
		t.Errorf("level did not follow the region indent\n%s", joined)
	}
}

// TestModelEntryCarriesEffortsFromSnapshot is the data path: the snapshot the
// Python collector writes is what feeds the renderer, so a field-name drift here
// would silently empty every selector.
func TestModelEntryCarriesEffortsFromSnapshot(t *testing.T) {
	raw := []byte(`{"models":[{"id":"glm-5.3-flash","efforts":["low","high","max"]},
		{"id":"hy3"}]}`)
	var state modelsState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if got := state.Models[0].Efforts; len(got) != 3 || got[0] != "low" || got[2] != "max" {
		t.Errorf("first model efforts = %#v", got)
	}
	if got := state.Models[1].Efforts; len(got) != 0 {
		t.Errorf("a model without the key must decode to no efforts, got %#v", got)
	}
}
