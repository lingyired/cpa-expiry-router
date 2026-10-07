// models.go — the plugin's second job: own the model list DSH shows.
//
// The collector (wb-models.py) publishes a catalogue snapshot:
//
//	models-state.json  { models: [ {id, name, display, credits, badges,
//	                                context_window, max_tokens, vision,
//	                                auto_kept, alive, probe_error}, ... ] }
//
// This file turns that catalogue plus a user selection into the DSH provider
// model list, by rewriting the marked region of the DSH profile patch:
//
//	# >>> workbuddy-models:start
//	- id: glm-5.3-flash
//	  name: "GLM-5.3-Flash · 0.06x"
//	  …
//	# <<< workbuddy-models:end
//
// Selection is one of:
//
//	auto   — follow the snapshot's threshold result (`auto_kept && alive`)
//	manual — exactly the ids the user ticked in the panel (dead ones are still
//	         refused, and the model DSH has selected as its default is always
//	         kept so the picker can never point at a removed entry)
//
// Everything is written atomically and DSH hot-reloads the file, so ticking a
// box and pressing save takes effect immediately.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	modelsPanelPath = "/models"
	// Same markers the Python writer uses, so both tools own one region.
	modelsStartMark = "# >>> workbuddy-models:start"
	modelsEndMark   = "# <<< workbuddy-models:end"
)

// ---------------------------------------------------------------------------
// snapshot + selection types
// ---------------------------------------------------------------------------

type modelEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Display       string   `json:"display"`
	Credits       *float64 `json:"credits"`
	Badges        []string `json:"badges"`
	ContextWindow int64    `json:"context_window"`
	MaxTokens     int64    `json:"max_tokens"`
	Vision        bool     `json:"vision"`
	Tools         bool     `json:"tools"`
	// The reasoning levels this model offers, in the catalog's own order; nil
	// when it offers no selectable level. Written verbatim into the DSH region
	// as `reasoningEfforts`, whose keys pi-ai reads as the offered levels.
	Efforts       []string `json:"efforts"`
	AutoKept      bool     `json:"auto_kept"`
	Probed        bool     `json:"probed"`
	Alive         bool     `json:"alive"`
	ProbeError    string   `json:"probe_error"`
	DroppedReason string   `json:"dropped_reason"`
}

type modelsState struct {
	GeneratedAt string       `json:"generated_at"`
	Provider    string       `json:"provider"`
	Platform    string       `json:"platform"`
	Accounts    []string     `json:"accounts"`
	MaxCredit   *float64     `json:"max_credit"`
	MinCredit   *float64     `json:"min_credit"`
	Probe       bool         `json:"probe"`
	Protected   string       `json:"protected"`
	Models      []modelEntry `json:"models"`
}

