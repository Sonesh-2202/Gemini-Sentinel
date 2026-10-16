package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime settings for the gemini-stealth-bridge daemon.
// Environment variables are 100% compatible with gemini-app-bridge.
type Config struct {
	Host               string
	Port               int
	HomeDir            string
	CredFile           string
	AccountsDir        string
	CookieDB           string
	APIKey             string
	AccountTokens      map[string]string
	Temporary          bool
	RejectDowngrade    bool
	LiveThoughts       bool
	SessionCache       bool
	KeepToolPreamble   bool
	MaxPromptChars     int
	MaxOldToolChars    int
	RotateCookiesEvery time.Duration
	ModelRefreshEvery  time.Duration
	StreamTimeout      time.Duration
	FirstByteTimeout   time.Duration
	StallTimeout       time.Duration
	IdleSessionTimeout time.Duration
	MaxSessions        int
	MaxQueueDepth      int
	SessionWaitTimeout time.Duration
	ProxyURL           string
	ImpersonateProfile string
	DefaultThinking    int
	UserAgent          string
	SecCHUA            string
	SecCHUAPlatform    string
	LogLevel           string
}

// ThinkingLevels maps Gemini Web's 3 thinking levels ("low", "medium", "high") to
// Google's Assistant_Lamda_ThinkingLevel protobuf enum (otAQ7b field 24):
//
//	1 = THINKING_LEVEL_STANDARD ("low", default)
//	4 = THINKING_LEVEL_MEDIUM   ("medium")
//	2 = THINKING_LEVEL_EXTENDED ("high")
var ThinkingLevels = map[string]int{
	"low":    1,
	"medium": 4,
	"high":   2,
}

// ServiceName is the user-facing brand. LegacyName is the previous project's
// home directory / token prefix, still accepted for migration.
const (
	ServiceName  = "hearth"
	LegacyName   = "gemini-stealth-bridge"
	TokenPrefix  = "hearth-"
	LegacyPrefix = "gsb-"
)

// LegacyHomeDir returns the previous project's data directory so existing
// installs can be migrated automatically.
func LegacyHomeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini-stealth-bridge")
}

