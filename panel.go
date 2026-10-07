// panel.go — Management API surface for the expiry-router plugin.
//
// The CPA control panel lists plugins that declare the `management_api`
// capability and links their resource routes, so the ranking this plugin uses
// is inspectable in the same UI as the provider dashboards:
//
//	GET /v0/resource/plugins/expiry-router/panel   → HTML priority viewer
//	GET /v0/management/plugins/expiry-router/state → raw route-state.json
//
// The viewer renders straight from route-state.json (no extra provider calls):
// the account ranking the scheduler uses, then every credit package of every
// account sorted by expiry, so "what burns first" is visible at a glance.
package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginID = pluginName

var (
	basePathMu   sync.RWMutex
	basePath     = "/v0/management"
	resourceBase = "/v0/resource/plugins/" + pluginID
)

func setManagementBasePath(path string) {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return
	}
	basePathMu.Lock()
	basePath = path
	basePathMu.Unlock()
}

func managementBase() string {
	basePathMu.RLock()
	defer basePathMu.RUnlock()
	return basePath
}

// ---------------------------------------------------------------------------
// route registration
// ---------------------------------------------------------------------------

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

func managementRegistration() managementRegistrationResponse {
	base := managementBase() + "/plugins/" + pluginID
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{
				Method:      http.MethodGet,
				Path:        base + "/state",
				Description: "Raw expiry ranking (route-state.json) the scheduler is following.",
			},
			{
				Method:      http.MethodGet,
				Path:        base + "/preview",
				Description: "Ranking plus every credit package of every account, in the runtime order.",
			},
			{
				Method:      http.MethodGet,
				Path:        base + "/models/state",
				Description: "Current model-list decision: mode, kept models, dropped models.",
			},
			{
				Method:      http.MethodPost,
				Path:        base + "/models",
				Description: "Save the model selection ({mode:manual,ids:[...]} or {mode:auto}) and rewrite the DSH list.",
			},
			{
				Method:      http.MethodPost,
				Path:        base + "/models/reset",
				Description: "Forget the manual selection and follow the credit threshold again.",
			},
		},
		Resources: []resourceRoute{
			{
				Path:        "/panel",
				Menu:        "Expiry Router",
				Description: "Account priority by credit-package expiry: every account, every package, soonest first.",
			},
			{
				Path:        "/models",
				Menu:        "Model picker",
				Description: "WorkBuddy model catalogue with credit multipliers: tick what DSH should offer.",
			},
		},
	}
}