type selection struct {
	Mode string   `json:"mode"` // auto | manual
	IDs  []string `json:"ids"`
	// MaxCredit overrides the snapshot's threshold for auto mode (percent-free
	// multiplier bound, e.g. 0.15 = "cheaper than 0.15x").
	MaxCredit *float64 `json:"max_credit,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

type modelsSnapshotCache struct {
	mu        sync.Mutex
	path      string
	modTime   time.Time
	size      int64
	state     *modelsState
	selection *selection
	lastApply string
	lastError string
	appliedAt time.Time
}

var modelsCache modelsSnapshotCache

func (c *modelsSnapshotCache) loadModels(path string) *modelsState {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, err := os.Stat(path)
	if err != nil {
		return c.state
	}
	if c.state != nil && info.ModTime().Equal(c.modTime) && info.Size() == c.size {
		return c.state
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c.state
	}
	parsed := &modelsState{}
	if err := json.Unmarshal(raw, parsed); err != nil {
		c.lastError = "models-state.json parse: " + err.Error()
		return c.state
	}
	c.state = parsed
	c.modTime = info.ModTime()
	c.size = info.Size()
	return c.state
}

func (c *modelsSnapshotCache) loadSelection(path string) *selection {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadSelectionLocked(path)
}

func (c *modelsSnapshotCache) loadSelectionLocked(path string) *selection {
	raw, err := os.ReadFile(path)
	if err != nil {
		c.selection = nil
		return nil
	}
	parsed := &selection{}
	if err := json.Unmarshal(raw, parsed); err != nil {
		c.lastError = "models-selection.json parse: " + err.Error()
		c.selection = nil
		return nil
	}
	if parsed.Mode == "" {
		parsed.Mode = "manual"
	}
	c.selection = parsed
	return parsed
}

func (c *modelsSnapshotCache) saveSelection(path string, next selection) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next.UpdatedAt = time.Now().Format(time.RFC3339)
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	c.selection = &next
	return nil
}

func (c *modelsSnapshotCache) clearSelection(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	c.selection = nil
	return nil
}

// ---------------------------------------------------------------------------
// choosing what to write
// ---------------------------------------------------------------------------

type resolvedModel struct {
	Entry   modelEntry
	Display string
	Reason  string
}

func effectiveThreshold(state *modelsState, sel *selection) *float64 {
	if sel != nil && sel.MaxCredit != nil {
		return sel.MaxCredit
	}
	if state != nil {
		return state.MaxCredit
	}
	return nil
}

func resolveModels(state *modelsState, sel *selection, protected string) (kept, dropped []resolvedModel) {
	if state == nil {
		return nil, nil
	}
	manual := map[string]bool{}
	mode := "auto"
	if sel != nil && strings.EqualFold(sel.Mode, "manual") {
		mode = "manual"
		for _, id := range sel.IDs {
			manual[strings.TrimSpace(id)] = true
		}
	}
	threshold := effectiveThreshold(state, sel)
	displays := disambiguate(state.Models)
	for _, model := range state.Models {
		display := displays[model.ID]
		selected := false
		reason := ""
		switch mode {
		case "manual":
			selected = manual[model.ID]
			if selected {
				reason = "手动勾选"
			} else {
				reason = "未勾选"
			}
		default:
			// The plugin owns the threshold maths (the panel can change it),
			// instead of trusting the snapshot's auto_kept flag.
			switch {
			case model.Credits == nil:
				reason = "倍率浮动（未知）"
			case threshold != nil && *model.Credits >= *threshold:
				reason = fmt.Sprintf("倍率 %.2fx ≥ %.2fx", *model.Credits, *threshold)
			default:
				selected = true
				reason = fmt.Sprintf("倍率 %.2fx < 阈值", *model.Credits)
			}
		}
		if model.ID == protected && protected != "" {
			selected = true
			reason = "DSH 默认模型（受保护）"
		}
		// A model the probe actually rejected is never written; one that was
		// never probed is allowed when explicitly selected.
		if selected && model.Probed && !model.Alive {
			model.ProbeError = firstNonEmpty(model.ProbeError, "探活未通过")
			dropped = append(dropped, resolvedModel{Entry: model, Display: display, Reason: "探活失败：" + truncate(model.ProbeError, 80)})
			continue
		}
		if selected {
			kept = append(kept, resolvedModel{Entry: model, Display: display, Reason: reason})
			continue
		}
		dropped = append(dropped, resolvedModel{Entry: model, Display: display, Reason: reason})
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i].Entry.Credits, kept[j].Entry.Credits
		switch {
		case a == nil && b == nil:
			return kept[i].Entry.ID < kept[j].Entry.ID
		case a == nil:
			return false
		case b == nil:
			return true
		case *a != *b:
			return *a < *b
		default:
			return kept[i].Entry.ID < kept[j].Entry.ID
		}
	})
	return kept, dropped
}

// disambiguate appends the id when two models share the same upstream name
// (hy4-preview vs hy4-preview-f are both "Hy4 preview").
func disambiguate(models []modelEntry) map[string]string {
	counts := map[string]int{}
	for _, model := range models {
		counts[model.Name]++
	}
	out := make(map[string]string, len(models))
	for _, model := range models {
		base := firstNonEmpty(model.Display, model.Name, model.ID)
		// The collector may already have disambiguated ("Hy4 preview (hy4-preview-f)")
		// — only add the id when it is still missing.
		if counts[model.Name] > 1 && !strings.Contains(base, "("+model.ID+")") {
			base = fmt.Sprintf("%s (%s)", base, model.ID)
		}
		out[model.ID] = base
	}
	return out
}

func modelDisplayName(resolved resolvedModel) string {
	bits := []string{resolved.Display}
	if resolved.Entry.Credits != nil {
		bits = append(bits, fmt.Sprintf("%.2fx", *resolved.Entry.Credits))
	} else {
		bits = append(bits, "倍率浮动")
	}
	bits = append(bits, resolved.Entry.Badges...)
	return strings.Join(bits, " · ")
}

// ---------------------------------------------------------------------------
// DSH patch writer
// ---------------------------------------------------------------------------

var (
	dshModelLine = regexp.MustCompile(`(?m)^\s*model:\s*(\S+)\s*$`)
	regionMu     sync.Mutex
)

// dshDefaultModel reads the model DSH currently has selected, so it is never
// filtered out of the list it points at.
func dshDefaultModel(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(raw)
	index := strings.Index(text, "- id: agent-default-model")
	if index < 0 {
		return ""
	}
	block := text[index:]
	if len(block) > 600 {
		block = block[:600]
	}
	if match := dshModelLine.FindStringSubmatch(block); len(match) == 2 {
		return strings.Trim(match[1], `"'`)
	}
	return ""
}

