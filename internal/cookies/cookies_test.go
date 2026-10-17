package cookies

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type stubCookieDoer struct {
	fn func(req *fhttp.Request) (*fhttp.Response, error)
}

func (d *stubCookieDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	return d.fn(req)
}

func (d *stubCookieDoer) CloseIdleConnections() {}

func TestAbsorbSetCookieAndSAPISIDHash(t *testing.T) {
	initial := "SID=s1; SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=old_ts"
	merged, changed := AbsorbSetCookie(initial, []string{
		"__Secure-1PSIDTS=new_ts; Path=/; Secure; HttpOnly",
		"UNRELATED_COOKIE=ignore_me; Path=/",
	})
	if !changed {
		t.Fatal("expected changed=true when __Secure-1PSIDTS rotated")
	}
	if !strings.Contains(merged, "__Secure-1PSIDTS=new_ts") || strings.Contains(merged, "UNRELATED_COOKIE") {
		t.Fatalf("unexpected merged header: %s", merged)
	}

	hash := SAPISIDHash(merged, "https://gemini.google.com")
	if !strings.HasPrefix(hash, "SAPISIDHASH ") || len(hash) < 50 {
		t.Fatalf("unexpected SAPISIDHASH: %s", hash)
	}
}

func TestDualPathCookieRotator(t *testing.T) {
	var seenBodies []string
	var seenURLs []string
	doer := &stubCookieDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			seenURLs = append(seenURLs, req.URL.String())
			if req.Body != nil {
				b, _ := io.ReadAll(req.Body)
				seenBodies = append(seenBodies, string(b))
			}
			if strings.Contains(req.URL.Path, "RotateCookiesPage") {
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`<script>init('rot_id_999', 1, 540)</script>`)),
				}, nil
			}
			// RotateCookies POST
			lastBody := ""
			if len(seenBodies) > 0 {
				lastBody = seenBodies[len(seenBodies)-1]
			}
			if strings.HasPrefix(lastBody, "[000,") {
				return &fhttp.Response{
					StatusCode: 200,
					Header: fhttp.Header{
						"Set-Cookie": []string{"__Secure-1PSIDTS=fresh_psidts; Path=/; Secure; HttpOnly"},
					},
					Body: io.NopCloser(strings.NewReader("[]")),
				}, nil
			}
			return &fhttp.Response{
				StatusCode: 200,
				Header: fhttp.Header{
					"Set-Cookie": []string{"__Secure-1PSIDCC=fresh_psidcc; Path=/; Secure; HttpOnly"},
				},
				Body: io.NopCloser(strings.NewReader("[]")),
			}, nil
		},
	}

	sc, err := transport.NewSentinelClient(transport.ClientConfig{CustomDoer: doer})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}

	rot := NewRotator()
	initial := "SAPISID=sap; __Secure-1PSID=psid; __Secure-1PSIDTS=old_ts; __Secure-1PSIDCC=old_cc"
	// Call 1 uses primary [000,"-0000000000000000000"] path
	res, err := rot.RotateAccount(context.Background(), "acct1", initial, sc)
	if err != nil {
		t.Fatalf("RotateAccount primary failed: %v", err)
	}
	if !res.Rotated || res.Status != 200 || !strings.Contains(res.CookieHeader, "__Secure-1PSIDTS=fresh_psidts") {
		t.Fatalf("expected primary PSIDTS rotation, got: %+v", res)
	}
	// Call 2 within 60s falls back to 658 iframe path
	res2, err := rot.RotateAccount(context.Background(), "acct1", res.CookieHeader, sc)
	if err != nil {
		t.Fatalf("RotateAccount fallback failed: %v", err)
	}
	if !res2.Rotated || !strings.Contains(res2.CookieHeader, "__Secure-1PSIDCC=fresh_psidcc") {
		t.Fatalf("expected fallback PSIDCC rotation, got: %+v", res2)
	}
	if len(seenBodies) != 2 || seenBodies[0] != `[000,"-0000000000000000000"]` || seenBodies[1] != `[658,"rot_id_999"]` {
		t.Fatalf("unexpected RotateCookies POST bodies: %v", seenBodies)
	}
}

func TestSaveAndLoadAccountFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acct.json")
	rec := CredentialFile{
		Name:    "acct",
		Email:   "acct@gmail.com",
		Cookies: "SAPISID=123",
		AT:      "SNlM0e_tok",
		BL:      "boq_123",
	}
	mtime, err := SaveCredentialFile(path, rec)
	if err != nil || mtime <= 0 {
		t.Fatalf("SaveCredentialFile failed: mtime=%d err=%v", mtime, err)
	}
	loaded, mtime2, err := LoadAccountFile(path)
	if err != nil || mtime2 <= 0 {
		t.Fatalf("LoadAccountFile failed: %v", err)
	}
	if loaded.Email != "acct@gmail.com" || loaded.AT != "SNlM0e_tok" {
		t.Fatalf("unexpected loaded record: %+v", loaded)
	}
	st, _ := os.Stat(path)
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %o", st.Mode().Perm())
	}
	if runtime.GOOS == "windows" && st == nil {
		t.Fatal("expected account file to exist")
	}
}

