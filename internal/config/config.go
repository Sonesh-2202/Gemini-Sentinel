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

// Config holds all runtime settings for the sentinel daemon.
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
	// AutoAdminKey (SENTINEL_AUTO_ADMIN_KEY, default true) makes the server
	// generate and persist a strong admin key in the data home `.env` when
	// SENTINEL_API_KEY is unset, so admin endpoints work without manual setup.
	AutoAdminKey bool
	EnvFilePath  string // resolved path of the data-home .env settings file
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

// ServiceName is the user-facing brand. BrandFull is the full project name.
const (
	ServiceName = "sentinel"
	BrandFull   = "Gemini-Sentinel"
	TokenPrefix = "sentinel-"
)

// FromEnv constructs a Config from process environment variables.
// A local `.env` file inside the data home is loaded first, but only fills
// keys not already present in the environment, so explicit exports always win.
func FromEnv() *Config {
	applyDotEnvDefaults()
	dataHome := resolveHomeDir()
	credFile := envStr("WEB_CRED_FILE", filepath.Join(dataHome, "web-credentials.json"))
	accountsDir := filepath.Join(dataHome, "accounts")

	tokens := make(map[string]string)
	for _, pair := range strings.FieldsFunc(envStr("SENTINEL_ACCOUNT_TOKENS", ""), func(r rune) bool { return r == ',' || r == ';' }) {
		if idx := strings.IndexByte(pair, ':'); idx > 0 {
			tok := strings.TrimSpace(pair[:idx])
			acc := strings.TrimSpace(pair[idx+1:])
			if tok != "" && acc != "" {
				tokens[tok] = acc
			}
		}
	}

	defThinkKey := strings.ToLower(strings.TrimSpace(envStr("SENTINEL_DEFAULT_THINKING", "high")))
	defThink, ok := ThinkingLevels[defThinkKey]
	if !ok {
		defThink = 2
	}

	impersonate := envStr("SENTINEL_IMPERSONATE", "chrome_150_psk")
	ua, chUA, platform := browserHeadersForOS(runtime.GOOS, impersonate)

	return &Config{
		Host:               firstNonEmpty(os.Getenv("SENTINEL_HOST"), envStr("HOST", "127.0.0.1")),
		Port:               firstNonEmptyIntN(20133, "SENTINEL_PORT", "PORT"),
		HomeDir:            dataHome,
		CredFile:           credFile,
		AccountsDir:        accountsDir,
		CookieDB:           strings.TrimSpace(os.Getenv("CHROME_COOKIES")),
		APIKey:             strings.TrimSpace(envStr("SENTINEL_API_KEY", "")),
		AccountTokens:      tokens,
		Temporary:          envBool("SENTINEL_TEMPORARY", false),
		RejectDowngrade:    envBool("SENTINEL_REJECT_DOWNGRADE", true),
		LiveThoughts:       envBool("SENTINEL_LIVE_THOUGHTS", true),
		SessionCache:       envBool("SENTINEL_SESSION_CACHE", true),
		KeepToolPreamble:   envBool("SENTINEL_TOOL_PREAMBLE", false),
		MaxPromptChars:     envInt("SENTINEL_MAX_PROMPT_CHARS", 850000),
		MaxOldToolChars:    envInt("SENTINEL_MAX_OLD_TOOL_CHARS", 32000),
		RotateCookiesEvery: time.Duration(envInt("SENTINEL_ROTATE_COOKIES_MS", 540000)) * time.Millisecond,
		ModelRefreshEvery:  time.Duration(envInt("SENTINEL_MODEL_REFRESH_MS", 600000)) * time.Millisecond,
		StreamTimeout:      time.Duration(envInt("SENTINEL_STREAM_TIMEOUT_MS", 600000)) * time.Millisecond,
		FirstByteTimeout:   time.Duration(envInt("SENTINEL_FIRST_BYTE_TIMEOUT_MS", 60000)) * time.Millisecond,
		StallTimeout:       time.Duration(envInt("SENTINEL_STALL_TIMEOUT_MS", 90000)) * time.Millisecond,
		IdleSessionTimeout: time.Duration(envInt("SENTINEL_IDLE_SESSION_MS", 180000)) * time.Millisecond,
		MaxSessions:        envInt("SENTINEL_MAX_SESSIONS", 16),
		MaxQueueDepth:      envInt("SENTINEL_MAX_QUEUE", 4),
		SessionWaitTimeout: time.Duration(envInt("SENTINEL_SESSION_WAIT_MS", 10000)) * time.Millisecond,
		ProxyURL:           strings.TrimSpace(envStr("SENTINEL_PROXY_SERVER", "")),
		ImpersonateProfile: impersonate,
		DefaultThinking:    defThink,
		UserAgent:          ua,
		SecCHUA:            chUA,
		SecCHUAPlatform:    platform,
		LogLevel:           strings.ToLower(strings.TrimSpace(envStr("SENTINEL_LOG_LEVEL", "info"))),
		AutoAdminKey:       envBool("SENTINEL_AUTO_ADMIN_KEY", true),
		EnvFilePath:        EnvFileName(dataHome),
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

// legacy env-var tiers were removed: only SENTINEL_* names are honored.
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

func firstNonEmptyIntN(fallback int, keys ...string) int {
	for _, key := range keys {
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
