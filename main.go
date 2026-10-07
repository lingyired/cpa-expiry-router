// expiry-router — a CLIProxyAPI scheduler plugin that routes each request to
// the account whose credits expire soonest.
//
// Why this exists
// ---------------
// WorkBuddy and Trae credits are bundles of packages, each with its own expiry
// (WorkBuddy `CycleEndTime`, Trae `entitlement_base_info.end_time`). The
// per-provider plugins never look at expiry: they count a per-account total and
// fail over in auth-file order. The host's own priority/weight fields do not
// survive either, because those plugins rewrite their auth files.
//
// The host keeps exactly ONE plugin-scheduler slot (the highest `priority`
// among plugins declaring the Scheduler capability wins), so a per-provider
// scheduler cannot serve both providers. This plugin owns that slot and decides
// for every provider that has a ranking.
//
// Where the data comes from
// -------------------------
// Not from here: reading provider billing APIs means re-implementing each
// provider's signing/header protocol (and Trae's is signed). Instead an
// external collector polls each provider plugin's own management API — which
// already does that work — and writes a small ranking file:
//
//	{
//	  "generated_at": "2026-09-27T11:00:00+08:00",
//	  "providers": {
//	    "workbuddy": { "order": ["workbuddy-<uid>.json", ...], "reasons": {...} },
//	    "trae":      { "order": ["trae-cn-<uid>.json", ...] }
//	  }
//	}
//
// `Pick` then returns the first candidate of that order which the host actually
// offers for this request. Anything unknown, missing or stale falls through to
// the host's built-in scheduler (`Handled: false`), so the plugin can never
// break a request it does not understand.
//
// Scope of the ranking
// --------------------
// Platforms are never merged; accounts inside one platform always are. Each
// provider key in the ranking file holds only its own accounts, and `order` is
// walked per provider, so WorkBuddy's accounts compete with each other and
// Trae's accounts compete with each other — but a WorkBuddy account is never
// compared against a Trae account, and no total is summed across the two. The
// same rule reaches the panel, which groups accounts and packages by provider
// instead of pooling them. Only the optional "expiry" strategy compares
// deadlines across providers, and it is off by default.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName    = "expiry-router"
	pluginVersion = "0.4.0"

	defaultStateName    = "route-state.json"
	defaultMaxStateAge  = 6 * time.Hour
	defaultStateRecheck = time.Second

	// pluginLogo is an inline SVG data URI (a clock), so the management panel
	// shows an icon without hosting an asset anywhere.
	pluginLogo = "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAzMiAzMiI+PGNpcmNsZSBjeD0iMTYiIGN5PSIxNiIgcj0iMTMiIGZpbGw9Im5vbmUiIHN0cm9rZT0iIzRhYThmZiIgc3Ryb2tlLXdpZHRoPSIzIi8+PHBhdGggZD0iTTE2IDguNXY3LjVsNS41IDMuNSIgZmlsbD0ibm9uZSIgc3Ryb2tlPSIjNGFhOGZmIiBzdHJva2Utd2lkdGg9IjMiIHN0cm9rZS1saW5lY2FwPSJyb3VuZCIvPjwvc3ZnPg=="
)

// ---------------------------------------------------------------------------
// configuration (plugins.configs.expiry-router)
// ---------------------------------------------------------------------------

