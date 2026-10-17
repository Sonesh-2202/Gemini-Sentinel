package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DotEnvFileName is the settings file stored in the sentinel data directory.
// `sentinel --setup` writes generated settings there; every subsequent run
// applies the file's keys to fill any unset environment variables, so users
// never need to export anything manually.
const DotEnvFileName = ".env"

// LoadDotEnv reads a small KEY=VALUE file and returns its non-comment,
// non-empty entries. Values may be wrapped in single or double quotes.
// Unknown keys are passed through verbatim. Missing files return nil.
func LoadDotEnv(path string) map[string]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if val == "" {
			out[key] = ""
			continue
		}
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out
}

// SaveDotEnv merges keys into the file at path (creating it if needed),
// updating values for keys it owns and appending any new ones. Pre-existing
// keys not present in pairs and comment lines are preserved as-is so user
// edits survive. All writes are atomic-by-convention: the whole file is
// rewritten with 0600 permissions.
func SaveDotEnv(path string, pairs map[string]string) error {
	for k, v := range pairs {
		if strings.TrimSpace(k) != k || k == "" || strings.ContainsAny(k, " \t\n\r=") {
			return fmt.Errorf("refusing to persist invalid key %q to %s", k, path)
		}
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("refusing to persist key %q: value contains a newline", k)
		}
	}

	eol := "\n"
	var lines []string
	if raw, err := os.ReadFile(path); err == nil {
		if strings.Contains(string(raw), "\r\n") {
			eol = "\r\n"
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSuffix(line, "\r")
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				if eq := strings.IndexByte(line, '='); eq > 0 {
					key := strings.TrimSpace(line[:eq])
					if _, ok := pairs[key]; ok {
						lines = append(lines, key+"="+pairs[key])
						continue
					}
				}
			}
			lines = append(lines, line)
		}
	}

	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			seen[strings.TrimSpace(line[:eq])] = true
		}
	}
	for _, k := range sortedKeys(pairs) {
		if !seen[k] {
			lines = append(lines, k+"="+pairs[k])
		}
	}

	if err := os.WriteFile(path, []byte(strings.Join(lines, eol)), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// EnvFileNameFor returns the default settings file for a data directory.
func EnvFileName(dir string) string {
	return filepath.Join(dir, DotEnvFileName)
}

// resolveHomeDir mirrors FromEnv's data-home resolution so `.env`
// discovery and FromEnv never disagree.
func resolveHomeDir() string {
	home, _ := os.UserHomeDir()
	return envStr("SENTINEL_HOME", filepath.Join(home, ".gemini-sentinel"))
}

// applyDotEnvDefaults loads the sentinel `.env` file(s) and applies each key
// only when the process environment does not already define it — explicit
// environment always wins, so the file only fills gaps (zero-config defaults,
// never overrides). SENTINEL_ENV_FILE=off disables loading entirely, and a
// custom SENTINEL_ENV_FILE path replaces the per-directory lookup.
func applyDotEnvDefaults() {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SENTINEL_ENV_FILE")), "off") {
		return
	}
	if custom := strings.TrimSpace(os.Getenv("SENTINEL_ENV_FILE")); custom != "" {
		applyDotEnvPairs(LoadDotEnv(custom))
		return
	}
	dirs := []string{resolveHomeDir()}
	for _, dir := range dirs {
		applyDotEnvPairs(LoadDotEnv(EnvFileName(dir)))
	}
}

func applyDotEnvPairs(pairs map[string]string) {
	for k, v := range pairs {
		if _, present := os.LookupEnv(k); present {
			continue
		}
		_ = os.Setenv(k, v)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
