// panel_group_test.go — the panel must never present one pooled credit total.
//
// Rule under test (user-stated): platforms are NEVER merged, accounts inside one
// platform ALWAYS are. So `/preview` has to answer with one pool per platform
// and no cross-platform total, and each platform's rows have to be ordered among
// themselves (soonest expiry first, within that platform).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func testState() *routeState {
	return &routeState{
		GeneratedAt: "2026-09-29T00:26:52Z",
		Providers: map[string]providerRanking{
			"workbuddy": {
				Order:   []string{"wb-2", "wb-1"},
				Reasons: map[string]string{"wb-2": "组内轮换", "wb-1": "单账号"},
				Accounts: []accountDetail{
					{AuthID: "wb-1", Nickname: "Ling", Total: 1584, AtRisk: 847, Soonest: "10-27 09:53",
						Packages: []packageDetail{{Name: "包A", Remain: 1584, Size: 2000, End: "2026-10-27 09:53:00"}}},
					{AuthID: "wb-2", Nickname: "机灵", Total: 2351, AtRisk: 2244, Soonest: "10-27 09:37",
						Packages: []packageDetail{{Name: "包B", Remain: 2351, Size: 2351, End: "2026-10-27 09:37:00"}}},
				},
			},
			"trae": {
				Order:   []string{"tr-1", "tr-2"},
				Reasons: map[string]string{"tr-1": "单账号"},
				Accounts: []accountDetail{
					{AuthID: "tr-1", Nickname: "lingyirEd", Total: 2646, AtRisk: 96, Soonest: "09-30 23:59",
						Packages: []packageDetail{{Name: "每月登录赠送", Remain: 96, Size: 96, End: "2026-09-30 23:59:59"}}},
					{AuthID: "tr-2", Nickname: "用户9365811994", Total: 4397, AtRisk: 247, Soonest: "09-30 23:59",
						Packages: []packageDetail{{Name: "每月登录赠送", Remain: 247, Size: 247, End: "2026-09-30 23:59:59"}}},
				},
			},
		},
	}
}

func TestPreviewHasNoCrossPlatformTotal(t *testing.T) {
	cfg := pluginConfig{}
	raw, err := json.Marshal(previewPayload(cfg, testState()))
	if err != nil {
		t.Fatalf("marshal preview: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal preview: %v", err)
	}
	if _, ok := payload["totals"]; ok {
		t.Fatal("preview still exposes a top-level \"totals\" — that is a cross-platform pool")
	}
	byProvider, ok := payload["by_provider"].(map[string]any)
	if !ok {
		t.Fatal("preview has no by_provider object")
	}
	if len(byProvider) != 2 {
		t.Fatalf("by_provider has %d platforms, want 2", len(byProvider))
	}
	for _, name := range []string{"workbuddy", "trae"} {
		entry, ok := byProvider[name].(map[string]any)
		if !ok {
			t.Fatalf("by_provider[%s] missing", name)
		}
		totals, ok := entry["totals"].(map[string]any)
		if !ok {
			t.Fatalf("by_provider[%s].totals missing", name)
		}
		// Each platform's pool must equal that platform's own accounts only.
		want := int64(0)
		for _, account := range testState().Providers[name].Accounts {
			want += account.Total
		}
		if got := int64(totals["remain"].(float64)); got != want {
			t.Fatalf("by_provider[%s].totals.remain = %d, want %d (its own accounts only)", name, got, want)
		}
	}
}