type pluginConfig struct {
	// Strategy is "order" (per-provider ranking: each platform's accounts are
	// ranked against each other, and the first provider the host lists that has
	// an available ranked account wins — the default, and the only mode that
	// keeps platforms separate), "expiry" (that ranking plus a cross-provider
	// comparison, so one model id served by two providers burns whichever
	// package expires first) or "off".
	Strategy string `yaml:"strategy"`
	// StatePath is where the collector writes the provider rankings. Empty
	// means "<executable directory>/route-state.json".
	StatePath string `yaml:"state_path"`
	// MaxStateAgeSeconds rejects rankings older than this (fail-open).
	MaxStateAgeSeconds int `yaml:"max_state_age_seconds"`
	// LogPicks prints one line whenever the selected account changes.
	LogPicks *bool `yaml:"log_picks"`

	// --- model list management (panel: /models) ---
	// ModelsStatePath is the catalogue snapshot written by wb-models.py.
	ModelsStatePath string `yaml:"models_state_path"`
	// DSHPatchPath is the DSH profile patch whose marked region this plugin owns.
	DSHPatchPath string `yaml:"dsh_patch_path"`
	// SelectionPath stores the checkbox selection (absent = follow the threshold).
	SelectionPath string `yaml:"selection_path"`
	// ManageDSH turns the DSH model-list writer on/off.
	ManageDSH *bool `yaml:"manage_dsh"`
	// DSHModelPrefix is prepended to every model name this plugin writes into
	// the DSH patch, so a client that lists models from several CPA routes can
	// tell them apart at a glance. Empty (the default) writes the bare name.
	// It is applied only to the DSH region — the panel's own labels keep the
	// unprefixed name.
	DSHModelPrefix string `yaml:"dsh_model_prefix"`

	// ModelFailover maps a client-requested model id to the providers to try
	// when EVERY workbuddy credential is under an upstream 6004 model-level
	// limit for it, in order of preference:
	//
	//   model_failover:
	//     deepseek-v4.1-flash: ["trae"]
	//
	// It is explicit rather than inferred because the host validates a router's
	// provider target by "does this provider have auth?", not by "does this
	// provider serve this model" — so a guessed target would be accepted and
	// then break the request instead of failing loudly. Empty disables the
	// model.route path entirely, which is the safe default.
	ModelFailover map[string][]string `yaml:"model_failover"`
}

// failoverTargets returns the configured alternate providers for `model`,
// matching case-insensitively because model ids are not case-stable between a
// client request and the executor.
func (c pluginConfig) failoverTargets(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" || len(c.ModelFailover) == 0 {
		return nil
	}
	if targets, ok := c.ModelFailover[model]; ok {
		return targets
	}
	for name, targets := range c.ModelFailover {
		if strings.EqualFold(strings.TrimSpace(name), model) {
			return targets
		}
	}
	return nil
}

// dshModelPrefix returns the configured DSH model-name prefix, verbatim
// (including any trailing separator the operator wrote).
func (c pluginConfig) dshModelPrefix() string {
	return c.DSHModelPrefix
}

func (c pluginConfig) strategy() string {
	if strings.TrimSpace(c.Strategy) == "" {
		return "order"
	}
	return strings.ToLower(strings.TrimSpace(c.Strategy))
}

func (c pluginConfig) statePath() string {
	if p := strings.TrimSpace(c.StatePath); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		if idx := strings.LastIndex(exe, "/"); idx > 0 {
			return exe[:idx] + "/" + defaultStateName
		}
	}
	return defaultStateName
}

func (c pluginConfig) maxAge() time.Duration {
	if c.MaxStateAgeSeconds > 0 {
		return time.Duration(c.MaxStateAgeSeconds) * time.Second
	}
	return defaultMaxStateAge
}

func (c pluginConfig) logPicks() bool {
	return c.LogPicks == nil || *c.LogPicks
}

// ---------------------------------------------------------------------------
// ranking file
// ---------------------------------------------------------------------------

type providerRanking struct {
	Order     []string          `json:"order"`
	Reasons   map[string]string `json:"reasons,omitempty"`
	UpdatedAt string            `json:"updated_at,omitempty"`
	Source    string            `json:"source,omitempty"`
	// Soonest maps an auth id to the Unix second its soonest-expiring live
	// credit package dies. It is the machine-comparable counterpart of the
	// prose in Reasons, and the only field the "expiry" strategy compares
	// across providers; an absent entry means "expiry unknown" (sorts last).
	Soonest map[string]int64 `json:"soonest,omitempty"`
	// Accounts carries the per-account package detail the collector saw, so the
	// management panel can show every package of every account without a second
	// round of provider calls.
	Accounts        []accountDetail `json:"accounts,omitempty"`
	UnknownAccounts []string        `json:"unknown_accounts,omitempty"`
	// PausedModels maps an auth id to the models upstream is currently
	// rate-limiting on that credential (code 6004), each with its own recovery
	// instant. The limit is MODEL-scoped: the credential keeps serving every
	// other model, so a pick for an unaffected model must still consider it.
	// Absent/empty means nothing is paused.
	PausedModels map[string]map[string]string `json:"paused_models,omitempty"`
	// Rotation describes how the collector grouped and rotated the accounts, so
	// the panel can explain a pick instead of just showing an order.
	Rotation *rotationReport `json:"rotation,omitempty"`
}

