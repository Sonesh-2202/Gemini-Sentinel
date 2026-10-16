<div align="center">

# 🛡️ Gemini-Sentinel

**Use your Gemini web subscription from any OpenAI-compatible app — no browser automation, no 600 MB headless Chrome.**

[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=flat-square&logo=go)](./go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat-square)](./LICENSE)
[![Transport](https://img.shields.io/badge/Transport-uTLS_%2B_HTTP%2F2-blue?style=flat-square)](#1-architecture--how-it-works)
[![API](https://img.shields.io/badge/OpenAI-Compatible-412991?style=flat-square&logo=openai)](#7-making-your-first-api-calls)
[![Bind](https://img.shields.io/badge/Bind-Loopback_Only-success?style=flat-square)](./SECURITY.md)

[Quick Start](#3-quick-start-build--configure) • [API Examples](#7-making-your-first-api-calls) • [Coding Agents](#12-coding-agents--9router-setup) • [Configuration](#14-configuration-reference) • [Troubleshooting](#16-troubleshooting)

</div>

---

**Gemini-Sentinel** (binary `sentinel`) is a lightweight local Go daemon by **Sonesh SK** that authenticates with `gemini.google.com` using your browser cookies and exposes an OpenAI-compatible gateway (`POST /v1/chat/completions` and `POST /v1/responses`) on your local machine.

> [!WARNING]
> **Security & Unofficial Research Software Notice — Read First**
> - **Unofficial Software:** This project is not affiliated with, endorsed by, or supported by Google. It relies on browser-session cookies, undocumented Gemini Web RPCs, and browser-fingerprint transport (`uTLS`), making it fundamentally different from the official Gemini API. Do not deploy it as a commercial or production service.
> - **Loopback Only:** Safe for local loopback use (`127.0.0.1`, `::1`, `localhost`) when maintained properly. **Never expose port `20133` directly to a LAN or the internet.** The server speaks plaintext HTTP and intentionally refuses to start on non-loopback addresses.
> - **Cookies are Passwords:** Anyone holding your browser cookies or account `.json` files can impersonate your Google session. For remote access, keep `sentinel` on loopback behind a separately hardened HTTPS reverse proxy. See [`SECURITY.md`](./SECURITY.md) for full details.

> [!TIP]
> **Upgrading from Hearth or `gemini-stealth-bridge` (`gsb`)?**
> Gemini-Sentinel continues the Hearth and `gemini-stealth-bridge` line (MIT, © 2026 azrinaldi). Just build and run `sentinel`:
> - Accounts automatically migrate from `~/.hearth` or `~/.gemini-stealth-bridge` to `~/.gemini-sentinel` on first start (originals are left untouched).
> - Legacy `HEARTH_*` and `BRIDGE_*` environment variables, `hearth-<name>` / `gsb-<name>` API keys, and `hearth/` / `gsb/` model prefixes remain 100% backward-compatible.
> - **What's New:** `sentinel --status` preflight checks, `GET /metrics` (Prometheus), `POST /v1/count-tokens`, `SENTINEL_*` settings, and automated CI/tagged releases. See [`CHANGELOG.md`](./CHANGELOG.md).

---

## ✨ Key Capabilities

- **OpenAI-Compatible Endpoints:** Drop-in support for `/v1/chat/completions` and `/v1/responses` (streaming SSE & non-streaming).
- **Zero Headless Browser Overhead:** Communicates directly via Chrome-fingerprinted `uTLS` (`chrome_150_psk`) + HTTP/2.
- **Native Reasoning & Tool Calling:** Maps OpenAI `reasoning_effort` / `thinking` budgets (`low`, `medium`, `high`) and streams live thoughts + function calls.
- **1M+ Context & Session Continuity:** Automatically offloads older conversation turns as text attachments and caches continuation tokens for instant multi-turn resumption.
- **Multi-Account Isolation & Failover:** Per-account keys strictly isolate quota; the admin key unlocks load-balanced failover across accounts.
- **Built-in Guardrails:** Rejects silent upstream model downgrades (`429 model_substitution`), blocks SSRF on remote image fetches, and enforces strict payload limits.
- **Observability:** Offline `--status` preflight validation, live `--probe` quota checks, and `/metrics` Prometheus exposition.

---

## 📖 Table of Contents

1. [Architecture & How It Works](#1-architecture--how-it-works)
2. [Requirements](#2-requirements)
3. [Quick Start: Build & Configure](#3-quick-start-build--configure)
4. [Connect Your Google Account](#4-connect-your-google-account)
5. [Verify Setup (`--probe` & `--status`)](#5-verify-setup---probe----status)
6. [Start the Server](#6-start-the-server)
7. [Making Your First API Calls](#7-making-your-first-api-calls)
8. [Model Selection & Routing Tricks](#8-model-selection--routing-tricks)
9. [Thinking Levels (`low` / `medium` / `high`)](#9-thinking-levels-low--medium--high)
10. [Images, Tool Calling, Long Context & Token Budgets](#10-images-tool-calling-long-context--token-budgets)
11. [Observability (`--status`, `/metrics`)](#11-observability---status-metrics)
12. [Coding Agents & `9router` Setup](#12-coding-agents--9router-setup)
13. [Multiple Google Accounts](#13-multiple-google-accounts)
14. [Configuration Reference](#14-configuration-reference)
15. [API Endpoints & Hard Limits](#15-api-endpoints--hard-limits)
16. [Troubleshooting](#16-troubleshooting)
17. [Everyday Operations & Security Hygiene](#17-everyday-operations--security-hygiene)
18. [Development, Internals & Roadmap](#18-development-internals--roadmap)
19. [Legal, Contributing & License](#19-legal-contributing--license)

---

## 1. Architecture & How It Works

```text
Your App (curl, OpenCode, Cline, Cursor, 9router…)
        │
        │  OpenAI-style HTTP on [http://127.0.0.1:20133/v1](http://127.0.0.1:20133/v1)
        ▼
  ┌──────────────── sentinel ────────────────┐
  │ • Account keys  ──► Locked to 1 account  │
  │ • Admin key     ──► Pool + Admin APIs    │
  │ • Live model & quota validation          │
  │ • Prompt offloader & tool bridge         │
  └────────────────────┬─────────────────────┘
                       │
                       │  Chrome-fingerprint HTTPS (uTLS + HTTP/2)
                       ▼
           gemini.google.com (your web session)
```

1. **Authenticate once:** Log into Gemini in your normal desktop browser.
2. **Import session:** `sentinel` extracts and decrypts your session cookies into `~/.gemini-sentinel/accounts/<name>.json`.
3. **Connect clients:** Point any OpenAI-compatible SDK or coding agent to `http://127.0.0.1:20133/v1`.
4. **Translate & Stream:** `sentinel` converts OpenAI payloads into Gemini Web RPCs, streams SSE deltas back in real time, and rotates cookies automatically in the background.

Each imported account receives **two dedicated API keys**: `sentinel-<name>` and `<name>` (e.g., `sentinel-acc1` and `acc1`). Account keys are strictly isolated—they can never view or spend quota from another account.

---

## 2. Requirements

- **Go 1.26 or newer** (check with `go version`; defined in [`go.mod`](./go.mod)).
- **OS:** Windows (PowerShell), macOS, or Linux, plus `curl` for testing.
- **Browser:** Chrome, Brave, Chromium, Edge, or Firefox with an active logged-in session at `gemini.google.com/app`.
- **Environment:** A local machine you control (do not run as Administrator/root, and do not deploy on shared hosts without reviewing [`SECURITY.md`](./SECURITY.md)).

---

## 3. Quick Start: Build & Configure

### Step 1 — Build from Source

Do not run untrusted prebuilt binaries with access to your browser profile. Build directly from a reviewed checkout:

```sh
git clone [https://github.com/Sonesh-2202/gemini-sentinel.git](https://github.com/Sonesh-2202/gemini-sentinel.git)
cd gemini-sentinel
go mod download

# Linux / macOS
go build -trimpath -o sentinel ./cmd/sentinel

# Windows PowerShell
go build -trimpath -o .\sentinel.exe .\cmd\sentinel

# Verify build
./sentinel --version
```

Alternatively, install directly to your `GOPATH/bin` (requires lowercase module path):

```sh
go install [github.com/sonesh-2202/gemini-sentinel/cmd/sentinel@latest](https://github.com/sonesh-2202/gemini-sentinel/cmd/sentinel@latest)
```

### Step 2 — Create Your Admin Key

Gemini-Sentinel uses two key tiers:

| Key Tier | Unlocks | Format / Example |
| :--- | :--- | :--- |
| **Account Key** | Chat, Responses, Models, Token Count (isolated to that single account) | `sentinel-acc1` or `acc1` *(auto-created)* |
| **Admin Key** (`SENTINEL_API_KEY`) | All accounts (with failover) **+** `/v1/accounts`, `/v1/quota`, `/v1/sessions`, `/v1/refresh`, `/metrics` | 64-char random hex string *(you generate)* |

Generate a strong 64-character hex key:

```sh
python3 -c "import secrets; print(secrets.token_hex(32))"
```

Export it before running the server (see [`config.example.env`](./config.example.env) for a template):

```sh
# Linux / macOS
export SENTINEL_API_KEY="paste-your-64-char-hex-key-here"

# Windows PowerShell
$env:SENTINEL_API_KEY="paste-your-64-char-hex-key-here"
```

> [!IMPORTANT]
> **Admin Key Rules:**
> - Must be **≥ 16 characters**. Weak keys (`public`, `test`, `password`, etc.) cause the server to **refuse startup**.
> - If left unset, admin endpoints return `401`/`403` while per-account keys continue to work normally.
> - Pass keys via HTTP headers (`Authorization: Bearer …`, `x-api-key: …`, or `x-goog-api-key: …`). Keys in URL query strings (`?key=`) are **rejected**.

---

## 4. Connect Your Google Account

Choose **one** of the three methods below. You can repeat this step for multiple accounts (`acc1`, `acc2`, etc. — allowed characters: `A–Z a–z 0–9 . _ -`, max 64 chars).

### Method A — Direct Browser Import (Recommended)

Ensure you are logged into `gemini.google.com` in your browser—no need to close the browser window:

```sh
./sentinel --import-chrome acc1
```

<details>
<summary><b>Using Multiple Browser Profiles? (Click to expand)</b></summary>

Specify the profile name or full directory path:

```sh
# By profile name (Windows, macOS, Linux)
./sentinel --import-chrome acc1 --chrome-profile "Profile 4"

# By profile folder (Linux / macOS)
./sentinel --import-chrome acc1 --chrome-profile ~/.config/google-chrome/Default

# By profile folder (Windows PowerShell / cmd)
.\sentinel.exe --import-chrome acc1 --chrome-profile "C:\Users\<you>\AppData\Local\Google\Chrome\User Data\Profile 4"
```
</details>

`sentinel` extracts and decrypts your `google.com` cookies, verifies them against Gemini's `/app` bootstrap, saves `~/.gemini-sentinel/accounts/acc1.json` (permissions `0600`), and prints your email alongside your keys (`sentinel-acc1`, `acc1`).

### Method B — Import via Cookie Extension

Export your `google.com` cookies (JSON array or raw `Cookie:` header containing at least `SAPISID` and `__Secure-1PSID`) using a browser extension like Cookie-Editor:

```sh
# From a JSON or text file (delete the file immediately after importing!)
./sentinel --import-cookie acc1 --cookie-file ./cookies.json

# Or pass the raw cookie string directly
./sentinel --import-cookie acc1 --cookie-value "SAPISID=…; __Secure-1PSID=…; __Secure-1PSIDTS=…"
```

### Method C — Interactive Login Helper

```sh
./sentinel --login acc1
./sentinel --login acc1 --chrome-profile "Profile 4"   # for multi-profile setups
```

Opens `https://gemini.google.com/app` in your browser. Sign in, return to the terminal, and press **Enter** to auto-import (or paste a raw cookie string).

---

## 5. Verify Setup (`--probe` & `--status`)

Before launching the daemon, verify your account authentication, available models, and remaining quota:

```sh
./sentinel --probe
```

**Expected healthy output:**

```text
Account acc1            email=user@example.com        capacity=2 models=4 fails=0
  - gemini-flash-lite        (Flash-Lite, id=…, cap=2)
  - gemini-flash             (Flash, id=…, cap=2)
  - gemini-pro               (Pro, id=…, cap=2)
  - gemini-argon             (Argon, id=…, cap=2)
  quota: Gemini Pro          remaining=48384/48384 resets=2026-10-16T08:00:00Z
```

Note the exact model IDs listed (`gemini-flash`, `gemini-pro`, etc.). If `--probe` fails, check [Troubleshooting](#16-troubleshooting) before starting the server.

**Additional CLI Helpers:**
```sh
./sentinel --status   # Offline preflight check: validates bind address, admin key, and account files
./sentinel --rotate   # Force an immediate cookie-rotation cycle (normally runs every 9 minutes)
```

---

## 6. Start the Server

Start via the helper script (builds automatically if needed) or invoke the binary directly:

```sh
export SENTINEL_API_KEY="paste-your-key-here"
./start.sh
# Or directly: ./sentinel (Windows: .\sentinel.exe)
```

**Startup output:**

```text
sentinel (sentinel dev …) listening on [http://127.0.0.1:20133](http://127.0.0.1:20133)
  transport: browserless Chrome uTLS + HTTP/2 (chrome_150_psk)
  data:      /home/you/.gemini-sentinel (1 account(s) loaded)
  models:    [http://127.0.0.1:20133/v1/models](http://127.0.0.1:20133/v1/models)
```

Verify health in another terminal (no API key required for `/health`):

```sh
curl -s [http://127.0.0.1:20133/health](http://127.0.0.1:20133/health)
# {"accounts":1,"models":4,"status":"ok",…}
```

---

## 7. Making Your First API Calls

All examples assume the server is running on `http://127.0.0.1:20133` with account `acc1` (key `sentinel-acc1`).

### 7.1 List Available Models

```sh
curl -s [http://127.0.0.1:20133/v1/models](http://127.0.0.1:20133/v1/models) \
  -H "Authorization: Bearer sentinel-acc1" | python3 -m json.tool
```

### 7.2 Chat Completions (Non-Streaming)

```sh
curl -s [http://127.0.0.1:20133/v1/chat/completions](http://127.0.0.1:20133/v1/chat/completions) \
  -H "Authorization: Bearer sentinel-acc1" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-flash",
    "messages": [{"role": "user", "content": "Say OK if you can hear me."}]
  }'
```

```json
{
  "id": "chatcmpl-…",
  "object": "chat.completion",
  "model": "gemini-flash",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "OK"},
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}
}
```

### 7.3 Chat Completions (Streaming SSE)

```sh
curl -N [http://127.0.0.1:20133/v1/chat/completions](http://127.0.0.1:20133/v1/chat/completions) \
  -H "Authorization: Bearer sentinel-acc1" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-flash",
    "stream": true,
    "messages": [{"role": "user", "content": "Count to 5 slowly."}]
  }'
```

Streams `data: {…}` SSE chunks ending in `data: [DONE]`, with `choices[0].delta` carrying incremental `content` and `reasoning_content` tokens.

### 7.4 OpenAI Responses API

```sh
curl -s [http://127.0.0.1:20133/v1/responses](http://127.0.0.1:20133/v1/responses) \
  -H "Authorization: Bearer sentinel-acc1" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-flash",
    "input": "What is 6 * 7?"
  }'
```

Returns `{"id":"resp_…","object":"response","status":"completed","output":[…],"usage":{…}}`. Pass `"stream": true` with `curl -N` for the full event stream (`response.created` → deltas → `response.completed`).

### 7.5 Understanding Response Codes & Fields

- `finish_reason: "stop"` — Standard completion. `"tool_calls"` — The model invoked one or more tools (see [§10](#10-images-tool-calling-long-context--token-budgets)).
- `usage.completion_tokens_details.reasoning_tokens` — Estimated tokens spent on internal thinking.
- `HTTP 429 model_substitution` — Gemini Web attempted to silently downgrade your requested model (typically due to depleted Pro quota). `sentinel` blocked the downgrade and returned `Retry-After` and `X-Resets-At` headers.
- `HTTP 429 rate_limit_error` — Account quota bucket is exhausted until the indicated reset timestamp.

---

## 8. Model Selection & Routing Tricks

Use any exact model slug from `--probe` or `/v1/models` (`gemini-flash`, `gemini-pro`, `gemini-argon`, `gemini-flash-lite`), or take advantage of these shorthands:

| Feature | Example | Behavior |
| :--- | :--- | :--- |
| **Router Prefix** | `"model": "sentinel/gemini-pro"` | Prefix is stripped automatically (`hearth/` and `gsb/` also supported) |
| **Inline Thinking Override** | `"model": "gemini-pro@think=high"` | Overrides thinking effort for that call if unset in the JSON body |
| **Default Model Fallback** | `"model": ""` | Selects the first available model on that account |

Model matching is case-insensitive and also accepts upstream model IDs or display names. Unrecognized models return `404 model_not_found` specifying which account failed the lookup.

---

## 9. Thinking Levels (`low` / `medium` / `high`)

Gemini Web supports three discrete thinking levels: `low` (1), `medium` (4), and `high` (2). You can control reasoning effort per request using any of these formats (first match wins):

```json
{ "model": "gemini-pro", "reasoning_effort": "high", "messages": […] }
{ "model": "gemini-pro", "thinking": {"type": "enabled", "budget_tokens": 10000}, "messages": […] }
{ "model": "gemini-pro@think=medium", "messages": […] }
```

- **Token Budget Mapping:** `budget_tokens` ≤ `4096` → `low` | ≤ `16384` → `medium` | `> 16384` → `high`.
- **Disabling Thinking:** `thinking: {"type": "disabled"}` or `reasoning: {"enabled": false}` maps to `low`.
- **Global Default:** Controlled by `SENTINEL_DEFAULT_THINKING` (defaults to `high`).
- **Thought Streaming:** Thoughts stream via `reasoning_content` (Chat Completions) or `response.reasoning_summary_text.delta` (Responses API). Set `SENTINEL_LIVE_THOUGHTS=false` to suppress thought output.

---

## 10. Images, Tool Calling, Long Context & Token Budgets

### Multimodal Image Input
Pass inline base64 data URIs or remote `http(s)` image URLs:

```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "What is in this image?"},
    {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0…"}}
  ]
}
```
*Security Guardrails:* Remote URLs are fetched server-side with a **20 MB cap**, max **5 redirects**, MIME validation, and full **SSRF protection** (loopback, private LAN, link-local, and cloud metadata IPs are blocked on initial connect and every redirect).

### Tool / Function Calling
Supports standard OpenAI `tools` and `tool_choice` (`auto`, `none`, `required`, or pinned function). Replies use `finish_reason: "tool_calls"` and stream incremental `tool_calls` deltas. Limits: **≤ 64 tools** and **≤ 256 KB per tool schema**.

### 1M+ Context & Session Caching
Supports up to ~1M characters inline. When history exceeds `SENTINEL_MAX_PROMPT_CHARS` (`850,000`), older turns are seamlessly compacted and uploaded as `text/plain` file attachments (≤ 340 KB each). Multi-turn chats resume instantly via cached continuation tokens (disable globally with `SENTINEL_SESSION_CACHE=false` or per request with `"temporary": true`).

### Local Token Budgeting (`/v1/count-tokens`)
Estimate token usage locally (~4 bytes/token) without making an upstream network call:

```sh
curl -s [http://127.0.0.1:20133/v1/count-tokens](http://127.0.0.1:20133/v1/count-tokens) \
  -H "Authorization: Bearer sentinel-acc1" \
  -H "Content-Type: application/json" \
  -d '{"messages": [{"role": "user", "content": "Summarize this thread."}]}'
# {"characters":26,"object":"token_count","tokens":7}
```
Accepts `text`, `input`, or `messages`. Works with both account and admin keys.

---

## 11. Observability (`--status`, `/metrics`)

### Offline Preflight (`--status`)
Validate configuration before starting the daemon (exits non-zero if bind is non-loopback or admin key is weak; prints zero secrets):

```sh
./sentinel --status
# sentinel status (sentinel dev …)
#   bind:      ok (127.0.0.1:20133, loopback-only)
#   admin key: set (64 chars)
#   data:      /home/you/.gemini-sentinel
#   accounts:  1 file(s) in /home/you/.gemini-sentinel/accounts
#   next:      sentinel --probe  (verify models & quota) then start the server
```

### Prometheus Metrics (`/metrics`)
Query live server metrics using the **admin key**:

```sh
curl -s [http://127.0.0.1:20133/metrics](http://127.0.0.1:20133/metrics) -H "Authorization: Bearer $SENTINEL_API_KEY"
# sentinel_http_requests_total{method="GET",path="/v1/models",status="200"} 3
# sentinel_accounts 1
# sentinel_models 4
# sentinel_account_fails{id="acc1"} 0
```

Every HTTP response includes an `X-Request-ID` header matching the server's structured access log (`method path status duration_ms`). Logs never record API keys, cookies, prompts, or query strings. Set `SENTINEL_LOG_LEVEL=error` (or `silent`) to silence access logs.

---

## 12. Coding Agents & `9router` Setup

### Option A — Direct Agent Connection (Simplest)
Works out of the box with **OpenCode, Cline, Cursor, Codex CLI, `omp`, and `curl`**:

| Setting | Value |
| :--- | :--- |
| **Base URL** | `http://127.0.0.1:20133/v1` |
| **API Key** | `sentinel-acc1` (single account) or your `$SENTINEL_API_KEY` (multi-account pool) |
| **Model** | `gemini-argon`, `gemini-pro`, `gemini-pro@think=high`, etc. |

### Option B — Multi-Account Pool via `9router`

<details>
<summary><b>Click to expand 9router registration scripts & ~/.omp/agent/models.yml config</b></summary>

1. **Create the `Gemini-Sentinel` Provider Node in `9router`** (`http://127.0.0.1:20128`):

```sh
NODE_ID=$(curl -s -X POST [http://127.0.0.1:20128/api/provider-nodes](http://127.0.0.1:20128/api/provider-nodes) \
  -H "Content-Type: application/json" \
  -d '{"type":"openai-compatible","apiType":"chat","name":"Gemini-Sentinel","prefix":"sentinel","baseUrl":"[http://127.0.0.1:20133/v1](http://127.0.0.1:20133/v1)"}' \
  | node -pe 'JSON.parse(fs.readFileSync(0,"utf8")).node.id')
echo "Node: $NODE_ID"
```

2. **Register each Google account as a provider entry:**

```sh
NODE_ID=$(curl -s [http://127.0.0.1:20128/api/provider-nodes](http://127.0.0.1:20128/api/provider-nodes) \
  | node -pe 'JSON.parse(fs.readFileSync(0,"utf8")).nodes.find(n => n.prefix === "sentinel").id')
for f in ~/.gemini-sentinel/accounts/*.json; do
  [ -e "$f" ] || continue
  acc="$(basename "$f" .json)"
  curl -s -X POST [http://127.0.0.1:20128/api/providers](http://127.0.0.1:20128/api/providers) \
    -H "Content-Type: application/json" \
    -d "{\"provider\":\"$NODE_ID\",\"name\":\"sentinel-$acc\",\"apiKey\":\"sentinel-$acc\",\"priority\":1,\"allowOverwrite\":true}"
  echo
done
```

3. **Configure your agent (`~/.omp/agent/models.yml`):**

```yaml
providers:
  9router-local:
    baseUrl: [http://127.0.0.1:20128/v1](http://127.0.0.1:20128/v1)
    apiKey: <your-9router-api-key>
    api: openai-completions
    compat:
      supportsReasoningEffort: true
    models:
      - id: sentinel/gemini-argon
        name: sentinel/gemini-argon
        reasoning: true
        thinking:
          mode: effort
          efforts: [low, medium, high]
        input: [text, image]
        contextWindow: 1048576
        maxTokens: 65536
        compat:
          supportsReasoningEffort: true
```
</details>

---

## 13. Multiple Google Accounts

Import additional accounts (`acc2`, `acc3`, …) using [Step 4](#4-connect-your-google-account) and verify with `./sentinel --probe`.

- **Account Keys (`sentinel-acc1`):** Strictly pinned to that single account. If `acc1` runs out of quota, the request fails immediately—it will never spill over to `acc2`.
- **Admin Key (`$SENTINEL_API_KEY`):** Automatically routes across all loaded accounts using least-busy selection, automatic failover on errors/quota exhaustion, and session-cache affinity for active multi-turn threads.

**Admin Management Commands:**
```sh
curl -s [http://127.0.0.1:20133/v1/accounts](http://127.0.0.1:20133/v1/accounts) -H "Authorization: Bearer $SENTINEL_API_KEY"
curl -s [http://127.0.0.1:20133/v1/quota](http://127.0.0.1:20133/v1/quota)    -H "Authorization: Bearer $SENTINEL_API_KEY"
curl -s [http://127.0.0.1:20133/v1/sessions](http://127.0.0.1:20133/v1/sessions) -H "Authorization: Bearer $SENTINEL_API_KEY"
curl -s [http://127.0.0.1:20133/metrics](http://127.0.0.1:20133/metrics)     -H "Authorization: Bearer $SENTINEL_API_KEY"
curl -s -X POST [http://127.0.0.1:20133/v1/refresh](http://127.0.0.1:20133/v1/refresh) -H "Authorization: Bearer $SENTINEL_API_KEY"
```

---

## 14. Configuration Reference

`SENTINEL_*` takes precedence; legacy `HEARTH_*` and `BRIDGE_*` (plus `HOST`/`PORT`) remain supported. See [`config.example.env`](./config.example.env).

| Variable (Fallbacks) | Default | Description |
| :--- | :--- | :--- |
| `SENTINEL_HOST` (`HEARTH_HOST`, `BRIDGE_HOST`, `HOST`) | `127.0.0.1` | Bind address. Non-loopback values refuse startup. |
| `SENTINEL_PORT` (`HEARTH_PORT`, `BRIDGE_PORT`, `PORT`) | `20133` | Local HTTP listen port. |
| `SENTINEL_HOME` (`HEARTH_HOME`, `BRIDGE_HOME`) | `~/.gemini-sentinel` | Data directory for `accounts/*.json` & `session-cache.json`. |
| `SENTINEL_API_KEY` (`HEARTH_API_KEY`, `BRIDGE_API_KEY`) | *(empty)* | Admin key (≥ 16 chars). Weak keys fail startup; empty denies admin routes. |
| `SENTINEL_ACCOUNT_TOKENS` | *Auto-derived* | Explicit `token:account` pairs to lock the token set. |
| `SENTINEL_LOG_LEVEL` | `info` | `info` logs `method path status duration_ms`; `error` / `silent` suppresses. |
| `SENTINEL_IMPERSONATE` | `chrome_150_psk` | uTLS fingerprint (`chrome_150_psk`, `chrome_146`, `chrome_133`, `firefox_135`, `safari_18_0`). |
| `SENTINEL_PROXY_SERVER` | *(empty)* | Outbound `http://` or `socks5://` proxy for Gemini traffic. |
| `SENTINEL_DEFAULT_THINKING` | `high` | Default reasoning level (`low`, `medium`, `high`). |
| `SENTINEL_REJECT_DOWNGRADE` | `true` | Reject silent upstream model swaps with `429 model_substitution`. |
| `SENTINEL_LIVE_THOUGHTS` | `true` | Stream `reasoning_content` / reasoning summary deltas. |
| `SENTINEL_SESSION_CACHE` | `true` | Persist multi-turn continuation state across requests. |
| `SENTINEL_TOOL_PREAMBLE` | `false` | Keep pre-tool text as main answer (`true`) or move to reasoning (`false`). |
| `SENTINEL_MAX_PROMPT_CHARS` | `850000` | Inline character budget before older turns offload to file attachments. |
| `SENTINEL_MAX_OLD_TOOL_CHARS` | `32000` | Max characters per historical tool output before compaction. |
| `SENTINEL_FIRST_BYTE_TIMEOUT_MS` | `60000` | Timeout waiting for initial upstream response bytes (60s). |
| `SENTINEL_STALL_TIMEOUT_MS` | `90000` | Streaming stall timeout before triggering failover (90s). |
| `SENTINEL_STREAM_TIMEOUT_MS` | `600000` | Maximum total lifetime of a single generation request (10m). |
| `SENTINEL_ROTATE_COOKIES_MS` | `540000` | Background cookie rotation interval (9m). |
| `SENTINEL_MODEL_REFRESH_MS` | `600000` | Background model & quota catalog refresh interval (10m). |

---

## 15. API Endpoints & Hard Limits

| Method | Path | Auth Required | Description |
| :--- | :--- | :--- | :--- |
| `GET` | `/health` | None | Service health, transport profile, account & model counts |
| `GET` | `/v1/models` | Account or Admin | Live `otAQ7b` model catalog (1,048,576 context window) |
| `POST` | `/v1/chat/completions` | Account or Admin | OpenAI Chat Completions (SSE streaming, tools, vision, reasoning) |
| `POST` | `/v1/responses` | Account or Admin | OpenAI Responses API (reasoning & function-call events) |
| `POST` | `/v1/count-tokens` | Account or Admin | Local token & character estimator (zero upstream traffic) |
| `GET` | `/metrics` | **Admin Only** | Prometheus metrics exposition |
| `GET` | `/v1/accounts` | **Admin Only** | Account health, fail counts, and status (`403` for account keys) |
| `GET` | `/v1/quota` | **Admin Only** | Live `qpEbW` quota buckets and reset timestamps |
| `GET` | `/v1/sessions` | **Admin Only** | HTTP/2 transport pool & session diagnostics |
| `POST` | `/v1/refresh` | **Admin Only** | Hot-reload account JSON files and refresh upstream model catalogs |

**Hard Request Limits** *(violations return `400`, `413`, or `429`)*:
- **Max Request Body:** 32 MB
- **Max Messages:** 500 per request
- **Max Tools:** 64 tools (max 256 KB schema per tool)
- **Max Remote Image:** 20 MB (max 5 redirects, SSRF-guarded)

---

## 16. Troubleshooting

| Symptom / Error | Likely Cause | Resolution |
| :--- | :--- | :--- |
| `refusing non-loopback bind` at startup | `HOST` is set to `0.0.0.0`, `::`, or a LAN IP | Bind to `127.0.0.1`, `::1`, or `localhost`. Use a hardened HTTPS reverse proxy for remote access. |
| `refusing to start with … admin key` | `SENTINEL_API_KEY` is `< 16` chars or a placeholder (`public`) | Generate a 64-char hex key ([§3 Step 2](#step-2--create-your-admin-key)). |
| `401 invalid api key` | Missing auth header, typo in key, or `?key=` in URL | Pass `Authorization: Bearer sentinel-<name>` in headers. |
| `403 admin authorization required` | Account key used on `/v1/accounts`, `/v1/quota`, or `/metrics` | Authenticate with `$SENTINEL_API_KEY`. |
| `404 model_not_found` | Requested model is unavailable on that Google account | Run `./sentinel --probe` and copy the exact model slug. |
| `429 model_substitution` | Quota depleted; Gemini tried to silently swap models | Wait until `X-Resets-At`, switch models, or use an admin key with multiple accounts. |
| `429 rate_limit_error` / `session_busy` | Quota bucket empty or account concurrency saturated | Respect `Retry-After` (2s for busy sessions) or add another account. |
| `502 upstream_auth_error` | Session cookies expired or revoked by Google | Re-import cookies ([§4](#4-connect-your-google-account)) and verify with `./sentinel --probe`. |
| `502 model_identity_unknown` | Upstream response omitted model identity proof | Retry request; disable strict checking only if necessary via `SENTINEL_REJECT_DOWNGRADE=false`. |

Still stuck? Run `./sentinel --probe` to inspect per-account and per-model diagnostics. **Never paste cookies, API keys, or private prompts into GitHub issues.**

---

## 17. Everyday Operations & Security Hygiene

- **Backups:** Back up `~/.gemini-sentinel/accounts/` before major upgrades.
- **Revoking an Account:** Delete `~/.gemini-sentinel/accounts/<name>.json` and call `POST /v1/refresh` with your admin key.
- **Full Uninstall / Wipe:** Stop `sentinel` and delete `~/.gemini-sentinel`.
- **Credential Leak Response:** If an account `.json` or cookie export is exposed, immediately log out of the Google session in your browser (revoking the server token), delete the local files, rotate `SENTINEL_API_KEY`, and review [`SECURITY.md`](./SECURITY.md).
- **Verifying Releases:** Tagged releases for Windows, Linux, and macOS include SHA-256 checksums and a module SBOM via `.github/workflows/release.yml` (see [`docs/RELEASE.md`](./docs/RELEASE.md)). Verify binaries before execution:
  ```powershell
  Get-FileHash .\sentinel.exe -Algorithm SHA256
  ```

---

## 18. Development, Internals & Roadmap

### Running Tests & Static Analysis
All tests run offline against synthetic fixtures and mocks—never real Google credentials:

```sh
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
```
CI enforces `gofmt`, `go vet`, `-race` tests, and `govulncheck` on every push.

### Codebase Architecture

| Package / Path | Responsibility |
| :--- | :--- |
| [`cmd/sentinel/main.go`](./cmd/sentinel/main.go) | CLI flags (`--probe`, `--rotate`, `--status`, `--login`, `--import-*`, `--version`), directory migration, startup security gates, graceful shutdown |
| [`internal/config`](./internal/config) | `SENTINEL_*` / `HEARTH_*` / `BRIDGE_*` env resolution, loopback validation, OS-matched browser headers |
| [`internal/cookies`](./internal/cookies) | Cookie canonicalization, `SAPISIDHASH`, atomic `0600` persistence, dual-path rotation (`__Secure-1PSIDTS`, `SIDCC`), SQLite/AES browser decryption |
| [`internal/transport`](./internal/transport) | `Chrome_150_PSK` uTLS + HTTP/2 client, header ordering, per-account session pools, bounded queues |
| [`internal/gemini`](./internal/gemini) | Auth bootstrap, `batchexecute` (`otAQ7b` models, `qpEbW` quota), `StreamGenerate`, `wrb.fr` parser, model-identity verification |
| [`internal/prompt`](./internal/prompt) | Message normalization, 1M-context attachment offloading, SSRF-guarded remote image fetching |
| [`internal/toolbridge`](./internal/toolbridge) | OpenAI tool schema validation and streaming tool-call delta translation |
| [`internal/server`](./internal/server) | HTTP router, bearer/header auth, Chat/Responses/Token handlers, Prometheus metrics, session cache |
| [`internal/version`](./internal/version) | Build-stamped version and commit metadata |

### Architectural Roadmap (Toward an Official API Gateway)
Gemini-Sentinel is currently an experimental research adapter. For commercial or policy-compliant production deployments, the long-term architectural path replaces browser-cookie extraction and undocumented web RPCs with an official API backend while retaining the local OpenAI-compatible gateway, tool bridge, and observability layer:

```text
Official Gemini API  ──►  Provider Abstraction  ──►  Local OpenAI-Compatible Gateway
```

---

## 19. Legal, Contributing & License

### Legal & Policy Disclaimer
Automating `gemini.google.com` web sessions may violate the [Google Terms of Service](https://policies.google.com/terms), [Generative AI Additional Terms](https://policies.google.com/terms/generative-ai), or account policies, and carries the risk of quota restrictions or account suspension. Use only with accounts you are authorized to control, respect rate limits, do not use for bulk scraping or abuse, and keep the daemon restricted to your local machine. Provided **AS-IS** without warranty of any kind.

### Contributing
Contributions that improve security, correctness, documentation, testing, or maintainability are welcome. Do not submit PRs designed to bypass quotas/safeguards, harvest credentials, or conceal abusive traffic. Always include unit tests and never commit real credentials or session fixtures.

### License
Released under the [MIT License](./LICENSE).
- © 2026 **Sonesh SK** (`Gemini-Sentinel`)
- © 2026 **azrinaldi** (original Hearth / `gemini-stealth-bridge`)