func TestGroupsStayPerPlatform(t *testing.T) {
	view := buildView(pluginConfig{}, testState())
	if len(view.Groups) != 2 {
		t.Fatalf("got %d groups, want one per platform", len(view.Groups))
	}
	for _, group := range view.Groups {
		ranking, ok := testState().Providers[group.Provider]
		if !ok {
			t.Fatalf("group %q is not a provider", group.Provider)
		}
		if len(group.Accounts) != len(ranking.Accounts) {
			t.Fatalf("group %s has %d accounts, want %d", group.Provider, len(group.Accounts), len(ranking.Accounts))
		}
		for _, account := range group.Accounts {
			if account.Provider != group.Provider {
				t.Fatalf("group %s contains an account of %s", group.Provider, account.Provider)
			}
		}
		for _, pkg := range group.Packages {
			if pkg.Provider != group.Provider {
				t.Fatalf("group %s contains a package of %s", group.Provider, pkg.Provider)
			}
		}
		// Within the group the soonest expiry leads, and ranks are per platform.
		for i := 1; i < len(group.Packages); i++ {
			if group.Packages[i].EndSort.Before(group.Packages[i-1].EndSort) {
				t.Fatalf("group %s packages are not soonest-first", group.Provider)
			}
		}
	}
}

func TestRenderedPanelGroupsByPlatform(t *testing.T) {
	html := string(renderPanel(pluginConfig{}, testState()))
	if strings.Contains(html, "全部积分包") {
		t.Fatal("panel still renders one merged 全部积分包 table")
	}
	if !strings.Contains(html, "各平台合计") {
		t.Fatal("panel does not show the per-platform totals table")
	}
	for _, name := range []string{"workbuddy", "trae"} {
		if !strings.Contains(html, `class="platform">`+name) {
			t.Fatalf("panel has no %s section header", name)
		}
	}
}

// TestManagementDispatchServesPerPlatformPreview drives the same entry point the
// compiled dylib exposes, so the check covers the route wiring and not just the
// payload builder.
func TestManagementDispatchServesPerPlatformPreview(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "route-state.json")
	raw, err := json.Marshal(testState())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	configMu.Lock()
	previous := config
	config = pluginConfig{StatePath: statePath}
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		config = previous
		configMu.Unlock()
	})
	cache = stateCache{}

	call := func(method, path string) pluginapi.ManagementResponse {
		t.Helper()
		request, err := json.Marshal(map[string]any{"method": method, "path": path})
		if err != nil {
			t.Fatal(err)
		}
		out, err := handleManagement(request)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		// The ABI hands the host this struct; Body is []byte and therefore
		// base64 in the JSON envelope, exactly as the dylib answers it.
		parsed := struct {
			OK     bool                         `json:"ok"`
			Result pluginapi.ManagementResponse `json:"result"`
		}{}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("%s %s: unmarshal envelope: %v", method, path, err)
		}
		if !parsed.OK {
			t.Fatalf("%s %s: envelope not ok: %s", method, path, out)
		}
		return parsed.Result
	}

	// /preview: one pool per platform, no top-level totals.
	preview := call("GET", managementBase()+"/plugins/"+pluginID+"/preview")
	var payload map[string]any
	if err := json.Unmarshal(preview.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["totals"]; ok {
		t.Fatal("/preview still answers with a cross-platform totals key")
	}
	byProvider, ok := payload["by_provider"].(map[string]any)
	if !ok || len(byProvider) != 2 {
		t.Fatalf("/preview by_provider = %v, want one entry per platform", payload["by_provider"])
	}

	// The HTML panel keeps the two platforms apart as well.
	panel := call("GET", resourceBase+"/panel")
	html := string(panel.Body)
	for _, name := range []string{"workbuddy", "trae"} {
		if !strings.Contains(html, name) {
			t.Fatalf("panel HTML never mentions %s", name)
		}
	}
	if strings.Contains(html, `id="total-remain"`) {
		t.Fatal("panel still renders a single cross-platform remaining figure")
	}
}

