package cookies

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
	_ "modernc.org/sqlite"
)

// BrowserProfiles maps well-known Linux browser names to their default Cookie DB paths.
var BrowserProfiles = map[string]string{
	"chrome":   ".config/google-chrome/Default/Cookies",
	"chromium": ".config/chromium/Default/Cookies",
	"brave":    ".config/BraveSoftware/Brave-Browser/Default/Cookies",
	"edge":     ".config/microsoft-edge/Default/Cookies",
	"vivaldi":  ".config/vivaldi/Default/Cookies",
}

var (
	keyringApps   = []string{"chrome", "brave", "chromium", "microsoft-edge", "vivaldi", "opera"}
	knownBrowsers = []string{"chrome", "brave", "chromium", "edge", "vivaldi"}
	// dpapiUnprotect decrypts a Windows DPAPI blob; overridable in tests.
	dpapiUnprotect = platformDPAPIUnprotect
)

// CookieRow represents a decrypted cookie from a browser SQLite database.
type CookieRow struct {
	Host  string
	Name  string
	Value string
}

// ReadResult summarizes a browser cookie DB extraction.
type ReadResult struct {
	Cookies     []CookieRow
	Header      string
	Decrypted   int
	Plain       int
	Failed      int
	DomainBound int
	KeyFound    bool
}

func isKnownBrowser(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "chrome", "chromium", "brave", "edge", "vivaldi":
		return true
	default:
		return false
	}
}

func expandPercentEnv(s string) string {
	for {
		start := strings.IndexByte(s, '%')
		if start < 0 {
			return s
		}
		end := strings.IndexByte(s[start+1:], '%')
		if end <= 0 {
			return s
		}
		end += start + 1
		key := s[start+1 : end]
		val := os.Getenv(key)
		if val == "" {
			return s
		}
		s = s[:start] + val + s[end+1:]
	}
}