func managementHTMLResponse(body []byte) pluginapi.ManagementResponse {
	headers := http.Header{}
	headers.Set("Content-Type", "text/html; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: body}
}

func managementJSONResponse(status int, payload any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(payload)
	headers := http.Header{}
	headers.Set("Content-Type", "application/json; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body}
}

// ---------------------------------------------------------------------------
// request handling
// ---------------------------------------------------------------------------

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")
	cfg := loadedConfig()
	state := cache.load(cfg.statePath(), 0) // panel view always re-reads the file

	switch {
	case req.Method == http.MethodPost && path == managementBase()+"/plugins/"+pluginID+"/models":
		return okEnvelope(managementJSONResponse(http.StatusOK, saveModelsSelection(cfg, req.Body)))
	case req.Method == http.MethodPost && path == managementBase()+"/plugins/"+pluginID+"/models/reset":
		return okEnvelope(managementJSONResponse(http.StatusOK, resetModelsSelection(cfg)))
	case req.Method == http.MethodGet && path == managementBase()+"/plugins/"+pluginID+"/models/state":
		return okEnvelope(managementJSONResponse(http.StatusOK, modelsStatePayload(cfg)))
	case req.Method == http.MethodGet && strings.HasPrefix(path, resourceBase):
		sub := strings.TrimPrefix(path, resourceBase)
		switch sub {
		case "", "/", "/panel":
			return okEnvelope(managementHTMLResponse(renderPanel(cfg, state)))
		case "/models", "/models/":
			return okEnvelope(managementHTMLResponse(modelsPageHTML(cfg, "", "")))
		}
		return okEnvelope(managementHTMLResponse(renderNotFound(sub)))
	case req.Method == http.MethodGet && path == managementBase()+"/plugins/"+pluginID+"/state":
		return okEnvelope(managementJSONResponse(http.StatusOK, statePayload(cfg, state)))
	case req.Method == http.MethodGet && path == managementBase()+"/plugins/"+pluginID+"/preview":
		return okEnvelope(managementJSONResponse(http.StatusOK, previewPayload(cfg, state)))
	default:
		return okEnvelope(managementJSONResponse(http.StatusNotFound, map[string]any{
			"error": "not found",
			"path":  path,
		}))
	}
}

// ---------------------------------------------------------------------------
// view model
// ---------------------------------------------------------------------------

type panelPackage struct {
	Provider  string    `json:"provider"`
	Account   string    `json:"account"`
	UID       string    `json:"uid,omitempty"`
	AuthID    string    `json:"auth_id,omitempty"`
	Name      string    `json:"name"`
	Remain    int64     `json:"remain"`
	Size      int64     `json:"size"`
	End       string    `json:"end,omitempty"`
	EndSort   time.Time `json:"-"`
	Rank      int       `json:"rank,omitempty"`
	IsSoonest bool      `json:"is_soonest,omitempty"`
}

type panelAccount struct {
	Provider  string `json:"provider"`
	Nickname  string `json:"nickname"`
	UID       string `json:"uid,omitempty"`
	AuthID    string `json:"auth_id"`
	Rank      int    `json:"rank,omitempty"`
	Soonest   string `json:"soonest,omitempty"`
	AtRisk    int64  `json:"at_risk"`
	Total     int64  `json:"total"`
	Packages  int    `json:"packages"`
	Reason    string `json:"reason,omitempty"`
	Disabled  bool   `json:"disabled"`
	Exhausted bool   `json:"exhausted"`
	// Paused lists the models upstream is currently rate-limiting on this
	// credential (code 6004) with each recovery instant, e.g.
	// "deepseek-v4.1-flash 至 09-29 11:25". Empty means nothing is paused.
	Paused string `json:"paused,omitempty"`
	// Rotation explains how the collector grouped this account: "urgent"
	// (expires within 24h, hard priority), "rotate" (shares a deadline with
	// another account and takes turns) or "single".
	Rotation string `json:"rotation,omitempty"`
}

// panelRotation summarises the collector's grouping so the panel can explain
// WHY an order looks the way it does, not just show it.
type panelRotation struct {
	WindowDays float64 `json:"window_days,omitempty"`
	Cursor     int     `json:"cursor,omitempty"`
	Groups     []int   `json:"groups,omitempty"`
	Urgent     int     `json:"urgent,omitempty"`
	HasData    bool    `json:"has_data,omitempty"`
}

// panelGroup is ONE platform's slice of the ranking, and it is the unit the
// panel and `/preview` are built from. Platforms are never merged: accounts
// inside one platform are ranked against each other (so that platform's
// soonest-expiring packages burn first), and nothing is compared or summed
// across platforms. Every total below is therefore a per-platform pool.
type panelGroup struct {
	Provider    string         `json:"provider"`
	Accounts    []panelAccount `json:"accounts"`
	Packages    []panelPackage `json:"packages"`
	TotalAtRisk int64          `json:"at_risk"`
	TotalRemain int64          `json:"total"`
	Rotation    *panelRotation `json:"rotation,omitempty"`
}

type panelView struct {
	Plugin      string
	Version     string
	Strategy    string
	StatePath   string
	GeneratedAt string
	Age         string
	Stale       bool
	MaxAge      string
	ParseNote   string
	Providers   []string
	// Groups holds the per-platform view: one entry per provider, in the same
	// order as Providers. The flat Accounts/Packages/Depleted slices below are
	// the same rows, still tagged with their provider, kept because consumers
	// (the DSH status card) read them; a consumer that renders a single list
	// must group by provider first, never pool the rows.
	Groups      []panelGroup
	Accounts    []panelAccount
	Packages    []panelPackage
	Depleted    []panelPackage
	Rotations   []panelRotation
	PausedCount int

	// Urgent lists every package that still has a balance AND dies within the
	// window that was actually used, from ALL platforms, ordered
	// soonest-first. Unlike Groups this one list deliberately mixes platforms,
	// so the operator can answer "what should I spend first?" in one glance.
	//
	// It is a VIEW ONLY: the scheduler never consults it, never compares a
	// Trae deadline against a WorkBuddy one, and never spends one platform's
	// credits on the other's traffic. The table header in the HTML says so.
	Urgent []panelPackage
	// UrgentWindow is the window Urgent was built with, chosen by widening
	// through urgentWindows until something matched (see pickUrgentWindow).
	UrgentWindow time.Duration
	// UrgentLabel is UrgentWindow rendered for humans ("24 小时" / "3 天" /
	// "7 天"), so the panel can say which step of the ladder it is showing.
	UrgentLabel string
}

// urgentWindows is the widening ladder for the "about to expire" list. The
// panel starts at the narrowest window that finds anything, so the list is
// never empty while credits are still dying; the label tells the operator
// which step it landed on, so a 7-day view is never mistaken for a 24h one.
var urgentWindows = []struct {
	Window time.Duration
	Label  string
}{
	{24 * time.Hour, "24 小时"},
	{3 * 24 * time.Hour, "3 天"},
	{7 * 24 * time.Hour, "7 天"},
}

// pickUrgentWindow returns the first window in the ladder that catches at
// least one live package, plus the packages it caught. When nothing matches
// even the widest step, it returns the widest window and an empty list, so the
// panel can say "nothing expires within 7 days" rather than showing an
// artificially narrow "nothing today".
func pickUrgentWindow(live []panelPackage, now time.Time) (time.Duration, string, []panelPackage) {
	widest := urgentWindows[len(urgentWindows)-1]
	for _, step := range urgentWindows {
		matched := make([]panelPackage, 0, len(live))
		limit := now.Add(step.Window)
		for _, pkg := range live {
			if !pkg.EndSort.IsZero() && !pkg.EndSort.After(limit) {
				matched = append(matched, pkg)
			}
		}
		if len(matched) > 0 {
			return step.Window, step.Label, matched
		}
	}
	return widest.Window, widest.Label, nil
}

func previewPayload(cfg pluginConfig, state *routeState) any {
	view := buildView(cfg, state)
	byProvider := make(map[string]any, len(view.Groups))
	for _, group := range view.Groups {
		byProvider[group.Provider] = map[string]any{
			"accounts": group.Accounts,
			"packages": group.Packages,
			"rotation": group.Rotation,
			"totals": map[string]any{
				"at_risk": group.TotalAtRisk,
				"remain":  group.TotalRemain,
			},
		}
	}
	return map[string]any{
		"plugin":       view.Plugin,
		"version":      view.Version,
		"strategy":     view.Strategy,
		"state_path":   view.StatePath,
		"generated_at": view.GeneratedAt,
		"age":          view.Age,
		"stale":        view.Stale,
		"max_age":      view.MaxAge,
		"providers":    view.Providers,
		// by_provider is the authoritative shape: one pool per platform.
		"by_provider": byProvider,
		// No cross-platform "totals" key exists on purpose — see panelGroup.
		"accounts": view.Accounts,
		"packages": view.Packages,
		// urgent is the one deliberately cross-platform list: packages dying
		// within urgent_window, soonest first. View only — the scheduler never
		// reads it. The window widens 24h -> 3d -> 7d until it catches
		// something; urgent_label names the step that was used.
		"urgent":        view.Urgent,
		"urgent_window": view.UrgentWindow.String(),
		"urgent_label":  view.UrgentLabel,
		"rotations":     view.Rotations,
		"paused_count":  view.PausedCount,
	}
}

func statePayload(cfg pluginConfig, state *routeState) any {
	if state == nil {
		return map[string]any{
			"state_path": cfg.statePath(),
			"error":      "no ranking file yet — run the collector",
		}
	}
	return state
}

func buildView(cfg pluginConfig, state *routeState) panelView {
	view := panelView{
		Plugin:    pluginName,
		Version:   pluginVersion,
		Strategy:  cfg.strategy(),
		StatePath: cfg.statePath(),
		MaxAge:    cfg.maxAge().String(),
	}
	if state == nil {
		view.ParseNote = "还没读到排序文件：插件当前不干预选号（交回宿主）。请先运行采集器。"
		return view
	}
	view.GeneratedAt = state.GeneratedAt
	if age, ok := stateAge(state); ok {
		view.Age = age.Round(time.Second).String()
		view.Stale = age > cfg.maxAge()
	}

	providers := make([]string, 0, len(state.Providers))
	for name := range state.Providers {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	view.Providers = providers

	// One clock for the whole render, so every row in the urgent list is
	// measured against the same instant. The window is picked after the walk,
	// from the live packages it collected.
	now := time.Now()
	livePackages := make([]panelPackage, 0, 32)

	for _, name := range providers {
		ranking := state.Providers[name]
		group := panelGroup{Provider: name}
		if ranking.Rotation != nil {
			rotation := panelRotation{
				WindowDays: ranking.Rotation.WindowDays,
				Cursor:     ranking.Rotation.Cursor,
				Groups:     ranking.Rotation.Groups,
				Urgent:     ranking.Rotation.Urgent,
				HasData:    true,
			}
			view.Rotations = append(view.Rotations, rotation)
			group.Rotation = &rotation
		}
		rankOf := map[string]int{}
		for index, id := range ranking.Order {
			rankOf[id] = index + 1
		}
		for _, account := range ranking.Accounts {
			packages := append([]packageDetail(nil), account.Packages...)
			sort.SliceStable(packages, func(i, j int) bool {
				return packageEnd(packages[i].End).Before(packageEnd(packages[j].End))
			})
			row := panelAccount{
				Provider:  name,
				Nickname:  firstNonEmpty(account.Nickname, account.UID, account.AuthID),
				UID:       account.UID,
				AuthID:    account.AuthID,
				Rank:      rankOf[account.AuthID],
				Soonest:   account.Soonest,
				AtRisk:    account.AtRisk,
				Total:     account.Total,
				Packages:  len(account.Packages),
				Reason:    ranking.Reasons[account.AuthID],
				Disabled:  account.Disabled,
				Exhausted: account.Exhausted,
				Paused:    formatPausedModels(account.PausedModels),
			}
			if row.Paused != "" {
				view.PausedCount++
			}
			// The tier is recorded per account by the collector; fall back to
			// "ranked" so a state file written by an older collector still renders.
			switch tier := rotationTier(ranking, account.AuthID); tier {
			case "urgent":
				row.Rotation = "24小时内到期·优先"
			case "rotate":
				row.Rotation = "组内轮换"
			}
			view.Accounts = append(view.Accounts, row)
			group.Accounts = append(group.Accounts, row)
			group.TotalAtRisk += account.AtRisk
			group.TotalRemain += account.Total
			soonestMarked := false
			for _, pkg := range packages {
				live := pkg.Remain > 0
				entry := panelPackage{
					Provider:  name,
					Account:   row.Nickname,
					UID:       account.UID,
					AuthID:    account.AuthID,
					Name:      pkg.Name,
					Remain:    pkg.Remain,
					Size:      pkg.Size,
					End:       pkg.End,
					EndSort:   packageEnd(pkg.End),
					Rank:      row.Rank,
					IsSoonest: live && !soonestMarked,
				}
				if entry.IsSoonest {
					soonestMarked = true
				}
				if live {
					view.Packages = append(view.Packages, entry)
					group.Packages = append(group.Packages, entry)
					// Deferred: the "what dies soon" ladder is chosen once the
					// whole walk is done, because the window depends on how
					// soon ANY platform's next package expires.
					livePackages = append(livePackages, entry)
				} else {
					view.Depleted = append(view.Depleted, entry)
				}
			}
		}
		view.Groups = append(view.Groups, group)
	}

	sort.SliceStable(view.Accounts, func(i, j int) bool {
		a, b := view.Accounts[i], view.Accounts[j]
		if (a.Rank == 0) != (b.Rank == 0) {
			return b.Rank == 0 // ranked accounts first
		}
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		return a.Provider < b.Provider
	})
	sort.SliceStable(view.Packages, func(i, j int) bool {
		a, b := view.Packages[i], view.Packages[j]
		if a.EndSort.IsZero() != b.EndSort.IsZero() {
			return !a.EndSort.IsZero()
		}
		if !a.EndSort.Equal(b.EndSort) {
			return a.EndSort.Before(b.EndSort)
		}
		if a.Rank != b.Rank {
			return a.Rank < b.Rank
		}
		return a.Remain > b.Remain
	})
	// Each group is ordered on its own: within a platform the ranked accounts
	// lead and the packages run soonest-expiry-first, and no row from another
	// platform ever enters the comparison.
	for index := range view.Groups {
		group := &view.Groups[index]
		sort.SliceStable(group.Accounts, func(i, j int) bool {
			a, b := group.Accounts[i], group.Accounts[j]
			if (a.Rank == 0) != (b.Rank == 0) {
				return b.Rank == 0 // ranked accounts first
			}
			return a.Rank < b.Rank
		})
		sort.SliceStable(group.Packages, func(i, j int) bool {
			a, b := group.Packages[i], group.Packages[j]
			if a.EndSort.IsZero() != b.EndSort.IsZero() {
				return !a.EndSort.IsZero()
			}
			if !a.EndSort.Equal(b.EndSort) {
				return a.EndSort.Before(b.EndSort)
			}
			if a.Rank != b.Rank {
				return a.Rank < b.Rank
			}
			return a.Remain > b.Remain
		})
	}
	// The urgent list is ordered soonest-first ACROSS platforms on purpose: the
	// whole point is "what dies first", which is a question about time, not
	// about which platform owns the package. Tie-breaks stay deterministic.
	// The window is picked first, widening 24h -> 3d -> 7d until it catches
	// something, so the list is never empty while credits are still dying.
	view.UrgentWindow, view.UrgentLabel, view.Urgent = pickUrgentWindow(livePackages, now)
	sort.SliceStable(view.Urgent, func(i, j int) bool {
		a, b := view.Urgent[i], view.Urgent[j]
		if !a.EndSort.Equal(b.EndSort) {
			return a.EndSort.Before(b.EndSort)
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Remain > b.Remain
	})
	return view
}

func packageEnd(value string) time.Time {
	text := strings.TrimSpace(value)
	if text == "" {
		return time.Time{}
	}
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		candidate := text
		if len(candidate) > len(layout)+2 && layout == "2006-01-02 15:04:05" {
			candidate = candidate[:len(layout)]
		}
		if parsed, err := time.ParseInLocation(layout, candidate, time.Local); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "-"
}

// formatPausedModels renders an account's model-scoped 6004 windows for the
// panel: "deepseek-v4.1-flash 至 09-29 11:25". Instants already in the past are
// dropped — the plugin ignores lapsed windows, so showing them would be a lie.
// The list is sorted so the rendering is stable between refreshes.
func formatPausedModels(paused map[string]string) string {
	if len(paused) == 0 {
		return ""
	}
	now := time.Now()
	parts := make([]string, 0, len(paused))
	for model, until := range paused {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(until))
		if err != nil || !at.After(now) {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s 至 %s", model, at.Local().Format("01-02 15:04")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "；")
}

// rotationTier reports the tier the collector recorded for an auth id, or "" for
// a state file written before tiers were published.
func rotationTier(ranking providerRanking, authID string) string {
	if ranking.Rotation == nil {
		return ""
	}
	return ranking.Rotation.Tiers[authID]
}

// ---------------------------------------------------------------------------
// HTML
// ---------------------------------------------------------------------------

var panelTemplate = template.Must(template.New("panel").Funcs(template.FuncMap{
	"remaining": func(pkg panelPackage) template.HTML {
		if pkg.Size > 0 {
			return template.HTML(fmt.Sprintf("<b>%d</b> / %d", pkg.Remain, pkg.Size))
		}
		return template.HTML(fmt.Sprintf("<b>%d</b>", pkg.Remain))
	},
	// until renders how long is left before a package dies, e.g. "5h12m".
	// A past deadline still prints (the collector may lag behind reality), so
	// an expired-but-unrefreshed row never silently disappears from the list.
	"until": func(end time.Time) string {
		if end.IsZero() {
			return "无到期日"
		}
		left := time.Until(end)
		if left <= 0 {
			return "已过期"
		}
		left = left.Round(time.Minute)
		days := int(left.Hours()) / 24
		hours := int(left.Hours()) % 24
		minutes := int(left.Minutes()) % 60
		if days > 0 {
			return fmt.Sprintf("%dd%dh", days, hours)
		}
		if hours > 0 {
			return fmt.Sprintf("%dh%dm", hours, minutes)
		}
		return fmt.Sprintf("%dm", minutes)
	},
}).Parse(panelHTML))

func renderPanel(cfg pluginConfig, state *routeState) []byte {
	var builder strings.Builder
	view := buildView(cfg, state)
	if err := panelTemplate.Execute(&builder, view); err != nil {
		return []byte("<pre>panel render error: " + template.HTMLEscapeString(err.Error()) + "</pre>")
	}
	return []byte(builder.String())
}

func renderNotFound(sub string) []byte {
	return []byte("<p>unknown panel path: " + template.HTMLEscapeString(sub) + "</p>" +
		`<p><a href="` + resourceBase + `/panel">← Expiry Router panel</a></p>`)
}

const panelHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Expiry Router · 账号优先级</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 13px/1.6 -apple-system,BlinkMacSystemFont,"SF Pro Text","PingFang SC",sans-serif;
         margin: 0; padding: 20px 24px 48px; background: Canvas; color: CanvasText; }
  h1 { font-size: 17px; margin: 0 0 4px; }
  h2 { font-size: 14px; margin: 26px 0 8px; }
  .meta { color: GrayText; margin-bottom: 14px; }
  .meta code { background: color-mix(in srgb, CanvasText 8%, Canvas); padding: 1px 5px; border-radius: 4px; }
  .banner { padding: 8px 12px; border-radius: 8px; margin: 12px 0; }
  .banner.warn { background: color-mix(in srgb, #f5a623 22%, Canvas); }
  .banner.err  { background: color-mix(in srgb, #ff4d4f 22%, Canvas); }
  .cards { display: flex; gap: 12px; flex-wrap: wrap; margin: 14px 0 4px; }
  .card { border: 1px solid color-mix(in srgb, CanvasText 18%, Canvas); border-radius: 10px;
          padding: 10px 14px; min-width: 120px; }
  .card .k { color: GrayText; font-size: 11px; }
  .card .v { font-size: 20px; font-weight: 600; font-variant-numeric: tabular-nums; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid color-mix(in srgb, CanvasText 12%, Canvas);
           font-variant-numeric: tabular-nums; }
  th { color: GrayText; font-weight: 600; font-size: 11px; text-transform: uppercase; letter-spacing: .04em; }
  tr:hover td { background: color-mix(in srgb, CanvasText 6%, Canvas); }
  .num { text-align: right; }
  .rank { font-weight: 700; }
  .pill { display: inline-block; padding: 1px 7px; border-radius: 999px; font-size: 11px;
          background: color-mix(in srgb, CanvasText 12%, Canvas); }
  .pill.hot { background: color-mix(in srgb, #ff9f0a 30%, Canvas); }
  .pill.off { background: color-mix(in srgb, #ff4d4f 26%, Canvas); }
  .muted { color: GrayText; }
  details { margin-top: 10px; }
  summary { cursor: pointer; color: GrayText; }
  h3.platform { font-size: 13px; margin: 18px 0 6px; padding: 4px 10px; border-radius: 8px;
                background: color-mix(in srgb, CanvasText 8%, Canvas); }
  h3.platform .muted { font-weight: 400; font-size: 11px; }
</style>
</head>
<body>
<h1>{{.Plugin}} <span class="muted">v{{.Version}}</span></h1>
<div class="meta">
  策略 <code>{{.Strategy}}</code> · 排序文件 <code>{{.StatePath}}</code><br>
  采集时间 {{if .GeneratedAt}}{{.GeneratedAt}}（{{.Age}}前，上限 {{.MaxAge}}）{{else}}—{{end}}
</div>

{{if .ParseNote}}<div class="banner err">{{.ParseNote}}</div>{{end}}
{{if .Stale}}<div class="banner warn">排序已超过上限（{{.Age}} &gt; {{.MaxAge}}）：插件当前<b>不干预</b>选号，交回宿主内置调度。请运行采集器刷新。</div>{{end}}

<h2>{{.UrgentLabel}}内即将失效的积分包（优先烧这些）</h2>
{{if .Urgent}}
<table>
  <thead><tr>
    <th>剩余时间</th><th>到期日</th><th>平台</th><th>账号</th>
    <th>包名</th><th class="num">剩余 / 总量</th>
  </tr></thead>
  <tbody>
  {{range .Urgent}}
    <tr>
      <td><span class="pill hot">{{until .EndSort}}</span></td>
      <td>{{.End}}</td>
      <td>{{.Provider}}</td>
      <td>{{.Account}}</td>
      <td>{{.Name}}</td>
      <td class="num">{{.Remain}} / {{.Size}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
<div class="muted" style="margin-top:6px">
  窗口按 24 小时 → 3 天 → 7 天逐级放宽，取第一个有内容的档位，当前为
  <b>{{.UrgentLabel}}</b>；上方「剩余时间」是相对现在的真实倒计时。<br>
  {{if ne .UrgentLabel "24 小时"}}<b>注意：24 小时内没有包到期</b>，所以这里放宽到了 {{.UrgentLabel}}，今天不必抢着烧。<br>{{end}}
  本表<b>只用于查看</b>，故意把两个平台按到期时间混排，方便判断先烧哪个。
  调度器<b>不</b>读这张表：它仍然只在自己平台内排序账号（见下方「账号优先级」），
  跨平台既不相加积分、也不互相比较到期日。
</div>
{{else}}
<div class="muted" style="margin:6px 0 0">未来 7 天内没有积分包到期，暂时没有需要抢着烧的额度。</div>
{{end}}

<div class="cards">
  <div class="card"><div class="k">账号</div><div class="v">{{len .Accounts}}</div></div>
  <div class="card"><div class="k">积分包</div><div class="v">{{len .Packages}}</div></div>
  <div class="card"><div class="k">参与排序的 provider</div><div class="v" style="font-size:13px">{{range .Providers}}{{.}} {{end}}</div></div>
  {{if .PausedCount}}<div class="card"><div class="k">上游限流中的账号</div><div class="v">{{.PausedCount}}</div></div>{{end}}
</div>

{{if .Groups}}
<h2>各平台合计（平台之间不合并，平台内账号合并排序）</h2>
<table>
  <thead><tr>
    <th>Provider</th><th class="num">账号</th><th class="num">积分包</th>
    <th class="num">本平台可用合计</th><th class="num">本平台即将作废</th>
  </tr></thead>
  <tbody>
  {{range .Groups}}
    <tr>
      <td><b>{{.Provider}}</b></td>
      <td class="num">{{len .Accounts}}</td>
      <td class="num">{{len .Packages}}</td>
      <td class="num">{{.TotalRemain}}</td>
      <td class="num">{{.TotalAtRisk}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
<div class="muted" style="margin-top:6px">
  每个平台只统计自己的账号：同一平台的账号互相竞争排序（最早到期的包先烧），
  不同平台的积分<b>从不相加</b>，也不互相比较到期日。
</div>
{{end}}

{{if .Rotations}}
<div class="banner">
  <b>轮换分组</b>：<b>同一平台内</b>到期日相差在窗口内的账号视为同一批，按顺序轮流使用（游标每轮 +1），
  避免只烧一个账号；<b>24 小时内到期</b>的账号单独成组并<b>恒定优先</b>，先把即将作废的积分用掉。
  分组只在自己的平台内进行，跨平台不轮换也不合并。
  {{range .Rotations}}<div class="muted">分组 {{.Groups}} · 窗口 {{.WindowDays}} 天 · 游标 {{.Cursor}} · 24 小时内到期 {{.Urgent}} 个</div>{{end}}
</div>
{{end}}

<h2>账号优先级（插件实际使用的顺序 · 按平台分组）</h2>
{{range .Groups}}
<h3 class="platform">{{.Provider}}
  <span class="muted">本平台可用合计 {{.TotalRemain}} · 即将作废 {{.TotalAtRisk}}</span>
</h3>
<table>
  <thead><tr>
    <th>#</th><th>账号</th><th>最早到期</th>
    <th class="num">即将作废</th><th class="num">可用合计</th><th class="num">包数</th><th>状态</th><th>上游限流</th>
  </tr></thead>
  <tbody>
  {{range .Accounts}}
    <tr>
      <td class="rank">{{if .Rank}}{{.Rank}}{{else}}—{{end}}</td>
      <td>{{.Nickname}} <span class="muted">{{.UID}}</span></td>
      <td>{{if .Soonest}}{{.Soonest}}{{else}}<span class="muted">未知</span>{{end}}</td>
      <td class="num">{{.AtRisk}}</td>
      <td class="num">{{.Total}}</td>
      <td class="num">{{.Packages}}</td>
      <td>
        {{if .Disabled}}<span class="pill off">已禁用</span>{{end}}
        {{if .Exhausted}}<span class="pill off">已耗尽</span>{{end}}
        {{if and (not .Disabled) (not .Exhausted)}}<span class="pill">可用</span>{{end}}
        {{if not .Rank}}<span class="pill">未参与排序</span>{{end}}
        {{if .Rotation}}<span class="pill">{{.Rotation}}</span>{{end}}
      </td>
      <td>
        {{if .Paused}}<span class="pill off">限流</span> <span class="muted">{{.Paused}}</span>
        {{else}}<span class="muted">—</span>{{end}}
      </td>
    </tr>
  {{else}}
    <tr><td colspan="8" class="muted">该平台没有账号数据（采集器还没跑，或 provider 未登录）</td></tr>
  {{end}}
  </tbody>
</table>
{{else}}
<p class="muted">没有账号数据（采集器还没跑，或 provider 未登录）</p>
{{end}}

<h2>积分包 · 按到期日升序（越靠上越该先烧 · 按平台分组）</h2>
{{range .Groups}}
<h3 class="platform">{{.Provider}}
  <span class="muted">{{len .Packages}} 个包 · 本平台剩余合计 {{.TotalRemain}}</span>
</h3>
<table>
  <thead><tr>
    <th>到期日</th><th>账号</th><th>包名</th>
    <th class="num">剩余 / 总量</th><th class="num">优先级</th>
  </tr></thead>
  <tbody>
  {{range .Packages}}
    <tr>
      <td>{{if .End}}{{.End}}{{else}}<span class="muted">无到期日</span>{{end}}
          {{if .IsSoonest}}<span class="pill hot">该账号最先到期</span>{{end}}</td>
      <td>{{.Account}}</td>
      <td>{{.Name}}</td>
      <td class="num">{{remaining .}}</td>
      <td class="num">{{if .Rank}}{{.Rank}}{{else}}—{{end}}</td>
    </tr>
  {{else}}
    <tr><td colspan="5" class="muted">该平台没有积分包数据</td></tr>
  {{end}}
  </tbody>
</table>
{{else}}
<p class="muted">没有积分包数据</p>
{{end}}

<details>
  <summary>已用完的包（{{len .Depleted}} 个，仅存档）</summary>
  <table>
    <thead><tr><th>到期日</th><th>账号</th><th>包名</th><th class="num">剩余 / 总量</th></tr></thead>
    <tbody>
    {{range .Depleted}}
      <tr class="muted"><td>{{.End}}</td><td>{{.Provider}} · {{.Account}}</td><td>{{.Name}}</td>
          <td class="num">{{.Remain}} / {{.Size}}</td></tr>
    {{else}}
      <tr><td colspan="4" class="muted">没有已用完的包</td></tr>
    {{end}}
    </tbody>
  </table>
</details>

<details>
  <summary>脚本接口</summary>
  <p class="muted">
    JSON：<code>{{.StatePath}}</code> 对应的
    <code>GET /v0/management/plugins/{{.Plugin}}/state</code>（原始排序）与
    <code>/preview</code>（<code>by_provider</code> 给出每个平台自己的账号、积分包与合计；顶层
    <code>accounts</code> / <code>packages</code> 是带 provider 标记的平铺副本，渲染前请先按平台分组，
    不要跨平台求和）<br>
    刷新排序：<code>wb-route</code>（采集器，launchd 每 30 分钟一次）
  </p>
</details>
</body>
</html>
`
