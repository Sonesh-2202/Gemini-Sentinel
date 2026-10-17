# Changelog

All notable changes to Gemini-Sentinel are recorded here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `sentinel --setup`: **one-command setup** that imports your Google account
  from the local browser, generates and persists a strong admin key to the
  data-home `.env`, verifies the live model catalog, and (by default) starts
  the server in the same run. `--setup-no-start` performs setup only.
- `--setup [name]` accepts an optional account name (default `main`);
  re-running is idempotent and refreshes that account's cookies.
- Automatic `.env` loading: the first `.env` found in the data home
  (`~/.gemini-sentinel/.env` by default) fills unset environment variables,
  so `SENTINEL_API_KEY` etc. no longer need shell exports. Explicit
  environment variables always win; `SENTINEL_ENV_FILE=off` disables loading.
- Automatic admin-key provisioning on plain server starts
  (`SENTINEL_AUTO_ADMIN_KEY`, default `true`): if accounts are loaded but no
  admin key is configured, one strong key is generated once, persisted, and
  reused on later runs.

### Changed

- `--status` and the no-credentials warning now recommend `sentinel --setup`
  instead of the multi-step manual import flow.
- README Quick Start rebuilt around the two-command `--setup` path; legacy
  python key generation and shell-export steps remain as advanced options.
- README §4 rewritten with a browser-compatibility matrix and every import
  outcome documented (Firefox, Chromium v10/v11, App-Bound v20, locked
  cookie DB, missing SAPISID, flag ordering), plus a "how long do cookies
  last?" answer: automatic rotation keeps sessions alive — re-import is
  only required when Google revokes or expires a session.

### Fixed

- `--setup` hints used an invalid flag order (`sentinel --setup main
  --chrome-profile …`): Go's flag parsing stops at the first positional
  argument, so browser/profile flags were silently ignored. Hints and flag
  help now show `sentinel --chrome-profile brave --setup main`, and
  `--setup` rejects stray extra arguments with a clear explanation.
- Browser-import errors for running browsers now say exactly what to do
  ("close the browser completely and retry") instead of a raw file-lock
  error.

### Added

- `sentinel --status`: offline preflight checks (bind, admin key, accounts)
  with next-step guidance and nonzero exit on problems.
- `GET /metrics`: Prometheus text exposition (per-route request counters,
  account/model gauges, per-account failure gauges). Admin key only.
- `POST /v1/count-tokens`: local token budgeting for `text`, `input`, or
  `messages` payloads using the same estimator as usage reporting.
- GitHub Actions CI (`vet`, `gofmt`, `test`, `race`, `govulncheck`) and
  tagged-release builds (Windows/Linux/macOS + SHA-256 + module SBOM).
- `SENTINEL_*` environment names (plus generic `HOST`/`PORT`).

### Changed

- **Branding**: the placeholder project name is fully retired; the binary,
  module, data dir (`~/.gemini-sentinel`), and all user-facing strings now
  say **Gemini-Sentinel** only. Attribution and MIT notice retained in
  `LICENSE`.
- Per-account API keys are now `sentinel-<name>` (and bare `<name>`).
- Request logs and `/metrics` never carry secrets; startup errors no
  longer echo key material.

### Removed

- Legacy placeholder-name compatibility: `HEARTH_*`/`BRIDGE_*`
  environment variables, `hearth-`/`gsb-` token prefixes, `hearth/`/`gsb/`
  model prefixes, and old data-directory migration. All previous
  endpoints, limits, and security restrictions are preserved.
