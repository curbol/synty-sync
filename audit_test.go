package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/config"
	"github.com/curbol/synty-sync/internal/lockfile"
	"github.com/curbol/synty-sync/internal/manifest"
	"github.com/curbol/synty-sync/internal/portal"
	"github.com/curbol/synty-sync/internal/syncer"
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
	for _, tc := range []struct {
		args []string
		// hintsOnly is whether the message may suggest --only: a subcommand that does
		// not bind the flag must not send the user into "flag provided but not defined".
		hintsOnly bool
	}{
		{args: []string{"sync", "polygon-city", "--dry-run"}, hintsOnly: true},
		{args: []string{"status", "somepack"}, hintsOnly: true},
		{args: []string{"list", "extra"}},
		{args: []string{"select", "extra"}},
		{args: []string{"update", "v1", "v2"}},
	} {
		err := run(tc.args)
		if err == nil {
			t.Errorf("%v: accepted a stray positional argument", tc.args)
			continue
		}
		if !strings.Contains(err.Error(), "argument") {
			t.Errorf("%v: err = %v, want it to explain the stray argument", tc.args, err)
		}
		if got := strings.Contains(err.Error(), "--only"); got != tc.hintsOnly {
			t.Errorf("%v: mentions --only = %v, want %v: %v", tc.args, got, tc.hintsOnly, err)
		}
	}
}

