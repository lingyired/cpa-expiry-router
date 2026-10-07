# cpa-expiry-router

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) **scheduler plugin**
that routes every request to the account whose credits expire soonest, rotating
between accounts that expire at about the same time, and failing over to another
provider when an upstream model-level rate limit hits every account at once.

## Why

WorkBuddy and Trae credits are bundles of packages, each with its own expiry
(WorkBuddy `CycleEndTime`, Trae `entitlement_base_info.end_time`). The
per-provider plugins never look at expiry — they count a per-account total and
fail over in auth-file order — and the host's own `priority` / `weight` fields
do not survive, because those plugins rewrite their auth files.

The host keeps exactly **one** plugin-scheduler slot (highest `priority` among
plugins declaring the Scheduler capability wins), so a per-provider scheduler
cannot serve both providers. This plugin owns that slot and decides for every
provider that has a ranking.

### What is merged, and what is not

**Platforms are never merged; accounts inside one platform always are.** The
ranking file is keyed by provider, each key holds only that platform's accounts,
and `pick` walks one provider's `order` at a time — so Trae's accounts compete
with each other (its soonest-expiring packs burn first) and WorkBuddy's accounts
compete with each other, while a WorkBuddy account is never compared against a
Trae account and no total is summed across the two. The panel and the DSH status
card follow the same rule: accounts and packages are grouped by platform instead
of pooled.

Only the optional `expiry` strategy steps outside that boundary, comparing
deadlines across providers for a model id both serve. It is off (`strategy:
"order"`), and the reason is measured rather than stylistic: on 2026-09-28 it
sent `deepseek-v4.1-flash` to Trae, which returned an empty `content` with the
answer in `reasoning_content` (1 empty in 5 short prompts, and empty for a long
agent-style prompt).

The panel's top section — **N 内即将失效的积分包**, also published as `urgent`
in `/preview` — is the one deliberate exception, and it is a *view*, not a
policy: it lists every package of **every** platform that still has a balance
and dies within the chosen window, soonest first, so the operator can answer
"what should I burn first?" in one glance. The window **widens 24 hours → 3 days
→ 7 days** and stops at the first step that finds anything, so the section is
never blank while credits are still dying; the step it landed on is published as
`urgent_label` (with the raw length in `urgent_window`) and printed in the
heading, so a 7-day list is never misread as a 24-hour one. The scheduler never
reads any of it, never compares a Trae deadline against a WorkBuddy one, and
never spends one platform's credits on the other's traffic. The table says so in
its own caption.

## Design

```
 provider plugin management APIs                 expiry-router.dylib
 (they already do signing/identity)              (in-process, per request)
 ┌───────────────────────────┐                   ┌──────────────────────┐
 │ /v0/management/plugins/   │                   │ scheduler.pick       │
 │   workbuddy/accounts      │ ── collector ──▶  │  read route-state.json│
 │   trae/accounts           │   (wb-route.py)   │  first candidate of   │
 └───────────────────────────┘        │          │  the provider's order │
                                      ▼          └──────────────────────┘
                             route-state.json  ────────────┘
```

Data collection is deliberately **outside** the plugin: reading each provider's
billing API inside the plugin would mean re-implementing that provider's signing
and client-identity protocol in Go (Trae's `ug` plane carries a full
VSCode-plugin identity header set). The collector asks each provider plugin's
own management API instead. The plugin only makes the per-request decision,
hot-reloading the ranking file.

Nothing is guessed: a provider without a ranking, a missing file, or a ranking
older than `max_state_age_seconds` all fall through to the host's built-in
scheduler (`Handled: false`), so a request can never be broken by this plugin.

### Rotation and pauses

Two rules override a plain "soonest first" walk, both applied by the collector
when it writes `order`:

- **Deadline groups rotate.** Accounts whose deadlines fall within
  `group_window_days` (3) of each other are one group and take turns: each
  collector run advances `rotation_cursor`, so a batch of packages expiring
  together is consumed evenly instead of burning one account while the others
  sit unused. Without this, three packages expiring the same week would drain
  the first account to zero first.
