# StepFun Code-GUI

**v1.0.0** · [简体中文](README.md) | [English](README.en.md)

An orchestration visualisation panel for Step Code. `subagent` and `workflow` runs
executing in the terminal are rendered live in the browser as a liquid-glass interface.

> The internal package name is `step-orchestra`; the repository and product name is StepFun Code-GUI.

![Architecture](docs/architecture.svg)

---

## Architectural rationale

Step Code's UI layer is `pi-tui`, which renders terminal characters differentially.
Extensions have no public API for injecting custom graphical components, and a terminal
cannot express real blur or refraction. Given that constraint, the system is divided
across four processes by responsibility:

| Layer | Responsibility |
|---|---|
| TypeScript extension | Normalises host events into JSONL and writes them to a child process's stdin; contains no business logic |
| Go gateway | Reconstructs topology and maintains state, pushed over SSE |
| Browser panel | Carries all graphical rendering and animation |
| Reverse channel | Returns commands via the gateway's stdout, used to send messages and apply credentials |

The principal benefit of this split is change isolation: host API adjustments affect only
the `extensions/` directory, and the panel can be developed and debugged independently of Step Code.

---

## Quick start

```bash
# 1. Build the Go gateway (roughly 10 MB, self-contained, no runtime dependencies)
cd gateway
go build -o ../bin/step-orchestra-gateway .      # append .exe on Windows

# 2. Local end-to-end verification (Step Code not required)
node tools/mock-feed.mjs

# 3. To inspect the panel manually
node tools/mock-feed.mjs --serve
```

`mock-feed.mjs` replays a complete scenario (one parallel workflow fan-out and one subagent
chain), then requests `/api/snapshot` to run assertions and report results.

---

## Installing into Step Code

```bash
step install /absolute/path/to/step-orchestra
step list
```

Once installed, run `/orchestra` inside Step Code to print the panel address:

```
[step-orchestra] panel http://127.0.0.1:47810/?t=<token>
```

The gateway starts automatically when the extension loads and exits when the session ends;
no further configuration is required.

### Package structure

`package.json` declares its entry point via `"pi": { "extensions": ["extensions/index.ts"] }`
(matching the official `with-deps` example; a **top-level** `extensions` field is not recognised by the host).

`step install` records the source in `~/.stepcode/config.toml`:

```toml
# StepCode configuration
packages = [ "E:\\Project\\...\\step-orchestra" ]
```

> The official documentation describes `~/.stepcode/agent/settings.json`, but 0.1.1
> was observed writing to `~/.stepcode/config.toml`. Treat `step list` as authoritative.

Installation can also be bypassed with `step -e <entry path>` for temporary loading.

---

## Event contract

The extension subscribes to the following host events (`pi.on(event, handler)`);
field definitions defer to `extensions/types.ts`.

| Host event | Purpose |
|---|---|
| `session_start` | Session metadata (ID / cwd / model) |
| `agent_start` / `agent_end` / `agent_settled` | Turn and overall settlement |
| `tool_call` | Node creation; `subagent` / `workflow` are flagged as orchestration nodes and their mode extracted |
| `tool_execution_update` | **Primary source of orchestration progress.** The payload sits under `event.partialResult` on 0.1.1 and `event.details` on 0.84.x; both are parsed |
| `tool_result` | Terminal node state, duration, token usage |
| `message_start` / `message_update` / `message_end` | Conversation stream: `assistantMessageEvent.text_delta` accumulates into a streaming bubble |
| `session_shutdown` | Shut the gateway down |

`message_update` fires once per token; only `assistantMessageEvent.type === "text_delta"`
increments are consumed. Thinking and tool-call deltas are not forwarded, as they already
surface as tool nodes.

### Upstream resilience

`extensions/progress.ts` maintains an **alias table** for each counter field
(for example `running` covers `running` / `runningCount` / `active` / `activeCount`) and
descends up to three levels through wrappers such as `partialResult`, `progress`,
`workflowProgress`, `snapshot`, and `details`. A renamed host field degrades to a
**partial read** rather than a blank panel.

Observed differences between upstream versions:

| Item | 0.1.1 | 0.84.x |
|---|---|---|
| Progress payload field | `event.partialResult` | `event.details` |
| `subagent` tool | Provided by an example extension; arguments are `{agent\|tasks\|chain}` | Built in, with an additional `workflow` tool |
| Install config location | `~/.stepcode/config.toml` | Documented as `settings.json` |
| Package entry declaration | `"pi": { "extensions": [...] }` | — |

`inferMode()` therefore derives the fan-out mode from **argument shape**: a `chain` array
means chain, a `tasks` array means parallel, and `agent` or `task` means single.
It does not depend on an explicit `mode` field.

---

## Security design

| Item | Measure |
|---|---|
| Network exposure | Bound to `127.0.0.1` only; port conflicts resolve by incrementing (up to 12 attempts) |
| Authentication | A 24-byte random token per run; `/events` and `/api/snapshot` both validate it using constant-time comparison |
| Credential leakage | Arguments pass through `redact.ts`: values under `key` / `token` / `secret` / `password` / `authorization` / `cookie` are replaced wholesale with `***` |
| Payload growth | Strings truncate at 2048 characters, at most 24 keys per level, recursion depth capped at 4 |
| Injection | The front end writes host data exclusively through `textContent`; `innerHTML` is used only for static icon paths declared in-file |
| Data at rest | No files are written; tokens exist only in extension memory |

---

## Conversation mode and view switching

The panel hosts two views: **Orchestration** (tool and subagent topology) and
**Conversation** (interaction with the agent). Both remain mounted within the same grid
cell; switching only toggles visibility.

### The switch control

| Item | Design |
|---|---|
| Position | Right end of the topbar, immediately left of the connection indicator; a global view control, not owned by either panel column |
| Label | Shows the **target mode**: "Conversation" while in the orchestration view, "Orchestration" while in the conversation view |
| Icon | Follows the label — a speech bubble when the target is conversation, a node graph when it is orchestration; switched in CSS via `data-target`, without replacing DOM |
| Hover | Lifts 1 px, border turns accent purple, glass highlight strengthens (240 ms) |
| Press | `scale(0.972)`, transition compressed to 90 ms |
| Focus | `:focus-visible` 2 px purple ring; keyboard reachable |
| Mid-transition | `aria-busy="true"` with `pointer-events: none`, suppressing repeated activation within the 240 ms window |
| Disabled | Switching is blocked while the event stream is down (`opacity: .45`, `cursor: not-allowed`, no hover feedback) |

### Why switching does not stutter, flicker, or shift

| Risk | Handling |
|---|---|
| Layout shift | Both views overlap in the same `grid-area: stage` cell, so container size is constant |
| Rebuild flicker | Views are **never unmounted**; switching only changes `opacity` and `visibility` |
| Lost scroll | The DOM survives, so scroll positions in both the tree and the conversation are preserved |
| Lost selection | The selected node is a DOM attribute and is unaffected by switching |
| Lost draft | The input's DOM survives; both content and caret position persist |
| Data reload | Switching issues **no requests**; SSE keeps writing to both views |
| Animation races | The 240 ms `aria-busy` window suppresses repeat activation; `prefers-reduced-motion` reduces transitions to 0.01 ms |

### Conversation data flow

```
input box ──POST /api/send──▶ gateway ──one JSON line on stdout──▶ extension ──pi.sendUserMessage()──▶ host
host ──message_start/update/end──▶ extension (accumulates text_delta) ──▶ gateway ──SSE──▶ bubble updates in place
```

This reverse channel is the only downstream path in the system: stdin carries upstream
events, stdout carries downstream commands.

The send button's disabled state is a compound condition: empty input, a busy agent
(`ctx.isIdle()` false), or a downed event stream. Sending while the agent is busy does not
fail — the extension downgrades automatically to `deliverAs: "followUp"` and queues the message.

Message bubbles reuse DOM keyed by id, and streaming updates touch only the text node
rather than rebuilding the list. Scrolling uses a bottom-pinning strategy (auto-follow
within 72 px of the bottom, otherwise a "Back to bottom" button appears) so that scrolling
back through history is not interrupted.

---

## API profile switching

Switch between multiple accounts' coding plans without restarting.

