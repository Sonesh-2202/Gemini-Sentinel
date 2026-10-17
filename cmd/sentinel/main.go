package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/config"
	"github.com/sonesh-2202/gemini-sentinel/internal/cookies"
	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
	"github.com/sonesh-2202/gemini-sentinel/internal/server"
	"github.com/sonesh-2202/gemini-sentinel/internal/version"
)

var reSafeAccountName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func main() {
	var (
		loginAcct     = flag.String("login", "", "Interactive browser sign-in to capture a fresh session into ~/.gemini-sentinel/accounts/<name>.json")
		importChrome  = flag.String("import-chrome", "", "Import Google cookies from local Chrome/Brave/Chromium/Edge/Firefox SQLite DB into accounts/<name>.json")
		cookieDB      = flag.String("cookie-db", "", "Explicit path to browser Cookies SQLite database or profile directory for --import-chrome / --login")
		chromeProfile = flag.String("chrome-profile", "", "Browser profile name (e.g. \"Profile 4\"), profile directory, or Cookies SQLite path for --import-chrome / --login")
		importCookie  = flag.String("import-cookie", "", "Import raw Cookie header or Cookie-Editor JSON from --cookie-value, --cookie-file, or stdin into accounts/<name>.json")
		cookieValue   = flag.String("cookie-value", "", "Raw Cookie header or JSON string for --import-cookie")
		cookieFile    = flag.String("cookie-file", "", "Path to file containing raw Cookie header or Cookie-Editor JSON array for --import-cookie")
		setupRun      = flag.Bool("setup", false, "One-command setup: import your browser's Google account, auto-generate/persist the admin key, verify models, then start the server (pass an account name as the first argument, e.g. sentinel --setup acc1)")
		setupNoStart  = flag.Bool("setup-no-start", false, "With --setup: perform setup only and exit without starting the server")
		probeOnce     = flag.Bool("probe", false, "Bootstrap all configured accounts, verify models & quota, and exit")
		rotateOnce    = flag.Bool("rotate", false, "Run dual-path cookie rotation on all configured accounts and exit")
		statusOnce    = flag.Bool("status", false, "Run local preflight checks (bind, keys, accounts) without network access and exit")
		showVersion   = flag.Bool("version", false, "Print build version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	cfg := config.Load()
	if *statusOnce {
		os.Exit(runStatus(cfg))
	}
	dbOrProfile := *cookieDB
	if dbOrProfile == "" {
		dbOrProfile = *chromeProfile
	}

	if *setupRun {
		acctName := "main"
		if args := flag.Args(); len(args) > 0 {
			acctName = args[0]
		}
		if err := runSetup(cfg, acctName, dbOrProfile); err != nil {
			log.Fatalf("[setup] failed: %v", err)
		}
		if *setupNoStart {
			return
		}
		// Reload config so the freshly persisted keys/accounts are visible,
		// then fall through to start the server in the same process.
		cfg = config.Load()
	}
	if *loginAcct != "" {
		if err := runInteractiveLogin(cfg, *loginAcct, dbOrProfile); err != nil {
			log.Fatalf("[login] failed: %v", err)
		}
		return
	}
	if *importChrome != "" {
		if _, err := runImportChrome(cfg, *importChrome, dbOrProfile); err != nil {
			log.Fatalf("[import-chrome] failed: %v", err)
		}
		return
	}
	if *importCookie != "" {
		if _, err := runImportCookie(cfg, *importCookie, *cookieValue, *cookieFile); err != nil {
			log.Fatalf("[import-cookie] failed: %v", err)
		}
		return
	}
	if err := validateServerBind(cfg); err != nil {
		log.Fatal(err)
	}

	srv := server.New(cfg)
	loaded := srv.LoadCredentials()

	// Zero-config admin key: when starting the server with accounts but no
	// admin key, generate one once and persist it to the data-home .env so
	// admin endpoints (failover pool, /metrics, …) work out of the box.
	applyAutoAdminKeyIfNeeded(&cfg, srv, loaded)

	if err := validateAuthConfig(cfg, loaded); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *probeOnce {
		if loaded == 0 {
			log.Fatalf("[probe] no accounts found in %s", cfg.AccountsDir)
		}
		accts := srv.EnsureModels(ctx, true)
		for _, a := range accts {
			if a.ModelsError != nil {
				fmt.Printf("Account %-16s email=%-28s ERROR: %v\n", a.ID, a.Email, a.ModelsError)
			} else {
				fmt.Printf("Account %-16s email=%-28s capacity=%d models=%d fails=%d\n",
					a.ID, a.Email, a.Capacity, len(a.Models), a.Fails)
			}
			for _, m := range a.Models {
				fmt.Printf("  - %-24s (%s, id=%s, cap=%d)\n", m.ModelName, m.DisplayName, m.ModelID, m.Capacity)
			}
			for _, q := range a.Quota {
				if q.Unlimited {
					fmt.Printf("  quota: %-18s unlimited\n", q.Label)
				} else {
					fmt.Printf("  quota: %-18s remaining=%d/%d resets=%s\n", q.Label, q.Remaining, q.Total, q.ResetAt)
				}
			}
		}
		return
	}

	if *rotateOnce {
		if loaded == 0 {
			log.Fatalf("[rotate] no accounts found in %s", cfg.AccountsDir)
		}
		for _, a := range srv.Accounts() {
			res, err := a.Client.RotateCookies(ctx)
			if err != nil {
				fmt.Printf("Account %-16s error: %v\n", a.ID, err)
			} else {
				fmt.Printf("Account %-16s HTTP %d rotated=%v next=%s\n", a.ID, res.Status, res.Rotated, res.NextInterval)
			}
		}
		return
	}

	srv.StartBackgroundWorkers(ctx)

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		log.Printf("sentinel (%s) listening on http://%s", version.String(), addr)
		log.Printf("  transport: browserless Chrome uTLS + HTTP/2 (%s)", cfg.ImpersonateProfile)
		log.Printf("  data:      %s (%d account(s) loaded)", cfg.HomeDir, loaded)
		log.Printf("  models:    http://%s/v1/models", addr)
		if loaded == 0 {
			log.Printf("  WARNING:   no credentials found — run `sentinel --setup` (imports your Google account in one step) or place accounts in %s", cfg.AccountsDir)
		}
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[fatal] listen %s: %v", addr, err)
		}
	}()

	<-ctx.Done()
	log.Printf("[shutdown] stopping sentinel...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	srv.Pool.Reset()
}

func validateServerBind(cfg config.Config) error {
	if !config.IsLoopbackHost(cfg.Host) {
		return fmt.Errorf("refusing non-loopback bind on %q: this daemon serves credentials and currently supports plaintext HTTP only; use SENTINEL_HOST=127.0.0.1 (or HOST=) or place a reviewed HTTPS/authentication proxy in front of a separately hardened build", cfg.Host)
	}
	return nil
}

// validateAuthConfig fails startup on fail-open authentication: the
// "public" placeholder and short/weak admin keys are rejected. An empty admin
// key is allowed (admin endpoints stay disabled and return 401/403) because
// per-account tokens still protect chat completions; operators needing
// /v1/accounts, /v1/quota, /v1/sessions, /v1/refresh or /metrics must set a
// strong SENTINEL_API_KEY of at least 16 characters.
func validateAuthConfig(cfg config.Config, loadedAccounts int) error {
	if cfg.APIKey == "public" {
		return fmt.Errorf("refusing to start with a placeholder admin key: set a strong random SENTINEL_API_KEY (>=16 chars) or unset it to disable admin endpoints")
	}
	if cfg.APIKey != "" && !config.AdminKeyValid(cfg.APIKey) {
		return fmt.Errorf("refusing to start with weak SENTINEL_API_KEY (%d chars): use at least 16 random characters", len(cfg.APIKey))
	}
	if loadedAccounts > 0 && cfg.APIKey == "" && len(cfg.AccountTokens) == 0 {
		// Per-account sentinel-<id> tokens are auto-registered at load time, so
		// chat remains authenticated; admin endpoints fail closed.
		log.Printf("[auth] SENTINEL_API_KEY is unset: admin endpoints (/v1/accounts, /v1/quota, /v1/sessions, /v1/refresh, /metrics) will deny all requests until a strong key is configured")
	}
	return nil
}

// runStatus performs offline preflight checks and reports next steps.
// It never touches the network and never prints secrets.
func runStatus(cfg config.Config) int {
	fail := 0
	fmt.Printf("sentinel status (%s)\n", version.String())
	if err := validateServerBind(cfg); err != nil {
		fmt.Printf("  bind:     FAIL — %v\n", err)
		fail = 1
	} else {
		fmt.Printf("  bind:     ok (%s:%d, loopback-only)\n", cfg.Host, cfg.Port)
	}
	switch {
	case cfg.APIKey == "public" || (cfg.APIKey != "" && !config.AdminKeyValid(cfg.APIKey)):
		fmt.Printf("  admin key: FAIL — placeholder or weak; set SENTINEL_API_KEY (>=16 chars)\n")
		fail = 1
	case cfg.APIKey == "":
		fmt.Printf("  admin key: missing — chat keys work, admin endpoints will deny all\n")
	default:
		fmt.Printf("  admin key: set (%d chars)\n", len(cfg.APIKey))
	}
	fmt.Printf("  data:      %s\n", cfg.HomeDir)
	entries, err := os.ReadDir(cfg.AccountsDir)
	count := 0
	if err != nil {
		fmt.Printf("  accounts:  none yet (%s not found)\n", cfg.AccountsDir)
	} else {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				count++
			}
		}
		fmt.Printf("  accounts:  %d file(s) in %s\n", count, cfg.AccountsDir)
	}
	if count == 0 {
		fmt.Printf("  next:      sentinel --setup   (one command: imports account + creates admin key)\n")
	} else {
		fmt.Printf("  next:      start the server with `sentinel` — or run `sentinel --probe` to verify models & quota\n")
	}
	return fail
}