func renderModelBlock(models []resolvedModel, indent string) []string {
	// The operator-configured prefix applies to the DSH region only; the
	// panel's own labels call modelDisplayName directly and stay unprefixed.
	prefix := loadedConfig().dshModelPrefix()
	lines := []string{}
	for _, model := range models {
		lines = append(lines, fmt.Sprintf("%s- id: %s", indent, model.Entry.ID))
		lines = append(lines, fmt.Sprintf("%s  name: %s", indent, yamlQuote(prefix+modelDisplayName(model))))
		if model.Entry.ContextWindow > 0 {
			lines = append(lines, fmt.Sprintf("%s  contextWindow: %d", indent, model.Entry.ContextWindow))
		}
		if model.Entry.MaxTokens > 0 {
			lines = append(lines, fmt.Sprintf("%s  maxTokens: %d", indent, model.Entry.MaxTokens))
		}
		lines = append(lines, renderEfforts(model.Entry.Efforts, indent)...)
		lines = append(lines, indent+"  input:")
		lines = append(lines, indent+"    - text")
		if model.Entry.Vision {
			lines = append(lines, indent+"    - image")
		}
	}
	return lines
}

// renderEfforts renders one model's `reasoningEfforts:` mapping, or nothing when
// the model offers no selectable level.
//
// dsh's pi-ai route resolves an absent `reasoningEfforts` from pi-ai's installed
// catalog, and the catalog describes none of these ids, so such a model lands as
// non-reasoning and its effort selector never appears. Declaring the levels is
// what turns the selector on. Each level doubles as the wire spelling the
// request carries, and these ids take the level verbatim, so a level maps to
// itself.
func renderEfforts(levels []string, indent string) []string {
	if len(levels) == 0 {
		return nil
	}
	lines := []string{indent + "  reasoningEfforts:"}
	for _, level := range levels {
		lines = append(lines, fmt.Sprintf("%s    %s: %s", indent, level, level))
	}
	return lines
}

func yamlQuote(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// writeDSHModels replaces the marked region (recreating the markers when the
// DSH settings UI has stripped them, which it does on every save).
func writeDSHModels(path string, models []resolvedModel) (string, error) {
	regionMu.Lock()
	defer regionMu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	start, end := -1, -1
	for index, line := range lines {
		if strings.Contains(line, modelsStartMark) {
			start = index
		} else if strings.Contains(line, modelsEndMark) {
			end = index
			break
		}
	}

	var updated []string
	switch {
	case start >= 0 && end > start:
		indent := lines[start][:len(lines[start])-len(strings.TrimLeft(lines[start], " "))]
		updated = append(updated, lines[:start+1]...)
		updated = append(updated, renderModelBlock(models, indent)...)
		updated = append(updated, lines[end:]...)
	case start >= 0 && end < 0:
		return "", fmt.Errorf("start marker without end marker in %s", path)
	default:
		// No markers: locate the cliproxyapi provider's models list.
		provider, modelsKey := -1, -1
		for index, line := range lines {
			if strings.TrimSpace(line) == "cliproxyapi:" {
				provider = index
			} else if provider >= 0 && modelsKey < 0 && strings.TrimSpace(line) == "models:" {
				modelsKey = index
				break
			}
		}
		if modelsKey < 0 || provider+1 >= len(lines) {
			return "", fmt.Errorf("no `cliproxyapi:` provider with a `models:` list in %s", path)
		}
		keyIndent := len(lines[provider+1]) - len(strings.TrimLeft(lines[provider+1], " "))
		bodyEnd := len(lines)
		for index := modelsKey + 1; index < len(lines); index++ {
			if strings.TrimSpace(lines[index]) == "" {
				continue
			}
			if len(lines[index])-len(strings.TrimLeft(lines[index], " ")) <= keyIndent {
				bodyEnd = index
				break
			}
		}
		indent := lines[modelsKey][:len(lines[modelsKey])-len(strings.TrimLeft(lines[modelsKey], " "))] + "  "
		updated = append(updated, lines[:modelsKey+1]...)
		updated = append(updated, indent+modelsStartMark)
		updated = append(updated, renderModelBlock(models, indent)...)
		updated = append(updated, indent+modelsEndMark)
		updated = append(updated, lines[bodyEnd:]...)
	}

	tmp := path + ".tmp-expiryrouter"
	if err := os.WriteFile(tmp, []byte(strings.Join(updated, "\n")), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d models", len(models)), nil
}

// applyModels is the single entry point: read snapshot + selection, decide,
// write the DSH region. Called on save and whenever the snapshot changes.
func applyModels(cfg pluginConfig) (kept, dropped []resolvedModel, note string, err error) {
	if !cfg.manageDSH() {
		return nil, nil, "manage_dsh 已关闭：插件不写 DSH", nil
	}
	state := modelsCache.loadModels(cfg.modelsStatePath())
	if state == nil || len(state.Models) == 0 {
		return nil, nil, "还没有模型快照（models-state.json）—— 先运行 wb-models --emit-state", nil
	}
	sel := modelsCache.loadSelection(cfg.selectionPath())
	protected := firstNonEmpty(dshDefaultModel(cfg.dshPatchPath()), state.Protected)
	kept, dropped = resolveModels(state, sel, protected)
	if len(kept) == 0 {
		return kept, dropped, "", fmt.Errorf("选择结果为空，拒绝写入（避免把 DSH 模型列表清空）")
	}
	note, err = writeDSHModels(cfg.dshPatchPath(), kept)
	modelsCache.mu.Lock()
	modelsCache.lastApply = note
	modelsCache.lastError = ""
	modelsCache.appliedAt = time.Now()
	modelsCache.mu.Unlock()
	return kept, dropped, note, err
}

// modelApplierLoop keeps DSH in sync when the collector publishes a new
// snapshot, without waiting for a panel visit.
func modelApplierLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	lastStamp := ""
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			cfg := loadedConfig()
			if !cfg.manageDSH() {
				continue
			}
			state := modelsCache.loadModels(cfg.modelsStatePath())
			if state == nil {
				continue
			}
			stamp := fmt.Sprintf("%s|%v", state.GeneratedAt, cfg.selectionPath())
			if stamp == lastStamp {
				continue
			}
			if _, _, note, err := applyModels(cfg); err != nil {
				pickLogger.Printf("%s: models apply failed: %v", pluginName, err)
			} else if note != "" {
				pickLogger.Printf("%s: %s", pluginName, note)
				lastStamp = stamp
			}
		}
	}
}

