package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/config"
	"github.com/curbol/synty-sync/internal/manifest"
	"github.com/curbol/synty-sync/internal/portal"
	"github.com/curbol/synty-sync/internal/web"
)

// Asking a subcommand for help is not a failure.
func TestSubcommandHelpSucceeds(t *testing.T) {
	for _, args := range [][]string{{"sync", "-h"}, {"status", "--help"}, {"select", "-h"}} {
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

// Go's flag parsing stops at the first non-flag argument, so a stray positional
// silently drops every flag after it. `sync <pack> --dry-run` would then perform a
// real sync: full delta downloaded, committed lockfile rewritten.
func TestStrayArgumentIsRejected(t *testing.T) {
	for _, args := range [][]string{
		{"sync", "polygon-city", "--dry-run"},
		{"status", "somepack"},
		{"list", "extra"},
		{"update", "v1", "v2"},
	} {
		err := run(args)
		if err == nil {
			t.Errorf("%v: accepted a stray positional argument", args)
			continue
		}
		if !strings.Contains(err.Error(), "argument") {
			t.Errorf("%v: err = %v, want it to explain the stray argument", args, err)
		}
	}
}

// An expired session must say what to do next, not just what broke. main is the
// only layer that knows which cookie source was used.
func TestExpiredSessionErrorNamesTheCookieSource(t *testing.T) {
	base := errors.New("expired or missing session")
	err := explainSession(fmt.Errorf("%w", portal.ErrExpiredSession), "zen")
	if !errors.Is(err, portal.ErrExpiredSession) {
		t.Fatalf("wrapping lost the sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "zen") {
		t.Errorf("err = %q, want it to name the session source", err)
	}
	// An unrelated error passes through untouched.
	if got := explainSession(base, "zen"); got != base {
		t.Errorf("unrelated error was rewritten: %v", got)
	}
}

// list and the run summary are the tool's actual output; writing them to a
// package-level stdout leaves every branch of them unassertable.
func TestListWritesTheLockfileToItsWriter(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "synty-sync.lock.json")
	if err := os.WriteFile(lockPath, []byte(`{
  "generatedAt": "t",
  "packs": {
    "zeta-pack": {"displayName": "Zeta", "files": {
      "T|Godot_4_5_1": {"fileToken": "T", "variant": "Godot_4_5_1", "version": "v2", "fileId": 1, "tracked": true}}},
    "alpha-pack": {"displayName": "Alpha", "files": {
      "U|SourceFiles": {"fileToken": "U", "variant": "SourceFiles", "version": "v1", "fileId": 2, "tracked": false}}}
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := list(&out, lockPath); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Index(got, "alpha-pack") > strings.Index(got, "zeta-pack") {
		t.Errorf("packs not sorted by slug:\n%s", got)
	}
	if !strings.Contains(got, "* T|Godot_4_5_1  v2") {
		t.Errorf("tracked file not marked as downloaded:\n%s", got)
	}
	if strings.Contains(got, "* U|SourceFiles") {
		t.Errorf("untracked file marked as downloaded:\n%s", got)
	}
}

// select reaches the store before it touches the manifest, and it must stay that
// way: an expired session that got as far as Reconcile would rewrite the committed
// allowlist from an enumeration that returned nothing.
func TestSelectAbortsOnExpiredSessionWithoutTouchingTheManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><body><h1>Login</h1></body></html>`)
	}))
	defer srv.Close()

	manifestPath := filepath.Join(t.TempDir(), "synty-sync.toml")
	const seeded = "variant_includes = [\"Godot_*\"]\n\n[[pack]]\n  slug = \"pirate-pack\"\n  name = \"Pirate Pack\"\n  enabled = true\n"
	if err := os.WriteFile(manifestPath, []byte(seeded), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &portal.Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1", Cookie: "stale"}
	err := selectPacks(context.Background(), client, manifestPath, "localhost:18814")
	if !errors.Is(err, portal.ErrExpiredSession) {
		t.Fatalf("err = %v, want ErrExpiredSession", err)
	}

	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != seeded {
		t.Errorf("the manifest was rewritten on an expired session:\n%s", after)
	}
}

// selectPacks rewrites the committed allowlist from whatever the page hands back,
// so each of these is a way the user's selection can be lost: a save that drops the
// packs they kept, an empty submission that disables everything, and a tab left open
// from an earlier run whose slugs no longer name anything owned.
func TestSelectPacksWritesOnlyWhatWasChosen(t *testing.T) {
	openBrowserWas := web.OpenBrowser
	web.OpenBrowser = func(string) {}
	t.Cleanup(func() { web.OpenBrowser = openBrowserWas })

	const seeded = "variant_includes = [\"Godot_*\"]\n\n[[pack]]\n  slug = \"pirate-pack\"\n  name = \"Pirate Pack\"\n  enabled = true\n"
	for _, tc := range []struct {
		name        string
		addr        string
		seed        string
		post        []string
		wantErr     bool
		wantEnabled []string
	}{
		{
			name:        "the chosen pack is enabled and the rest stay off",
			addr:        "localhost:18811",
			seed:        "variant_includes = [\"Godot_*\"]\n",
			post:        []string{"pirate-pack"},
			wantEnabled: []string{"pirate-pack"},
		},
		{
			name:        "an empty submission will not wipe a live selection",
			addr:        "localhost:18812",
			seed:        seeded,
			post:        nil,
			wantErr:     true,
			wantEnabled: []string{"pirate-pack"},
		},
		{
			name:        "a stale tab's slugs will not wipe a live selection",
			addr:        "localhost:18813",
			seed:        seeded,
			post:        []string{"long-gone"},
			wantErr:     true,
			wantEnabled: []string{"pirate-pack"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				const sentinel = `<input class='sky-pilot-search-input'>`
				if r.URL.Query().Get("line_items_page") == "1" {
					fmt.Fprint(w, `<div class='sky-pilot'>`+sentinel+
						`<a href='/apps/downloads/customers/1/orders/2/order_items/3' class='sky-pilot-list-item'>Pirate Pack</a>`+
						`<a href='/apps/downloads/customers/1/orders/2/order_items/4' class='sky-pilot-list-item'>Dungeon Pack</a></div>`)
					return
				}
				fmt.Fprint(w, `<div class='sky-pilot'>`+sentinel+`</div>`)
			}))
			defer srv.Close()

			manifestPath := filepath.Join(t.TempDir(), "synty-sync.toml")
			if err := os.WriteFile(manifestPath, []byte(tc.seed), 0o644); err != nil {
				t.Fatal(err)
			}

			stdoutWas := stdout
			stdout = &bytes.Buffer{}
			defer func() { stdout = stdoutWas }()

			client := &portal.Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1", Cookie: "x=y"}
			done := make(chan error, 1)
			go func() { done <- selectPacks(context.Background(), client, manifestPath, tc.addr) }()

			token := waitForSelectPage(t, tc.addr)
			resp, err := http.PostForm("http://"+tc.addr+"/save", url.Values{"pack": tc.post, "csrf": {token}})
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			select {
			case err := <-done:
				if (err != nil) != tc.wantErr {
					t.Fatalf("selectPacks err = %v, wantErr = %v", err, tc.wantErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("selectPacks did not return after the save")
			}

			man, err := manifest.Load(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			got := man.EnabledSet()
			if len(got) != len(tc.wantEnabled) {
				t.Fatalf("enabled = %v, want %v", got, tc.wantEnabled)
			}
			for _, slug := range tc.wantEnabled {
				if !got[slug] {
					t.Errorf("enabled = %v, want it to contain %q", got, slug)
				}
			}
			if len(man.VariantIncludes) != 1 || man.VariantIncludes[0] != "Godot_*" {
				t.Errorf("variant_includes lost on write: %v", man.VariantIncludes)
			}
		})
	}
}

// waitForSelectPage blocks until the page is being served and returns the token its
// form carries, which is what separates a submission from the rendered page from one
// any other tab could forge.
func waitForSelectPage(t *testing.T, addr string) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		m := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindSubmatch(body)
		if m == nil {
			t.Fatal("the select page carries no form token")
		}
		return string(m[1])
	}
	t.Fatalf("select page never came up on %s", addr)
	return ""
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

