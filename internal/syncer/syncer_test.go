package syncer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/curbol/hexed-haven/tools/synty/internal/lockfile"
	"github.com/curbol/hexed-haven/tools/synty/internal/model"
	"github.com/curbol/hexed-haven/tools/synty/internal/portal"
)

// --- pure classify tests ---

func TestClassify(t *testing.T) {
	av := model.FileEntry{FileToken: "T", Variant: "Godot_4_5_1", Version: "v2", FileID: 9}
	cacheHit := func(string, string) bool { return true }
	cacheMiss := func(string, string) bool { return false }

	if c := classify(av, lockfile.File{}, false, cacheHit); c != New {
		t.Errorf("no prior => %v, want New", c)
	}
	if c := classify(av, lockfile.File{Tracked: false, Version: "v2"}, true, cacheHit); c != DownloadNow {
		t.Errorf("untracked prior => %v, want DownloadNow", c)
	}
	if c := classify(av, lockfile.File{Tracked: true, Version: "v1", CachePath: "p", SHA256: "s"}, true, cacheHit); c != Changed {
		t.Errorf("version differs => %v, want Changed", c)
	}
	if c := classify(av, lockfile.File{Tracked: true, Version: "v2", CachePath: "p", SHA256: "s"}, true, cacheMiss); c != CacheMissing {
		t.Errorf("cache gone => %v, want CacheMissing", c)
	}
	if c := classify(av, lockfile.File{Tracked: true, Version: "v2", CachePath: "p", SHA256: "s"}, true, cacheHit); c != Unchanged {
		t.Errorf("all match => %v, want Unchanged", c)
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

type serverOpts struct{ page1 string }

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
		http.Redirect(w, r, "/files/"+m[1]+".zip", http.StatusFound)
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment")
		fmt.Fprintf(w, "ZIP-%s", filepath.Base(r.URL.Path))
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
	}
}

func TestEndToEndSync(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "synty-library.lock.json")

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
	if err := os.WriteFile(flat, []byte("EXISTING-CONTENT"), 0o644); err != nil {
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
	if string(got) != "EXISTING-CONTENT" {
		t.Errorf("adopted content changed: %q (re-downloaded instead of adopted?)", got)
	}
	if _, err := os.Stat(flat); err == nil {
		t.Error("flat zip still at library root after adopt")
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
	_, err := Run(context.Background(), newClient(srv.URL), seed, lockPath, runOpts(lib, false))
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("want expired-session error, got %v", err)
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