// ---------------------------------------------------------------------------
// panel page + form handling
// ---------------------------------------------------------------------------

type modelsView struct {
	Plugin      string
	Platform    string
	GeneratedAt string
	MaxCredit   string
	Threshold   string
	Probe       bool
	Mode        string
	StatePath   string
	PatchPath   string
	Rows        []modelsRow
	Kept        []modelsRow
	Dropped     []modelsRow
	KeptCount   int
	DropCount   int
	Note        string
	Error       string
	AppliedAt   string
	Protected   string
}

type modelsRow struct {
	ID              string
	Label           string
	Credits         string
	CreditsValue    string
	Below           bool
	Cheap           bool
	Checked         bool
	Alive           bool
	ProbeError      string
	ProbeFailed     bool
	Reason          string
	ContextWindow   int64
	MaxTokens       int64
	Vision          bool
	Badges          string
	ProbeErrorShort string
}

var modelsTemplate = template.Must(template.New("models").Parse(modelsPanelHTML))

func buildModelsView(cfg pluginConfig, note, errText string) modelsView {
	view := modelsView{
		Plugin:    pluginName,
		StatePath: cfg.modelsStatePath(),
		PatchPath: cfg.dshPatchPath(),
		Note:      note,
		Error:     errText,
	}
	modelsCache.mu.Lock()
	if !modelsCache.appliedAt.IsZero() {
		view.AppliedAt = modelsCache.appliedAt.Format("15:04:05")
	}
	modelsCache.mu.Unlock()
	state := modelsCache.loadModels(cfg.modelsStatePath())
	if state == nil {
		view.Error = firstNonEmpty(errText, "还没有模型快照：先运行 wb-models --emit-state")
		return view
	}
	sel := modelsCache.loadSelection(cfg.selectionPath())
	view.Platform = state.Platform
	view.GeneratedAt = state.GeneratedAt
	view.Probe = state.Probe
	if state.MaxCredit != nil {
		view.MaxCredit = fmt.Sprintf("%.2fx", *state.MaxCredit)
	}
	if threshold := effectiveThreshold(state, sel); threshold != nil {
		view.Threshold = fmt.Sprintf("%g", *threshold)
	}
	if sel != nil && strings.EqualFold(sel.Mode, "manual") {
		view.Mode = "manual"
	} else {
		view.Mode = "auto"
	}
	protected := firstNonEmpty(dshDefaultModel(cfg.dshPatchPath()), state.Protected)
	view.Protected = protected
	kept, dropped := resolveModels(state, sel, protected)
	toRow := func(model resolvedModel, checked bool) modelsRow {
		row := modelsRow{
			ID:              model.Entry.ID,
			Label:           modelDisplayName(model),
			Checked:         checked,
			Alive:           model.Entry.Alive,
			ProbeError:      model.Entry.ProbeError,
			ProbeFailed:     !model.Entry.Alive && strings.TrimSpace(model.Entry.ProbeError) != "",
			ProbeErrorShort: truncate(model.Entry.ProbeError, 60),
			Reason:          model.Reason,
			ContextWindow:   model.Entry.ContextWindow,
			MaxTokens:       model.Entry.MaxTokens,
			Vision:          model.Entry.Vision,
			Badges:          strings.Join(model.Entry.Badges, " · "),
		}
		if model.Entry.Credits != nil {
			row.Credits = fmt.Sprintf("%.2fx", *model.Entry.Credits)
			row.CreditsValue = fmt.Sprintf("%g", *model.Entry.Credits)
			row.Cheap = *model.Entry.Credits < 1
		} else {
			row.Credits = "浮动"
		}
		return row
	}
	threshold := effectiveThreshold(state, sel)
	for _, model := range kept {
		view.Kept = append(view.Kept, toRow(model, true))
	}
	for _, model := range dropped {
		view.Dropped = append(view.Dropped, toRow(model, false))
	}
	view.KeptCount, view.DropCount = len(view.Kept), len(view.Dropped)
	view.Rows = append(view.Rows, view.Kept...)
	view.Rows = append(view.Rows, view.Dropped...)
	sort.SliceStable(view.Rows, func(i, j int) bool {
		a, b := view.Rows[i], view.Rows[j]
		av, aerr := strconv.ParseFloat(a.CreditsValue, 64)
		bv, berr := strconv.ParseFloat(b.CreditsValue, 64)
		switch {
		case aerr != nil && berr != nil:
			return a.ID < b.ID
		case aerr != nil:
			return false
		case berr != nil:
			return true
		case av != bv:
			return av < bv
		default:
			return a.ID < b.ID
		}
	})
	_ = threshold
	return view
}