// rotationReport is the collector's account of the 3-day grouping it applied.
type rotationReport struct {
	Cursor     int     `json:"cursor"`
	WindowDays float64 `json:"window_days"`
	Groups     []int   `json:"groups,omitempty"`
	Urgent     int     `json:"urgent"`
	// Tiers labels each ranked auth id with the tier the collector put it in:
	// "urgent" (expires within 24h — hard priority), "rotate" (shares a deadline
	// with another account and takes turns) or "single". Optional: a state file
	// from an older collector simply has none.
	Tiers map[string]string `json:"tiers,omitempty"`
}

type accountDetail struct {
	AuthID    string          `json:"auth_id"`
	Nickname  string          `json:"nickname"`
	UID       string          `json:"uid"`
	Disabled  bool            `json:"disabled"`
	Exhausted bool            `json:"exhausted"`
	Soonest   string          `json:"soonest,omitempty"`
	AtRisk    int64           `json:"at_risk,omitempty"`
	Total     int64           `json:"total,omitempty"`
	Packages  []packageDetail `json:"packages,omitempty"`
	// PausedModels is the model-scoped 6004 view for this account.
	PausedModels map[string]string `json:"paused_models,omitempty"`
}

type packageDetail struct {
	Name   string `json:"name"`
	Remain int64  `json:"remain"`
	Size   int64  `json:"size"`
	End    string `json:"end,omitempty"`
}

type routeState struct {
	GeneratedAt string                     `json:"generated_at"`
	Providers   map[string]providerRanking `json:"providers"`
}

type stateCache struct {
	mu       sync.Mutex
	path     string
	modTime  time.Time
	size     int64
	checked  time.Time
	state    *routeState
	parseErr string
}

func (c *stateCache) load(path string, recheck time.Duration) *routeState {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.state != nil && now.Sub(c.checked) < recheck {
		return c.state
	}
	info, err := os.Stat(path)
	if err != nil {
		c.checked = now
		if c.state != nil {
			// Keep serving the last good ranking until it ages out.
			return c.state
		}
		c.parseErr = err.Error()
		return nil
	}
	c.checked = now
	if c.state != nil && info.ModTime().Equal(c.modTime) && info.Size() == c.size {
		return c.state
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		c.parseErr = err.Error()
		return c.state
	}
	parsed := &routeState{}
	if err := json.Unmarshal(raw, parsed); err != nil {
		c.parseErr = err.Error()
		return c.state
	}
	c.state = parsed
	c.modTime = info.ModTime()
	c.size = info.Size()
	c.parseErr = ""
	return c.state
}

// ---------------------------------------------------------------------------
// plugin plumbing
// ---------------------------------------------------------------------------

