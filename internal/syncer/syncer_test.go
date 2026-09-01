package syncer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/lockfile"
	"github.com/curbol/synty-sync/internal/model"
	"github.com/curbol/synty-sync/internal/portal"
)

// --- pure classify tests ---

func TestClassify(t *testing.T) {
	av := model.FileEntry{FileToken: "T", Variant: "Godot_4_5_1", Version: "v2", FileID: 9}
	cacheHit := func(lockfile.File) bool { return true }
	cacheMiss := func(lockfile.File) bool { return false }
	tracked := func(version string) lockfile.File {
		return lockfile.File{Tracked: true, Version: version, CachePath: "p", SHA256: "s", SizeBytes: 1}
	}
	// A cache check that fails the test if consulted, for the branches that must
	// decide before ever touching the disk.
	neverCalled := func(lockfile.File) bool {
		t.Error("cacheOK consulted on a branch that should decide without it")
		return false
	}

	for _, tc := range []struct {
		name     string
		prior    lockfile.File
		hasPrior bool
		cacheOK  func(lockfile.File) bool
		want     Class
	}{
		{"no prior at all", lockfile.File{}, false, neverCalled, New},
		{"prior was filtered out", lockfile.File{Version: "v2"}, true, neverCalled, DownloadNow},
		{"untracked prior also outdated stays DownloadNow", lockfile.File{Version: "v1"}, true, neverCalled, DownloadNow},
		{"version bumped", tracked("v1"), true, neverCalled, Changed},
		{"same version, bytes gone", tracked("v2"), true, cacheMiss, CacheMissing},
		{"same version, no recorded path", lockfile.File{Tracked: true, Version: "v2"}, true, neverCalled, CacheMissing},
		{"everything matches", tracked("v2"), true, cacheHit, Unchanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(av, tc.prior, tc.hasPrior, tc.cacheOK); got != tc.want {
				t.Errorf("classify = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- hermetic end-to-end ---

var orderItemRe = regexp.MustCompile(`/order_items/(\d+)`)
var downloadRe = regexp.MustCompile(`/apps/downloads/downloads/(\d+)`)

// itemFixtureByOrderItem maps the synthetic library anchors to real scrubbed item
// pages: Pirate, Elven Warriors (Unity-only), Dungeon, Fantasy Kingdom.
var itemFixtureByOrderItem = map[string]string{
	"1": "item_1.html", // POLYGON - Pirate Pack
	"3": "item_3.html", // Elven Warriors - Sidekick (Unity only)
	"4": "item_4.html", // POLYGON - Dungeon Pack
	"6": "item_6.html", // POLYGON - Fantasy Kingdom Pack
}

const searchBox = `<input type='search' class='sky-pilot-search-input' placeholder='Search My Products'>`

const libraryPage1 = `<!doctype html><html><body>
<div class='sky-pilot rte'><h2>Your Library</h2>` + searchBox + `
<div class='sky-pilot-files-list'>
<a href='/apps/downloads/customers/1/orders/100/order_items/1' class='sky-pilot-list-item'>POLYGON - Pirate Pack</a>
<a href='/apps/downloads/customers/1/orders/100/order_items/4' class='sky-pilot-list-item'>POLYGON - Dungeon Pack</a>
<a href='/apps/downloads/customers/1/orders/100/order_items/6' class='sky-pilot-list-item'>POLYGON - Fantasy Kingdom Pack</a>
<a href='/apps/downloads/customers/1/orders/100/order_items/3' class='sky-pilot-list-item'>Elven Warriors - Sidekick Modular Characters</a>
</div></div></body></html>`

// Empty overflow page: search box present, no heading, zero rows (matches the
// live store past the last page).
const emptyAuthPage = `<!doctype html><html><body><div class='sky-pilot'>` + searchBox +
	`<div class='sky-pilot-files-list'></div></div></body></html>`

const logoutShell = `<!doctype html><html><body><h1>Login</h1><form action='/account/login'></form></body></html>`

// serverOpts tunes the fixture store. itemHTML replaces the fixture served for a
// given order_item id, so a test can bump a version or break one pack's markup
// without hand-rolling a second mux. downloadName overrides the served filename for
// a fileId, so a test can use Synty's real <token>_<variant>_<version>.zip shape
// (which is what the cache matches on) instead of the default fileId-based name.
type serverOpts struct {
	page1        string
	itemHTML     func(orderItem string) (string, bool)
	downloadName func(fileID string) (string, bool)
	// fileBody replaces the archive bytes served for a filename, so a test can put a
	// login page or a truncated body where package bytes belong.
	fileBody func(filename string) (body []byte, contentType string, ok bool)
	// downloadStatus replaces the redirect for one fileId with a bare status, so a
	// test can pull a single file out from under the run.
	downloadStatus func(fileID string) (int, bool)
}

// packageBytes is a body that sniffs as an archive rather than a document, the way
// real package bytes do.
func packageBytes(name string) []byte {
	return append([]byte("PK\x03\x04"), []byte(name)...)
}

func newServer(t *testing.T, opts serverOpts) *httptest.Server {
	t.Helper()
	page1 := opts.page1
	if page1 == "" {
		page1 = libraryPage1
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/downloads/orders/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("line_items_page") == "1" {
			fmt.Fprint(w, page1)
			return
		}
		fmt.Fprint(w, emptyAuthPage) // terminator
	})
	mux.HandleFunc("/apps/downloads/customers/", func(w http.ResponseWriter, r *http.Request) {
		m := orderItemRe.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		if opts.itemHTML != nil {
			if html, ok := opts.itemHTML(m[1]); ok {
				fmt.Fprint(w, html)
				return
			}
		}
		name, ok := itemFixtureByOrderItem[m[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "portal", name))
		if err != nil {
			t.Errorf("read fixture %s: %v", name, err)
		}
		w.Write(b)
	})
	mux.HandleFunc("/apps/downloads/downloads/", func(w http.ResponseWriter, r *http.Request) {
		m := downloadRe.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		name := m[1] + ".zip"
		if opts.downloadStatus != nil {
			if code, ok := opts.downloadStatus(m[1]); ok {
				w.WriteHeader(code)
				return
			}
		}
		if opts.downloadName != nil {
			if n, ok := opts.downloadName(m[1]); ok {
				name = n
			}
		}
		http.Redirect(w, r, "/files/"+name, http.StatusFound)
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		w.Header().Set("Content-Disposition", "attachment")
		if opts.fileBody != nil {
			if body, ct, ok := opts.fileBody(name); ok {
				w.Header().Set("Content-Type", ct)
				w.Write(body)
				return
			}
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(packageBytes(name))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func newClient(base string) *portal.Client {
	return &portal.Client{HTTP: http.DefaultClient, BaseURL: base, CustomerID: "1", Cookie: "x=y"}
}

func godotSourceFilter(v model.Variant) bool {
	return strings.HasPrefix(string(v), "Godot_") || v == "SourceFiles"
}

func runOpts(lib string, dry bool) Options {
	return Options{
		LibraryRoot: lib, Filter: godotSourceFilter, DryRun: dry,
		FullVerify: !dry, Concurrency: 4, Now: "2026-06-17T00:00:00Z",
		PackSelected: func(string) bool { return true },
	}
}

func TestEndToEndSync(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "synty-sync.lock.json")

	// First sync: everything new, bundled file deduped, Elven Warriors warns.
	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Bundled GENERIC_Particle_FX (fileId 2344711) appears under 3 packs but downloads once.
	bundledDownloads := 0
	for _, d := range rep.Downloaded {
		if d.FileID == 2344711 {
			bundledDownloads++
		}
	}
	if bundledDownloads != 1 {
		t.Errorf("bundled file downloaded %d times, want 1 (dedup)", bundledDownloads)
	}
	for _, d := range rep.Diffs {
		if d.Class != New {
			t.Errorf("first run: %s/%s class=%v, want New", d.PackSlug, d.Key, d.Class)
		}
	}
	if len(warnContaining(rep.Warnings, "Elven Warriors")) == 0 {
		t.Errorf("expected no-download warning for Elven Warriors, got %v", rep.Warnings)
	}

	// Lockfile written; bundled file shares one cachePath across packs.
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, slug := range []string{"polygon-pirate-pack", "polygon-dungeon-pack", "polygon-fantasy-kingdom-pack"} {
		f, ok := lf.Packs[slug].Files["GENERIC_Particle_FX|Godot_4_5_1"]
		if !ok || !f.Tracked || f.CachePath == "" {
			t.Fatalf("%s missing tracked bundled entry: %+v", slug, f)
		}
		paths[f.CachePath] = true
	}
	if len(paths) != 1 {
		t.Errorf("bundled file has %d distinct cachePaths, want 1 shared: %v", len(paths), paths)
	}

	// Second run as status (dry): all Unchanged, nothing downloaded, lockfile intact.
	before, _ := os.ReadFile(lockPath)
	rep2, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, true))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, d := range rep2.Diffs {
		if d.Class != Unchanged {
			t.Errorf("status: %s/%s class=%v, want Unchanged", d.PackSlug, d.Key, d.Class)
		}
	}
	if len(rep2.Downloaded) != 0 {
		t.Errorf("status downloaded %d files, want 0", len(rep2.Downloaded))
	}
	after, _ := os.ReadFile(lockPath)
	if string(before) != string(after) {
		t.Error("status (dry-run) modified the lockfile")
	}
}

func TestCacheMissingRedownloads(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatal(err)
	}
	lf, _ := lockfile.Load(lockPath)
	// Delete one cached file from the expendable cache.
	gone := lf.Packs["polygon-pirate-pack"].Files["POLYGON_Pirate|Godot_4_5_1"].CachePath
	if err := os.Remove(filepath.Join(lib, filepath.FromSlash(gone))); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range rep.Diffs {
		if d.Key == "POLYGON_Pirate|Godot_4_5_1" {
			found = true
			if d.Class != CacheMissing {
				t.Errorf("class = %v, want CacheMissing", d.Class)
			}
		}
	}
	if !found {
		t.Error("missing diff for deleted file")
	}
	if _, err := os.Stat(filepath.Join(lib, filepath.FromSlash(gone))); err != nil {
		t.Errorf("file not re-downloaded: %v", err)
	}
}

func TestMigrateAdoptsExistingFlatZip(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	// Pre-place a Synty-named flat zip matching POLYGON_Pirate Godot_4_5_1 v1_0_1.
	flat := filepath.Join(lib, "POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip")
	if err := os.WriteFile(flat, packageBytes("EXISTING-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatal(err)
	}
	adopted := false
	for _, d := range rep.Adopted {
		if d.FileID == 2282645 {
			adopted = true
		}
	}
	if !adopted {
		t.Errorf("fileId 2282645 not adopted: %+v", rep.Adopted)
	}
	for _, d := range rep.Downloaded {
		if d.FileID == 2282645 {
			t.Error("2282645 was downloaded; should have been adopted from the flat zip")
		}
	}
	lf, _ := lockfile.Load(lockPath)
	f := lf.Packs["polygon-pirate-pack"].Files["POLYGON_Pirate|Godot_4_5_1"]
	if !f.Tracked || f.CachePath == "" {
		t.Fatalf("adopted entry not tracked: %+v", f)
	}
	got, _ := os.ReadFile(filepath.Join(lib, filepath.FromSlash(f.CachePath)))
	if string(got) != string(packageBytes("EXISTING-CONTENT")) {
		t.Errorf("adopted content changed: %q (re-downloaded instead of adopted?)", got)
	}
	if _, err := os.Stat(flat); err == nil {
		t.Error("flat zip still at library root after adopt")
	}
}

func TestAdoptsExistingLayoutFileWithoutLockfile(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	// A zip already sitting in the <fileToken>/ layout that no lockfile records — the
	// state after a lost/degraded lockfile against a populated cache.
	dir := filepath.Join(lib, "POLYGON_Pirate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	layoutZip := filepath.Join(dir, "POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip")
	if err := os.WriteFile(layoutZip, packageBytes("LAYOUT-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatal(err)
	}

	// fileId 2282645 (POLYGON_Pirate Godot_4_5_1 v1_0_1) is adopted from the layout, not re-downloaded.
	for _, d := range rep.Downloaded {
		if d.FileID == 2282645 {
			t.Error("layout file 2282645 was re-downloaded; want adopted")
		}
	}
	adopted := false
	for _, d := range rep.Adopted {
		if d.FileID == 2282645 {
			adopted = true
		}
	}
	if !adopted {
		t.Errorf("fileId 2282645 not adopted from the layout: %+v", rep.Adopted)
	}
	// Adopted, not overwritten by a download.
	lf, _ := lockfile.Load(lockPath)
	f := lf.Packs["polygon-pirate-pack"].Files["POLYGON_Pirate|Godot_4_5_1"]
	if !f.Tracked || f.CachePath == "" {
		t.Fatalf("adopted layout file not tracked: %+v", f)
	}
	got, _ := os.ReadFile(filepath.Join(lib, filepath.FromSlash(f.CachePath)))
	if string(got) != string(packageBytes("LAYOUT-CONTENT")) {
		t.Errorf("adopted content changed to %q (re-downloaded instead of adopted?)", got)
	}
}

func TestPackSelectedLimitsToAllowlist(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	opts := runOpts(lib, true) // dry: classify only
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rep.Diffs {
		if d.PackSlug != "polygon-pirate-pack" {
			t.Errorf("diff for non-selected pack %q: %+v", d.PackSlug, d)
		}
	}
	// Only the selected pack appears in the rebuilt lockfile.
	if _, ok := rep.NewLockfile.Packs["polygon-dungeon-pack"]; ok {
		t.Error("excluded pack present in lockfile")
	}
	if _, ok := rep.NewLockfile.Packs["polygon-pirate-pack"]; !ok {
		t.Error("selected pack missing from lockfile")
	}
}

func TestDisabledPackPreservedInLockfile(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	// First sync with every pack enabled populates the lockfile.
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatal(err)
	}
	lf, _ := lockfile.Load(lockPath)
	if _, ok := lf.Packs["polygon-dungeon-pack"]; !ok {
		t.Fatalf("setup: dungeon pack should be present after a full sync")
	}

	// Disable the dungeon pack, then re-sync: its record (and downloaded files) must
	// survive rather than being silently erased from the committed lockfile.
	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool { return slug != "polygon-dungeon-pack" }
	if _, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, opts); err != nil {
		t.Fatal(err)
	}
	after, _ := lockfile.Load(lockPath)
	if _, ok := after.Packs["polygon-dungeon-pack"]; !ok {
		t.Error("disabled pack was dropped from the lockfile")
	}
	if _, ok := after.Packs["polygon-pirate-pack"]; !ok {
		t.Error("enabled pack missing from the lockfile")
	}
}

func TestBuildLockfileRepointsCarriedBundledFile(t *testing.T) {
	// A file (id 42) shared by an in-scope and an out-of-scope pack, both pointing at
	// one old path. This run re-downloads it (version bump) under the in-scope pack to
	// a new path; the carried-forward out-of-scope pack must be repointed so the two
	// owning packs never diverge on cachePath.
	shared := lockfile.File{FileToken: "T", Variant: "V", FileID: 42, Version: "v1", Tracked: true, CachePath: "T/old.zip", SHA256: "oldsha"}
	prev := lockfile.Lockfile{Packs: map[string]lockfile.Pack{
		"in":  {Files: map[string]lockfile.File{"T|V": shared}},
		"out": {Files: map[string]lockfile.File{"T|V": shared}},
	}}
	packFiles := []packWithFiles{{
		pack:  model.Pack{Slug: "in"},
		files: []model.FileEntry{{FileToken: "T", Variant: "V", FileID: 42, Version: "v2"}},
	}}
	resolvedByID := map[int]resolved{42: {cachePath: "T/new.zip", sha: "newsha", size: 10, version: "v2", now: true}}
	opts := Options{Filter: func(model.Variant) bool { return true }, Now: "now"}
	report := Report{NewLockfile: lockfile.Lockfile{Packs: map[string]lockfile.Pack{}}}

	buildLockfile(&report, packFiles, opts, resolvedByID, nil, prev)

	got := report.NewLockfile.Packs["out"].Files["T|V"]
	if got.CachePath != "T/new.zip" || got.SHA256 != "newsha" {
		t.Errorf("carried out-of-scope pack not repointed to the new path: %+v", got)
	}
	// The version travels with the bytes: leaving v1 here would name one version
	// against another version's sha, and split the two owning packs' records.
	if got.Version != "v2" {
		t.Errorf("carried entry version = %q, want v2 to match the bytes it now points at", got.Version)
	}
}

func TestDownloadFailsFastOnPermanent4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := model.FileEntry{FileToken: "T", Variant: "Godot_4_5_1", DownloadHref: "/dl"}
	opts := Options{LibraryRoot: t.TempDir(), Attempts: 3, Backoff: time.Millisecond}
	if _, err := downloadWithRetry(context.Background(), newClient(srv.URL), opts, f); err == nil {
		t.Fatal("expected an error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("permanent 4xx retried: %d attempts, want 1 (fail fast)", n)
	}
}

func TestDownloadRetriesForbidden(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden) // an expired signature; a fresh resolve may fix it
	}))
	defer srv.Close()

	f := model.FileEntry{FileToken: "T", Variant: "Godot_4_5_1", DownloadHref: "/dl"}
	opts := Options{LibraryRoot: t.TempDir(), Attempts: 3, Backoff: time.Millisecond}
	if _, err := downloadWithRetry(context.Background(), newClient(srv.URL), opts, f); err == nil {
		t.Fatal("expected an error after retries")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("403 should keep retrying: %d attempts, want 3", n)
	}
}

func TestOnlyGlobPreservesOtherLockfilePacks(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	// Full sync populates the lockfile with several packs.
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatal(err)
	}
	lf, _ := lockfile.Load(lockPath)
	if _, ok := lf.Packs["polygon-dungeon-pack"]; !ok {
		t.Fatalf("setup: dungeon pack should be in the lockfile after a full sync")
	}

	// A scoped sync (--only pirate) touches only pirate; it must not drop the
	// out-of-scope packs from the lockfile.
	opts := runOpts(lib, false)
	opts.OnlyGlob = "polygon-pirate-pack"
	if _, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, opts); err != nil {
		t.Fatal(err)
	}
	after, _ := lockfile.Load(lockPath)
	if _, ok := after.Packs["polygon-pirate-pack"]; !ok {
		t.Error("scoped pack missing after --only sync")
	}
	for _, slug := range []string{"polygon-dungeon-pack", "polygon-fantasy-kingdom-pack"} {
		if _, ok := after.Packs[slug]; !ok {
			t.Errorf("--only sync dropped out-of-scope pack %q from the lockfile", slug)
		}
	}
}