func modelsPageHTML(cfg pluginConfig, note, errText string) []byte {
	var builder strings.Builder
	if err := modelsTemplate.Execute(&builder, buildModelsView(cfg, note, errText)); err != nil {
		return []byte("<pre>models panel error: " + template.HTMLEscapeString(err.Error()) + "</pre>")
	}
	return []byte(builder.String())
}

func resourceURL() string { return resourceBase + modelsPanelPath }

// modelsStatePayload is the machine-readable view of the decision.
func modelsStatePayload(cfg pluginConfig) any {
	state := modelsCache.loadModels(cfg.modelsStatePath())
	sel := modelsCache.loadSelection(cfg.selectionPath())
	if state == nil {
		return map[string]any{
			"state_path": cfg.modelsStatePath(),
			"error":      "no catalogue snapshot yet — run wb-models --emit-state",
		}
	}
	protected := firstNonEmpty(dshDefaultModel(cfg.dshPatchPath()), state.Protected)
	kept, dropped := resolveModels(state, sel, protected)
	mode := "auto"
	if sel != nil && strings.EqualFold(sel.Mode, "manual") {
		mode = "manual"
	}
	toRows := func(models []resolvedModel) []map[string]any {
		rows := make([]map[string]any, 0, len(models))
		for _, model := range models {
			rows = append(rows, map[string]any{
				"id":             model.Entry.ID,
				"name":           modelDisplayName(model),
				"credits":        model.Entry.Credits,
				"badges":         model.Entry.Badges,
				"context_window": model.Entry.ContextWindow,
				"max_tokens":     model.Entry.MaxTokens,
				"vision":         model.Entry.Vision,
				"alive":          model.Entry.Alive,
				"reason":         model.Reason,
			})
		}
		return rows
	}
	modelsCache.mu.Lock()
	lastApply, lastError, appliedAt := modelsCache.lastApply, modelsCache.lastError, modelsCache.appliedAt
	modelsCache.mu.Unlock()
	// Empty until the first successful apply, and the zero time would render as
	// "0001-01-01T00:00:00Z" — a timestamp no consumer can tell from a real one.
	// The HTML view guards the same field the same way.
	appliedAtText := ""
	if !appliedAt.IsZero() {
		appliedAtText = appliedAt.Format(time.RFC3339)
	}
	return map[string]any{
		"mode":         mode,
		"state_path":   cfg.modelsStatePath(),
		"patch_path":   cfg.dshPatchPath(),
		"selection":    cfg.selectionPath(),
		"generated_at": state.GeneratedAt,
		"protected":    protected,
		"kept":         toRows(kept),
		"dropped":      toRows(dropped),
		"last_apply":   lastApply,
		"last_error":   lastError,
		"applied_at":   appliedAtText,
	}
}