var (
	configMu sync.RWMutex
	config   pluginConfig
	cache    stateCache

	lastMu     sync.Mutex
	lastPick   = map[string]string{}
	firstPick  = true
	pickLogger = log.New(os.Stderr, "", log.LstdFlags)
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
	// ModelRouter declares the `model.route` capability. Unlike the scheduler
	// (one slot, highest priority wins), model routers are CHAINED: the host
	// offers the request to each router in turn and takes the first valid
	// handled answer, skipping routers that decline. So this plugin can add a
	// provider-level failover without displacing anyone, and must return
	// Handled:false whenever it has nothing better to say than the host.
	ModelRouter bool `json:"model_router"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return pickAuth(request)
	case pluginabi.MethodModelRoute:
		return routeModel(request)
	case pluginabi.MethodManagementRegister:
		// The host injects its management BasePath; keep it so the panel routes
		// stay correct if the host ever moves them.
		var registrationRequest pluginapi.ManagementRegistrationRequest
		if err := json.Unmarshal(request, &registrationRequest); err == nil {
			setManagementBasePath(registrationRequest.BasePath)
		}
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := pluginConfig{}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	// Compare a JSON signature, not the struct: the *bool fields are freshly
	// allocated on every unmarshal, so a struct compare always says "changed".
	rawSignature, _ := json.Marshal(cfg)
	signature := string(rawSignature)
	configMu.Lock()
	config = cfg
	changed := signature != lastConfigSignature
	lastConfigSignature = signature
	configMu.Unlock()
	if changed {
		pickLogger.Printf("%s: configured strategy=%s state=%s max_age=%s models_state=%s manage_dsh=%v model_failover=%v",
			pluginName, cfg.strategy(), cfg.statePath(), cfg.maxAge(), cfg.modelsStatePath(), cfg.manageDSH(), cfg.ModelFailover)
	}
	applierOnce.Do(func() { go modelApplierLoop(applierStop) })
	return nil
}

var (
	applierOnce         sync.Once
	applierStop         = make(chan struct{})
	lastConfigSignature string
)

func loadedConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return config
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "lingsmbp",
			GitHubRepository: "https://github.com/lingsmbp/cpa-expiry-router",
			Logo:             pluginLogo,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "strategy",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{"order", "expiry", "off"},
					Description: "order = follow the collector's per-provider ranking; expiry = also compare soonest package expiry across providers serving one model id; off = defer to the host's built-in scheduler.",
				},
				{
					Name:        "state_path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Ranking file written by the collector (default: <executable dir>/route-state.json).",
				},
				{
					Name:        "max_state_age_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Ignore rankings older than this many seconds (default 21600 = 6h).",
				},
				{
					Name:        "log_picks",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Log one line whenever the chosen account changes (default true).",
				},
				{
					Name:        "models_state_path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Model catalogue snapshot written by wb-models.py (default: next to state_path).",
				},
				{
					Name:        "dsh_patch_path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "DSH profile patch whose model list this plugin owns.",
				},
				{
					Name:        "selection_path",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Where the panel's checkbox selection is stored (absent = follow the credit threshold).",
				},
				{
					Name:        "dsh_model_prefix",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Prepended to every model name written into the DSH patch (e.g. \"CPA · W· \"), so models from several CPA routes are tellable apart in the client. Empty writes the bare name; the panel's own labels never carry it.",
				},
				{
					Name:        "manage_dsh",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Write the DSH model list from this plugin (default true).",
				},
				{
					Name:        "model_failover",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Map of client model id → alternate provider(s) to use when EVERY workbuddy credential is under an upstream 6004 model-level limit for that model, e.g. {\"deepseek-v4.1-flash\":[\"trae\"]}. Explicit on purpose: the host checks only that a router's target provider has auth, not that it serves the model, so a guessed target would be accepted and then break the request. Empty (default) disables the model.route failover.",
				},
			},
		},
		Capabilities: registrationCapability{Scheduler: true, ManagementAPI: true, ModelRouter: true},
	}
}

func pickAuth(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := loadedConfig()
	if cfg.strategy() == "off" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	state := cache.load(cfg.statePath(), defaultStateRecheck)
	if state == nil || len(state.Providers) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	if age, ok := stateAge(state); ok && age > cfg.maxAge() {
		pickLogger.Printf("%s: ranking is %s old (> %s), deferring to the host", pluginName, age.Round(time.Minute), cfg.maxAge())
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	providers := requestProviders(req)
	if len(providers) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	if cfg.logPicks() && firstPick {
		firstPick = false
		ids := make([]string, 0, len(req.Candidates))
		for _, candidate := range req.Candidates {
			ids = append(ids, candidate.Provider+":"+candidate.ID)
		}
		pickLogger.Printf("%s: first pick provider=%s providers=%v candidates=%v",
			pluginName, req.Provider, providers, ids)
	}

	present := make(map[string]bool, len(req.Candidates))
	for _, candidate := range req.Candidates {
		present[candidate.ID] = true
	}

	// Drop candidates upstream is currently rate-limiting for THIS model (6004).
	// The limit is model-scoped, so the same credential is still a perfectly
	// good answer for any other model — filtering by model is what keeps that
	// capacity usable instead of pausing the account wholesale.
	if paused := pausedForModel(state, providers, req.Model, present); len(paused) > 0 {
		ids := make([]string, 0, len(paused))
		for id, until := range paused {
			ids = append(ids, id+"@"+until)
		}
		sort.Strings(ids)
		pickLogger.Printf("%s: model %s — skipping %d account(s) under an upstream 6004 limit: %v",
			pluginName, req.Model, len(paused), ids)
		for id := range paused {
			delete(present, id)
		}
	}
	if len(present) == 0 {
		// Every candidate is limited for this model. Deferring is deliberate:
		// the host then fails over across providers (e.g. to Trae for the same
		// upstream model) instead of this plugin pinning a credential that is
		// known to be throttled right now.
		pickLogger.Printf("%s: every candidate for model %s is under an upstream 6004 limit — deferring to the host", pluginName, req.Model)
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	if cfg.strategy() == "expiry" {
		provider, id, ok := pickByExpiry(state, providers, present)
		if !ok {
			pickLogger.Printf("%s: no provider offers an available ranked account — deferring", pluginName)
			return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
		}
		notePick(cfg, provider, id, state.Providers[provider].Reasons[id])
		return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: id, Handled: true})
	}

	for _, provider := range providers {
		ranking, ok := state.Providers[provider]
		if !ok || len(ranking.Order) == 0 {
			continue
		}
		for _, id := range ranking.Order {
			if !present[id] {
				continue
			}
			notePick(cfg, provider, id, ranking.Reasons[id])
			return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: id, Handled: true})
		}
		pickLogger.Printf("%s: provider %s has a ranking but none of its accounts is currently available — deferring",
			pluginName, provider)
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}

// pausedForModel returns the candidates upstream is rate-limiting for `model`,
// as `{auth id: recovery instant}`.
//
// The ledger is model-scoped because WorkBuddy's 6004 limit is: one credential
// is throttled for one model while every other model on it keeps working. A
// candidate is skipped only when the collector recorded a window for exactly
// this model that has not lapsed yet — an expired window is ignored here as
// well as in the collector, so recovery needs no restart.
func pausedForModel(state *routeState, providers []string, model string, present map[string]bool) map[string]string {
	if state == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	now := time.Now()
	paused := map[string]string{}
	for _, provider := range providers {
		ranking, ok := state.Providers[provider]
		if !ok {
			continue
		}
		for id, models := range ranking.PausedModels {
			if !present[id] || len(models) == 0 {
				continue
			}
			until, limited := lookupModelWindow(models, model)
			if !limited {
				continue
			}
			if at, err := time.Parse(time.RFC3339, until); err == nil && !at.After(now) {
				continue // window already lapsed → the account is usable again
			}
			paused[id] = until
		}
	}
	return paused
}

// lookupModelWindow finds `model` in a credential's paused-model map, comparing
// case-insensitively (upstream model ids are not case-stable between the client
// request and the executor's log line).
func lookupModelWindow(models map[string]string, model string) (string, bool) {
	if until, ok := models[model]; ok {
		return until, true
	}
	for name, until := range models {
		if strings.EqualFold(name, model) {
			return until, true
		}
	}
	return "", false
}

// routeModel implements the `model.route` capability: a provider-level failover
// for the case the scheduler cannot express.
//
// The scheduler can only pick among the candidates the host already chose, so
// when EVERY WorkBuddy credential is rate-limited for the requested model it has
// nothing left to offer and must defer — leaving the host to fail the request.
// A model router runs earlier and may name a different PROVIDER entirely, which
// is what lets the same upstream model be served elsewhere.
//
// The redirect target comes from `model_failover` in this plugin's config, never
// from a guess. The host validates a router's provider target only by "does this
// provider have auth?", not by "does this provider serve this model", so an
// invented target would not fail loudly — it would be accepted and then break
// the request. Requiring the operator to name the pair keeps a wrong answer
// impossible by construction, and an empty map (the default) disables this path
// entirely.
func routeModel(raw []byte) ([]byte, error) {
	var req pluginapi.ModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	decline := func() ([]byte, error) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	cfg := loadedConfig()
	if cfg.strategy() == "off" {
		return decline()
	}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		return decline()
	}
	targets := cfg.failoverTargets(model)
	if len(targets) == 0 {
		return decline()
	}

	available := map[string]bool{}
	for _, provider := range req.AvailableProviders {
		available[provider] = true
	}
	state := cache.load(cfg.statePath(), defaultStateRecheck)
	if state == nil {
		return decline()
	}
	ranking, ok := state.Providers["workbuddy"]
	if !ok || len(ranking.Order) == 0 {
		return decline()
	}

	// Only act when EVERY workbuddy account that serves this model is limited.
	// One healthy account is reason enough to leave routing alone — this is a
	// last-resort redirect, not a load balancer.
	limited, healthy := 0, 0
	for _, id := range ranking.Order {
		if until, ok := lookupModelWindow(ranking.PausedModels[id], model); ok {
			if at, err := time.Parse(time.RFC3339, until); err == nil && at.After(time.Now()) {
				limited++
				continue
			}
		}
		healthy++
	}
	if limited == 0 || healthy > 0 {
		return decline()
	}

	for _, provider := range targets {
		if provider == "workbuddy" || !available[provider] {
			continue
		}
		pickLogger.Printf("%s: model.route — all %d workbuddy account(s) are under an upstream 6004 limit for %s; routing to provider %s",
			pluginName, limited, model, provider)
		return okEnvelope(pluginapi.ModelRouteResponse{
			Handled:    true,
			TargetKind: pluginapi.ModelRouteTargetProvider,
			Target:     provider,
			Reason:     "all workbuddy credentials are rate-limited (code 6004) for " + model,
		})
	}
	return decline()
}

// pickByExpiry chooses among the available candidates of every requested
// provider by the expiry of the soonest-expiring credit package each account
// holds, so one model id served by two providers burns whichever package dies
// first instead of always preferring the first provider the host lists.
//
// Reachability is already decided by the host (only live candidates arrive in
// `present`), so every entry here is selectable. Ties go to the account with
// more credits inside that soonest window, then to the larger balance; an
// account the collector could not date sorts after every dated one, and the
// host's provider order breaks a remaining tie.
//
// @param state - the loaded ranking file.
// @param providers - provider keys in host order.
// @param present - auth ids the host currently offers.
// @returns the winning provider, auth id, and whether one was found.
func pickByExpiry(state *routeState, providers []string, present map[string]bool) (string, string, bool) {
	type candidate struct {
		provider    string
		id          string
		soonest     int64
		atRisk      int64
		total       int64
		providerIdx int
		rankIdx     int
	}
	ranked := make([]candidate, 0, len(present))
	for providerIdx, provider := range providers {
		ranking, ok := state.Providers[provider]
		if !ok {
			continue
		}
		details := make(map[string]accountDetail, len(ranking.Accounts))
		for _, account := range ranking.Accounts {
			details[account.AuthID] = account
		}
		for rankIdx, id := range ranking.Order {
			if !present[id] {
				continue
			}
			detail := details[id]
			ranked = append(ranked, candidate{
				provider:    provider,
				id:          id,
				soonest:     ranking.Soonest[id],
				atRisk:      detail.AtRisk,
				total:       detail.Total,
				providerIdx: providerIdx,
				rankIdx:     rankIdx,
			})
		}
	}
	if len(ranked) == 0 {
		return "", "", false
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if (a.soonest == 0) != (b.soonest == 0) {
			return b.soonest == 0 // dated accounts first
		}
		if a.soonest != b.soonest {
			return a.soonest < b.soonest
		}
		if a.atRisk != b.atRisk {
			return a.atRisk > b.atRisk
		}
		if a.total != b.total {
			return a.total > b.total
		}
		if a.providerIdx != b.providerIdx {
			return a.providerIdx < b.providerIdx
		}
		return a.rankIdx < b.rankIdx
	})
	winner := ranked[0]
	return winner.provider, winner.id, true
}

func requestProviders(req pluginapi.SchedulerPickRequest) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "mixed" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	add(req.Provider)
	for _, provider := range req.Providers {
		add(provider)
	}
	return out
}

func stateAge(state *routeState) (time.Duration, bool) {
	if state == nil || strings.TrimSpace(state.GeneratedAt) == "" {
		return 0, false
	}
	generated, err := time.Parse(time.RFC3339, strings.TrimSpace(state.GeneratedAt))
	if err != nil {
		return 0, false
	}
	return time.Since(generated), true
}

func notePick(cfg pluginConfig, provider, id, reason string) {
	lastMu.Lock()
	previous := lastPick[provider]
	lastPick[provider] = id
	lastMu.Unlock()
	if previous == id || !cfg.logPicks() {
		return
	}
	if reason != "" {
		pickLogger.Printf("%s: %s → %s (%s)", pluginName, provider, id, reason)
		return
	}
	pickLogger.Printf("%s: %s → %s", pluginName, provider, id)
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