func TestProgressReported(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	var msgs []string
	opts := runOpts(lib, false)
	opts.Progress = func(m string) { msgs = append(msgs, m) }
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, opts); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "pack") {
		t.Errorf("no enumerate progress reported: %v", msgs)
	}
	if !strings.Contains(joined, "download") {
		t.Errorf("no per-file download progress reported: %v", msgs)
	}
}

func TestExpiredSessionAborts(t *testing.T) {
	srv := newServer(t, serverOpts{page1: logoutShell})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	// Pre-existing lockfile must not be clobbered.
	seed := lockfile.Lockfile{GeneratedAt: "old", Packs: map[string]lockfile.Pack{"keep": {DisplayName: "Keep"}}}
	if err := lockfile.Save(lockPath, seed); err != nil {
		t.Fatal(err)
	}
	// errors.Is, not a substring: main.go and any future caller distinguish an expired
	// session from other failures by identity, so a %v somewhere up the chain must fail
	// here rather than slip through on the word "session".
	_, err := Run(context.Background(), newClient(srv.URL), seed, lockPath, runOpts(lib, false))
	if !errors.Is(err, portal.ErrExpiredSession) {
		t.Fatalf("want ErrExpiredSession, got %v", err)
	}
	lf, _ := lockfile.Load(lockPath)
	if _, ok := lf.Packs["keep"]; !ok {
		t.Error("lockfile was clobbered on expired session")
	}
}

func TestEmptyLibraryTerminates(t *testing.T) {
	srv := newServer(t, serverOpts{page1: emptyAuthPage})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("empty library should terminate cleanly, got %v", err)
	}
	if len(rep.Diffs) != 0 || len(rep.NewLockfile.Packs) != 0 {
		t.Errorf("empty library produced packs: %+v", rep.NewLockfile.Packs)
	}
}

func warnContaining(warnings []string, sub string) []string {
	var out []string
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			out = append(out, w)
		}
	}
	return out
}