func saveModelsSelection(cfg pluginConfig, body []byte) map[string]any {
	var request struct {
		Mode      string   `json:"mode"`
		IDs       []string `json:"ids"`
		MaxCredit *float64 `json:"max_credit"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return map[string]any{"ok": false, "error": "invalid body: " + err.Error()}
	}
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode == "" {
		mode = "manual"
	}
	if request.MaxCredit != nil {
		if *request.MaxCredit <= 0 || *request.MaxCredit > 100 {
			return map[string]any{"ok": false, "error": "max_credit 必须在 (0, 100] 之间"}
		}
	}
	if mode == "auto" {
		// Keep the threshold, drop only the manual id list.
		next := selection{Mode: "auto"}
		if request.MaxCredit != nil {
			next.MaxCredit = request.MaxCredit
		} else if existing := modelsCache.loadSelection(cfg.selectionPath()); existing != nil {
			next.MaxCredit = existing.MaxCredit
		}
		if err := modelsCache.saveSelection(cfg.selectionPath(), next); err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
	} else {
		clean := make([]string, 0, len(request.IDs))
		seen := map[string]bool{}
		for _, id := range request.IDs {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			clean = append(clean, id)
		}
		next := selection{Mode: "manual", IDs: clean}
		if request.MaxCredit != nil {
			next.MaxCredit = request.MaxCredit
		} else if existing := modelsCache.loadSelection(cfg.selectionPath()); existing != nil {
			next.MaxCredit = existing.MaxCredit
		}
		if err := modelsCache.saveSelection(cfg.selectionPath(), next); err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
	}
	if request.MaxCredit != nil {
		syncPythonThreshold(cfg, *request.MaxCredit)
	}
	kept, dropped, note, err := applyModels(cfg)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error(), "kept": len(kept), "dropped": len(dropped)}
	}
	return map[string]any{
		"ok":      true,
		"mode":    mode,
		"kept":    len(kept),
		"dropped": len(dropped),
		"note":    note,
	}
}

func resetModelsSelection(cfg pluginConfig) map[string]any {
	if err := modelsCache.clearSelection(cfg.selectionPath()); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	kept, dropped, note, err := applyModels(cfg)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true, "mode": "auto", "kept": len(kept), "dropped": len(dropped), "note": note}
}

const modelsPanelHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Expiry Router · 模型清单</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 13px/1.6 -apple-system,BlinkMacSystemFont,"SF Pro Text","PingFang SC",sans-serif;
         margin: 0; padding: 20px 24px 60px; background: Canvas; color: CanvasText; }
  h1 { font-size: 17px; margin: 0 0 4px; }
  .meta { color: GrayText; margin-bottom: 10px; }
  code { background: color-mix(in srgb, CanvasText 8%, Canvas); padding: 1px 5px; border-radius: 4px; }
  .banner { padding: 8px 12px; border-radius: 8px; margin: 12px 0; }
  .banner.ok { background: color-mix(in srgb, #34c759 22%, Canvas); }
  .banner.err { background: color-mix(in srgb, #ff4d4f 22%, Canvas); }
  .bar { position: sticky; top: 0; z-index: 5; background: Canvas; padding: 10px 0; display: flex; gap: 8px;
         align-items: center; flex-wrap: wrap;
         border-bottom: 1px solid color-mix(in srgb, CanvasText 14%, Canvas); }
  .bar .sep { width: 1px; height: 20px; background: color-mix(in srgb, CanvasText 18%, Canvas); margin: 0 4px; }
  button { font: inherit; padding: 5px 12px; border-radius: 8px; cursor: pointer;
           border: 1px solid color-mix(in srgb, CanvasText 22%, Canvas); background: Canvas; color: CanvasText; }
  button.primary { background: #0a84ff; border-color: #0a84ff; color: white; font-weight: 600; }
  input[type=text], input[type=number] { font: inherit; padding: 5px 9px; border-radius: 8px;
           border: 1px solid color-mix(in srgb, CanvasText 22%, Canvas); background: Canvas; color: CanvasText; }
  input[type=number] { width: 76px; }
  #keyInput { min-width: 240px; }
  label.chk { display: inline-flex; align-items: center; gap: 5px; cursor: pointer; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 5px 9px; border-bottom: 1px solid color-mix(in srgb, CanvasText 12%, Canvas); }
  th { color: GrayText; font-weight: 600; font-size: 11px; text-transform: uppercase; }
  tr:hover td { background: color-mix(in srgb, CanvasText 6%, Canvas); }
  tr.off td { opacity: .55; }
  tr.dead td { background: color-mix(in srgb, #ff4d4f 10%, Canvas); }
  .num { text-align: right; font-variant-numeric: tabular-nums; }
  .pill { display: inline-block; padding: 0 7px; border-radius: 999px; font-size: 11px;
          background: color-mix(in srgb, CanvasText 12%, Canvas); }
  .pill.hot { background: color-mix(in srgb, #ff9f0a 30%, Canvas); }
  .pill.bad { background: color-mix(in srgb, #ff4d4f 26%, Canvas); }
  .pill.ok  { background: color-mix(in srgb, #34c759 26%, Canvas); }
  .muted { color: GrayText; }
  td.id code { font-size: 12px; }
  #authBox { display: none; margin: 10px 0; }
  #toast { margin-left: auto; }
  tfoot td { color: GrayText; }
</style>
</head>
<body>
<h1>{{.Plugin}} · 模型清单</h1>
<div class="meta">
  Provider <code>{{.Platform}}</code> · 快照 {{.GeneratedAt}} · 模式 <b>{{if eq .Mode "manual"}}手动勾选{{else}}自动（阈值 <span id="modeThreshold">{{.Threshold}}</span>x）{{end}}</b>{{if .AppliedAt}} · 上次写入 {{.AppliedAt}}{{end}}<br>
  {{if .Protected}}受保护默认模型 <b>{{.Protected}}</b> · {{end}}共 {{len .Rows}} 个模型（启用 {{.KeptCount}} / 未启用 {{.DropCount}}）<br>
  快照 <code>{{.StatePath}}</code> · DSH <code>{{.PatchPath}}</code>
</div>
{{if .Note}}<div class="banner ok">{{.Note}}</div>{{end}}
{{if .Error}}<div class="banner err">{{.Error}}</div>{{end}}

<div class="bar">
  <span class="muted">阈值 &lt;</span>
  <input type="number" id="threshold" step="0.01" min="0.01" max="100" value="{{.Threshold}}">
  <span class="muted">x</span>
  <button type="button" onclick="applyThreshold()">按此阈值自动选择</button>
  <label class="chk muted"><input type="checkbox" id="onlyCheap" onchange="render()"> 只显示 &lt; 阈值</label>
  <div class="sep"></div>
  <button type="button" onclick="selectVisible(true)">全选（可见）</button>
  <button type="button" onclick="selectVisible(false)">全部不选</button>
  <button type="button" onclick="invert()">反选</button>
  <div class="sep"></div>
  <button class="primary" type="button" onclick="apply('save')">保存并应用</button>
  <button type="button" onclick="apply('reset')">回到自动</button>
  <span id="toast" class="muted"></span>
</div>

<div id="authBox">
  <span class="muted">需要管理 Key 才能写入（只存本标签页）：</span>
  <input type="text" id="keyInput" placeholder="管理面板 Key">
  <button type="button" onclick="saveKey()">使用</button>
</div>

<table>
  <thead><tr>
    <th style="width:34px"></th><th>模型</th><th class="num">倍率</th><th class="num">上下文</th>
    <th class="num">最大输出</th><th>能力</th><th>探活</th><th>说明</th>
  </tr></thead>
  <tbody>
  {{range .Rows}}
    <tr data-credits="{{.CreditsValue}}" data-alive="{{if .ProbeFailed}}0{{else}}1{{end}}" class="{{if .ProbeFailed}}dead{{else if not .Checked}}off{{end}}">
      <td><input type="checkbox" name="ids" value="{{.ID}}" {{if .Checked}}checked{{end}}></td>
      <td class="id"><code>{{.ID}}</code></td>
      <td class="num">{{.Credits}}</td>
      <td class="num">{{.ContextWindow}}</td>
      <td class="num">{{.MaxTokens}}</td>
      <td>{{if .Vision}}<span class="pill">图片</span> {{end}}{{if .Badges}}<span class="pill hot">{{.Badges}}</span>{{end}}</td>
      <td>{{if .ProbeFailed}}<span class="pill bad">失败</span>{{else if .Alive}}<span class="pill ok">可用</span>{{else}}<span class="pill">未探活</span>{{end}}</td>
      <td class="muted">{{if .ProbeFailed}}{{.ProbeErrorShort}}{{else}}{{.Reason}}{{end}}</td>
    </tr>
  {{else}}
    <tr><td colspan="8" class="muted">还没有模型快照：先运行 wb-models --emit-state</td></tr>
  {{end}}
  </tbody>
  <tfoot><tr><td colspan="8" class="muted">
    勾选后点「保存并应用」→ 立即重写 DSH 模型列表（DSH 热加载）。
    探活失败的模型不会被写入；DSH 当前默认模型永远保留。
  </td></tr></tfoot>
</table>

<script>
const API = "/v0/management/plugins/expiry-router";
const SS_KEY = "expiry-router-mgmt-key";
const PANEL_STORE = "cli-proxy-auth";
const ENC_PREFIX = "enc::v1::";
const SECRET_SALT = "cli-proxy-api-webui::secure-storage";

function _enc(t){return new TextEncoder().encode(t)}
function _dec(b){return new TextDecoder().decode(b)}
function _keyBytes(){try{return _enc(SECRET_SALT+"|"+window.location.host+"|"+navigator.userAgent)}catch(e){return _enc(SECRET_SALT)}}
function _xor(d,k){const r=new Uint8Array(d.length);for(let i=0;i<d.length;i++)r[i]=d[i]^k[i%k.length];return r}
function _b64d(s){const bin=atob(s);const b=new Uint8Array(bin.length);for(let i=0;i<bin.length;i++)b[i]=bin.charCodeAt(i);return b}
function deobfuscate(p){if(!p||!p.startsWith(ENC_PREFIX))return p;try{return _dec(_xor(_b64d(p.slice(ENC_PREFIX.length)),_keyBytes()))}catch(e){return p}}
function isEmbedded(){try{return window.self!==window.top}catch(e){return false}}
function readPanelKey(){
  if(!isEmbedded())return null;
  let raw;try{raw=localStorage.getItem(PANEL_STORE)}catch(e){return null}
  if(!raw)return null;
  try{const parsed=JSON.parse(deobfuscate(raw));const st=(parsed&&parsed.state)||parsed||{};
      return typeof st.managementKey==="string"&&st.managementKey?st.managementKey:null}catch(e){return null}
}
function readUrlKey(){
  const m=new URLSearchParams(window.location.search).get("key");
  if(m){history.replaceState(null,"",window.location.pathname)}
  return m;
}
function getKey(){try{return sessionStorage.getItem(SS_KEY)||readPanelKey()||readUrlKey()}catch(e){return readPanelKey()||readUrlKey()}}
function saveKey(){
  const v=document.getElementById("keyInput").value.trim();
  if(!v)return;
  try{sessionStorage.setItem(SS_KEY,v)}catch(e){}
  document.getElementById("authBox").style.display="none";
  apply("save");
}
function showAuth(){document.getElementById("authBox").style.display="block";
  const i=document.getElementById("keyInput");i&&i.focus()}
function toast(text,kind){const el=document.getElementById("toast");
  el.textContent=text;el.className=kind==="err"?"pill bad":(kind==="ok"?"pill ok":"muted")}

function rows(){return [...document.querySelectorAll("tbody tr[data-credits]")]}
function threshold(){const v=parseFloat(document.getElementById("threshold").value);return isNaN(v)?null:v}
function render(){
  const only=document.getElementById("onlyCheap").checked;
  const t=threshold();
  let visible=0;
  rows().forEach(tr=>{
    const c=tr.dataset.credits===""?null:parseFloat(tr.dataset.credits);
    const below=(c!==null&&t!==null&&c<t);
    const show=!only||below;
    tr.style.display=show?"":"none";
    if(show)visible++;
  });
  toast("显示 "+visible+" / "+rows().length+" 个");
}
function selectVisible(state){rows().forEach(tr=>{if(tr.style.display!=="none")tr.querySelector("input[name=ids]").checked=state})}
function invert(){rows().forEach(tr=>{const c=tr.querySelector("input[name=ids]");if(tr.style.display!=="none")c.checked=!c.checked})}

async function post(path,body){
  const key=getKey();
  if(!key){toast("需要管理 Key","err");showAuth();return null}
  toast("正在写入…");
  try{
    const res=await fetch(API+path,{method:"POST",credentials:"omit",
      headers:{"Content-Type":"application/json","Authorization":"Bearer "+key},
      body:JSON.stringify(body||{})});
    const data=await res.json().catch(()=>({}));
    if(!res.ok||data.ok===false){
      toast("失败："+(data.error||("HTTP "+res.status)),"err");
      if(res.status===401||res.status===403)showAuth();
      return null;
    }
    return data;
  }catch(e){toast("请求失败："+e,"err");return null}
}
async function apply(action){
  const t=threshold();
  if(action==="reset"){
    // Keep the threshold, drop only the manual ticks (that is what the label
    // promises). /models/reset remains the "forget everything" route.
    const data=await post("/models",{mode:"auto",max_credit:t});
    if(data){toast("已回到自动（阈值 "+t+"x）：保留 "+data.kept+" 个","ok");setTimeout(()=>location.reload(),900)}
    return;
  }
  const ids=rows().filter(tr=>tr.querySelector("input[name=ids]").checked)
                 .map(tr=>tr.querySelector("input[name=ids]").value);
  const data=await post("/models",{mode:"manual",ids:ids,max_credit:t});
  if(data){toast("已应用：保留 "+data.kept+" 个 / 剔除 "+data.dropped+" 个","ok");setTimeout(()=>location.reload(),900)}
}
async function applyThreshold(){
  const t=threshold();
  if(t===null){toast("阈值无效","err");return}
  const data=await post("/models",{mode:"auto",max_credit:t});
  if(data){toast("阈值 "+t+"x → 自动选择保留 "+data.kept+" 个","ok");setTimeout(()=>location.reload(),900)}
}
render();
</script>
</body>
</html>
`

// ---------------------------------------------------------------------------
// config helpers for the model panel
// ---------------------------------------------------------------------------

func (c pluginConfig) modelsStatePath() string {
	if path := strings.TrimSpace(c.ModelsStatePath); path != "" {
		return path
	}
	return filepath.Join(filepath.Dir(c.statePath()), "models-state.json")
}

func (c pluginConfig) dshPatchPath() string {
	if path := strings.TrimSpace(c.DSHPatchPath); path != "" {
		return path
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".dsh", "profiles", "desktop", "cordis.patch.yml")
}

func (c pluginConfig) selectionPath() string {
	if path := strings.TrimSpace(c.SelectionPath); path != "" {
		return path
	}
	return filepath.Join(filepath.Dir(c.statePath()), "models-selection.json")
}

func (c pluginConfig) manageDSH() bool {
	return c.ManageDSH == nil || *c.ManageDSH
}

// syncPythonThreshold keeps wb-models.conf's max_credit in step with the panel,
// so the command-line writer and the collector probe scope stay consistent.
func syncPythonThreshold(cfg pluginConfig, threshold float64) {
	path := filepath.Join(filepath.Dir(cfg.modelsStatePath()), "wb-models.conf")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	conf := map[string]any{}
	if err := json.Unmarshal(raw, &conf); err != nil {
		return
	}
	conf["max_credit"] = threshold
	updated, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(updated, '\n'), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		return
	}
	pickLogger.Printf("%s: wb-models.conf max_credit → %g", pluginName, threshold)
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}
