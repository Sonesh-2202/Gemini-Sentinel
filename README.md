# gemini-stealth-bridge (`gsb`)

Use your Gemini web subscription from any OpenAI-compatible app — no browser automation, no 600 MB headless Chrome. `gsb` is a small Go program that signs into `gemini.google.com` with your own browser cookies and serves them as `POST /v1/chat/completions` and `POST /v1/responses` on your own machine.

> **Security — read this first.**
> Safe for local loopback use when configured and maintained correctly.
> **Not safe for direct LAN or internet exposure.**
> Browser cookies are passwords: anyone holding them can impersonate your Google session.
> The server speaks plaintext HTTP, listens on loopback only (`127.0.0.1`, `::1`, `localhost`), and refuses to start on any other address.
> For remote access, keep it on loopback behind a separately hardened HTTPS reverse proxy.
> Details: `SECURITY.md`.

---

## Table of contents

1. [How it works](#1-how-it-works-plain-english)
2. [What you need](#2-what-you-need)
3. [Step 1 — Build it](#3-step-1--build-it)
4. [Step 2 — Create your admin key](#4-step-2--create-your-admin-key)
5. [Step 3 — Connect your Google account](#5-step-3--connect-your-google-account)
6. [Step 4 — Check the account works (`--probe`)](#6-step-4--check-the-account-works---probe)
7. [Step 5 — Start the server](#7-step-5--start-the-server)
8. [Step 6 — Make a model run (your first calls)](#8-step-6--make-a-model-run-your-first-calls)
9. [Choosing a model](#9-choosing-a-model)
10. [Thinking levels (`low` / `medium` / `high`)](#10-thinking-levels-low--medium--high)
11. [Images, tools, and long chats](#11-images-tools-and-long-chats)
12. [Using it with coding agents](#12-using-it-with-coding-agents)
13. [Multiple Google accounts](#13-multiple-google-accounts)
14. [Settings reference](#14-settings-reference)
15. [Endpoints, keys, and limits](#15-endpoints-keys-and-limits)
16. [When something goes wrong](#16-when-something-goes-wrong)
17. [Everyday operations](#17-everyday-operations)
18. [Testing, internals, license](#18-testing-internals-license)

---

## 1. How it works (plain English)

```
Your app (curl, OpenCode, Cline, 9router…)
        │  OpenAI-style HTTP on http://127.0.0.1:20133/v1
        ▼
  ┌────────── gsb ──────────┐
  │ account keys → account   │
  │ admin key → admin APIs   │
  │ model + quota checks     │
  │ prompt + tools builder   │
  └──────────┬──────────────┘
             │  Chrome-fingerprint HTTPS (uTLS + HTTP/2)
             ▼
   gemini.google.com (your own login session)
```

1. You log into Gemini once in your **real browser**.
2. `gsb` copies that login's cookies into `~/.gemini-stealth-bridge/accounts/<name>.json`.
3. Your apps talk to `gsb` exactly like they talk to OpenAI.
4. `gsb` translates each request, sends it to Gemini as your browser would, and streams the answer back.
5. Cookies are refreshed automatically in the background.

Each account file gets **two API keys**: `gsb-<name>` and `<name>` (e.g. `gsb-acc1` and `acc1`). These keys only ever reach **that one account** — they cannot see or spend another account.

---

## 2. What you need

- **Go 1.26 or newer** (`go version` to check; see `go.mod`). The toolchain self-provisions if network allows.
- **A desktop browser** (Chrome, Brave, Chromium, Edge, or Firefox) with **one logged-in Google session** that can open `gemini.google.com/app`.
- **Windows PowerShell**, macOS Terminal, or Linux shell, plus `curl` for testing.
- Your machine only — never install this on a shared server without understanding `SECURITY.md`.

---

## 3. Step 1 — Build it

```sh
git clone https://github.com/azrinaldi/gemini-stealth-bridge.git
cd gemini-stealth-bridge
go build -o gsb ./cmd/gsb        # Windows: go build -o gsb.exe ./cmd/gsb
./gsb --version                  # prints e.g. "gsb dev (commit unknown, …)"
```

Or install it onto your `PATH`:

```sh
go install github.com/azrinaldi/gemini-stealth-bridge/cmd/gsb@latest
```

`./start.sh` also builds when needed and starts the server (see Step 5).

---

## 4. Step 2 — Create your admin key

There are **two kinds of keys**:

| Key | What it unlocks | Example |
|---|---|---|
| **Account key** (`gsb-<name>` or `<name>`) | Chat, Responses, and Models — for that account only | `gsb-acc1` |
| **Admin key** (`BRIDGE_API_KEY`) | Everything above (all accounts) **plus** `/v1/accounts`, `/v1/quota`, `/v1/sessions`, `/v1/refresh` | random 64-hex string |

Account keys are created automatically when you add an account (Step 3). The admin key you create yourself:

```sh
# Print a strong random key (64 hex chars). Copy the output.
python3 -c "import secrets; print(secrets.token_hex(32))"
```

Then, every time you start the server, provide it:

```sh
export BRIDGE_API_KEY="paste-your-key-here"   # PowerShell: $env:BRIDGE_API_KEY="…"
```

Rules:

- Minimum **16 characters**; `public`, `test`, `password`, etc. are rejected and the server **refuses to start**.
- No admin key = admin endpoints answer `401`/`403`, while per-account chat keys keep working.
- Send keys in **headers** (`Authorization: Bearer …`, `x-api-key: …`, `x-goog-api-key: …`). Keys in URLs (`?key=`) are **not accepted**.
- A ready-to-copy template lives in `config.example.env`. Never commit a real key.

---

## 5. Step 3 — Connect your Google account

Pick **one** method. Method A is easiest.

### Method A — Read cookies from your browser (recommended)

Close nothing; just be logged into Google in that browser profile first:

```sh
./gsb --import-chrome acc1
```

Multi-profile setups — tell it which profile:

```sh
# By profile name (Windows, macOS, Linux)
./gsb --import-chrome acc1 --chrome-profile "Profile 4"

# By profile folder (Linux / macOS)
./gsb --import-chrome acc1 --chrome-profile ~/.config/google-chrome/Default

# By profile folder (Windows PowerShell / cmd)
./gsb --import-chrome acc1 --chrome-profile "C:\Users\<you>\AppData\Local\Google\Chrome\User Data\Profile 4"
```

What just happened: `gsb` copied and decrypted the `google.com` cookies, tested them against Gemini (`/app` bootstrap), and saved `~/.gemini-stealth-bridge/accounts/acc1.json`. It prints your account email and the two API keys (`gsb-acc1`, `acc1`).

### Method B — Paste cookies via a browser extension

1. Install a "Cookie-Editor" style extension, open `google.com`, export cookies (JSON array) or copy the raw `Cookie:` header.
2. Import it (needs at least `SAPISID` and `__Secure-1PSID`):

```sh
# From a file (raw header or Cookie-Editor JSON both work)
./gsb --import-cookie acc1 --cookie-file ./cookies.json

# Or paste the value directly
./gsb --import-cookie acc1 --cookie-value "SAPISID=…; __Secure-1PSID=…; __Secure-1PSIDTS=…"
```

### Method C — Interactive login helper

```sh
./gsb --login acc1
./gsb --login acc1 --chrome-profile "Profile 4"   # multi-profile machines
```

This opens `https://gemini.google.com/app` in your browser. Sign in, press **Enter** in the terminal, and it imports from the browser profile (or paste a cookie line instead of pressing Enter empty).

Repeat any method per Google account (`acc2`, `acc3`, …). Account names allow `A–Z a–z 0–9 . _ -`, max 64 chars.

---

## 6. Step 4 — Check the account works (`--probe`)

Before starting the server, verify login, model list, and quota:

```sh
./gsb --probe
```

Healthy output looks like:

```text
Account acc1             email=user@example.com        capacity=2 models=4 fails=0
  - gemini-flash-lite        (Flash-Lite, id=…, cap=2)
  - gemini-flash             (Flash, id=…, cap=2)
  - gemini-pro               (Pro, id=…, cap=2)
  - gemini-argon             (Argon, id=…, cap=2)
  quota: Gemini Pro         remaining=48384/48384 resets=2026-10-16T08:00:00Z
```

Write down the **model names** in the `- …` column — those are the exact strings your apps must send (plus the tricks in [Choosing a model](#9-choosing-a-model)).

If `--probe` fails, jump to [When something goes wrong](#16-when-something-goes-wrong) — do not start the server yet.

Optional maintenance command:

```sh
./gsb --rotate     # force one cookie-rotation round now (normally automatic)
```

---

## 7. Step 5 — Start the server

```sh
export BRIDGE_API_KEY="paste-your-key-here"
./start.sh
```

or directly:

```sh
export BRIDGE_API_KEY="paste-your-key-here"
./gsb
```

You should see:

```text
gemini-stealth-bridge (gsb dev …) listening on http://127.0.0.1:20133
  transport: browserless Chrome uTLS + HTTP/2 (chrome_150_psk)
  data:      /home/you/.gemini-stealth-bridge (1 account(s) loaded)
  models:    http://127.0.0.1:20133/v1/models
```

Sanity check in another terminal:

```sh
curl -s http://127.0.0.1:20133/health
# {"accounts":1,"models":4,"status":"ok",…}
```

`/health` needs no key. Everything else does. The server **will not start** on `HOST=0.0.0.0`, `::`, a LAN IP, or a public IP — that is intentional (plaintext HTTP).

---

## 8. Step 6 — Make a model run (your first calls)

All examples assume the server runs at `http://127.0.0.1:20133` and your account is `acc1` (key `gsb-acc1`). Replace the model name with one from your `--probe` output.

### 8.1 List models

```sh
curl -s http://127.0.0.1:20133/v1/models \
  -H "Authorization: Bearer gsb-acc1" | python3 -m json.tool
```

Expected: `{"object":"list","data":[{"id":"gemini-flash",…}, …]}`.

### 8.2 Simple chat call (non-streaming)

```sh
curl -s http://127.0.0.1:20133/v1/chat/completions \
  -H "Authorization: Bearer gsb-acc1" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-flash",
    "messages": [{"role": "user", "content": "Say OK if you can hear me."}]
  }'
```

Expected shape:

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

If `content` contains `reasoning_content` too, that is the model's thinking (see §10).

### 8.3 Streaming call (watch tokens arrive)

```sh
curl -N http://127.0.0.1:20133/v1/chat/completions \
  -H "Authorization: Bearer gsb-acc1" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-flash",
    "stream": true,
    "messages": [{"role": "user", "content": "Count to 5 slowly."}]
  }'
```

You will see `data: {…}` chunks ending with `data: [DONE]`. Each chunk's `choices[0].delta` holds `content` and/or `reasoning_content` pieces.

### 8.4 Responses API call

```sh
curl -s http://127.0.0.1:20133/v1/responses \
  -H "Authorization: Bearer gsb-acc1" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-flash",
    "input": "What is 6 * 7?"
  }'
```

Expected: `{"id":"resp_…","object":"response","status":"completed","output":[…],"usage":{…}}`. Add `"stream": true` and use `curl -N` for the SSE event stream (`response.created` → deltas → `response.completed`).

### 8.5 What each reply field means

- `finish_reason: "stop"` — normal answer. `"tool_calls"` — the model wants to call your tools (see §11).
- `usage.completion_tokens_details.reasoning_tokens` — how much thinking was used.
- HTTP `429 model_substitution` — Gemini silently served a different model than asked; `gsb` blocked it and tells you when to retry (`Retry-After`, `X-Resets-At`). Usually means that model's quota is spent.
- HTTP `429 rate_limit_error` — quota bucket empty until the reset time shown.

---

## 9. Choosing a model

Use a name from `--probe` or `/v1/models` **exactly** (`gemini-flash`, `gemini-pro`, `gemini-argon`, `gemini-flash-lite`, …). Three conveniences also work:

| Trick | Example | Meaning |
|---|---|---|
| `gsb/` prefix (for routers) | `"model": "gsb/gemini-pro"` | Prefix is stripped |
| `@think=` suffix | `"model": "gemini-pro@think=high"` | One-call thinking override |
| Empty model | `"model": ""` | First model on that account |

Matching also accepts the upstream model ID or display name, case-insensitively. Unknown names return `404 model_not_found` naming the account that couldn't serve it.

---

## 10. Thinking levels (`low` / `medium` / `high`)

Gemini Web has exactly three thinking levels: `low` (1), `medium` (4), `high` (2). Set one per request any of these ways (first match wins; `@think=` suffix fills in when the body says nothing):

```json
{ "model": "gemini-pro", "reasoning_effort": "high", "messages": […] }
{ "model": "gemini-pro", "thinking": {"type": "enabled", "budget_tokens": 10000}, "messages": […] }
{ "model": "gemini-pro@think=medium", "messages": […] }
```

- `budget_tokens` ≤ 4096 → `low`, ≤ 16384 → `medium`, above → `high`.
- `thinking: {"type": "disabled"}` (or `reasoning: {"enabled": false}`) → `low`.
- Default when you say nothing: `BRIDGE_DEFAULT_THINKING` (`high` unless changed).
- Reasoning streams as `reasoning_content` (Chat API) or `response.reasoning_summary_text.delta` (Responses API). Set `BRIDGE_LIVE_THOUGHTS=false` to suppress it.

---

## 11. Images, tools, and long chats

**Images** — inline base64 or remote URLs inside message content:

```json
{"role": "user", "content": [
  {"type": "text", "text": "What is in this image?"},
  {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0…"}}
]}
```

Remote `http(s)` images are downloaded server-side (20 MB cap, 5 redirects, SSRF-guarded: loopback/private/reserved/metadata addresses blocked, redirects rechecked). Non-image content is rejected.

**Tool calling** — standard OpenAI `tools` + `tool_choice` (`auto`/`none`/`required`/single-function). The model replies with `finish_reason: "tool_calls"`. Streaming sends incremental `tool_calls` deltas. Keep it to ≤ 64 tools and ≤ 256 KB per tool schema.

**Long chats** — up to ~1M characters inline; older turns are automatically offloaded as `text/plain` attachments (≤ 340 KB each), and multi-turn sessions resume via cached continuation tokens (disable with `BRIDGE_SESSION_CACHE=false` or per-request `"temporary": true`).

---

## 12. Using it with coding agents

### Option A — Direct connection (simplest)

Point any OpenAI-compatible client at the bridge:

| Setting | Value |
|---|---|
| **Base URL** | `http://127.0.0.1:20133/v1` |
| **API Key** | `gsb-acc1` (or `acc1`) via `Authorization: Bearer …` |
| **Model** | e.g. `gemini-argon`, optionally `gemini-pro@think=high` |

Works with `omp`, Codex CLI, OpenCode, Cline, Cursor, and plain `curl`.

### Option B — Multi-account pool via `9router`

1. In `9router` (`http://127.0.0.1:20128` → Providers → Add Compatible Provider): type `OpenAI Compatible`, API type `chat` (or `responses`), name `Gemini Stealth Bridge`, prefix `gsb`, base URL `http://127.0.0.1:20133/v1`. Or via `curl`:

```sh
NODE_ID=$(curl -s -X POST http://127.0.0.1:20128/api/provider-nodes \
  -H "Content-Type: application/json" \
  -d '{"type":"openai-compatible","apiType":"chat","name":"Gemini Stealth Bridge","prefix":"gsb","baseUrl":"http://127.0.0.1:20133/v1"}' \
  | node -pe 'JSON.parse(fs.readFileSync(0,"utf8")).node.id')
echo "Node: $NODE_ID"
```

2. Register one provider entry per Google account:

```sh
NODE_ID=$(curl -s http://127.0.0.1:20128/api/provider-nodes \
  | node -pe 'JSON.parse(fs.readFileSync(0,"utf8")).nodes.find(n => n.prefix === "gsb").id')
for f in ~/.gemini-stealth-bridge/accounts/*.json; do
  [ -e "$f" ] || continue
  acc="$(basename "$f" .json)"
  curl -s -X POST http://127.0.0.1:20128/api/providers \
    -H "Content-Type: application/json" \
    -d "{\"provider\":\"$NODE_ID\",\"name\":\"gsb-$acc\",\"apiKey\":\"gsb-$acc\",\"priority\":1,\"allowOverwrite\":true}"
  echo
done
```

3. Example agent config (`~/.omp/agent/models.yml`):

```yaml
providers:
  9router-local:
    baseUrl: http://127.0.0.1:20128/v1
    apiKey: <your-9router-api-key>
    api: openai-completions
    compat:
      supportsReasoningEffort: true
    models:
      - id: gsb/gemini-argon
        name: gsb/gemini-argon
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

---

## 13. Multiple Google accounts

Add each account (`acc1`, `acc2`, …) via Step 3 and re-probe. Routing rules:

- An **account key** always uses its own account. If that account can't serve the model/quota, the request fails — it never silently spends another account.
- The **admin key** may use any account with automatic failover and least-busy preference plus session-cache affinity for continuing chats.
- List accounts/quota only with the admin key:

```sh
curl -s http://127.0.0.1:20133/v1/accounts -H "Authorization: Bearer $BRIDGE_API_KEY"
curl -s http://127.0.0.1:20133/v1/quota    -H "Authorization: Bearer $BRIDGE_API_KEY"
curl -s http://127.0.0.1:20133/v1/sessions -H "Authorization: Bearer $BRIDGE_API_KEY"
curl -s -X POST http://127.0.0.1:20133/v1/refresh -H "Authorization: Bearer $BRIDGE_API_KEY"
```

---

## 14. Settings reference

| Variable | Default | What it does |
|---|---|---|
| `HOST` / `BRIDGE_HOST` | `127.0.0.1` | Bind address. Anything non-loopback refuses startup (plaintext HTTP) |
| `PORT` / `BRIDGE_PORT` | `20133` | Listen port |
| `BRIDGE_HOME` | `~/.gemini-stealth-bridge` | Data dir for `accounts/*.json` + `session-cache.json` |
| `BRIDGE_API_KEY` | *(empty)* | **Admin** key for `/v1/accounts`, `/v1/quota`, `/v1/sessions`, `/v1/refresh`. ≥ 16 random chars; `public`/short = fatal; empty = admin endpoints deny all |
| `BRIDGE_ACCOUNT_TOKENS` | auto from `accounts/*.json` | `token:account` pairs. Set to lock the token set |
| `BRIDGE_LOG_LEVEL` | `info` | `info` = method/path/status/duration per request (never secrets); `error`/`silent` = quiet |
| `BRIDGE_IMPERSONATE` | `chrome_150_psk` | uTLS profile (`chrome_150_psk`, `chrome_146`, `chrome_133`, `firefox_135`, `safari_18_0`) |
| `BRIDGE_PROXY_SERVER` | *(empty)* | Outbound `http://` or `socks5://` proxy for Gemini traffic |
| `BRIDGE_DEFAULT_THINKING` | `high` | Default thinking (`low`/`medium`/`high`) |
| `BRIDGE_REJECT_DOWNGRADE` | `true` | Block silent model swaps with `429 model_substitution` |
| `BRIDGE_LIVE_THOUGHTS` | `true` | Stream `reasoning_content` / reasoning-summary deltas |
| `BRIDGE_SESSION_CACHE` | `true` | Resume multi-turn sessions via cached continuation tokens |
| `BRIDGE_TOOL_PREAMBLE` | `false` | Keep pre-tool text as answer (`true`) or move it to reasoning (`false`) |
| `BRIDGE_MAX_PROMPT_CHARS` | `850000` | Inline budget before old turns offload to attachments |
| `BRIDGE_MAX_OLD_TOOL_CHARS` | `32000` | Cap per old tool output before compaction |
| `BRIDGE_FIRST_BYTE_TIMEOUT_MS` | `60000` | Give up waiting for first upstream bytes after this |
| `BRIDGE_STALL_TIMEOUT_MS` | `90000` | Fail over if no generation frame arrives within this |
| `BRIDGE_STREAM_TIMEOUT_MS` | `600000` | Max lifetime of one generation request |
| `BRIDGE_ROTATE_COOKIES_MS` | `540000` | Cookie-rotation interval (9 min) |
| `BRIDGE_MODEL_REFRESH_MS` | `600000` | Model/quota refresh interval (10 min) |

A copy-paste template without secrets is in `config.example.env`.

---

## 15. Endpoints, keys, and limits

| Method | Path | Needs | Notes |
|---|---|---|---|
| `GET` | `/health` | nothing | Status, profile, account/model counts |
| `GET` | `/v1/models` | account or admin | Live `otAQ7b` catalog; 1,048,576 context |
| `POST` | `/v1/chat/completions` | account or admin | Chat API, SSE streaming, tools, images |
| `POST` | `/v1/responses` | account or admin | Responses API, reasoning + function-call events |
| `GET` | `/v1/accounts` | **admin only** | Account keys get `403` |
| `GET` | `/v1/quota` | **admin only** | Live `qpEbW` buckets |
| `GET` | `/v1/sessions` | **admin only** | Transport pool diagnostics |
| `POST` | `/v1/refresh` | **admin only** | Reload accounts + refresh models |

Hard limits (violations → `400`/`413`/`429`): 32 MB body, 500 messages, 64 tools, 256 KB per tool schema, 20 MB per remote image, 5 image redirects, timeouts above. Every response carries `X-Request-ID` for correlating with server logs.

---

## 16. When something goes wrong

| Symptom | Likely cause | Fix |
|---|---|---|
| `refusing non-loopback bind` at startup | `HOST` is LAN/public/wildcard | Keep `HOST=127.0.0.1`; use a reverse proxy for remote |
| `refusing to start with … admin key` | `public` or < 16 chars | Generate a fresh key (§4) |
| `401 invalid api key` | Wrong/missing header, or `?key=` URL | Use a header; check `gsb-<name>` spelling |
| `403 admin authorization required` | Account key on admin endpoint | Use `BRIDGE_API_KEY` |
| `404 model_not_found` | Name not on that account | Re-run `--probe`; copy exact name |
| `429 model_substitution` | Quota spent; upstream swapped models | Wait until `X-Resets-At`, try another model/account |
| `429 rate_limit_error` / `Retry-After` | Bucket empty | Same as above |
| `502 upstream_auth_error` / bootstrap fails | Cookies expired/revoked | Re-import (§5), then `--probe` |
| `502 model_identity_unknown` | Upstream hid model identity | Retry; strict mode requires identity proof |
| `empty/incomplete upstream` errors | Transient upstream cut | Retry once; check network/proxy |
| `session_busy 429` | Account saturated | Wait 2 s (`retry-after`) or add accounts |

Still stuck? Re-run `./gsb --probe` and read the per-model error lines — they name the failing account and reason. Never paste cookies, keys, or prompts into bug reports.

---

## 17. Everyday operations

- **Back up** `~/.gemini-stealth-bridge/accounts/` before upgrades.
- **Revoke one account:** delete `accounts/<name>.json`, then `POST /v1/refresh` with the admin key.
- **Wipe everything:** stop the daemon, delete `BRIDGE_HOME`.
- **Cookies leaked?** Follow `SECURITY.md`: revoke Google sessions, delete files, rotate `BRIDGE_API_KEY`, re-import from a clean profile, check account activity.
- **Remote access:** no built-in remote mode — keep loopback, terminate TLS at a reviewed proxy (HTTPS only, managed certs + rotation, strong proxy auth, rate/size limits, no secret logging).
- **Releases:** checklist, checksums, and SBOM guidance live in `docs/RELEASE.md`.

---

## 18. Testing, internals, license

```sh
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
```

Tests never touch real Google credentials or real browser cookies.

| Package / File | Job |
|---|---|
| `cmd/gsb/main.go` | CLI (`--probe`, `--rotate`, `--login`, `--import-chrome`, `--import-cookie`, `--version`), bind/auth startup gates, graceful shutdown |
| `internal/config/config.go` | Env config, loopback validation, timeout defaults, OS-matched browser headers |
| `internal/cookies/jar.go` | Cookie parsing/canonicalization, `SAPISIDHASH`, atomic `0600` credential saves |
| `internal/cookies/rotate.go` | Dual-path rotation (`__Secure-1PSIDTS`, `SIDCC`) |
| `internal/cookies/chrome_sqlite.go` | Browser SQLite extraction + AES decryption (Chrome/Brave/Edge/Firefox) |
| `internal/transport/client.go` | `Chrome_150_PSK` uTLS + HTTP/2, header order, first-byte/stall timeouts |
| `internal/transport/session.go` | Per-account pool, bounded queues, idle reaping |
| `internal/gemini/models.go` | Live catalog, capacity, thinking-level mapping |
| `internal/gemini/client.go` | Auth bootstrap, `batchexecute` (`otAQ7b`, `qpEbW`), cookie absorption |
| `internal/gemini/stream.go` | `StreamGenerate` payloads, live frames, file uploads |
| `internal/gemini/parser.go` | `wrb.fr` parsing, reasoning extraction, model-identity checks |
| `internal/prompt/prompt.go` | Prompt flattening, 1M-context offload, SSRF-guarded image fetch |
| `internal/toolbridge/` | Tool validation, schema checks, streaming tool-call deltas |
| `internal/server/` | Router + auth, chat/responses handlers, quota/session logic, caches |
| `internal/version/version.go` | Build-stamped version info |

MIT License. See [LICENSE](./LICENSE).

**Legal note:** automating `gemini.google.com` sessions may violate Google's Terms of Service and risk quota loss or account action. Use only accounts you control, respect rate limits, keep the bridge on your own machine. Software is provided as-is with no safety guarantees.
#   G e m i n i - S e n t i n e l  
 