func runImportChrome(cfg config.Config, acctName, explicitDB string) (*gemini.AccountClient, error) {
	if !reSafeAccountName.MatchString(acctName) {
		return nil, fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	var dbPath string
	if strings.TrimSpace(explicitDB) != "" {
		p, err := cookies.FindBrowserCookieDB(explicitDB)
		if err != nil {
			return nil, err
		}
		dbPath = p
	} else {
		if cfg.CookieDB != "" {
			dbPath = cookies.FindChromeCookieDB(cfg.CookieDB)
		}
		if dbPath == "" {
			dbPath = cookies.FindChromeCookieDB("")
		}
		if dbPath == "" {
			return nil, fmt.Errorf("could not locate Chrome/Brave/Chromium Cookies SQLite database")
		}
	}
	hdr, err := cookies.ReadChromeCookies(dbPath)
	if err != nil {
		return nil, err
	}
	if cookies.ExtractSAPISID(hdr) == "" {
		return nil, fmt.Errorf("no SAPISID cookie found in %s — sign in to gemini.google.com first", dbPath)
	}
	outPath := filepath.Join(cfg.AccountsDir, acctName+".json")
	srv := server.New(cfg)
	ac := gemini.NewAccountClient(acctName, hdr, outPath, srv.Pool, srv.Rotator)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ac.Bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap with imported cookies failed: %w", err)
	}
	fmt.Printf("Imported account %q (%s) -> %s\n", acctName, ac.Email(), outPath)
	printStandaloneConnectionInfo(cfg, acctName)
	return ac, nil
}

