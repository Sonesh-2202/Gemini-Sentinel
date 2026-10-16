# Release checklist and operations

## Supported versions

- Go: see `go.mod` (`go 1.26.0`; the toolchain auto-downloads via GOTOOLCHAIN).
- Windows: 10/11 (x86-64). Linux and macOS build from the same source.

## Clean Windows build

```powershell
go version
go mod download
go build -trimpath -o hearth.exe ./cmd/hearth
.\hearth.exe --version
```

Reproducible build with stamped metadata:

```powershell
$ver="v1.0.0"; $sha=(git rev-parse --short HEAD); $date=(Get-Date -Format o)
go build -trimpath -ldflags "-X github.com/soneshsk/hearth/internal/version.Version=$ver -X github.com/soneshsk/hearth/internal/version.Commit=$sha -X github.com/soneshsk/hearth/internal/version.Date=$date" -o hearth.exe ./cmd/hearth
```

## Validation before every release

```powershell
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
```

Optional scanners (report as unavailable if missing, do not silently skip):

```powershell
staticcheck ./...
govulncheck ./...
gosec ./...
go test -cover ./...
```

## Checksums

```powershell
Get-FileHash hearth.exe -Algorithm SHA256
```

Publish the SHA-256 next to the binary. Verify with:

```powershell
(Get-FileHash hearth.exe -Algorithm SHA256).Hash -eq "<published-hash>"
```

## Dependency and license report

```powershell
go list -m all
go mod verify
go-licenses report ./...   # if go-licenses is installed
```

SBOM guidance: `go list -m -json all` output is the module inventory;
convert to SPDX with `bom` or `syft` when available, and archive it with
the release.

## Secure local execution

```powershell
$env:HEARTH_API_KEY = "<64-hex-char secret>"
.\hearth.exe
```

Keep `HEARTH_HOST=127.0.0.1`. The daemon exits on non-loopback binds.

## Upgrade / rollback

1. Stop the daemon (Ctrl-C / SIGTERM; 5s graceful shutdown).
2. Back up `~/.hearth/accounts/` and `session-cache.json`.
3. Replace the binary, re-run validation, restart.
4. Roll back by restoring the previous binary and the backup.

## Credential revocation and deletion

- Logout: delete `accounts/<name>.json` then `POST /v1/refresh` (admin).
- Full wipe: stop the daemon, delete the whole `HEARTH_HOME` directory.
- If cookies leak, follow `SECURITY.md` incident response.