// FromEnv constructs a Config from process environment variables.
// HEARTH_* names take precedence; the older BRIDGE_* (and HOST/PORT) names
// remain accepted so existing setups keep working.
func FromEnv() *Config {
	home, _ := os.UserHomeDir()
	bridgeHome := envFirst("HEARTH_HOME", "BRIDGE_HOME", filepath.Join(home, ".hearth"))
	credFile := envStr("WEB_CRED_FILE", filepath.Join(bridgeHome, "web-credentials.json"))
	accountsDir := filepath.Join(bridgeHome, "accounts")

	tokens := make(map[string]string)
	for _, pair := range strings.FieldsFunc(envFirst("HEARTH_ACCOUNT_TOKENS", "BRIDGE_ACCOUNT_TOKENS", ""), func(r rune) bool { return r == ',' || r == ';' }) {
		if idx := strings.IndexByte(pair, ':'); idx > 0 {
			tok := strings.TrimSpace(pair[:idx])
			acc := strings.TrimSpace(pair[idx+1:])
			if tok != "" && acc != "" {
				tokens[tok] = acc
			}
		}
	}

	defThinkKey := strings.ToLower(strings.TrimSpace(envFirst("HEARTH_DEFAULT_THINKING", "BRIDGE_DEFAULT_THINKING", "high")))
	defThink, ok := ThinkingLevels[defThinkKey]
	if !ok {
		defThink = 2
	}

	impersonate := envFirst("HEARTH_IMPERSONATE", "BRIDGE_IMPERSONATE", "chrome_150_psk")
	ua, chUA, platform := browserHeadersForOS(runtime.GOOS, impersonate)

	return &Config{
		Host:               firstNonEmpty(os.Getenv("HEARTH_HOST"), os.Getenv("BRIDGE_HOST"), envStr("HOST", "127.0.0.1")),
		Port:               firstNonEmptyInt3("HEARTH_PORT", "BRIDGE_PORT", "PORT", 20133),
		HomeDir:            bridgeHome,
		CredFile:           credFile,
		AccountsDir:        accountsDir,
		CookieDB:           strings.TrimSpace(os.Getenv("CHROME_COOKIES")),
		APIKey:             strings.TrimSpace(envFirst("HEARTH_API_KEY", "BRIDGE_API_KEY", "")),
		AccountTokens:      tokens,
		Temporary:          envBoolFirst("HEARTH_TEMPORARY", "BRIDGE_TEMPORARY", false),
		RejectDowngrade:    envBoolFirst("HEARTH_REJECT_DOWNGRADE", "BRIDGE_REJECT_DOWNGRADE", true),
		LiveThoughts:       envBoolFirst("HEARTH_LIVE_THOUGHTS", "BRIDGE_LIVE_THOUGHTS", true),
		SessionCache:       envBoolFirst("HEARTH_SESSION_CACHE", "BRIDGE_SESSION_CACHE", true),
		KeepToolPreamble:   envBoolFirst("HEARTH_TOOL_PREAMBLE", "BRIDGE_TOOL_PREAMBLE", false),
		MaxPromptChars:     envIntFirst("HEARTH_MAX_PROMPT_CHARS", "BRIDGE_MAX_PROMPT_CHARS", 850000),
		MaxOldToolChars:    envIntFirst("HEARTH_MAX_OLD_TOOL_CHARS", "BRIDGE_MAX_OLD_TOOL_CHARS", 32000),
		RotateCookiesEvery: time.Duration(envIntFirst("HEARTH_ROTATE_COOKIES_MS", "BRIDGE_ROTATE_COOKIES_MS", 540000)) * time.Millisecond,
		ModelRefreshEvery:  time.Duration(envIntFirst("HEARTH_MODEL_REFRESH_MS", "BRIDGE_MODEL_REFRESH_MS", 600000)) * time.Millisecond,
		StreamTimeout:      time.Duration(envIntFirst("HEARTH_STREAM_TIMEOUT_MS", "BRIDGE_STREAM_TIMEOUT_MS", 600000)) * time.Millisecond,
		FirstByteTimeout:   time.Duration(envIntFirst("HEARTH_FIRST_BYTE_TIMEOUT_MS", "BRIDGE_FIRST_BYTE_TIMEOUT_MS", 60000)) * time.Millisecond,
		StallTimeout:       time.Duration(envIntFirst("HEARTH_STALL_TIMEOUT_MS", "BRIDGE_STALL_TIMEOUT_MS", 90000)) * time.Millisecond,
		IdleSessionTimeout: time.Duration(envIntFirst("HEARTH_IDLE_SESSION_MS", "BRIDGE_IDLE_SESSION_MS", 180000)) * time.Millisecond,
		MaxSessions:        envIntFirst("HEARTH_MAX_SESSIONS", "BRIDGE_MAX_SESSIONS", 16),
		MaxQueueDepth:      envIntFirst("HEARTH_MAX_QUEUE", "BRIDGE_MAX_QUEUE", 4),
		SessionWaitTimeout: time.Duration(envIntFirst("HEARTH_SESSION_WAIT_MS", "BRIDGE_SESSION_WAIT_MS", 10000)) * time.Millisecond,
		ProxyURL:           strings.TrimSpace(envFirst("HEARTH_PROXY_SERVER", "BRIDGE_PROXY_SERVER", "")),
		ImpersonateProfile: impersonate,
		DefaultThinking:    defThink,
		UserAgent:          ua,
		SecCHUA:            chUA,
		SecCHUAPlatform:    platform,
		LogLevel:           strings.ToLower(strings.TrimSpace(envFirst("HEARTH_LOG_LEVEL", "BRIDGE_LOG_LEVEL", "info"))),
	}
}

// Load returns a value copy of FromEnv().
func Load() Config {
	return *FromEnv()
}