func runImportCookie(cfg config.Config, acctName, rawVal, filePath string) (*gemini.AccountClient, error) {
	if !reSafeAccountName.MatchString(acctName) {
		return nil, fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	if rawVal == "" && filePath != "" {
		b, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		rawVal = string(b)
	}
	if rawVal == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		rawVal = string(b)
	}
	hdr, _, err := cookies.ParseImportPayload(rawVal)
	if err != nil {
		return nil, err
	}
	if cookies.ExtractSAPISID(hdr) == "" {
		return nil, fmt.Errorf("cookie payload does not contain SAPISID")
	}
	outPath := filepath.Join(cfg.AccountsDir, acctName+".json")
	srv := server.New(cfg)
	ac := gemini.NewAccountClient(acctName, hdr, outPath, srv.Pool, srv.Rotator)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ac.Bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap with imported cookies failed: %w", err)
	}
	fmt.Printf("Imported account %q (%s) -> %s\n", acctName, ac.Email(), outPath)
	printStandaloneConnectionInfo(cfg, acctName)
	return ac, nil
}

func runInteractiveLogin(cfg config.Config, acctName, explicitDB string) error {
	if !reSafeAccountName.MatchString(acctName) {
		return fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	if explicitDB == "" {
		explicitDB = cfg.CookieDB
	}
	targetURL := "https://gemini.google.com/app"
	browser, profileDir := cookies.ParseProfileHint(explicitDB)
	if explicitDB != "" {
		fmt.Printf("Opening %s in your browser (target profile: %s)...\n", targetURL, explicitDB)
	} else {
		fmt.Printf("Opening %s in your default browser...\n", targetURL)
		fmt.Println("Note: No --chrome-profile specified; auto-import reads from the Default profile.")
		fmt.Println("      On multi-profile setups, pass --chrome-profile \"<Profile Name or Path>\" (e.g. \"Profile 4\").")
	}
	openBrowserURL(targetURL, browser, profileDir)
	fmt.Println("1. Sign in to Google Gemini in your browser.")
	fmt.Println("2. Press Enter to auto-import cookies from your local browser profile,")
	fmt.Println("   or paste a raw Cookie header / Cookie-Editor JSON line and press Enter:")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line != "" {
		_, err := runImportCookie(cfg, acctName, line, "")
		return err
	}
	_, err := runImportChrome(cfg, acctName, explicitDB)
	return err
}

func findBrowserBinary(browser string) string {
	b := strings.ToLower(strings.TrimSpace(browser))
	if b == "" {
		b = "chrome"
	}
	switch runtime.GOOS {
	case "windows":
		roots := []string{
			os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("LOCALAPPDATA"),
		}
		var rels []string
		switch b {
		case "brave":
			rels = []string{`BraveSoftware\Brave-Browser\Application\brave.exe`}
		case "edge":
			rels = []string{`Microsoft\Edge\Application\msedge.exe`}
		case "vivaldi":
			rels = []string{`Vivaldi\Application\vivaldi.exe`}
		case "chromium":
			rels = []string{`Chromium\Application\chrome.exe`}
		default:
			rels = []string{`Google\Chrome\Application\chrome.exe`}
		}
		for _, r := range roots {
			if r == "" {
				continue
			}
			for _, rel := range rels {
				cand := filepath.Join(r, rel)
				if st, err := os.Stat(cand); err == nil && !st.IsDir() {
					return cand
				}
			}
		}
	default:
		var names []string
		switch b {
		case "brave":
			names = []string{"brave-browser", "brave"}
		case "edge":
			names = []string{"microsoft-edge", "microsoft-edge-stable"}
		case "vivaldi":
			names = []string{"vivaldi", "vivaldi-stable"}
		case "chromium":
			names = []string{"chromium", "chromium-browser"}
		default:
			names = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
		}
		for _, n := range names {
			if p, err := exec.LookPath(n); err == nil {
				return p
			}
		}
	}
	return ""
}

func openBrowserURL(url, browser, profileDir string) {
	if profileDir != "" {
		if runtime.GOOS == "darwin" {
			appName := "Google Chrome"
			switch strings.ToLower(browser) {
			case "brave":
				appName = "Brave Browser"
			case "edge":
				appName = "Microsoft Edge"
			case "vivaldi":
				appName = "Vivaldi"
			case "chromium":
				appName = "Chromium"
			}
			cmd := exec.Command("open", "-a", appName, "--args", "--profile-directory="+profileDir, url)
			if err := cmd.Start(); err == nil {
				return
			}
		} else if bin := findBrowserBinary(browser); bin != "" {
			cmd := exec.Command(bin, "--profile-directory="+profileDir, url)
			if err := cmd.Start(); err == nil {
				return
			}
		}
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func printStandaloneConnectionInfo(cfg config.Config, acctName string) {
	fmt.Printf("  base url: http://%s:%d/v1\n", cfg.Host, cfg.Port)
	fmt.Printf("  api key : sentinel-%s (or %s)\n", acctName, acctName)
}