// A sync where the store answers downloads with a login page has to exit non-zero.
// The files it could not fetch are the whole point of the command, and a silent
// success is what turns a dead session into a mirror everyone believes is current.
func TestSyncWithFailedDownloadsExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "synty-sync.toml")
	if err := os.WriteFile(manifestPath, []byte(
		"variant_includes = [\"Godot_*\"]\n\n[[pack]]\nslug = \"pirate\"\nname = \"Pirate\"\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const sentinel = `<input class='sky-pilot-search-input'>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("line_items_page") == "1":
			fmt.Fprint(w, `<div class='sky-pilot'>`+sentinel+
				`<a href='/apps/downloads/customers/1/orders/2/order_items/3' class='sky-pilot-list-item'>Pirate</a></div>`)
		case r.URL.Query().Get("line_items_page") != "":
			fmt.Fprint(w, `<div class='sky-pilot'>`+sentinel+`</div>`)
		case strings.Contains(r.URL.Path, "/order_items/"):
			fmt.Fprint(w, `<div class='sky-pilot-list-item'>
			  <div class='sky-pilot-file-heading'>POLYGON_Pirate_Godot_4_5_1 | v1.0.0 <span class='sky-pilot-file-size'>(40 MB)</span></div>
			  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/77?x=1'>Download</a></div>
			</div>`)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><title>Log in</title>`)
		}
	}))
	defer srv.Close()

	client := &portal.Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1", Cookie: "x=y"}
	lib := t.TempDir()
	cfg := config.Config{LibraryPath: lib, Concurrency: 2}

	var out bytes.Buffer
	restore := stdout
	stdout = &out
	t.Cleanup(func() { stdout = restore })

	err := runSyncOrStatus(context.Background(), client, cfg, manifestPath, filepath.Join(dir, "synty-sync.lock.json"), "", false)
	if err == nil {
		t.Fatal("a sync that downloaded nothing it was asked for returned success")
	}
	if !strings.Contains(out.String(), "failed") {
		t.Errorf("the report does not name the failures:\n%s", out.String())
	}
}

// Flags are the last layer over config.toml and the environment, and the shell
// leaves a quoted ~ alone. Without the same expansion the other layers get, a quoted
// --library "~/assets" puts a multi-gigabyte mirror in a directory named "~".
func TestApplyFlagsIsTheLastLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	base := config.Config{LibraryPath: "/from/env", Concurrency: 4, CustomerID: "from-env"}

	got := applyFlags(base, "~/assets", "from-flag", 8)
	if got.LibraryPath != filepath.Join(home, "assets") {
		t.Errorf("LibraryPath = %q, want the expanded %q", got.LibraryPath, filepath.Join(home, "assets"))
	}
	if got.CustomerID != "from-flag" || got.Concurrency != 8 {
		t.Errorf("flags did not win: %+v", got)
	}

	// An unset flag leaves the layer beneath it alone.
	unchanged := applyFlags(base, "", "", 0)
	if unchanged != base {
		t.Errorf("empty flags changed the config: %+v, want %+v", unchanged, base)
	}
}
