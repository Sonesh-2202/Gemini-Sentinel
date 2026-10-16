# Changelog

All notable changes to Hearth are recorded here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `hearth --status`: offline preflight checks (bind, admin key, accounts)
  with next-step guidance and nonzero exit on problems.
- `GET /metrics`: Prometheus text exposition (per-route request counters,
  account/model gauges, per-account failure gauges). Admin key only.
- `POST /v1/count-tokens`: local token budgeting for `text`, `input`, or
  `messages` payloads using the same estimator as usage reporting.
- GitHub Actions CI (`vet`, `gofmt`, `test`, `race`, `govulncheck`) and
  tagged-release builds (Windows/Linux/macOS + SHA-256 + module SBOM).
- `HEARTH_*` environment names; `BRIDGE_*`/`HOST`/`PORT` remain accepted.
- Automatic first-run migration of `~/.gemini-stealth-bridge/accounts`
  into `~/.hearth/accounts` (source untouched, no overwrites).

### Changed

- **Rebrand**: `gemini-stealth-bridge` → **Hearth** by Sonesh SK
  (module `github.com/soneshsk/hearth`, binary `hearth`, data dir
  `~/.hearth`). Fork origin and original MIT notice retained in `LICENSE`.
- Per-account API keys are now `hearth-<name>` (and bare `<name>`);
  `gsb-<name>` tokens and the `gsb/` model prefix keep working.
- Request logs and `/metrics` never carry secrets; startup errors no
  longer echo key material.

### Removed

- Nothing. All previous endpoints, limits, and security restrictions
  are preserved.