// IsLoopbackHost reports whether host is a loopback address.
// Only 127.0.0.0/8, ::1, and "localhost" are accepted. Everything else,
// including 0.0.0.0, ::, LAN, public, and malformed values, returns false
// so the caller fails closed.
func IsLoopbackHost(h string) bool {
	host := strings.TrimSpace(h)
	// Strip optional brackets (e.g. "[::1]") and optional port suffix.
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end > 0 {
			host = host[1:end]
		} else {
			return false
		}
	} else if strings.Count(host, ":") == 1 && strings.IndexByte(host, ':') >= 0 {
		// Bare host:port form such as "127.0.0.1:20133" is not a valid bind
		// hostname on its own; reject so callers pass Host and Port separately.
		if bare, _, err := splitHostPortLenient(host); err == nil {
			host = bare
		} else {
			return false
		}
	}
	lower := strings.ToLower(host)
	if lower == "localhost" {
		return true
	}
	// net.ParseIP handles 127.x.x.x, ::1, and ::ffff:127.0.0.1; IsLoopback
	// covers the full 127.0.0.0/8 range.
	ip := net.ParseIP(strings.TrimSuffix(host, "."))
	return ip != nil && ip.IsLoopback()
}

// AdminKeyValid reports whether an admin bearer token is strong enough for
// production use: at least 16 characters and not a well-known placeholder.
func AdminKeyValid(key string) bool {
	if len(key) < 16 {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(key))
	switch lower {
	case "public", "test", "password", "changeme", "admin", "secret":
		return false
	}
	return strings.TrimSpace(key) != ""
}

// browserHeadersForOS aligns User-Agent and Sec-CH-UA* headers with both the
// host kernel's TCP stack (avoiding OS/JA4T cross-layer contradiction) and the
// active Chrome uTLS profile version.
func browserHeadersForOS(goos, profile string) (ua, secCHUA, platform string) {
	ver := "150"
	p := strings.ToLower(profile)
	switch {
	case strings.Contains(p, "152"):
		ver = "152"
	case strings.Contains(p, "150"):
		ver = "150"
	case strings.Contains(p, "146"):
		ver = "146"
	case strings.Contains(p, "144"):
		ver = "144"
	case strings.Contains(p, "133"):
		ver = "133"
	case strings.Contains(p, "131"):
		ver = "131"
	}

	secCHUA = `"Google Chrome";v="` + ver + `", "Chromium";v="` + ver + `", "Not?A_Brand";v="24"`
	switch goos {
	case "darwin":
		platform = `"macOS"`
		ua = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	case "windows":
		platform = `"Windows"`
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	default:
		platform = `"Linux"`
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + ver + ".0.0.0 Safari/537.36"
	}
	return ua, secCHUA, platform
}

func envStr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstNonEmptyInt(primaryKey, fallbackKey string, fallback int) int {
	return firstNonEmptyInt3(primaryKey, fallbackKey, "", fallback)
}

func firstNonEmptyInt3(primaryKey, fallbackKey, thirdKey string, fallback int) int {
	for _, key := range []string{primaryKey, fallbackKey, thirdKey} {
		if key == "" {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
			return fallback
		}
	}
	return fallback
}

// envFirst returns the first non-empty value among keys, or def.
func envFirst(keys ...string) string {
	for i, k := range keys {
		if i == len(keys)-1 {
			break
		}
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	if len(keys) > 0 {
		return keys[len(keys)-1]
	}
	return ""
}

func envIntFirst(primaryKey, fallbackKey string, fallback int) int {
	return firstNonEmptyInt3(primaryKey, fallbackKey, "", fallback)
}

func envBoolFirst(primaryKey, fallbackKey string, fallback bool) bool {
	for _, key := range []string{primaryKey, fallbackKey} {
		if v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v != "" {
			switch v {
			case "1", "true", "yes", "on":
				return true
			case "0", "false", "no", "off":
				return false
			default:
				return fallback
			}
		}
	}
	return fallback
}

func splitHostPortLenient(hostport string) (string, string, error) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p, nil
	}
	// Accept "host:port" without brackets only when there is exactly one colon.
	if strings.Count(hostport, ":") == 1 {
		idx := strings.IndexByte(hostport, ':')
		return hostport[:idx], hostport[idx+1:], nil
	}
	return "", "", fmt.Errorf("invalid host:port %q", hostport)
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