func normalizeInputPath(raw string) string {
	p := strings.TrimSpace(raw)
	p = strings.Trim(p, `"'`)
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = expandPercentEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	for len(p) > 1 && (strings.HasSuffix(p, "/") || strings.HasSuffix(p, `\`)) {
		if len(p) == 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
			break
		}
		p = p[:len(p)-1]
	}
	return p
}

func resolveCookiePath(rawPath string) (string, bool) {
	candidates := []string{rawPath}
	if os.PathSeparator == '/' && strings.Contains(rawPath, `\`) {
		candidates = append(candidates, strings.ReplaceAll(rawPath, `\`, "/"))
	} else if os.PathSeparator == '\\' && strings.Contains(rawPath, "/") {
		candidates = append(candidates, strings.ReplaceAll(rawPath, "/", `\`))
	}
	for _, p := range candidates {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			return p, true
		}
		relCandidates := []string{
			filepath.Join(p, "Network", "Cookies"),
			filepath.Join(p, "Cookies"),
			filepath.Join(p, "cookies.sqlite"),
			filepath.Join(p, "Default", "Network", "Cookies"),
			filepath.Join(p, "Default", "Cookies"),
		}
		for _, c := range relCandidates {
			if cst, err := os.Stat(c); err == nil && !cst.IsDir() {
				return c, true
			}
		}
	}
	return "", false
}

func isPathLike(s string) bool {
	if strings.ContainsAny(s, `/\`) || strings.HasSuffix(s, "Cookies") || strings.HasSuffix(strings.ToLower(s), ".sqlite") {
		return true
	}
	if len(s) >= 2 && s[1] == ':' && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) {
		return true
	}
	return false
}

func browserUserDataDirs(browser string) []string {
	b := strings.ToLower(strings.TrimSpace(browser))
	home, _ := os.UserHomeDir()
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" && home != "" {
		localAppData = filepath.Join(home, "AppData", "Local")
	}
	var dirs []string
	addWin := func() {
		if localAppData == "" {
			return
		}
		switch b {
		case "chrome":
			dirs = append(dirs, filepath.Join(localAppData, "Google", "Chrome", "User Data"))
		case "chromium":
			dirs = append(dirs, filepath.Join(localAppData, "Chromium", "User Data"))
		case "brave":
			dirs = append(dirs, filepath.Join(localAppData, "BraveSoftware", "Brave-Browser", "User Data"))
		case "edge":
			dirs = append(dirs, filepath.Join(localAppData, "Microsoft", "Edge", "User Data"))
		case "vivaldi":
			dirs = append(dirs, filepath.Join(localAppData, "Vivaldi", "User Data"))
		}
	}
	addMac := func() {
		if home == "" {
			return
		}
		appSupport := filepath.Join(home, "Library", "Application Support")
		switch b {
		case "chrome":
			dirs = append(dirs, filepath.Join(appSupport, "Google", "Chrome"))
		case "chromium":
			dirs = append(dirs, filepath.Join(appSupport, "Chromium"))
		case "brave":
			dirs = append(dirs, filepath.Join(appSupport, "BraveSoftware", "Brave-Browser"))
		case "edge":
			dirs = append(dirs, filepath.Join(appSupport, "Microsoft Edge"))
		case "vivaldi":
			dirs = append(dirs, filepath.Join(appSupport, "Vivaldi"))
		}
	}
	addLinux := func() {
		if home == "" {
			return
		}
		cfgDir := filepath.Join(home, ".config")
		switch b {
		case "chrome":
			dirs = append(dirs, filepath.Join(cfgDir, "google-chrome"))
		case "chromium":
			dirs = append(dirs, filepath.Join(cfgDir, "chromium"))
		case "brave":
			dirs = append(dirs, filepath.Join(cfgDir, "BraveSoftware", "Brave-Browser"))
		case "edge":
			dirs = append(dirs, filepath.Join(cfgDir, "microsoft-edge"))
		case "vivaldi":
			dirs = append(dirs, filepath.Join(cfgDir, "vivaldi"))
		}
	}

	switch runtime.GOOS {
	case "windows":
		addWin()
		addLinux()
		addMac()
	case "darwin":
		addMac()
		addLinux()
		addWin()
	default:
		addLinux()
		addWin()
		addMac()
	}
	return dirs
}

// ParseProfileHint extracts a target browser name and profile directory name (e.g. "Profile 4")
// from a --chrome-profile / --cookie-db argument so interactive login can launch the right profile.
func ParseProfileHint(nameOrPath string) (browser, profileDir string) {
	p := normalizeInputPath(nameOrPath)
	if p == "" {
		return "", ""
	}
	lower := strings.ToLower(p)
	if isKnownBrowser(lower) {
		return lower, "Default"
	}
	if idx := strings.IndexByte(p, ':'); idx > 1 && isKnownBrowser(p[:idx]) {
		return strings.ToLower(strings.TrimSpace(p[:idx])), strings.TrimSpace(p[idx+1:])
	}
	switch {
	case strings.Contains(lower, "brave"):
		browser = "brave"
	case strings.Contains(lower, "edge"):
		browser = "edge"
	case strings.Contains(lower, "vivaldi"):
		browser = "vivaldi"
	case strings.Contains(lower, "chromium"):
		browser = "chromium"
	case strings.Contains(lower, "chrome"):
		browser = "chrome"
	}
	if !strings.ContainsAny(p, `/\`) {
		return browser, p
	}
	norm := strings.ReplaceAll(p, `\`, "/")
	parts := strings.Split(strings.TrimRight(norm, "/"), "/")
	for len(parts) > 0 {
		last := strings.ToLower(parts[len(parts)-1])
		if last == "cookies" || last == "network" || strings.HasSuffix(last, ".sqlite") {
			parts = parts[:len(parts)-1]
			continue
		}
		break
	}
	if len(parts) == 0 {
		return browser, ""
	}
	last := parts[len(parts)-1]
	lastLower := strings.ToLower(last)
	if lastLower == "default" || strings.HasPrefix(lastLower, "profile ") || strings.HasPrefix(lastLower, "profile_") {
		return browser, last
	}
	if len(parts) >= 2 {
		parentLower := strings.ToLower(parts[len(parts)-2])
		if parentLower == "user data" || parentLower == "google-chrome" || parentLower == "chromium" || parentLower == "brave-browser" || parentLower == "microsoft-edge" || parentLower == "vivaldi" {
			return browser, last
		}
	}
	return browser, ""
}

// FindBrowserCookieDB resolves a browser name, profile name, profile directory, or file path
// to an existing SQLite Cookie DB.
func FindBrowserCookieDB(nameOrPath string) (string, error) {
	p := normalizeInputPath(nameOrPath)
	if p == "" {
		return "", fmt.Errorf("empty browser or cookie database path")
	}
	if resolved, ok := resolveCookiePath(p); ok {
		return resolved, nil
	}
	if isPathLike(p) {
		return "", fmt.Errorf("cookie database not found at %s", nameOrPath)
	}
	if idx := strings.IndexByte(p, ':'); idx > 1 && isKnownBrowser(p[:idx]) {
		bName := strings.ToLower(strings.TrimSpace(p[:idx]))
		profName := strings.TrimSpace(p[idx+1:])
		if profName != "" {
			for _, ud := range browserUserDataDirs(bName) {
				if resolved, ok := resolveCookiePath(filepath.Join(ud, profName)); ok {
					return resolved, nil
				}
			}
			return "", fmt.Errorf("no cookie database found for %s profile %q", bName, profName)
		}
	}
	lower := strings.ToLower(p)
	home, _ := os.UserHomeDir()
	rel, inMap := BrowserProfiles[lower]
	if isKnownBrowser(lower) || inMap {
		for _, ud := range browserUserDataDirs(lower) {
			if resolved, ok := resolveCookiePath(filepath.Join(ud, "Default")); ok {
				return resolved, nil
			}
			if resolved, ok := resolveCookiePath(ud); ok {
				return resolved, nil
			}
		}
		if inMap && home != "" {
			full := filepath.Join(home, rel)
			if resolved, ok := resolveCookiePath(full); ok {
				return resolved, nil
			}
			return "", fmt.Errorf("no cookie database found for %s at %s", nameOrPath, full)
		}
		return "", fmt.Errorf("no cookie database found for %s", nameOrPath)
	}
	// Check if nameOrPath is a profile directory name (e.g. "Profile 4", "Default") under any known browser.
	for _, bName := range knownBrowsers {
		for _, ud := range browserUserDataDirs(bName) {
			if resolved, ok := resolveCookiePath(filepath.Join(ud, p)); ok {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("unknown browser %q (known: chrome, chromium, brave, edge, vivaldi; or pass a profile directory/path)", nameOrPath)
}

// ReadBrowserCookies reads and decrypts Google session cookies from a Chrome/Chromium
// or Firefox SQLite database without locking the browser's live file.
func ReadBrowserCookies(dbPath string) (*ReadResult, error) {
	tmpDir := filepath.Join(os.TempDir(), fmt.Sprintf("sentinel-ck-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	tmpDB := filepath.Join(tmpDir, "Cookies.db")
	if err := copyFile(dbPath, tmpDB); err != nil {
		return nil, fmt.Errorf("copy cookie db: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			_ = copyFile(dbPath+suffix, tmpDB+suffix)
		}
	}

	db, err := sql.Open("sqlite", tmpDB)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if strings.HasSuffix(strings.ToLower(dbPath), ".sqlite") || strings.Contains(strings.ToLower(dbPath), "firefox") {
		return readFirefoxCookies(db)
	}
	return readChromiumCookies(db, dbPath)
}

func readFirefoxCookies(db *sql.DB) (*ReadResult, error) {
	rows, err := db.Query(`SELECT host, name, value FROM moz_cookies WHERE host LIKE '%google.com' OR host LIKE '%gemini.google%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &ReadResult{KeyFound: true}
	for rows.Next() {
		var host, name, value string
		if err := rows.Scan(&host, &name, &value); err != nil {
			continue
		}
		res.Plain++
		res.Cookies = append(res.Cookies, CookieRow{Host: host, Name: name, Value: value})
	}
	res.Header = bestHeaderFromRows(res.Cookies)
	return res, nil
}

type rawChromiumRow struct {
	host      string
	name      string
	encrypted []byte
	plain     string
}

func readChromiumCookies(db *sql.DB, dbPath string) (*ReadResult, error) {
	rows, err := db.Query(`SELECT host_key, name, encrypted_value, value FROM cookies WHERE host_key LIKE '%google%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rawRows []rawChromiumRow
	for rows.Next() {
		var r rawChromiumRow
		if err := rows.Scan(&r.host, &r.name, &r.encrypted, &r.plain); err != nil {
			continue
		}
		rawRows = append(rawRows, r)
	}

	key256, key128, keyFound := detectBestKey(rawRows, dbPath)
	res := &ReadResult{KeyFound: keyFound}
	for _, r := range rawRows {
		val := r.plain
		if len(r.encrypted) > 0 {
			if !keyFound {
				res.Failed++
				continue
			}
			dec := decryptCookieBytes(r.encrypted, key256, key128)
			if dec == nil {
				res.Failed++
				continue
			}
			stripped, bound := stripDomainPrefix(dec, r.host)
			if bound {
				res.DomainBound++
			}
			val = string(stripped)
			res.Decrypted++
		} else {
			res.Plain++
		}
		res.Cookies = append(res.Cookies, CookieRow{Host: r.host, Name: r.name, Value: val})
	}
	res.Header = bestHeaderFromRows(res.Cookies)
	return res, nil
}

type localStateFile struct {
	OSCrypt struct {
		EncryptedKey string `json:"encrypted_key"`
	} `json:"os_crypt"`
}

func extractLocalStateDPAPIBlob(localStatePath string) ([]byte, error) {
	data, err := os.ReadFile(localStatePath)
	if err != nil {
		return nil, err
	}
	var ls localStateFile
	if err := json.Unmarshal(data, &ls); err != nil {
		return nil, err
	}
	if ls.OSCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("missing os_crypt.encrypted_key in %s", localStatePath)
	}
	raw, err := base64.StdEncoding.DecodeString(ls.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, err
	}
	if len(raw) <= 5 || string(raw[:5]) != "DPAPI" {
		return nil, fmt.Errorf("unexpected encrypted_key prefix in %s", localStatePath)
	}
	return raw[5:], nil
}

func readLocalStateMasterKey(localStatePath string) ([]byte, error) {
	blob, err := extractLocalStateDPAPIBlob(localStatePath)
	if err != nil {
		return nil, err
	}
	return dpapiUnprotect(blob)
}

func findLocalStatePaths(dbPath string) []string {
	var out []string
	seen := make(map[string]bool)
	addIfFile := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			seen[p] = true
			out = append(out, p)
		}
	}
	if dbPath != "" {
		dir := filepath.Dir(dbPath)
		for i := 0; i < 4 && dir != ""; i++ {
			addIfFile(filepath.Join(dir, "Local State"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	for _, bName := range knownBrowsers {
		for _, ud := range browserUserDataDirs(bName) {
			addIfFile(filepath.Join(ud, "Local State"))
		}
	}
	return out
}

func candidatePasswords() []string {
	var passes []string
	if envPass := os.Getenv("CHROME_PASS"); envPass != "" {
		passes = append(passes, envPass)
	} else if runtime.GOOS != "windows" {
		seen := map[string]bool{}
		for _, app := range keyringApps {
			out, err := exec.Command("secret-tool", "lookup", "application", app).Output()
			if err == nil {
				p := strings.TrimSpace(string(out))
				if p != "" && !seen[p] {
					seen[p] = true
					passes = append(passes, p)
				}
			}
		}
	}
	passes = append(passes, "peanuts", "")
	return passes
}

type keyCandidate struct {
	k256 []byte
	k128 []byte
}

func detectBestKey(rows []rawChromiumRow, dbPath string) (best256, best128 []byte, ok bool) {
	var candidates []keyCandidate
	localStateKeyFound := false
	for _, lsPath := range findLocalStatePaths(dbPath) {
		if k, err := readLocalStateMasterKey(lsPath); err == nil && len(k) == 32 {
			candidates = append(candidates, keyCandidate{k256: k})
			localStateKeyFound = true
		}
	}
	passes := candidatePasswords()
	for _, p := range passes {
		k256 := pbkdf2.Key([]byte(p), []byte("saltysalt"), 1, 32, sha1.New)
		k128 := pbkdf2.Key([]byte(p), []byte("saltysalt"), 1, 16, sha1.New)
		candidates = append(candidates, keyCandidate{k256: k256, k128: k128})
		if runtime.GOOS == "darwin" && p != "" {
			k128Mac := pbkdf2.Key([]byte(p), []byte("saltysalt"), 1003, 16, sha1.New)
			candidates = append(candidates, keyCandidate{k128: k128Mac})
		}
	}

	encryptedRows := 0
	for _, r := range rows {
		if len(r.encrypted) > 0 {
			encryptedRows++
		}
	}
	if encryptedRows == 0 {
		return nil, nil, true
	}

	bestScore := 0
	for _, cand := range candidates {
		score := 0
		for _, r := range rows {
			if len(r.encrypted) == 0 {
				continue
			}
			if decryptCookieBytes(r.encrypted, cand.k256, cand.k128) != nil {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			best256 = cand.k256
			best128 = cand.k128
			ok = true
			if score == encryptedRows {
				break
			}
		}
	}
	if !ok && localStateKeyFound && len(candidates) > 0 {
		return candidates[0].k256, nil, true
	}
	return best256, best128, ok
}

func decryptCookieBytes(buf, key256, key128 []byte) []byte {
	if len(buf) < 4 {
		return nil
	}
	prefix := string(buf[:3])
	if prefix != "v10" && prefix != "v11" {
		if plain, err := dpapiUnprotect(buf); err == nil && len(plain) > 0 {
			return plain
		}
		return nil
	}
	rest := buf[3:]
	// Try AES-256-GCM (Chrome / Brave modern Linux keyring)
	if len(rest) >= 12+16 && len(key256) == 32 {
		if block, err := aes.NewCipher(key256); err == nil {
			if gcm, err := cipher.NewGCM(block); err == nil {
				nonce := rest[:12]
				ctAndTag := rest[12:]
				if plain, err := gcm.Open(nil, nonce, ctAndTag, nil); err == nil {
					return plain
				}
			}
		}
	}
	// Try AES-128-CBC (Chromium / v10 legacy)
	if len(rest) >= aes.BlockSize && len(rest)%aes.BlockSize == 0 && len(key128) == 16 {
		if block, err := aes.NewCipher(key128); err == nil {
			iv := bytes.Repeat([]byte{0x20}, aes.BlockSize)
			out := make([]byte, len(rest))
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, rest)
			pad := int(out[len(out)-1])
			if pad > 0 && pad <= aes.BlockSize && pad <= len(out) {
				validPad := true
				for i := len(out) - pad; i < len(out); i++ {
					if int(out[i]) != pad {
						validPad = false
						break
					}
				}
				if validPad {
					return out[:len(out)-pad]
				}
			}
		}
	}
	return nil
}

// Chrome 130+ prepends SHA-256(host_key) to the decrypted cookie value.
func stripDomainPrefix(buf []byte, host string) ([]byte, bool) {
	if len(buf) >= 32 {
		sum := sha256.Sum256([]byte(host))
		if bytes.Equal(buf[:32], sum[:]) {
			return buf[32:], true
		}
	}
	return buf, false
}

func bestHeaderFromRows(rows []CookieRow) string {
	score := func(h string) int {
		if h == ".google.com" || h == "google.com" {
			return 3
		}
		if strings.HasSuffix(h, ".google.com") {
			return 2
		}
		return 1
	}
	best := make(map[string]string)
	bestScore := make(map[string]int)
	for _, r := range rows {
		if !essentialSet[r.Name] || r.Value == "" {
			continue
		}
		s := score(r.Host)
		if s > bestScore[r.Name] {
			best[r.Name] = r.Value
			bestScore[r.Name] = s
		}
	}
	return CanonicalHeader(best)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if strings.Contains(err.Error(), "being used by another process") {
			return fmt.Errorf("open cookie db: %w — the browser that owns this profile is still running and holds the file locked; close the browser completely (all windows/background apps) and retry", err)
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// FindChromeCookieDB locates an existing Chrome/Brave/Chromium SQLite Cookie DB.
func FindChromeCookieDB(explicitPath string) string {
	if strings.TrimSpace(explicitPath) != "" {
		if p, err := FindBrowserCookieDB(explicitPath); err == nil {
			return p
		}
		return ""
	}
	for _, browser := range knownBrowsers {
		if p, err := FindBrowserCookieDB(browser); err == nil {
			return p
		}
	}
	return ""
}

// ReadChromeCookies reads and decrypts Google cookies from dbPath and returns the canonical Cookie header.
func ReadChromeCookies(dbPath string) (string, error) {
	res, err := ReadBrowserCookies(dbPath)
	if err != nil {
		return "", err
	}
	if res.Header == "" && res.Failed > 0 && res.Decrypted == 0 && res.Plain == 0 {
		return "", fmt.Errorf("failed to decrypt %d Google cookie(s) in %s (key_found=%v) — if Chrome uses App-Bound (v20) encryption, export cookies via Cookie-Editor and use --import-cookie", res.Failed, dbPath, res.KeyFound)
	}
	return res.Header, nil
}