- **A 24h deadline always wins.** Any account expiring within 24 hours forms a
  tier of its own and is ranked ahead of every rotating group, because those
  credits are about to be lost. The exception is deliberately absolute: it
  outranks the rotation even when a fresher account would otherwise be chosen.

An upstream `code 6004` model-level rate limit is handled separately, as a
**model-scoped pause**: the collector parses the reset instant the upstream
error itself carries (falling back to +24h only when it published none), stores
it in `rate-limit-ledger.json`, and republishes it as `paused_models`. The
plugin then skips that credential for that model only, and recovery is automatic
— a lapsed instant simply stops matching, so nothing has to be un-paused. When
every account of a provider is paused for a model, the scheduler defers and the
`model.route` layer redirects to another provider.

## Management panel

The plugin declares the `management_api` capability, so the CPA control panel
lists it like the provider dashboards (icon + config form) and links its pages:

| Route | What it is |
|---|---|
| `GET /v0/resource/plugins/expiry-router/panel` | HTML priority viewer (no API key, like the provider panels) |
| `GET /v0/management/plugins/expiry-router/state` | the raw ranking the scheduler is following |
| `GET /v0/management/plugins/expiry-router/preview` | ranking + every account + every package as JSON, pooled per platform under `by_provider` (there is deliberately no cross-platform `totals` key); `urgent` / `urgent_window` / `urgent_label` are the read-only cross-platform "dies soon" list and the ladder step it used |
| `GET /v0/resource/plugins/expiry-router/models` | HTML model picker (tick what DSH offers) |
| `GET /v0/management/plugins/expiry-router/models/state` | current model decision as JSON |
| `POST /v0/management/plugins/expiry-router/models` | save `{mode:manual,ids:[…],max_credit}` / `{mode:auto,max_credit}` and rewrite the DSH list |
| `POST /v0/management/plugins/expiry-router/models/reset` | drop the manual selection, follow the threshold |

The viewer is rendered from `route-state.json` alone (no extra provider calls).
Every table is **grouped by platform**, because that is how the scheduler works:
a platform's accounts are ranked against each other, and nothing is compared or
summed across platforms.

- **N 内即将失效的积分包** — at the very top, the packages that die within the
  chosen window (24 hours, widened to 3 days then 7 days when nothing is due
  sooner) across *all* platforms, soonest first, with a live countdown. The
  heading names the step in use, and a widened list says so explicitly.
  Read-only: the caption states that the scheduler does not consult it;
- **账号优先级** — the order the scheduler actually uses, grouped per provider,
  with each account's soonest expiry, the amount expiring in that window, its
  available total and its package count; each group carries that platform's own
  pooled total, never a cross-platform one;
- **积分包 · 按到期日升序** — every package that still has a balance, in one
  section per provider, soonest first, badged `该账号最先到期`;
- **已用完的包** — the depleted ones, kept for reference;
- a staleness banner when the ranking is older than `max_state_age_seconds`
  (i.e. when the plugin is currently not intervening).

The **Model picker** (`/models`) lists every model the provider exposes — credit
multiplier, context window, vision support, promo badges and a real liveness
probe result — as one table sorted by multiplier, with:

- an editable **threshold** (`< 0.15x` by default): "按此阈值自动选择" re-runs the
  automatic selection and writes the DSH list immediately; the value is mirrored
  into `wb-models.conf` so the CLI writer agrees;
- a client-side **"only show < threshold"** filter plus select-all-visible /
  select-none / invert;
- a manual mode where exactly the ticked ids are written (probe-rejected models
  are still refused, a model the probe never touched is allowed, and the model
  DSH has selected as its default is always kept).

Threshold is persisted in `models-selection.json` (`{"mode":"auto","max_credit":0.15}`),
so "回到自动" keeps your threshold and only drops the manual ticks.

### Reasoning levels in the generated region

Each written model carries `reasoningEfforts` when the WorkBuddy catalog reports
levels for it. dsh's pi-ai route reads an absent `reasoningEfforts` from pi-ai's
installed catalog, which describes none of these ids, so a model without one
resolved as non-reasoning and offered no effort selector.

