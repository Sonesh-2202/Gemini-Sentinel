// Package version carries build-stamped release metadata.
// Set via ldflags at release time:
//
//	go build -ldflags "-X github.com/sonesh-2202/gemini-sentinel/internal/version.Version=v1.2.3 -X github.com/sonesh-2202/gemini-sentinel/internal/version.Commit=<sha> -X github.com/sonesh-2202/gemini-sentinel/internal/version.Date=<rfc3339>" ./cmd/sentinel
package version

// Version is the semantic release version (dev when built without ldflags).
var Version = "dev"

// Commit is the source commit hash stamped at build time.
var Commit = "unknown"

// Date is the RFC3339 build timestamp stamped at build time.
var Date = "unknown"

// String returns a one-line build identifier safe to log and display.
func String() string {
	return "sentinel " + Version + " (commit " + Commit + ", built " + Date + ")"
}