// An expired session must say what to do next, not just what broke. main is the
// only layer that knows which cookie source was used.
func TestExpiredSessionErrorNamesTheCookieSource(t *testing.T) {
	base := errors.New("expired or missing session")
	db := "/home/u/.config/zen/abc.default/cookies.sqlite"
	err := explainSession(fmt.Errorf("%w", portal.ErrExpiredSession), "zen", db)
	if !errors.Is(err, portal.ErrExpiredSession) {
		t.Fatalf("wrapping lost the sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "zen") || !strings.Contains(err.Error(), db) {
		t.Errorf("err = %q, want it to name the session source and the profile it read", err)
	}
	// An unrelated error passes through untouched.
	if got := explainSession(base, "zen", db); got != base {
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	err = selectPacks(context.Background(), client, manifestPath, ln)
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
	const seeded = "variant_includes = [\"Godot_*\"]\n\n[[pack]]\n  slug = \"pirate-pack\"\n  name = \"Pirate Pack\"\n  enabled = true\n"
	for _, tc := range []struct {
		name        string
		seed        string
		post        []string
		wantErr     bool
		wantEnabled []string
	}{
		{
			name:        "the chosen pack is enabled and the rest stay off",
			seed:        "variant_includes = [\"Godot_*\"]\n",
			post:        []string{"pirate-pack"},
			wantEnabled: []string{"pirate-pack"},
		},
		{
			name:        "an empty submission will not wipe a live selection",
			seed:        seeded,
			post:        nil,
			wantErr:     true,
			wantEnabled: []string{"pirate-pack"},
		},
		{
			name:        "a stale tab's slugs will not wipe a live selection",
			seed:        seeded,
			post:        []string{"long-gone"},
			wantErr:     true,
			wantEnabled: []string{"pirate-pack"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(libraryStore([]stubPack{
				{orderItem: 3, name: "Pirate Pack"},
				{orderItem: 4, name: "Dungeon Pack"},
			}, nil))
			defer srv.Close()

			manifestPath := filepath.Join(t.TempDir(), "synty-sync.toml")
			if err := os.WriteFile(manifestPath, []byte(tc.seed), 0o644); err != nil {
				t.Fatal(err)
			}

			stdoutWas := stdout
			stdout = &bytes.Buffer{}
			defer func() { stdout = stdoutWas }()

			if err := driveSelect(t, srv, manifestPath, tc.post); (err != nil) != tc.wantErr {
				t.Fatalf("selectPacks err = %v, wantErr = %v", err, tc.wantErr)
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

// selectPacks re-encodes the whole manifest from a copy loaded before the page went
// up, and the wait for the page is a person's, so every field was written back at its
// pre-page value. An edit made in that window was reverted in a committed file the
// README invites hand-editing, with nothing printed about it. Only the selection is
// the page's to decide.
func TestSelectKeepsAManifestEditMadeWhileThePageWasOpen(t *testing.T) {
	srv := httptest.NewServer(libraryStore([]stubPack{
		{orderItem: 3, name: "Pirate Pack"},
		{orderItem: 4, name: "Dungeon Pack"},
	}, nil))
	defer srv.Close()

	manifestPath := filepath.Join(t.TempDir(), "synty-sync.toml")
	if err := os.WriteFile(manifestPath, []byte("variant_includes = [\"Godot_*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdoutWas := stdout
	stdout = &bytes.Buffer{}
	defer func() { stdout = stdoutWas }()

	edit := func() {
		if err := os.WriteFile(manifestPath, []byte("variant_includes = [\"Godot_*\", \"SourceSprites\"]\n"), 0o644); err != nil {
			t.Error(err)
		}
	}
	if err := driveSelectWith(t, srv, manifestPath, []string{"pirate-pack"}, edit); err != nil {
		t.Fatal(err)
	}

	man, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(man.VariantIncludes, ","); got != "Godot_*,SourceSprites" {
		t.Errorf("variant_includes = %q, want the edit made while the page was open to survive", got)
	}
	if enabled := man.EnabledSet(); len(enabled) != 1 || !enabled["pirate-pack"] {
		t.Errorf("enabled = %v, want the page to still decide the selection (just pirate-pack)", enabled)
	}
}

// driveSelect runs selectPacks against srv on an ephemeral port, fetches the page for
// its form token, posts the given slugs, and returns what selectPacks returned. Two
// tests spelled out the listener, the goroutine, the poll, the PostForm and the
// timeout select before they could assert anything, so the submission they were about
// was the one thing buried in it.
func driveSelect(t *testing.T, srv *httptest.Server, manifestPath string, post []string) error {
	t.Helper()
	return driveSelectWith(t, srv, manifestPath, post, nil)
}

// driveSelectWith is driveSelect with a hook that runs while the page is up and
// selectPacks is blocked on it, for a test about something happening inside that
// window rather than before or after it.
func driveSelectWith(t *testing.T, srv *httptest.Server, manifestPath string, post []string, duringPageOpen func()) error {
	t.Helper()
	client := &portal.Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1", Cookie: "x=y"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	done := make(chan error, 1)
	go func() { done <- selectPacks(context.Background(), client, manifestPath, ln) }()

	token := waitForSelectPage(t, addr, done)
	if duringPageOpen != nil {
		duringPageOpen()
	}
	resp, err := http.PostForm("http://"+addr+"/save", url.Values{"pack": post, "csrf": {token}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("selectPacks did not return after the save")
		return nil
	}
}

// formTokenRe reads the per-invocation token out of the rendered page. It is what
// separates a submission from the page synty-sync served from one any other tab could
// forge, so a test that posts a selection has to go and get it.
var formTokenRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// waitForSelectPage blocks until the page is being served and returns its form token.
// done, when non-nil, is the channel run's error arrives on: a run that failed before
// serving says why, which beats polling out on a page that was never coming.
func waitForSelectPage(t *testing.T, addr string, done <-chan error) string {
	t.Helper()
	for range 200 {
		select {
		case err := <-done:
			t.Fatalf("run returned before serving the page: %v", err)
		default:
		}
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		m := formTokenRe.FindSubmatch(body)
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
	// The pack lists no file, so its item page is not served by the walk: a fetch of it
	// lands here and is counted.
	srv := httptest.NewServer(libraryStore(
		[]stubPack{{orderItem: 3, name: "Off"}},
		func(w http.ResponseWriter, r *http.Request) {
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

// A dry run is only a promise if it survives the trip from the command line to the
// option. isDryRun is unit-tested and runSyncOrStatus is driven with dry passed straight
// in, so nothing followed either way of asking for one through run: a --dry-run that
// stopped being read, or a status dispatched on the flag alone, would download the
// entire delta and rewrite the committed lockfile while the report said it had only
// looked. status is the one users run to look before they leap.
func TestDryRunsDownloadNothingAndWriteNoLockfile(t *testing.T) {
	for _, args := range [][]string{{"sync", "-dry-run"}, {"status"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var downloads int32
			serveStore(t, libraryStore(
				[]stubPack{{orderItem: 3, name: "Pirate", token: "POLYGON_Pirate", fileID: 77, version: "v1.0.0"}},
				func(w http.ResponseWriter, r *http.Request) {
					atomic.AddInt32(&downloads, 1)
					w.Header().Set("Content-Type", "application/zip")
					fmt.Fprint(w, "PK\x03\x04 not really a pack")
				}))
			e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n\n[[pack]]\n  slug = \"pirate\"\n  name = \"Pirate\"\n  enabled = true\n")

			out := &bytes.Buffer{}
			stdoutWas := stdout
			stdout = out
			defer func() { stdout = stdoutWas }()

			if err := run(e.args(args[0], args[1:]...)); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if n := atomic.LoadInt32(&downloads); n != 0 {
				t.Errorf("%v issued %d download request(s)", args, n)
			}
			if _, err := os.Stat(e.lockPath); !os.IsNotExist(err) {
				t.Errorf("%v wrote the committed lockfile at %s (stat err %v)", args, e.lockPath, err)
			}
			// And it said so, rather than reporting downloads it did not make.
			if !strings.Contains(out.String(), "would download: 1 files") {
				t.Errorf("the report does not read as a dry run:\n%s", out.String())
			}
		})
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
	srv := httptest.NewServer(libraryStore(
		[]stubPack{{orderItem: 3, name: "Pirate", token: "POLYGON_Pirate", fileID: 77, version: "v1.0.0"}},
		loginPage))
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
	// Not just the word "failed": printReport always emits a "failed: N" summary
	// line, so matching that alone passes on a run with no failures at all.
	if !strings.Contains(out.String(), "failed: pirate POLYGON_Pirate|Godot_4_5_1") {
		t.Errorf("the report does not name the failed file:\n%s", out.String())
	}
}

// --cookies is applied by resolveCookie rather than through config.Flags, and the shell
// leaves a quoted "~/session.curl" alone, so it arrives with the tilde intact. Without
// the expansion every other path gets, a session source resolving to a directory named
// "~" is reported as a missing file rather than as the path the user typed.
func TestCookiesFlagExpandsATilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	curl := filepath.Join(home, "session.curl")
	if err := os.WriteFile(curl, []byte(`curl 'https://syntystore.com' -H 'Cookie: sid=abc'`), 0o600); err != nil {
		t.Fatal(err)
	}
	sess, err := resolveCookie(config.Config{SessionSource: "firefox"}, "~/session.curl")
	if err != nil {
		t.Fatalf("resolveCookie with a tilde path: %v", err)
	}
	if sess.Header != "sid=abc" {
		t.Errorf("cookie = %q, want the one in %s", sess.Header, curl)
	}
}

// Zero is how the config chain spells "not supplied", so --concurrency 0 ran at the
// configured default and said nothing. A typed zero or a negative is not a number of
// simultaneous fetches, so it is refused before the store is contacted.
func TestConcurrencyBelowOneIsRefused(t *testing.T) {
	reached := false
	serveStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")

	for _, n := range []string{"0", "-1"} {
		err := run(e.args("status", "-concurrency", n))
		if err == nil || !strings.Contains(err.Error(), "--concurrency") {
			t.Errorf("--concurrency %s: err = %v, want it refused by name", n, err)
		}
	}
	if reached {
		t.Error("the store was contacted with a concurrency that was refused")
	}
}

// runEnv is a throwaway config dir, manifest, lockfile, library and cookie file, so a
// test can drive run() the way a user does instead of entering below the wiring.
type runEnv struct {
	configDir, manifestPath, lockPath, libraryDir, cookiesPath string
}

func newRunEnv(t *testing.T, manifestBody string) runEnv {
	t.Helper()
	dir := t.TempDir()
	e := runEnv{
		configDir:    t.TempDir(),
		manifestPath: filepath.Join(dir, "synty-sync.toml"),
		lockPath:     filepath.Join(dir, "synty-sync.lock.json"),
		libraryDir:   t.TempDir(),
		cookiesPath:  filepath.Join(dir, "cookies.txt"),
	}
	if err := os.WriteFile(e.manifestPath, []byte(manifestBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// A Netscape cookies.txt so the session resolves off disk: the default source is
	// the user's real browser profile, which a test must never read.
	if err := os.WriteFile(e.cookiesPath, []byte("syntystore.com\tTRUE\t/\tTRUE\t0\tsession\tabc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e runEnv) args(cmd string, extra ...string) []string {
	return append([]string{cmd,
		"-config", e.configDir, "-manifest", e.manifestPath,
		"-library", e.libraryDir, "-cookies", e.cookiesPath,
		"-customer", "1234567890",
	}, extra...)
}

// selectArgs is args without the status/sync-only flags, which select does not bind
// and therefore rejects. Keeping them separate is the point of registerFlags.
func (e runEnv) selectArgs(extra ...string) []string {
	return append([]string{"select",
		"-config", e.configDir, "-manifest", e.manifestPath,
		"-cookies", e.cookiesPath, "-customer", "1234567890",
	}, extra...)
}

// serveStore points every subcommand at srv for the duration of a test. run builds
// its own client, so this is the only way in.
func serveStore(t *testing.T, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	prev := storeBaseURL
	storeBaseURL = srv.URL
	t.Cleanup(func() { storeBaseURL = prev; srv.Close() })
}

// stubPack is one pack a stubbed store lists, plus the single Godot file its item page
// offers. The order-item id is what the library anchor and the item-page route share;
// an empty token means the pack has no item page, so a test can prove one was never
// fetched.
type stubPack struct {
	orderItem int
	name      string
	token     string
	fileID    int
	version   string
}

// libraryStore serves the enumeration walk and the item pages behind it: page 1 lists
// packs, every later page is the empty authenticated terminator, and a pack carrying a
// token answers its item page with one downloadable file. Everything else — the signed
// URL a download resolves to, and an item page a test wants to count or refuse — goes
// to rest. That last handler is the only thing that actually differs between these
// tests, and each of them used to wrap its own copy of the walk around it.
func libraryStore(packs []stubPack, rest http.HandlerFunc) http.HandlerFunc {
	const sentinel = `<input class='sky-pilot-search-input'>`
	if rest == nil {
		rest = http.NotFound
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("line_items_page") == "1":
			var b strings.Builder
			b.WriteString(`<div class='sky-pilot'>` + sentinel)
			for _, p := range packs {
				fmt.Fprintf(&b, `<a href='/apps/downloads/customers/1/orders/2/order_items/%d' `+
					`class='sky-pilot-list-item'>%s</a>`, p.orderItem, p.name)
			}
			b.WriteString(`</div>`)
			fmt.Fprint(w, b.String())
		case r.URL.Query().Get("line_items_page") != "":
			fmt.Fprint(w, `<div class='sky-pilot'>`+sentinel+`</div>`)
		default:
			for _, p := range packs {
				if p.token == "" || !strings.HasSuffix(r.URL.Path, fmt.Sprintf("/order_items/%d", p.orderItem)) {
					continue
				}
				fmt.Fprintf(w, `<div class='sky-pilot-list-item'>
				  <div class='sky-pilot-file-heading'>%s_Godot_4_5_1 | %s <span class='sky-pilot-file-size'>(40 MB)</span></div>
				  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/%d?x=1'>Download</a></div>
				</div>`, p.token, p.version, p.fileID)
				return
			}
			rest(w, r)
		}
	}
}

// loginPage is what the store answers a download request with once the session is
// gone: a document where package bytes should be.
func loginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><title>Log in</title>`)
}

// Everything between parsing a flag and issuing a request happens in run and nowhere
// else: which id and which cookie reach the client, and which layer wins. Entering at
// runSyncOrStatus skips all of it, so a transposed argument to portal.New — the
// customer id sent as the Cookie header, three strings of the same type — would ship
// with the suite green.
func TestRunCarriesTheCustomerIDAndCookieToTheStore(t *testing.T) {
	var gotCookie, gotPath string
	serveStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie, gotPath = r.Header.Get("Cookie"), r.URL.Path
		// An authenticated page with no packs: the walk ends, nothing downloads.
		fmt.Fprint(w, `<html><body><input class="sky-pilot-search-input"></body></html>`)
	}))
	e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")

	var out bytes.Buffer
	prev := stdout
	stdout = &out
	defer func() { stdout = prev }()

	if err := run(e.args("status")); err != nil {
		t.Fatalf("status against a stub store: %v", err)
	}
	if !strings.Contains(gotCookie, "session=abc123") {
		t.Errorf("Cookie header = %q, want the cookie file's pair", gotCookie)
	}
	if strings.Contains(gotCookie, "1234567890") {
		t.Errorf("the customer id was sent as the cookie: %q", gotCookie)
	}
	if !strings.Contains(gotPath, "/1234567890") {
		t.Errorf("request path = %q, want the customer id from --customer", gotPath)
	}
	if !strings.Contains(out.String(), "library: "+e.libraryDir) {
		t.Errorf("the report names a library other than --library:\n%s", out.String())
	}
}

// The customer id has no default and every store URL is built from it, so run has to
// stop before the network rather than request a path with an empty segment.
func TestRunWithoutACustomerIDStopsBeforeTheStore(t *testing.T) {
	reached := false
	serveStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")

	args := []string{"status", "-config", e.configDir, "-manifest", e.manifestPath,
		"-library", e.libraryDir, "-cookies", e.cookiesPath}
	t.Setenv("SYNTY_CUSTOMER_ID", "")
	err := run(args)
	if err == nil || !strings.Contains(err.Error(), "no customer id") {
		t.Fatalf("err = %v, want the missing-customer-id error", err)
	}
	if reached {
		t.Error("the store was contacted without a customer id")
	}
}

// The config package's tests cover ResolveDir and Load in isolation, and every run()
// test passes --library and --customer, so nothing drove a real config.toml through
// run. Handing Load the raw --config value instead of ResolveDir's answer reads a stray
// ./config.toml when no flag is given (Load("") joins to a bare "config.toml"), and
// dropping a field from the Flags literal lets the file beat the flag; either mirrors
// into a library the user did not name, said only by the "library:" line printed after
// the downloads.
func TestRunReadsTheConfigFileItResolves(t *testing.T) {
	var gotPath string
	serveStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `<html><body><input class="sky-pilot-search-input"></body></html>`)
	}))
	t.Setenv("SYNTY_LIBRARY", "")
	t.Setenv("SYNTY_CUSTOMER_ID", "")
	t.Setenv("SYNTY_CONFIG_DIR", "")

	writeConfig := func(t *testing.T, dir, library string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("customer_id = \"5550001112223\"\nlibrary_path = %q\n", library)
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	libraryOf := func(t *testing.T, args ...string) string {
		t.Helper()
		e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")
		var out bytes.Buffer
		prev := stdout
		stdout = &out
		defer func() { stdout = prev }()
		args = append(args, "-manifest", e.manifestPath, "-cookies", e.cookiesPath)
		if err := run(append([]string{"status"}, args...)); err != nil {
			t.Fatalf("status %v: %v", args, err)
		}
		for line := range strings.SplitSeq(out.String(), "\n") {
			if rest, ok := strings.CutPrefix(line, "library: "); ok {
				return rest
			}
		}
		t.Fatalf("the summary printed no library line:\n%s", out.String())
		return ""
	}

	t.Run("a named config dir", func(t *testing.T) {
		dir, want := t.TempDir(), filepath.Join(t.TempDir(), "from-named-config")
		writeConfig(t, dir, want)
		if got := libraryOf(t, "-config", dir); got != want {
			t.Errorf("library = %q, want %q from the config.toml --config names", got, want)
		}
		if !strings.Contains(gotPath, "/5550001112223") {
			t.Errorf("request path = %q, want the customer id from config.toml", gotPath)
		}
	})

	t.Run("the resolved config dir when no flag names one", func(t *testing.T) {
		xdg, want := t.TempDir(), filepath.Join(t.TempDir(), "from-resolved-config")
		t.Setenv("XDG_CONFIG_HOME", xdg)
		writeConfig(t, filepath.Join(xdg, "synty-sync"), want)
		// A config.toml in the working directory is what Load("") would read instead.
		wd := t.TempDir()
		writeConfig(t, wd, filepath.Join(wd, "from-the-working-directory"))
		t.Chdir(wd)
		if got := libraryOf(t); got != want {
			t.Errorf("library = %q, want %q: the resolved config dir was not the one read", got, want)
		}
	})

	t.Run("flags beat the file", func(t *testing.T) {
		dir, want := t.TempDir(), filepath.Join(t.TempDir(), "from-the-flag")
		writeConfig(t, dir, filepath.Join(t.TempDir(), "from-the-file"))
		if got := libraryOf(t, "-config", dir, "-library", want, "-customer", "9990001112223"); got != want {
			t.Errorf("library = %q, want the --library value %q", got, want)
		}
		if !strings.Contains(gotPath, "/9990001112223") {
			t.Errorf("request path = %q, want the customer id from --customer", gotPath)
		}
	})
}

// An expired session has to keep its sentinel all the way out of run, where the exit
// status and the "log in again" hint are decided. Both subcommands that reach the
// store have their own explainSession wrap, so both are checked: select's is the one
// nothing else exercises, and a %v there would read as a plain error at the top while
// the user is told nothing about logging in.
func TestRunSurfacesTheExpiredSessionSentinel(t *testing.T) {
	for _, cmd := range []string{"sync", "select"} {
		t.Run(cmd, func(t *testing.T) {
			serveStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `<html><body><h1>Login</h1></body></html>`) // no logged-in sentinel
			}))
			e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")

			args := e.args(cmd)
			if cmd == "select" {
				args = e.selectArgs("-addr", "127.0.0.1:0")
			}
			err := run(args)
			if !errors.Is(err, portal.ErrExpiredSession) {
				t.Fatalf("err = %v, want ErrExpiredSession to survive run", err)
			}
			if !strings.Contains(err.Error(), e.cookiesPath) {
				t.Errorf("the error does not name the cookie source that failed: %v", err)
			}
			if _, statErr := os.Stat(e.lockPath); !os.IsNotExist(statErr) {
				t.Errorf("a lockfile was written on an expired session")
			}
		})
	}
}

// The run summary is the command's actual output, and every line of it was reachable
// only through a Contains("failed") check on one sync test. A swapped counts key or a
// mislabelled failure would read as a correct report.
func TestPrintReportNamesEveryOutcome(t *testing.T) {
	lib := t.TempDir()
	rep := syncer.Report{
		Diffs: []syncer.FileDiff{
			{Class: syncer.New}, {Class: syncer.Changed}, {Class: syncer.DownloadNow},
			{Class: syncer.CacheMissing}, {Class: syncer.Adopted}, {Class: syncer.Unchanged},
		},
		Downloaded: []syncer.FileDiff{{Class: syncer.New}},
		Adopted:    []syncer.FileDiff{{Class: syncer.Adopted}},
		Failures: []syncer.Failure{
			{PackSlug: "pirate", Key: "T|V", Err: "boom"},
			{PackSlug: "dungeon", Key: "U|V", Err: "404", Gone: true},
		},
		Removed:      []string{"old-pack"},
		PacksInScope: 1, // one of the two in the lockfile; the other was carried forward
		Swept:        2,
		SweptBytes:   4096,
		Warnings:     []string{"nothing matches the filter"},
		NewLockfile:  lockfile.Lockfile{Packs: map[string]lockfile.Pack{"a": {}, "b": {}}},
	}
	cfg := config.Config{LibraryPath: lib}

	var dry, wet bytes.Buffer
	printReport(&dry, true, cfg, rep)
	printReport(&wet, false, cfg, rep)

	for _, want := range []string{
		"library: " + lib,
		"packs: 1 of 2 in the lockfile  files selected: 6",
		"new=1 changed=1 download-now=1 cache-missing=1 adopted=1 unchanged=1",
		"swept 2 abandoned download temp(s), 4096 bytes reclaimed",
		"failed: pirate T|V: boom",
		"gone from the store: dungeon U|V: 404",
		"no longer in your library: old-pack",
		"warning: nothing matches the filter",
	} {
		for name, got := range map[string]string{"dry": dry.String(), "sync": wet.String()} {
			if !strings.Contains(got, want) {
				t.Errorf("%s report is missing %q:\n%s", name, want, got)
			}
		}
	}
	// New + Changed + DownloadNow + CacheMissing; an adopted or unchanged file counted
	// here would promise a download that never happens.
	if !strings.Contains(dry.String(), "would download: 4 files") {
		t.Errorf("dry report does not say what it would download:\n%s", dry.String())
	}
	if strings.Contains(dry.String(), "downloaded:") {
		t.Errorf("dry report claims work it did not do:\n%s", dry.String())
	}
	if !strings.Contains(wet.String(), "downloaded: 1 files  adopted: 1 existing  failed: 2") {
		t.Errorf("sync report does not tally what it did:\n%s", wet.String())
	}
	if strings.Contains(wet.String(), "would download") {
		t.Errorf("sync report hedges about work it already did:\n%s", wet.String())
	}
}

// A session for a different account enumerates a library disjoint from the lockfile,
// so every pack it records comes back "no longer in your library": hundreds of
// consecutive lines that bury the failures and warnings printed around them. A few is
// the ordinary case and each is still named; past that, the rest is counted.
func TestTheNoLongerInYourLibraryListIsCapped(t *testing.T) {
	var removed []string
	for i := range 300 {
		removed = append(removed, fmt.Sprintf("pack-%03d", i))
	}
	var out bytes.Buffer
	printReport(&out, true, config.Config{}, syncer.Report{Removed: removed})
	got := out.String()
	if n := strings.Count(got, "no longer in your library"); n > 20 {
		t.Errorf("the summary printed %d no-longer-in-your-library lines", n)
	}
	if !strings.Contains(got, "pack-000") {
		t.Errorf("the capped list names none of the packs:\n%s", got)
	}
	if !strings.Contains(got, fmt.Sprintf("and %d more", 300-maxListed)) {
		t.Errorf("the summary does not account for the packs it did not name:\n%s", got)
	}

	out.Reset()
	printReport(&out, true, config.Config{}, syncer.Report{Removed: removed[:maxListed]})
	if got := out.String(); strings.Count(got, "no longer in your library") != maxListed || strings.Contains(got, "more") {
		t.Errorf("a list at the cap was truncated:\n%s", got)
	}
}

// A file the store no longer serves fails forever, so it is reported without moving
// the exit status — otherwise a single 404 makes every future sync exit non-zero and
// the status stops meaning "something a re-run could fix". main restates the rule
// that syncer.ActionableFailures owns, so it needs its own guard: swapping the call
// for len(rep.Failures) leaves the rest of the suite green.
func TestSyncWithAGoneFileReportsItWithoutFailingTheRun(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "synty-sync.toml")
	if err := os.WriteFile(manifestPath, []byte(
		"variant_includes = [\"Godot_*\"]\n\n[[pack]]\nslug = \"pirate\"\nname = \"Pirate\"\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(libraryStore(
		[]stubPack{{orderItem: 3, name: "Pirate", token: "POLYGON_Pirate", fileID: 77, version: "v1.0.0"}},
		http.NotFound)) // the store no longer serves this file
	defer srv.Close()

	client := &portal.Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1", Cookie: "x=y"}
	cfg := config.Config{LibraryPath: t.TempDir(), Concurrency: 2}
	lockPath := filepath.Join(dir, "synty-sync.lock.json")

	var out bytes.Buffer
	restore := stdout
	stdout = &out
	t.Cleanup(func() { stdout = restore })

	if err := runSyncOrStatus(context.Background(), client, cfg, manifestPath, lockPath, "", false); err != nil {
		t.Errorf("a file the store no longer serves moved the exit status: %v", err)
	}
	if !strings.Contains(out.String(), "gone from the store") {
		t.Errorf("the report does not name the file as gone:\n%s", out.String())
	}
	// The record of everything the run did is still written.
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("no lockfile written: %v", err)
	}
}

// select rewrites the committed manifest from what the page returns, and Enumerate
// answers an unparseable library the same way it answers an empty one. Without the
// refusal the syncer already makes for the lockfile, a markup change reconciles the
// pack list down to nothing and Save deletes every [[pack]] entry the user had.
func TestSelectRefusesToRewriteTheManifestFromAnEmptyLibrary(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "synty-sync.toml")
	original := "variant_includes = [\"Godot_*\"]\n\n[[pack]]\n  slug = \"pirate\"\n  name = \"Pirate\"\n  enabled = false\n"
	if err := os.WriteFile(manifestPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// A logged-in page whose pack anchors no longer parse: the sentinel is there, so
	// Enumerate reports an empty library rather than an expired session.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div class='sky-pilot'><input class='sky-pilot-search-input'></div>`)
	}))
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client := &portal.Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1", Cookie: "x=y"}
	if err := selectPacks(context.Background(), client, manifestPath, ln); err == nil {
		t.Error("select rewrote the manifest from a library that listed no packs")
	}
	got, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("the committed manifest was rewritten:\n%s", got)
	}
}

// select is the documented first run in a project with no manifest yet, so it defaults
// to creating one in the working directory rather than erroring the way the commands
// that only read a manifest do.
func TestResolveManifestPathLetsSelectStartAProject(t *testing.T) {
	t.Chdir(t.TempDir())

	got, err := resolveManifestPath("", "select")
	if err != nil {
		t.Fatalf("select with no manifest anywhere: %v", err)
	}
	// t.TempDir can sit behind a symlink (/tmp -> /private/tmp), so compare basenames
	// and that it landed in the working directory rather than the literal path.
	if filepath.Base(got) != manifest.FileName {
		t.Errorf("resolveManifestPath = %q, want a %s in the working directory", got, manifest.FileName)
	}
	if _, err := resolveManifestPath("", "sync"); err == nil {
		t.Error("sync with no manifest anywhere reported success")
	}
}

// registerFlags binds only the flags that mean something for a subcommand, so one
// that does not is a parse error rather than accepted and quietly ignored. Its own
// comment names the failure: a shared flag set let `select --dry-run` serve the page
// and rewrite the committed manifest, which is the opposite of what the flag says.
// Nothing tested it: collapsing those conditionals into one unconditional set left
// the whole suite green.
//
// Parsing is all this drives: fs.Parse fails before any config dir, manifest or
// network is touched, so no case here can reach a real file.
func TestEachSubcommandTakesOnlyItsOwnFlags(t *testing.T) {
	// Every flag the CLI defines, and which subcommands may have it.
	matrix := map[string][]string{
		"manifest":    {"select", "status", "sync", "list"},
		"config":      {"select", "status", "sync"},
		"cookies":     {"select", "status", "sync"},
		"customer":    {"select", "status", "sync"},
		"library":     {"status", "sync"},
		"only":        {"status", "sync"},
		"concurrency": {"status", "sync"},
		"dry-run":     {"sync"},
		"addr":        {"select"},
	}
	// A value that parses for every flag type, so a rejection is always about scope.
	value := map[string]string{"concurrency": "2", "dry-run": ""}

	for flagName, allowed := range matrix {
		for _, cmd := range []string{"select", "status", "sync", "list", "update"} {
			args := []string{"-" + flagName}
			if v, ok := value[flagName]; ok {
				if v != "" {
					args = append(args, v)
				}
			} else {
				args = append(args, "x")
			}

			fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			registerFlags(fs, cmd)
			err := fs.Parse(args)

			if slices.Contains(allowed, cmd) {
				if err != nil {
					t.Errorf("%s does not accept -%s, but usage lists it: %v", cmd, flagName, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s accepted -%s; a flag that means nothing for a subcommand must be a parse error, "+
					"not silently ignored", cmd, flagName)
			}
		}
	}

	// And the help text says the same thing, so the two cannot drift: a flag listed
	// for a subcommand it cannot take sends the user to a parse error.
	var help strings.Builder
	usage(&help)
	for flagName, allowed := range matrix {
		line := findUsageLine(help.String(), "-"+flagName+" ")
		if line == "" {
			t.Errorf("usage() does not document -%s", flagName)
			continue
		}
		heading := usageHeadingFor(help.String(), line)
		for _, cmd := range allowed {
			if !strings.Contains(heading, cmd) {
				t.Errorf("usage() lists -%s under %q, which omits %s", flagName, heading, cmd)
			}
		}
		for _, cmd := range []string{"select", "status", "sync", "list"} {
			if !slices.Contains(allowed, cmd) && strings.Contains(heading, cmd) {
				t.Errorf("usage() lists -%s under %q, but %s does not accept it", flagName, heading, cmd)
			}
		}
	}
}

// findUsageLine returns the usage line introducing a flag, or "".
func findUsageLine(help, flag string) string {
	for _, line := range strings.Split(help, "\n") {
		if strings.Contains(line, flag) && strings.HasPrefix(strings.TrimSpace(line), "-") {
			return line
		}
	}
	return ""
}

// usageHeadingFor returns the subcommand-list heading a flag line sits under: the
// nearest preceding line indented less than the flag's own.
func usageHeadingFor(help, flagLine string) string {
	lines := strings.Split(help, "\n")
	at := slices.Index(lines, flagLine)
	if at < 0 {
		return ""
	}
	indent := func(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
	for i := at - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" && indent(lines[i]) < indent(flagLine) {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}

// list opens one JSON file beside the manifest. It used to resolve and parse the
// user config first, and Load rejects an unknown key outright, so a typo in
// config.toml broke a subcommand that never reads it.
func TestListDoesNotNeedAReadableUserConfig(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"),
		[]byte("customer_id = \"1\"\nnot_a_real_key = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNTY_CONFIG_DIR", cfgDir)

	// The config really is broken, so this test cannot pass by the file being ignored.
	if _, err := config.Load(cfgDir, config.Flags{}); err == nil {
		t.Fatal("this config was meant to be rejected; the test proves nothing as written")
	}

	project := t.TempDir()
	prev := stdout
	stdout = io.Discard
	defer func() { stdout = prev }()
	if err := run([]string{"list", "-manifest", filepath.Join(project, "synty-sync.toml")}); err != nil {
		t.Errorf("list failed over a user config it never reads: %v", err)
	}
}

// The select branch in run is four things nothing else exercises: the --addr default,
// the net.Listen error wrap, the listener reaching web.Serve, and the explainSession
// wrap on the way out. Entering at selectPacks skips all of it, so a bad bind string
// or a lost sentinel would ship with the suite green.
func TestRunSelectServesOnTheAddressItWasGiven(t *testing.T) {
	serveStore(t, libraryStore([]stubPack{{orderItem: 3, name: "POLYGON - Pirate Pack"}}, nil))
	e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")

	// An ephemeral port, so the test never contends for the 8787 default. Binding and
	// closing to learn the number is a race against anything that grabs it in between,
	// and it is accepted deliberately: run has to be handed an address rather than a
	// listener, so there is no way to exercise its --addr path without naming a port.
	// A fixed one would trade this window for a permanent collision.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	prevStdout := stdout
	stdout = &out
	defer func() { stdout = prevStdout }()

	done := make(chan error, 1)
	go func() { done <- run(e.selectArgs("-addr", addr)) }()

	// Submit the one pack the page offered, which is what proves the listener run
	// bound is the one web.Serve is answering on.
	base := "http://" + addr
	token := waitForSelectPage(t, addr, done)
	resp, err := http.PostForm(base+"/save", url.Values{
		"pack": {"polygon-pirate-pack"},
		"csrf": {token},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("select: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after the selection was submitted")
	}
	// The committed manifest holds what was chosen, and run said where it wrote it.
	man, err := manifest.Load(e.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !man.EnabledSet()["polygon-pirate-pack"] {
		t.Errorf("the chosen pack is not enabled in %s: %+v", e.manifestPath, man.Packs)
	}
	if !strings.Contains(out.String(), e.manifestPath) {
		t.Errorf("run did not report where it saved:\n%s", out.String())
	}
}

// An address already in use has to come back naming it, not as a bare syscall error:
// 8787 is the default and something else holding it is the likeliest way select fails.
func TestRunSelectReportsAnAddressItCannotBind(t *testing.T) {
	// A pack the store does own, so the manifest check below can actually fail: against
	// an empty library, falling through to the rest of select would write nothing
	// either, and the assertion would pass whatever run did.
	serveStore(t, libraryStore([]stubPack{{orderItem: 3, name: "Pirate Pack"}}, nil))
	e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	err = run(e.selectArgs("-addr", held.Addr().String()))
	if err == nil {
		t.Fatal("select bound an address another listener already holds")
	}
	if !strings.Contains(err.Error(), held.Addr().String()) {
		t.Errorf("error does not name the address it could not bind: %v", err)
	}
	// And it stopped there. A bind error that fell through to the rest of select would
	// enumerate the library and rewrite the committed manifest with no page ever shown.
	if _, statErr := os.Stat(e.manifestPath); statErr != nil {
		t.Fatal(statErr)
	}
	body, err := os.ReadFile(e.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "[[pack]]") {
		t.Errorf("select rewrote the manifest after failing to bind:\n%s", body)
	}
}

// variant_includes has no default, so a manifest without it is the state a first-time
// user is in after `select` creates one. The message has to name the key and show what
// a value looks like: without this guard the run gets as far as syncer.Run and fails
// with "Filter is required", which names nothing the user can act on.
func TestSyncWithoutVariantIncludesSaysWhatToAdd(t *testing.T) {
	e := newRunEnv(t, "[[pack]]\n  slug = \"pirate-pack\"\n  name = \"Pirate Pack\"\n  enabled = true\n")

	err := run([]string{"status",
		"-config", e.configDir, "-manifest", e.manifestPath,
		"-cookies", e.cookiesPath, "-customer", "1234567890",
		"-library", e.libraryDir,
	})
	if err == nil {
		t.Fatal("status ran against a manifest with no variant_includes")
	}
	for _, want := range []string{"variant_includes", e.manifestPath, "Godot_*"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// A bad --addr has to be reported as a bad --addr. The bind is what the user typed
// wrong, but the session is resolved from the browser's cookie store, so ordering the
// two the other way answers "0.0.0.0:8787" with whatever is wrong with their Firefox
// profile — on a headless box or a fresh container, that is the only thing they see.
func TestSelectReportsABadAddrBeforeReadingTheSession(t *testing.T) {
	e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")
	args := append([]string{"select",
		"-config", e.configDir, "-manifest", e.manifestPath,
		// A cookie source that cannot resolve, so whichever step runs first decides
		// which error comes back.
		"-cookies", filepath.Join(t.TempDir(), "nonexistent-cookies.txt"), "-customer", "1234567890",
	}, "-addr", "0.0.0.0:8787")

	err := run(args)
	if err == nil {
		t.Fatal("select accepted a non-loopback --addr")
	}
	if !strings.Contains(err.Error(), "0.0.0.0:8787") || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("the error is about the session, not the address the user typed: %v", err)
	}
}

// The selection page shows the account's whole library and its form rewrites the
// committed manifest, so it is built for one browser on this machine — and the handlers
// enforce exactly that. A wildcard or LAN --addr therefore cannot widen the page's
// reach, it can only break it: the browser such a bind is aimed at gets 421 while the
// port stands open to anything that can route to it. Refusing the bind is what keeps
// the flag from reading as a way to share the page.
func TestSelectRefusesANonLoopbackAddr(t *testing.T) {
	for _, tc := range []struct {
		bind string
		ok   bool
	}{
		{"localhost:0", true},
		{"127.0.0.1:0", true},
		{"[::1]:0", true},
		{":8787", false},
		{"0.0.0.0:8787", false},
		{"192.168.1.5:8787", false},
		{"8787", false}, // not host:port at all
	} {
		t.Run(tc.bind, func(t *testing.T) {
			ln, err := listenLocal(tc.bind)
			if ln != nil {
				ln.Close()
			}
			if tc.ok && err != nil {
				t.Fatalf("listenLocal(%q) failed: %v", tc.bind, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("listenLocal(%q) bound a listener the page cannot answer on", tc.bind)
				}
				if ln != nil {
					t.Error("a refused bind still returned a listener")
				}
			}
		})
	}
}

// select rebuilds the manifest's pack list from one enumeration, so a partial read
// drops entries and takes their enabled flags with it. The zero-pack case is refused
// outright, but a partial one is not detectable from here, and the only thing the user
// saw was a count of what remained. The lockfile side has always named what left
// (Report.Removed); this is the same record for the file that holds the selection.
func TestSelectNamesThePacksItDropsFromTheManifest(t *testing.T) {
	// The library lists only the pirate pack; the manifest holds both, enabled.
	srv := httptest.NewServer(libraryStore([]stubPack{{orderItem: 3, name: "Pirate Pack"}}, nil))
	defer srv.Close()

	manifestPath := filepath.Join(t.TempDir(), "synty-sync.toml")
	seed := "variant_includes = [\"Godot_*\"]\n\n[[pack]]\n  slug = \"pirate-pack\"\n  name = \"Pirate Pack\"\n  enabled = true\n\n[[pack]]\n  slug = \"dungeon-pack\"\n  name = \"Dungeon Pack\"\n  enabled = true\n"
	if err := os.WriteFile(manifestPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	stdoutWas := stdout
	out := &bytes.Buffer{}
	stdout = out
	defer func() { stdout = stdoutWas }()

	if err := driveSelect(t, srv, manifestPath, []string{"pirate-pack"}); err != nil {
		t.Fatalf("selectPacks: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "dungeon-pack") {
		t.Errorf("select removed dungeon-pack from the manifest without saying so:\n%s", got)
	}
	man, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Packs) != 1 {
		t.Errorf("manifest holds %d packs, want only the one the library listed", len(man.Packs))
	}
}

// lockfile.Load returns an empty lockfile for a path that does not exist, so `list`
// before the first sync printed nothing but the legend under an empty table, which
// reads as a broken command rather than an empty record.
func TestListSaysSoWhenThereIsNoLockfileYet(t *testing.T) {
	var out bytes.Buffer
	lockPath := filepath.Join(t.TempDir(), "synty-sync.lock.json")
	if err := list(&out, lockPath); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, lockPath) {
		t.Errorf("output does not name the lockfile it looked for:\n%s", got)
	}
	if strings.Contains(got, "* = downloaded") {
		t.Errorf("printed the legend for an empty table:\n%s", got)
	}
}

// The README tells the user to copy config.example.toml into their config dir, and
// both loaders reject an unknown key outright. So a renamed toml tag turns the
// committed example into a file that fails every command with "unknown key(s)", and
// the examples are the one schema description nothing else parses.
//
// The commented-out keys are uncommented first: left as they ship, a plain parse
// exercises only session_source and concurrency, and customer_id, library_path and the
// whole [[pack]] block — the set most likely to be renamed — would go unchecked.
func TestCommittedExamplesStillParse(t *testing.T) {
	// A leading "# " in front of a key or a table header, and nothing else: no prose
	// line in either file opens that way.
	commented := regexp.MustCompile(`(?m)^#[ \t]*(\[\[pack\]\]|[a-z_]+[ \t]*=)`)
	uncomment := func(t *testing.T, path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return commented.ReplaceAllFunc(raw, func(line []byte) []byte {
			return bytes.TrimLeft(line[1:], " \t")
		})
	}

	// The environment is cleared so the assertion is about the file rather than about
	// whatever the machine running the suite exports.
	t.Setenv("SYNTY_CUSTOMER_ID", "")
	t.Setenv("SYNTY_LIBRARY", "")
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), uncomment(t, "config.example.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgDir, config.Flags{})
	if err != nil {
		t.Fatalf("config.example.toml no longer parses as a config.toml: %v", err)
	}
	// Measured against what Load gives with no file at all: a non-empty library path
	// and a non-zero concurrency are what the defaults already supply, so asserting
	// only that passes with the example's keys never read.
	defaults, err := config.Load(t.TempDir(), config.Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CustomerID == defaults.CustomerID || cfg.LibraryPath == defaults.LibraryPath {
		t.Errorf("the example's customer_id or library_path did not reach the config: %+v", cfg)
	}
	// concurrency and session_source ship at their default values, so one read from the
	// file and one never read look the same. Changing the values, not the keys, still
	// holds the example's own key names to decoding.
	raw := uncomment(t, "config.example.toml")
	for _, sub := range []struct{ key, value string }{{"concurrency", "7"}, {"session_source", `"zen"`}} {
		line := regexp.MustCompile(`(?m)^` + sub.key + `[ \t]*=.*$`)
		if !line.Match(raw) {
			t.Errorf("config.example.toml no longer sets %s", sub.key)
		}
		raw = line.ReplaceAll(raw, []byte(sub.key+" = "+sub.value))
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err = config.Load(cfgDir, config.Flags{}); err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency != 7 || cfg.SessionSource != "zen" {
		t.Errorf("the example's concurrency or session_source key did not reach the config: %+v", cfg)
	}

	manifestPath := filepath.Join(t.TempDir(), manifest.FileName)
	if err := os.WriteFile(manifestPath, uncomment(t, "synty-sync.example.toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatalf("synty-sync.example.toml no longer parses as a manifest: %v", err)
	}
	if len(man.VariantIncludes) == 0 {
		t.Error("the example manifest carries no variant_includes, which every sync requires")
	}
	if len(man.Packs) == 0 {
		t.Error("the example manifest's [[pack]] block no longer decodes")
	}
}

// manifest.Load reports a path that does not exist as an empty manifest, so a typo in
// --manifest used to reach runSyncOrStatus and come back as "no variant_includes in
// <path>". The user opens the manifest they meant, finds variant_includes already
// there, and has nothing to act on. list and select are deliberately not checked: one
// derives a lockfile path beside a manifest that may not exist yet, the other is about
// to create one.
func TestSyncNamesAManifestThatIsNotThere(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "synty-sync.tml") // the typo

	for _, cmd := range []string{"status", "sync"} {
		_, err := resolveManifestPath(missing, cmd)
		if err == nil {
			t.Errorf("%s accepted a --manifest that does not exist", cmd)
			continue
		}
		if !strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), "variant_includes") {
			t.Errorf("%s: err = %v, want it to name the missing file", cmd, err)
		}
	}
	for _, cmd := range []string{"list", "select"} {
		if _, err := resolveManifestPath(missing, cmd); err != nil {
			t.Errorf("%s refused a manifest that does not have to exist yet: %v", cmd, err)
		}
	}
}

// guardFiles are the test files that hold guard tests. Every audit_test.go by
// convention, plus the two that cannot be one: install_test.go guards install.sh and
// the workflows from the root package, which already has an audit_test.go, and
// releaseyml_test.go is the whole of a package that exists only to be a guard's parser,
// and guard_test.go holds the PII guards, which cannot live in internal/fixtures's own
// suite because they are the suite. Matching on the filename alone left them outside
// the check that the convention is what makes these files worth having.
var guardFiles = []string{"audit_test.go", "install_test.go", "releaseyml_test.go", "guard_test.go"}

// The convention is that each guard test carries a comment naming the specific failure
// it prevents. That comment is what makes a red guard read as a regression rather than
// as a test to update, which is the whole reason these files are separate from the
// ordinary suites. Nine of them had drifted out of it — seven with no comment at all,
// two with their paragraphs stacked above a neighbour — and nothing could see that, so
// check it rather than trust it.
func TestEveryGuardTestSaysWhatItPrevents(t *testing.T) {
	var files []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata") {
			return fs.SkipDir
		}
		if !d.IsDir() && slices.Contains(guardFiles, d.Name()) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A wrong glob that matched nothing would pass this vacuously, and the count only
	// ever grows.
	if len(files) < 13 {
		t.Fatalf("found %d guard files (%v); the walk no longer reaches them", len(files), files)
	}

	for _, path := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			if fn.Doc == nil || strings.TrimSpace(fn.Doc.Text()) == "" {
				t.Errorf("%s:%d: %s has no comment naming the failure it prevents",
					path, fset.Position(fn.Pos()).Line, fn.Name.Name)
			}
		}
	}
}

// --only is the one flag whose whole job is to narrow what a run touches, and the
// four strings around it at the call site are all paths. Dropping the forward, or
// transposing it with one of them, left every test in the suite green while
// `sync --only one-pack` fetched every enabled pack's item page, downloaded the lot,
// and rebuilt the whole lockfile. registerFlags is proven to bind -only and syncer is
// proven to honour OnlyGlob; nothing connected the two.
func TestOnlyReachesTheRunFromTheFlag(t *testing.T) {
	var mu sync.Mutex
	fetched := map[string]bool{}
	packs := []stubPack{
		{orderItem: 3, name: "POLYGON - Pirate Pack", token: "POLYGON_Pirate", fileID: 11, version: "v1_0_0"},
		{orderItem: 4, name: "POLYGON - Dungeon Pack", token: "POLYGON_Dungeon", fileID: 12, version: "v1_0_0"},
	}
	serveStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/order_items/") {
			mu.Lock()
			fetched[path.Base(r.URL.Path)] = true
			mu.Unlock()
		}
		libraryStore(packs, nil)(w, r)
	}))
	e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n\n"+
		"[[pack]]\nslug = \"polygon-pirate-pack\"\nname = \"POLYGON - Pirate Pack\"\nenabled = true\n\n"+
		"[[pack]]\nslug = \"polygon-dungeon-pack\"\nname = \"POLYGON - Dungeon Pack\"\nenabled = true\n")

	var out bytes.Buffer
	prev := stdout
	stdout = &out
	defer func() { stdout = prev }()

	// status rather than sync: this is about which item pages the run reads, and a
	// download would need signed URLs the stub does not serve.
	if err := run(e.args("status", "-only", "polygon-pirate-pack")); err != nil {
		t.Fatalf("status --only: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !fetched["3"] {
		t.Errorf("the pack --only names was not read; fetched = %v", fetched)
	}
	if fetched["4"] {
		t.Errorf("--only did not reach the run: the pack it excludes was read too (fetched = %v)", fetched)
	}
}

// select is allowed a manifest that does not exist yet, since it creates one, but it
// only reaches the write after enumerating the library, serving the page and taking the
// whole selection. A directory that is not there fails inside atomicfile, naming a temp
// file that never existed, and the user has to tick every box again.
func TestSelectChecksItCanWriteTheManifestBeforeServingThePage(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "synty-sync.toml")
	absent := filepath.Join(dir, "newgame", "synty-sync.toml")

	// A manifest that is not there yet is still fine, which is the half that lets
	// select start a new project at all.
	if got, err := resolveManifestPath(present, "select"); err != nil || got != present {
		t.Errorf("resolveManifestPath(%q, select) = %q, %v; want the path and no error", present, got, err)
	}
	if _, err := resolveManifestPath(absent, "select"); err == nil {
		t.Errorf("select accepted %q, a path it cannot write", absent)
	}

	// And the refusal lands before the store is reached, rather than after the page has
	// taken a selection there is nowhere to put.
	reached := false
	serveStore(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		fmt.Fprint(w, `<html><body><input class="sky-pilot-search-input"></body></html>`)
	}))
	e := newRunEnv(t, "variant_includes = [\"Godot_*\"]\n")

	err := run(e.selectArgs("-manifest", absent))
	if err == nil {
		t.Fatal("select accepted a manifest path it cannot write")
	}
	if !strings.Contains(err.Error(), absent) {
		t.Errorf("err = %v, want it to name the path the user typed", err)
	}
	if reached {
		t.Error("select went to the store before checking it could write the result")
	}
}

// Help someone asked for is output, not a diagnostic: `synty-sync --help > help.txt`
// produced an empty file while the command exited 0. It also forced the flag-matrix
// test to swap os.Stderr for a pipe, which was the only process-global mutation in the
// suite and the reason those cases could never run in parallel. The three sites that
// print usage alongside an error keep stderr, where the error is.
func TestExplicitHelpGoesToStdout(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var out bytes.Buffer
		prev := stdout
		stdout = &out
		err := run(args)
		stdout = prev
		if err != nil {
			t.Errorf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "usage:") {
			t.Errorf("%v printed no help to stdout: %q", args, out.String())
		}
	}
}