### Configuration storage

| Item | Decision |
|---|---|
| Location | `~/.stepcode/agent/step-orchestra/profiles.json` (written by the gateway) |
| Permissions | Directory `0700`, file `0600`; best effort on Windows, where ACLs govern |
| Atomicity | Written to `.tmp` then renamed, so a crash cannot truncate the original |
| Format | `{ version, activeId, profiles: [{ id, name, provider, apiKey, baseUrl?, addedAt }] }` |
| Encryption | **None.** The same approach as `.netrc` and `~/.aws/credentials`: plaintext plus file permissions |
| Browser | **Never sees plaintext.** The list endpoint returns only `keyHint` (for example `sk-m…7890`) |

Plaintext is held by the gateway rather than in browser localStorage because localStorage is
fully readable by same-origin scripts, and the panel itself runs on localhost. Placing a key
in the browser would be equivalent to handing it to any injected script.

### How switching takes effect immediately

Step Code resolves credentials through the model registry. The documentation states that
`registerProvider`, when called after the initial load phase, **takes effect immediately
without `/reload`**; the config form also overrides only the fields it receives, leaving the
model catalogue intact.

```
click to switch → POST /api/profiles/activate → gateway writes activeId
                → stdout emits action{apply_profile, profile}
                → extension calls pi.registerProvider(provider, { apiKey })
                → the next request uses the new credential
```

Because only `apiKey` (and optionally `baseUrl`) is passed, that provider's model list is not reset.

### Detecting invalid credentials

Rather than probing the endpoint, the extension **observes real traffic**: it subscribes to
`after_provider_response` and, on 401 / 403, broadcasts `profile_status`. The panel then marks
the active profile in red with the reason. This produces no extra requests and no extra billing.

Static validation on the form side: name and provider are required; a key is required when
creating; keys shorter than 12 characters are flagged as "unusual length".

### Interaction design

| Element | Behaviour |
|---|---|
| Trigger | Right side of the topbar, between metrics and the conversation switch; shows a status dot, account name, and masked key |
| Status dot | Green for usable; grey for missing key; amber for unusual length; red for a rejected credential (with a breathing animation) |
| Expanding | Click opens a glass popover, syncing `aria-expanded` and rotating the caret |
| Closing | Click outside, press Esc, or click again |
| List row | Name + a "Current" badge + provider / mask / baseUrl + status description; the active row cannot be re-clicked |
| Inline actions | Edit and delete; deletion goes through a native `confirm` |
| Hover / press | Consistent with the mode switch: lifts 1 px / `scale(0.978)` |
| Editing | Leaving the key blank keeps the stored value (merged server-side), so the browser never has to round-trip a secret it was never given |

---

## Token usage display

