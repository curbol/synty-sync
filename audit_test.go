package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/curbol/synty-sync/internal/config"
	"github.com/curbol/synty-sync/internal/portal"
)

// Asking a subcommand for help is not a failure.
func TestSubcommandHelpSucceeds(t *testing.T) {
	for _, args := range [][]string{{"sync", "-h"}, {"status", "--help"}, {"browse", "-h"}} {
		if err := run(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// The whole point of the expired-session sentinel is that a bad session leaves the
// committed lockfile alone. Nothing exercised that through the CLI wiring.
func TestSyncAbortsOnExpiredSessionWithoutTouchingLockfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><h1>Login</h1></body></html>`) // no logged-in sentinel
	}))
	defer srv.Close()

	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "synty-sync.toml")
	if err := os.WriteFile(manifestPath, []byte(
		"variant_includes = [\"Godot_*\"]\n\n[[pack]]\nslug = \"p\"\nname = \"P\"\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "synty-sync.lock.json")
	const seeded = "{\n  \"generatedAt\": \"old\",\n  \"packs\": {}\n}\n"
	if err := os.WriteFile(lockPath, []byte(seeded), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &portal.Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1", Cookie: "x=y"}
	cfg := config.Config{LibraryPath: t.TempDir(), Concurrency: 2}
	err := runSyncOrStatus(context.Background(), client, cfg, manifestPath, lockPath, "", false)

	if !errors.Is(err, portal.ErrExpiredSession) {
		t.Fatalf("err = %v, want ErrExpiredSession to survive to the CLI", err)
	}
	after, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != seeded {
		t.Errorf("lockfile rewritten on an expired session:\n%s", after)
	}
}

// A sync must not act on packs the manifest has not enabled.
func TestSyncOnlyTouchesEnabledPacks(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "synty-sync.toml")
	if err := os.WriteFile(manifestPath, []byte(
		"variant_includes = [\"Godot_*\"]\n\n[[pack]]\nslug = \"off\"\nname = \"Off\"\nenabled = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var itemPageFetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const sentinel = `<input class='sky-pilot-search-input'>`
		if r.URL.Query().Get("line_items_page") == "1" {
			fmt.Fprint(w, `<div class='sky-pilot'>`+sentinel+
				`<a href='/apps/downloads/customers/1/orders/2/order_items/3' class='sky-pilot-list-item'>Off</a></div>`)
			return
		}
		if r.URL.Query().Get("line_items_page") != "" {
			fmt.Fprint(w, `<div class='sky-pilot'>`+sentinel+`</div>`)
			return
		}
		itemPageFetches++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := &portal.Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1", Cookie: "x=y"}
	cfg := config.Config{LibraryPath: t.TempDir(), Concurrency: 2}
	lockPath := filepath.Join(dir, "synty-sync.lock.json")
	if err := runSyncOrStatus(context.Background(), client, cfg, manifestPath, lockPath, "", true); err != nil {
		t.Fatalf("status: %v", err)
	}
	if itemPageFetches != 0 {
		t.Errorf("a disabled pack's item page was fetched %d times", itemPageFetches)
	}
}