func TestFindBrowserCookieDB_WindowsAndProfileDirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	userData := filepath.Join(tmp, "Google", "Chrome", "User Data")
	prof4 := filepath.Join(userData, "Profile 4")
	netDir := filepath.Join(prof4, "Network")
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		t.Fatal(err)
	}
	netCookies := filepath.Join(netDir, "Cookies")
	if err := os.WriteFile(netCookies, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Profile directory path with Windows backslashes -> resolves to <profile>/Network/Cookies
	winStylePath := strings.ReplaceAll(prof4, "/", `\`)
	got, err := FindBrowserCookieDB(winStylePath)
	if err != nil || got != netCookies {
		t.Fatalf("Windows backslash profile dir: got=%q err=%v, want=%q", got, err, netCookies)
	}

	// 2. Profile name shorthand ("Profile 4" and "chrome:Profile 4")
	got, err = FindBrowserCookieDB("Profile 4")
	if err != nil || got != netCookies {
		t.Fatalf("Profile name shorthand: got=%q err=%v, want=%q", got, err, netCookies)
	}
	got, err = FindBrowserCookieDB("chrome:Profile 4")
	if err != nil || got != netCookies {
		t.Fatalf("Browser:Profile shorthand: got=%q err=%v, want=%q", got, err, netCookies)
	}

	// 3. Fallback to legacy <profile>/Cookies when Network/Cookies is absent
	legacyProf := filepath.Join(userData, "Profile 2")
	if err := os.MkdirAll(legacyProf, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyCookies := filepath.Join(legacyProf, "Cookies")
	if err := os.WriteFile(legacyCookies, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = FindBrowserCookieDB(legacyProf)
	if err != nil || got != legacyCookies {
		t.Fatalf("Legacy profile Cookies fallback: got=%q err=%v, want=%q", got, err, legacyCookies)
	}

	// 4. %LOCALAPPDATA% expansion in path
	got, err = FindBrowserCookieDB(`%LOCALAPPDATA%\Google\Chrome\User Data\Profile 4`)
	if err != nil || got != netCookies {
		t.Fatalf("Percent env expansion: got=%q err=%v, want=%q", got, err, netCookies)
	}
}

func TestReadBrowserCookies_WindowsLocalStateDPAPI(t *testing.T) {
	tmp := t.TempDir()
	userData := filepath.Join(tmp, "Google", "Chrome", "User Data")
	netDir := filepath.Join(userData, "Profile 4", "Network")
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Create a 32-byte master key and wrap it in a simulated DPAPI Local State file.
	masterKey := bytes.Repeat([]byte{0x42}, 32)
	fakeDPAPIBlob := []byte("simulated-dpapi-wrapped-key")
	rawEncryptedKey := append([]byte("DPAPI"), fakeDPAPIBlob...)
	localStateJSON := fmt.Sprintf(`{"os_crypt":{"encrypted_key":%q}}`, base64.StdEncoding.EncodeToString(rawEncryptedKey))
	if err := os.WriteFile(filepath.Join(userData, "Local State"), []byte(localStateJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	origUnprotect := dpapiUnprotect
	dpapiUnprotect = func(blob []byte) ([]byte, error) {
		if !bytes.Equal(blob, fakeDPAPIBlob) {
			return nil, fmt.Errorf("unexpected DPAPI blob: %x", blob)
		}
		return masterKey, nil
	}
	defer func() { dpapiUnprotect = origUnprotect }()

	// Create SQLite Cookies DB with v10 AES-256-GCM + Chrome 130+ SHA-256(host_key) domain prefix.
	dbPath := filepath.Join(netDir, "Cookies")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cookies (host_key TEXT, name TEXT, encrypted_value BLOB, value TEXT)`); err != nil {
		db.Close()
		t.Fatal(err)
	}

	encryptV10 := func(host, val string) []byte {
		block, err := aes.NewCipher(masterKey)
		if err != nil {
			t.Fatal(err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatal(err)
		}
		nonce := bytes.Repeat([]byte{0x07}, 12)
		hostHash := sha256.Sum256([]byte(host))
		plain := append(hostHash[:], []byte(val)...)
		ct := gcm.Seal(nil, nonce, plain, nil)
		out := append([]byte("v10"), nonce...)
		return append(out, ct...)
	}

	for _, row := range []struct{ host, name, val string }{
		{".google.com", "SAPISID", "sap_win_123"},
		{".google.com", "__Secure-1PSID", "psid_win_456"},
		{".google.com", "__Secure-1PSIDTS", "psidts_win_789"},
	} {
		if _, err := db.Exec(`INSERT INTO cookies (host_key, name, encrypted_value, value) VALUES (?, ?, ?, '')`,
			row.host, row.name, encryptV10(row.host, row.val)); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()

	hdr, err := ReadChromeCookies(dbPath)
	if err != nil {
		t.Fatalf("ReadChromeCookies failed: %v", err)
	}
	if ExtractSAPISID(hdr) != "sap_win_123" || !strings.Contains(hdr, "__Secure-1PSID=psid_win_456") {
		t.Fatalf("unexpected decrypted header: %q", hdr)
	}
}

func TestParseProfileHint(t *testing.T) {
	cases := []struct {
		in          string
		wantBrowser string
		wantProfile string
	}{
		{`C:\Users\alice\AppData\Local\Google\Chrome\User Data\Profile 4`, "chrome", "Profile 4"},
		{`C:\Users\alice\AppData\Local\Google\Chrome\User Data\Profile 4\Network\Cookies`, "chrome", "Profile 4"},
		{`/home/alice/.config/BraveSoftware/Brave-Browser/Profile 2`, "brave", "Profile 2"},
		{`Profile 4`, "", "Profile 4"},
		{`chrome:Profile 4`, "chrome", "Profile 4"},
		{`edge`, "edge", "Default"},
	}
	for _, tc := range cases {
		b, p := ParseProfileHint(tc.in)
		if b != tc.wantBrowser || p != tc.wantProfile {
			t.Errorf("ParseProfileHint(%q) = (%q, %q), want (%q, %q)", tc.in, b, p, tc.wantBrowser, tc.wantProfile)
		}
	}
}