A single line of metric text between the conversation log and the composer, modelled on the
reply footer of
[`Neriah-Ado/stepfun-usage-monitor`](https://github.com/Neriah-Ado/stepfun-usage-monitor).

```
537.3 tok/s · 首字 3.0s · 输出 223 tok / 生成 0.4s · 均值 494.9 · 累计 51.3k tok · 上下文 168k/200k · 接近上限 · 10:23:04
```

| Item | Design |
|---|---|
| Form | **A single plain-text line**, monospaced, separated by mid-dots; no progress bar and no panel background of its own |
| Position | Between the conversation log and the composer, belonging to the input area |
| Timing | On `message_end`; values move only when a response lands, so there is no polling and no per-token churn |
| Number rules | **Two tracks**: exact values use thousands separators (`2,762`); cumulative values use compact units — raw below 1k, one decimal from 1k to 10k (`9.8k`), whole thousands from 10k to 1M (`51k`), one decimal of millions at or above 1M (`73.8M`) |
| Clock | Right-aligned and monospaced; excluded from text truncation |
| ≥75% / ≥92% | The whole line turns amber / red and appends "approaching limit" / "about to overflow, consider /compact" |

### How throughput is computed

The only clock available to the extension layer is the event arrival timestamp, hence:

```
TTFT             = arrival of first text_delta − message_start
generation time  = last text_delta − first text_delta
tok/s            = this turn's output tokens ÷ generation time × 1000
```

Two deliberate safeguards: generation windows shorter than 200 ms or longer than 1 h are
excluded from the moving average (they are noise, not throughput); and when only a single
delta arrives, `generationMs` is reported as `null` rather than 0, avoiding an anomalous
division in the front end. TTFT includes IPC latency and therefore reads high, which is
acceptable for a gauge.

### Why the context-window field is retained

The reference project reports **throughput**, whereas the context window is **remaining
headroom** — different semantics. The implementation therefore merges the two: the form and
number rules follow the reference, the fields are predominantly throughput-oriented, and the
context window with its warning is retained, because "approaching the limit" is the only
actionable signal on that line.

---

## Liquid glass implementation

| Layer | Technique |
|---|---|
| Refraction base | `backdrop-filter: blur(22px) saturate(175%)` |
| Glass body | 142° white gradient + 1 px specular border + four-way `inset` shadow simulating thickness |
| Specular highlight | `radial-gradient` following `--gx` / `--gy`, written by `glass.js` |
| Depth | Cards follow `--rx` / `--ry` for a parallax tilt of at most 4.5° |
| Liquid deformation | The empty-state halo applies SVG `feTurbulence` and `feDisplacementMap` |
| Performance guards | Pointer events coalesced via rAF; node entrance animation uses `backwards` rather than `both` (the latter locks `transform` and breaks hover); `contain: layout paint` |
| Degradation | Without `backdrop-filter` support, panels fall back to high-opacity solid colour; `prefers-reduced-motion` disables all animation |

> True refraction requires distorting the content behind the glass, and CSS cannot apply a
> displacement filter to `backdrop`. The impression of refraction is therefore produced by
> highlight, inner shadow, and tilt working together; SVG displacement filters appear only on
> decorative elements to avoid distorting text.

---

## Project structure

```
step-orchestra/
├── extensions/          # Step Code extension (TypeScript, ESM, Node ≥ 22)
│   ├── index.ts         #   Entry: event subscription and forwarding
│   ├── bridge.ts        #   Go child-process lifecycle + reverse channel parsing
│   ├── conversation.ts  #   Conversation mirror: replay / streaming / sending
│   ├── metrics.ts       #   Turn timing and throughput
│   ├── profiles.ts      #   Credential application: registerProvider hot swap
│   ├── progress.ts      #   Defensive WorkflowProgress parsing
│   ├── redact.ts        #   Redaction and truncation
│   └── types.ts         #   Wire protocol
├── gateway/             # Go gateway (standard library, zero third-party dependencies)
│   ├── main.go          #   stdin reader + HTTP/SSE + REST endpoints
│   ├── state.go         #   Topology state machine
│   ├── conversation.go  #   Conversation log (bounded, indexed by id)
│   ├── profiles.go      #   Credential store (atomic writes, masked projection)
│   └── hub.go           #   SSE broadcast
├── web/                 # Browser panel (no build step, plain HTML/CSS/JS)
│   ├── liquid-glass.css #   Design system + view stack + switch + profile popover
│   ├── glass.js         #   Pointer highlight / parallax tilt
│   ├── tree.js          #   Orchestration tree rendering (incremental reuse)
│   ├── chat.js          #   Conversation view (bubble reuse + bottom pinning)
│   ├── profiles.js      #   API profile picker
│   └── app.js           #   SSE client + mode switching + inspector
├── docs/
│   └── architecture.svg #   Architecture diagram
└── tools/
    ├── mock-feed.mjs    # End-to-end verification (orchestration, chat, reverse channel, credentials, usage)
    ├── test-progress.mjs# Host payload parsing regression
    └── check-layers.mjs # Static stacking-order check
```

Front-end debugging loop: run `node tools/mock-feed.mjs --serve`, edit files, and refresh the
browser — no build step is required.

---

## Verification status

| Item | Status |
|---|---|
| Extension loaded and executed by Step Code | Verified (`step -e` and automatic discovery) |
| Automatic discovery (`step install` → `config.toml`) | Verified (`step list`) |
| Extension lifecycle hooks | Verified (gateway exits cleanly after `session_shutdown`, no orphan processes) |
| Port conflict fallback | Verified (47810 in use, falls through to 47811) |
| Gateway ↔ front-end link (orchestration / chat / reverse channel / credentials / usage) | `tools/mock-feed.mjs` **33/33** assertions |
| Host payload parsing | `tools/test-progress.mjs` 11/11 assertions |
| Credential persistence to disk | Verified; `profiles.json` written and contains the key |
| Plaintext key never reaches the browser | Verified; response bodies contain no plaintext |
| Profile popover stacking order | `tools/check-layers.mjs` 11/11 (the script was confirmed to catch the original defect) |
| Reverse-channel authentication (no token rejected) | Verified, returns 401 |
| Real agent event stream (tool_call / messages) | **Not verified** — requires signing in and running a real task |
| `registerProvider` hot swap actually taking effect | **Not verified** — requires signing in and observing the next request |

### Reproducing the stacking-order check

```bash
node tools/check-layers.mjs      # static analysis, 11 assertions
```

The criterion is purely static: **a descendant can never cross the stacking context root it
belongs to**. Verifying the single invariant "`.topbar`'s z-index is strictly greater than
`.stage`'s" is therefore sufficient to guarantee that the profile popover nested inside
`.topbar` cannot be occluded by the content area. The script was validated in reverse during
the fix — reverting `z-index` to `2` immediately drops it to 9/11 with a non-zero exit code.

To confirm manually: click the "Configuration" button in the topbar; the popover should fully
cover the orchestration tree and conversation area. It should behave identically after
narrowing the window below 1080 px (where `.stage` collapses to a single column).

---

## Known limitations

- **The real agent event stream is unverified.** `~/.stepcode/auth.json` is empty (not signed in),
  and step does not establish a real session outside a TTY, so `session_start` and tool events
  never fired. Signing in and issuing one `subagent` call closes this gap.
- **Interface shape is calibrated against 0.1.1.** On 0.84.x, `partialResult` becomes `details`,
  and `subagent` may become a built-in tool alongside a new `workflow`. The parsing layer already
  accommodates both.
- **Subagent detail is aggregated.** Subagents run in separate processes, so the main process
  cannot observe their internal tool calls; the panel can only show the agent rows the host
  provides in its progress snapshot.
- **Fan-out is one level deep.** This is a Step Code constraint, so the tree is at most two levels.
- **Windows builds require the `.exe` suffix.** `bridge.ts` selects the filename by platform.
- **Ports are confined to 47810–47821.** If all are taken, startup fails and `/orchestra`
  reports the reason.

---

## Troubleshooting

| Symptom | Resolution |
|---|---|
| `/orchestra` reports a missing binary | The gateway has not been built; run `cd gateway && go build -o ../bin/step-orchestra-gateway .` |
| Panel is stuck on "connecting" | Check that the `t` parameter in the URL is intact; the token changes on every run |
| No nodes appear | Confirm the run uses `subagent` / `workflow`; ordinary tool calls require turning off the "orchestration only" filter |
| Port already in use | Override the starting port with `STEP_ORCHESTRA_PORT=50000` |
| Custom gateway path | Override with `STEP_ORCHESTRA_BIN=/path/to/gateway` |

---

## License

This project is licensed under the **GNU Affero General Public License v3
(AGPL-3.0-only)**. The full licence text is in [LICENSE](LICENSE).

### Restrictions and requirements

| Mode of use | Requirement |
|---|---|
| Running it yourself (including internally in production) | None |
| Distributing the software or a modified version | Provide the complete corresponding source under AGPL-3.0; retain copyright and licence notices; state the changes and their dates |
| Distributing modifications | The modifications are themselves bound by AGPL-3.0 |
| Providing it as a network service | Offer all remote users a prominent way to obtain the corresponding source (section 13) |
| Integrating into another program | If the result is a derivative work, the whole work must be licensed under AGPL-3.0 |
| Commercial use | Permitted, without waiving any requirement above |

- Copyright and licence notices may not be removed or circumvented;
- Derivative works may not be relicensed under more permissive terms;
- No further restrictions conflicting with this licence may be imposed (GPLv3 section 10);
- On violation the licence terminates automatically and must be reinstated by the copyright holder (section 8).
