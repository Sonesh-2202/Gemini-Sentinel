# Security Policy

## Supported versions

Only the latest commit on the default branch receives security fixes.
There is no LTS. Upgrade by rebuilding from source and re-running the
full test suite (`go vet ./...`, `go test -race ./...`).

## Threat model

`gsb` holds Google session cookies, which are **equivalent to passwords**.
Anyone who can reach the daemon's HTTP port with a valid API key can spend
your Gemini quota and read your prompts. Anyone who obtains a cookie file
can impersonate your Google session until the cookie is revoked.

The daemon is **safe for local loopback use when configured and maintained
correctly** and **not safe for direct LAN or internet exposure**. It serves
plaintext HTTP and relies on loopback binding plus bearer-token
authentication. There is no remote-access mode: for remote use, keep the
daemon on loopback and place a separately hardened HTTPS reverse proxy
(mTLS or equivalent) in front of it.

## Reporting a vulnerability

Open a private issue or contact the maintainer directly. Do not post
cookies, API keys, prompts, or `accounts/*.json` contents in any report.
Include: version (`gsb --version`), OS, Go version, redacted configuration
(key names only, never values), and steps to reproduce.

## Incident response: leaked cookies

1. In Google account settings, revoke third-party sessions / sign out all
   sessions for the affected account.
2. Delete stored credentials: remove
   `~/.gemini-sentinel/accounts/<name>.json` and
   `~/.gemini-sentinel/session-cache.json`.
3. Rotate `SENTINEL_API_KEY` to a fresh random value (>= 32 chars).
4. Re-import cookies from a clean browser profile (`gsb --import-chrome`).
5. Check Google account activity for unfamiliar sessions.