// TestUrgentListIsCrossPlatformButViewOnly pins the one list that IS allowed to
// mix platforms: packages dying within 24h, soonest first. It must (a) include
// both platforms, (b) exclude anything further out than 24h, (c) exclude empty
// packages, and (d) not become a cross-platform pool anywhere else.
func TestUrgentListIsCrossPlatformButViewOnly(t *testing.T) {
	soon := time.Now().Add(3 * time.Hour).Format("2006-01-02 15:04:05")
	soon2 := time.Now().Add(11 * time.Hour).Format("2006-01-02 15:04:05")
	later := time.Now().Add(30 * time.Hour).Format("2006-01-02 15:04:05")
	far := time.Now().Add(30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	state := &routeState{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Providers: map[string]providerRanking{
			"trae": {Order: []string{"tr-1"}, Accounts: []accountDetail{{AuthID: "tr-1", Nickname: "lingyirEd", Total: 400, AtRisk: 60, Packages: []packageDetail{
				{Name: "签到奖励", Remain: 60, Size: 200, End: soon},
				{Name: "已用完", Remain: 0, Size: 100, End: soon},
				{Name: "远期", Remain: 200, Size: 200, End: far},
			}}}},
			"workbuddy": {Order: []string{"wb-1"}, Accounts: []accountDetail{{AuthID: "wb-1", Nickname: "18617097898", Total: 900, AtRisk: 90, Packages: []packageDetail{
				{Name: "CodeBuddy个人体验版", Remain: 90, Size: 500, End: soon2},
				{Name: "30小时后", Remain: 500, Size: 500, End: later},
			}}}},
		},
	}
	view := buildView(pluginConfig{}, state)

	if len(view.Urgent) != 2 {
		t.Fatalf("urgent has %d rows, want 2 (the 3h and 11h packages)", len(view.Urgent))
	}
	// (a) both platforms present, interleaved by deadline.
	if view.Urgent[0].Provider != "trae" || view.Urgent[1].Provider != "workbuddy" {
		t.Fatalf("urgent is not interleaved across platforms: %+v", view.Urgent)
	}
	// (b) soonest first.
	for i := 1; i < len(view.Urgent); i++ {
		if view.Urgent[i].EndSort.Before(view.Urgent[i-1].EndSort) {
			t.Fatal("urgent is not soonest-first")
		}
	}
	// (c) nothing empty, nothing beyond the window that was actually chosen.
	for _, pkg := range view.Urgent {
		if pkg.Remain <= 0 {
			t.Fatalf("urgent contains an empty package: %+v", pkg)
		}
		if pkg.EndSort.After(time.Now().Add(view.UrgentWindow)) {
			t.Fatalf("urgent contains a package beyond the window: %+v", pkg)
		}
	}
	// The 3h/11h packages exist, so the narrowest step must have been kept:
	// the ladder must NOT over-widen when 24h already has content.
	if view.UrgentWindow != 24*time.Hour {
		t.Fatalf("urgent window = %v, want the 24h step when it has content", view.UrgentWindow)
	}
	if view.UrgentLabel != "24 小时" {
		t.Fatalf("urgent label = %q, want 24 小时", view.UrgentLabel)
	}

	// (d) the per-platform shape is untouched by the urgent list.
	payload, err := json.Marshal(previewPayload(pluginConfig{}, state))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed["totals"]; ok {
		t.Fatal("adding urgent must not reintroduce a cross-platform totals key")
	}
	if parsed["urgent"] == nil {
		t.Fatal("preview does not expose the urgent list")
	}
}

// buildStateWithSoonest makes a two-platform state whose single soonest live
// package expires after the given offset, used to exercise the widening ladder.
func buildStateWithSoonest(offset time.Duration, extra ...time.Duration) *routeState {
	accounts := map[string][]packageDetail{}
	at := func(d time.Duration) string { return time.Now().Add(d).Format("2006-01-02 15:04:05") }
	accounts["trae"] = append(accounts["trae"], packageDetail{Name: "签到奖励", Remain: 10, Size: 10, End: at(offset)})
	for i, d := range extra {
		accounts["trae"] = append(accounts["trae"], packageDetail{Name: fmt.Sprintf("额外%d", i), Remain: 5, Size: 5, End: at(d)})
	}
	return &routeState{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Providers: map[string]providerRanking{
			"trae":      {Order: []string{"tr-1"}, Accounts: []accountDetail{{AuthID: "tr-1", Nickname: "lingyirEd", Packages: accounts["trae"]}}},
			"workbuddy": {Order: []string{"wb-1"}, Accounts: []accountDetail{{AuthID: "wb-1", Nickname: "Ling", Packages: []packageDetail{{Name: "裂变包", Remain: 7, Size: 7, End: at(60 * 24 * time.Hour)}}}}},
		},
	}
}

// TestUrgentWindowWidens24hThen3dThen7d pins the ladder the user asked for:
// when nothing expires within 24h the list widens to 3 days, then to 7 days,
// and it says which step it landed on.
func TestUrgentWindowWidens24hThen3dThen7d(t *testing.T) {
	cases := []struct {
		name   string
		offset time.Duration
		window time.Duration
		label  string
	}{
		{"within 24h keeps the narrowest step", 5 * time.Hour, 24 * time.Hour, "24 小时"},
		{"nothing in 24h widens to 3d", 50 * time.Hour, 3 * 24 * time.Hour, "3 天"},
		{"nothing in 3d widens to 7d", 5 * 24 * time.Hour, 7 * 24 * time.Hour, "7 天"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := buildView(pluginConfig{}, buildStateWithSoonest(tc.offset))
			if view.UrgentWindow != tc.window {
				t.Fatalf("window = %v, want %v", view.UrgentWindow, tc.window)
			}
			if view.UrgentLabel != tc.label {
				t.Fatalf("label = %q, want %q", view.UrgentLabel, tc.label)
			}
			if len(view.Urgent) != 1 {
				t.Fatalf("urgent has %d rows, want the 1 package inside %v", len(view.Urgent), tc.window)
			}
		})
	}
}