The levels come from the snapshot's `efforts` field, which `wb-models.py` fills
from the catalog's per-model `reasoning` object. The catalog states two
different things and the renderer preserves the difference: a model with
`supportedEfforts` accepts a level per request and gets exactly those levels;
a model carrying only a fixed `effort` ignores every level sent to it and gets
that one level, because advertising a choice the upstream discards would be a
lie. A model with neither gets no `reasoningEfforts` key at all.

Click **Expiry Router** in the control panel's plugin list, or open the panel
route directly.

## Config

```yaml
plugins:
  configs:
    expiry-router:
      enabled: true
      priority: 500          # must beat qoder/trae/workbuddy/zcode (all declare a scheduler)
      strategy: "order"      # order | expiry | off
      state_path: "/Users/you/Library/Application Support/CLIProxyAPI/route-state.json"
      max_state_age_seconds: 21600
      log_picks: true
      # Optional: provider-level failover for the model.route capability.
      model_failover:
        deepseek-v4.1-flash:
          - "trae"
```

Values must not carry trailing `#` comments — the host passes this block as YAML
to plugins that parse it line by line.

## Ranking file

```json
{
  "generated_at": "2026-09-27T03:18:41Z",
  "providers": {
    "workbuddy": {
      "order": ["workbuddy-<uid>.json", "..."],
      "reasons": { "workbuddy-<uid>.json": "最早到期 09-30 23:59 (该窗口 500)" },
      "paused_models": {
        "workbuddy-<uid>.json": { "deepseek-v4.1-flash": "2026-09-29T11:25:14+08:00" }
      },
      "rotation": {
        "cursor": 7, "window_days": 3, "groups": [1, 2], "urgent": 0,
        "tiers": { "workbuddy-<uid>.json": "rotate" }
      }
    },
    "trae": { "order": [] }
  },
  "rotation_cursor": 8
}
```

`order` holds host auth IDs. `pick` walks it and returns the first ID the host
actually offers for this request; an empty `order` means "do not interfere".

`paused_models` is **model-scoped**, not credential-scoped: an upstream 6004
limit only stops the one model it names, so a paused account stays in `order`
and keeps serving every other model. `pick` skips a candidate only for the model
being requested.

`rotation` records the grouping the collector applied: accounts whose deadlines
fall within `window_days` of each other take turns (`cursor` advances one step
per collector run), while any account expiring within 24h is a tier of its own
and always ranks first.

## Two selection layers

The host gives a plugin only **one** scheduler slot, and a scheduler may only
pick among the candidates the host already chose — it cannot name a different
provider. That is enough for account rotation but not for a model-level upstream
limit that hits every account of a provider at once. Hence two capabilities:

| Capability | Scope | Used for |
|---|---|---|
| `scheduler.pick` | one slot, host-chosen candidates | skip paused accounts, rotate within a deadline group, burn the 24h-urgent account first |
| `model.route` | **chained** across plugins | when every workbuddy account is 6004-limited for a model, redirect to another provider |

`model.route` is chained: the host calls every plugin declaring the capability
and takes the first valid `Handled=true` response, so this plugin takes the
capability without displacing anything. It stays off unless `model_failover`
names a target, because the host validates a router's provider target only by
"does this provider have auth?" — not by "does this provider serve this model" —
so a guessed target would be accepted and then break the request.

## Build

Requires Go and a C compiler (CGO).

```bash
cd ~/Documents/github/cpa-expiry-router
GOPROXY="https://goproxy.cn,direct" go mod tidy     # proxy.golang.org may be unreachable
CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags="-s -w" \
  -o build/expiry-router.dylib .
cp build/expiry-router.dylib "/Users/you/Library/Application Support/CLIProxyAPI/plugins/"
# then restart the CPA service (cpa restart)
```

`go.mod` points at the local CLIProxyAPI checkout through a `replace` directive,
so the plugin always builds against the SDK version that is actually installed.

## Verify

```bash
# plugin registered and owns the scheduler slot (its registration line comes first)
curl -s -H "Authorization: Bearer <管理Key>" http://127.0.0.1:8318/v0/management/plugins

# plugin's own log lines (it writes to stderr → logs/launchd.err.log)
grep "expiry-router:" "…/CLIProxyAPI/logs/launchd.err.log" | tail
```

Flip the ranking file by hand and watch the next request follow it — that is the
hot-reload path; nothing else needs restarting.
