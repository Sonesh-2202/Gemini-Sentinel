package main

// setup.go implements the simplified first-run experience:
//
//	sentinel --setup            → import account + admin key + verify + serve
//	sentinel                    → plain serve (reuses persisted settings)
//
// Everything that previously required manual steps (generating a key with
// python3, exporting SENTINEL_API_KEY in every shell, running --probe
// separately) happens automatically and is persisted to .env inside the
// sentinel data home, so later runs pick it up without any shell exports.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/config"
	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
	"github.com/sonesh-2202/gemini-sentinel/internal/server"
)

// ensureAdminKey returns the admin key to use for admin-only endpoints
// (/v1/accounts, /v1/quota, /v1/sessions, /v1/refresh, /metrics).
//
// Precedence:
//  1. A key already set in the environment or an existing data-home .env
//     (cfg.APIKey, already populated by config.Load) — reused as-is.
//  2. Otherwise a fresh key is generated with crypto/rand and persisted to
//     the data-home .env file (permissions 0600) so it is reused on every
//     later run without any shell exports.
//
// The generated key is returned so the caller can show it exactly once at
// setup time; the persisted file is the durable source of truth.
func ensureAdminKey(cfg config.Config) (key string, persisted bool, err error) {
	if cfg.APIKey != "" {
		return cfg.APIKey, false, nil
	}
	key, err = randomHexKey()
	if err != nil {
		return "", false, fmt.Errorf("generate admin key: %w", err)
	}
	if err := os.MkdirAll(cfg.HomeDir, 0o700); err != nil {
		return "", false, fmt.Errorf("create data dir %s: %w", cfg.HomeDir, err)
	}
	if err := config.SaveDotEnv(cfg.EnvFilePath, map[string]string{"SENTINEL_API_KEY": key}); err != nil {
		return "", false, fmt.Errorf("save admin key to %s: %w", cfg.EnvFilePath, err)
	}
	return key, true, nil
}

func randomHexKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// runSetup is the one-command onboarding path behind `sentinel --setup`:
//
//  1. Ensures a persistent admin key (env key wins; otherwise generated and
//     saved into the data-home .env so later runs need no shell exports).
//  2. Imports the Google account from the local browser (reusing the
//     existing --import-chrome machinery and its Bootstrap validation).
//  3. Fetches the live model catalog so users see the exact slugs up front.
//
// It is idempotent: re-running refreshes the cookies for the same account
// name and never turns a second account on by accident.
func runSetup(cfg config.Config, acctName, dbOrProfile string) error {
	if !reSafeAccountName.MatchString(acctName) {
		return fmt.Errorf("invalid account name %q (use 1-64 chars of A-Z a-z 0-9 . _ -)", acctName)
	}
	if extra := flag.Args(); len(extra) > 1 {
		return fmt.Errorf("unexpected extra arguments %q — pass all flags (e.g. --chrome-profile) BEFORE the account name", extra[1:])
	}

	fmt.Printf("== Gemini-Sentinel setup — account %q ==\n", acctName)

	// Step 1: persistent admin key.
	key, generated, err := ensureAdminKey(cfg)
	if err != nil {
		// Not fatal: chat still works on auto-derived account keys; admin
		// endpoints simply stay disabled (previous behavior).
		log.Printf("[setup] admin key was not persisted (%v); admin endpoints stay disabled", err)
		key, generated = "", false
	}
	if key == "" {
		fmt.Println("admin key: (none set — admin endpoints stay disabled)")
	} else if generated {
		fmt.Printf("admin key (generated & saved to %s — this is shown once):\n  %s\n", cfg.EnvFilePath, key)
	} else {
		fmt.Println("admin key: already configured (kept as-is)")
	}

	// Step 2: browser cookie import.
	fmt.Println("importing your Google account from the local browser...")
	ac, err := runImportChrome(cfg, acctName, dbOrProfile)
	if err != nil {
		return fmt.Errorf(
			"browser import failed: %w\nHints:\n  - make sure you are signed in to https://gemini.google.com/app in a Chrome/Brave/Chromium/Edge/Firefox browser on this machine\n  - pick a browser/profile first (flags go BEFORE the account name):  sentinel --chrome-profile brave --setup %s\n  - or sign in interactively:       sentinel --login %s\n  - or paste cookies manually:      sentinel --import-cookie %s --cookie-file ./cookies.json",
			err, acctName, acctName, acctName)
	}

	// Step 3: live model verification.
	printSetupModels(ac)

	fmt.Printf("setup complete. start anytime with: sentinel  (Windows: .\\sentinel.exe)\n")
	return nil
}

// printSetupModels lists the account's live model slugs so the user knows
// exactly which names can be requested right away. Failures are non-fatal.
func printSetupModels(ac *gemini.AccountClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := ac.AvailableModels(ctx, true)
	if err != nil || res == nil || len(res.Models) == 0 {
		fmt.Printf("models: (could not list right now: %v — the server refreshes this automatically)\n", errValue(err))
		return
	}
	fmt.Printf("models available (OpenAI \"model\" field):")
	for i, m := range res.Models {
		if i > 0 {
			fmt.Printf(",")
		}
		fmt.Printf(" %s", m.ModelName)
	}
	fmt.Println()
}

func errValue(err error) string {
	if err == nil {
		return "empty result"
	}
	return err.Error()
}

// applyAutoAdminKeyIfNeeded fills an empty admin key on plain server starts
// (no --setup): generate once, persist to the data-home .env, and log the
// location without echoing the secret again. Explicit SENTINEL_API_KEY wins;
// SENTINEL_AUTO_ADMIN_KEY=0/false opts out.
func applyAutoAdminKeyIfNeeded(cfg *config.Config, srv *server.Server, loaded int) {
	if !cfg.AutoAdminKey || cfg.APIKey != "" || loaded == 0 {
		return
	}
	key, _, err := ensureAdminKey(*cfg)
	if err != nil {
		log.Printf("[auth] auto admin key unavailable (%v); admin endpoints stay disabled", err)
		return
	}
	cfg.APIKey = key
	srv.Cfg = *cfg
	log.Printf("[auth] auto-generated admin key saved to %s (admin endpoints enabled; delete it or set SENTINEL_API_KEY to override)", cfg.EnvFilePath)
}