// TestUrgentWideningKeepsEverythingInsideTheChosenWindow guards the subtle bug:
// after widening to 7 days the list must include the 3-day and 24-hour packages
// too, not only the one that triggered the widening.
func TestUrgentWideningKeepsEverythingInsideTheChosenWindow(t *testing.T) {
	view := buildView(pluginConfig{}, buildStateWithSoonest(
		25*time.Hour, // just past the 24h step, so the ladder widens to 3d
		30*time.Hour, // and these two sit inside the same widened window
		40*time.Hour,
	))
	// All three are beyond 24h but inside 3 days, so the ladder stops at 3d —
	// it must not jump further than necessary, and it must keep every row that
	// fits the window it landed on, not just the row that triggered it.
	if view.UrgentWindow != 3*24*time.Hour {
		t.Fatalf("window = %v, want 3d (the first step with content)", view.UrgentWindow)
	}
	if len(view.Urgent) != 3 {
		t.Fatalf("urgent has %d rows, want all 3 inside the 3d window", len(view.Urgent))
	}
	for i := 1; i < len(view.Urgent); i++ {
		if view.Urgent[i].EndSort.Before(view.Urgent[i-1].EndSort) {
			t.Fatal("urgent is not soonest-first after widening")
		}
	}
}

// TestUrgentEmptyOnlyWhenNothingIsInsideSevenDays: the list may only fall empty
// when even the widest step finds nothing, and it must say so rather than
// claiming a 24-hour window.
func TestUrgentEmptyOnlyWhenNothingIsInsideSevenDays(t *testing.T) {
	view := buildView(pluginConfig{}, buildStateWithSoonest(30*24*time.Hour))
	if len(view.Urgent) != 0 {
		t.Fatalf("urgent has %d rows, want none (nothing within 7d)", len(view.Urgent))
	}
	if view.UrgentLabel != "7 天" {
		t.Fatalf("label = %q, want the widest step 7 天", view.UrgentLabel)
	}
	html := string(renderPanel(pluginConfig{}, buildStateWithSoonest(30*24*time.Hour)))
	if !strings.Contains(html, "未来 7 天内没有积分包到期") {
		t.Fatal("empty panel does not state the widened 7-day window")
	}
}
