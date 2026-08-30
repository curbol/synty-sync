package syncer

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/lockfile"
	"github.com/curbol/synty-sync/internal/model"
	"github.com/curbol/synty-sync/internal/portal"
)

// A rate limit clears by waiting, unlike the other 4xx statuses, so a download must
// keep its remaining attempts.
func TestRateLimitIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		status    int
		permanent bool
	}{
		{http.StatusNotFound, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, false},       // expired signature; a fresh Resolve re-signs
		{http.StatusTooManyRequests, false}, // rate limit; backing off clears it
		{http.StatusInternalServerError, false},
	} {
		err := &portal.StatusError{Status: tc.status, Op: "download T|Godot"}
		if got := permanentDownloadFailure(err); got != tc.permanent {
			t.Errorf("status %d: permanent = %v, want %v", tc.status, got, tc.permanent)
		}
	}
}

// A stray flat zip at the library root must not displace a file the lockfile already
// tracks: adoption bypasses classify, so it would swap in unverified content and
// orphan the verified copy without any hash comparison.
func TestStrayFlatZipLeavesTrackedFileAlone(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	var key string
	var before lockfile.File
	for k, f := range lf.Packs["polygon-pirate-pack"].Files {
		if f.Tracked {
			key, before = k, f
			break
		}
	}
	if key == "" {
		t.Fatal("seed produced no tracked pirate file")
	}

	flat := fmt.Sprintf("%s_%s_%s.zip", before.FileToken, before.Variant, before.Version)
	if err := os.WriteFile(filepath.Join(lib, flat), packageBytes("STRAY"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	got := after.Packs["polygon-pirate-pack"].Files[key]
	if got.SHA256 != before.SHA256 || got.CachePath != before.CachePath {
		t.Errorf("tracked file repointed to the stray zip: %s/%s -> %s/%s",
			before.CachePath, before.SHA256, got.CachePath, got.SHA256)
	}
	if !cacheFileExists(lib, before.CachePath) {
		t.Errorf("verified copy at %s was displaced", before.CachePath)
	}
}

// An item page that stops parsing must abort the run: rebuilding the pack from an
// empty file list would erase every tracked entry it holds.
func TestUnparseableItemPageKeepsLockfile(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	broken := false
	srv := newServer(t, serverOpts{itemHTML: func(orderItem string) (string, bool) {
		if !broken || orderItem != "1" {
			return "", false
		}
		return `<div class='sky-pilot rte'><h2>Pirate</h2>
			<div class='renamed-item'>POLYGON_Pirate_Pack<br><span>SourceFiles | v3</span></div></div>`, true
	}})

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	seeded, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	want := len(seeded.Packs["polygon-pirate-pack"].Files)
	if want == 0 {
		t.Fatal("seed produced no pirate files")
	}

	broken = true
	if _, err := Run(context.Background(), newClient(srv.URL), seeded, lockPath, runOpts(lib, false)); err == nil {
		t.Fatal("a pack whose item page no longer parses must abort the run")
	}
	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(after.Packs["polygon-pirate-pack"].Files); got != want {
		t.Errorf("lockfile lost entries on a parse failure: %d -> %d", want, got)
	}
}

// Leaving PackSelected unset must not fall back to "every owned pack": selection is
// opt-in, so a caller that forgets it has to hear about it.
func TestPackSelectedIsRequired(t *testing.T) {
	srv := newServer(t, serverOpts{})
	opts := runOpts(t.TempDir(), true)
	opts.PackSelected = nil
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), "", opts); err == nil {
		t.Error("a nil PackSelected must be rejected, not treated as select-all")
	}
}

func cacheFileExists(libraryRoot, relPath string) bool {
	_, err := os.Stat(filepath.Join(libraryRoot, filepath.FromSlash(relPath)))
	return err == nil
}

// itemPage renders a one-file item page for a pack, so a test can control the
// version the store reports.
func itemPage(token, variant, version string, fileID int) string {
	return fmt.Sprintf(`<div class='sky-pilot-list-item'>
	  <div class='sky-pilot-file-heading'>%s_%s | %s <span class='sky-pilot-file-size'>(40 MB)</span></div>
	  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/%d?x=1'>Download</a></div>
	</div>`, token, variant, version, fileID)
}

// A file bundled under an in-scope and an out-of-scope pack must not end up
// recorded at two different versions. The carried entry is repointed at the new
// bytes, so keeping its old version attaches one version's number to another
// version's sha, and the next run's fileId lookup then picks between them by map
// order — drawing the stale one re-downloads an up-to-date multi-GB file.
func TestChangedBundledFileKeepsOwningPacksInAgreement(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	version := "v1_0_0"
	items := func(orderItem string) (string, bool) {
		switch orderItem {
		case "1", "4": // Pirate and Dungeon both bundle fileId 999
			return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", version, 999), true
		}
		return "", false
	}
	srv := newServer(t, serverOpts{itemHTML: items})

	all := runOpts(lib, false)
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, all); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	// Bump the version and re-run with Dungeon out of scope, so its entry is carried
	// forward while Pirate re-downloads the shared file.
	version = "v2_0_0"
	only := runOpts(lib, false)
	only.PackSelected = func(slug string) bool { return slug != "polygon-dungeon-pack" }
	if _, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, only); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	in := after.Packs["polygon-pirate-pack"].Files[key]
	out := after.Packs["polygon-dungeon-pack"].Files[key]
	if in.Version != out.Version {
		t.Errorf("same fileId recorded at two versions: in-scope %q vs carried %q", in.Version, out.Version)
	}
	if in.SHA256 != out.SHA256 || in.CachePath != out.CachePath {
		t.Errorf("owning packs diverged: %+v vs %+v", in, out)
	}
	if in.Version != "v2_0_0" {
		t.Errorf("version = %q, want the freshly downloaded v2_0_0", in.Version)
	}
}

// A pack whose item page yields no files at all cannot be rebuilt from live data
// without erasing whatever it held. Every row here parses (they carry a version and
// a download id) but every variant is unrecognized, so the parser is right not to
// error and the syncer has to be the one that refuses.
func TestPackParsingToZeroFilesAbortsRun(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	empty := false
	items := func(orderItem string) (string, bool) {
		if empty && orderItem == "1" {
			return itemPage("POLYGON_Pirate", "Ureal_5_3", "v1_0_0", 555), true
		}
		return "", false
	}
	srv := newServer(t, serverOpts{itemHTML: items})

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	seeded, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	want := len(seeded.Packs["polygon-pirate-pack"].Files)
	if want == 0 {
		t.Fatal("seed produced no pirate files")
	}

	empty = true
	if _, err := Run(context.Background(), newClient(srv.URL), seeded, lockPath, runOpts(lib, false)); err == nil {
		t.Fatal("a pack that parses to zero files must abort the run")
	}
	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(after.Packs["polygon-pirate-pack"].Files); got != want {
		t.Errorf("lockfile lost entries: %d -> %d", want, got)
	}
}

// One bad pack ends the run, and every pack is launched at once with only the
// semaphore staggering them, so the queue behind the failure would otherwise fetch a
// whole library's item pages for a run that is already going to abort.
func TestFailedPackStopsTheRemainingFetches(t *testing.T) {
	var fetches int32
	srv := newServer(t, serverOpts{itemHTML: func(string) (string, bool) {
		atomic.AddInt32(&fetches, 1)
		return `<html><body><div class='sky-pilot'>no rows</div></body></html>`, true
	}})
	opts := runOpts(t.TempDir(), true)
	opts.Concurrency = 1

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), "", opts); err == nil {
		t.Fatal("an item page with no file rows must abort the run")
	}
	// The library page lists four packs; only the one that failed should have been
	// fetched before the rest were abandoned.
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Errorf("fetched %d item pages, want 1: the queue kept going after the failure", got)
	}
}

// A run the user interrupts skips the packs still queued, leaving zero entries in
// their slots. Those read downstream as packs that own no files, and rebuilding a
// lockfile from them erases every record they hold, so the cancellation has to
// surface as an error rather than as a short result.
func TestCancelledFetchIsAnErrorNotAnEmptyLibrary(t *testing.T) {
	srv := newServer(t, serverOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	packs := []model.Pack{{Slug: "polygon-pirate-pack", ItemURL: "/apps/downloads/customers/1/orders/100/order_items/1"}}
	out, err := fetchAll(ctx, newClient(srv.URL), packs, 1)
	if err == nil {
		t.Fatalf("cancelled fetch returned %+v with no error; the caller would rebuild these packs as empty", out)
	}
}

// A malformed --only glob matches nothing, which is indistinguishable from "no
// packs selected" unless the bad pattern is reported.
func TestBadOnlyGlobIsRejected(t *testing.T) {
	srv := newServer(t, serverOpts{})
	opts := runOpts(t.TempDir(), true)
	opts.OnlyGlob = "polygon-[pirate"
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), "", opts); err == nil {
		t.Error("a malformed --only glob must be reported, not silently match nothing")
	}
}

// Filter has no default (variant_includes is user-authored), so omitting it should
// say so rather than panic mid-enumeration.
func TestFilterIsRequired(t *testing.T) {
	srv := newServer(t, serverOpts{})
	opts := runOpts(t.TempDir(), true)
	opts.Filter = nil
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), "", opts); err == nil {
		t.Error("a nil Filter must be rejected, not panic")
	}
}

// A file already sitting in the layout is adopted rather than re-downloaded. The
// flat-zip path already skips only tracked priors; the layout path skipped on any
// prior, so an untracked record (a run that aborted before saving, or a widened
// variant filter) forced a needless re-download of bytes already on disk.
func TestAdoptsLayoutFileWithUntrackedPrior(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			if orderItem == "1" {
				return itemPage("POLYGON_Pirate", "Godot_4_5_1", "v1_0_0", 4242), true
			}
			return "", false
		},
		downloadName: func(fileID string) (string, bool) {
			return "POLYGON_Pirate_Godot_4_5_1_v1_0_0.zip", fileID == "4242"
		},
	})

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	const key = "POLYGON_Pirate|Godot_4_5_1"
	pirate := lf.Packs["polygon-pirate-pack"]
	seeded := pirate.Files[key]
	if !seeded.Tracked {
		t.Fatalf("seed did not track the file: %+v", seeded)
	}

	// Degrade the record to untracked, as an aborted run would leave it, keeping the
	// downloaded bytes on disk.
	seeded.Tracked = false
	seeded.CachePath = ""
	seeded.SHA256 = ""
	pirate.Files[key] = seeded
	lf.Packs["polygon-pirate-pack"] = pirate

	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	for _, d := range rep.Downloaded {
		if d.Key == key {
			t.Errorf("re-downloaded %s though the bytes were already in the layout", key)
		}
	}
	for _, d := range rep.Diffs {
		if d.Key == key && d.Class != Adopted {
			t.Errorf("%s class = %v, want Adopted", key, d.Class)
		}
	}
}

// Pruning the prior version is best-effort, but a failure must not vanish: the old
// file stays in the cache with nothing recording it.
func TestFailedPruneIsReported(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	version := "v1_0_0"
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			if orderItem == "1" {
				return itemPage("POLYGON_Pirate", "Godot_4_5_1", version, 4242), true
			}
			return "", false
		},
		// Synty embeds the version in the filename, so a bump lands at a new path and
		// the prior one has to be pruned.
		downloadName: func(fileID string) (string, bool) {
			return "POLYGON_Pirate_Godot_4_5_1_" + version + ".zip", fileID == "4242"
		},
	})

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	prior := lf.Packs["polygon-pirate-pack"].Files["POLYGON_Pirate|Godot_4_5_1"]

	// Replace the cached file with a non-empty directory so the prune cannot succeed.
	full := filepath.Join(lib, filepath.FromSlash(prior.CachePath))
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(full, "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}

	version = "v2_0_0"
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, prior.CachePath) {
			found = true
		}
	}
	if !found {
		t.Errorf("a failed prune of %s was not reported; warnings = %v", prior.CachePath, rep.Warnings)
	}
}

// cachedFiles lists every non-temp file under the library root, so a test can assert
// that a rejected body left nothing at all behind.
func cachedFiles(t *testing.T, libraryRoot string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(libraryRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(libraryRoot, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// An expired session and a CDN refusal both answer a download href with a document,
// often at 200. Those bytes must never occupy a cache path, and a login page's digest
// must never be recorded as a pack's verified content.
func TestDocumentBodyIsNeitherStoredNorRecorded(t *testing.T) {
	srv := newServer(t, serverOpts{fileBody: func(string) ([]byte, string, bool) {
		return []byte("<!doctype html><title>Log in</title>"), "text/html; charset=utf-8", true
	}})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("a rejected body must fail its file, not the run: %v", err)
	}
	if len(rep.Downloaded) != 0 {
		t.Errorf("reported %d downloads for a run that only ever received login pages", len(rep.Downloaded))
	}
	if len(rep.Failures) == 0 {
		t.Fatal("no failures reported for a run where every body was a document")
	}
	if !rep.Failed() {
		t.Error("Failed() is false, so the command would exit 0 on a session that is not working")
	}
	if left := cachedFiles(t, lib); len(left) != 0 {
		t.Errorf("a rejected body left files in the cache: %v", left)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	for slug, p := range lf.Packs {
		for key, f := range p.Files {
			if f.Tracked || f.SHA256 != "" || f.CachePath != "" {
				t.Errorf("%s/%s recorded a login page as content: %+v", slug, key, f)
			}
		}
	}
}

// One pulled file must not cost the mirror. It is reported and left untracked; every
// other file still downloads and the lockfile is still written.
func TestAPulledFileCostsItsFileNotTheRun(t *testing.T) {
	const pulled = "2344711" // the bundled GENERIC_Particle_FX
	srv := newServer(t, serverOpts{downloadStatus: func(fileID string) (int, bool) {
		if fileID == pulled {
			return http.StatusNotFound, true
		}
		return 0, false
	}})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("one pulled file aborted the whole run: %v", err)
	}
	if len(rep.Downloaded) == 0 {
		t.Error("nothing downloaded; the pulled file took the rest of the run with it")
	}
	if len(rep.Failures) != 1 || rep.Failures[0].FileID != 2344711 {
		t.Fatalf("failures = %+v, want exactly the pulled file", rep.Failures)
	}
	if !rep.Failures[0].Gone {
		t.Error("a 404 is not marked Gone, so the run exits non-zero on a file no re-run can fetch")
	}
	if rep.Failed() {
		t.Error("Failed() is true for a file the store no longer serves")
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(lf.Packs) == 0 {
		t.Fatal("the lockfile was not written, so every file downloaded this run is unrecorded")
	}
	f := lf.Packs["polygon-pirate-pack"].Files["GENERIC_Particle_FX|Godot_4_5_1"]
	if f.Tracked {
		t.Errorf("the pulled file is recorded as tracked: %+v", f)
	}
}

// A retryable failure is the user's to act on, so it must move the exit status.
func TestARetryableFailureMakesTheRunFail(t *testing.T) {
	srv := newServer(t, serverOpts{downloadStatus: func(fileID string) (int, bool) {
		if fileID == "2344711" {
			return http.StatusInternalServerError, true
		}
		return 0, false
	}})
	lib := t.TempDir()
	opts := runOpts(lib, false)
	opts.Attempts, opts.Backoff = 2, time.Millisecond

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), filepath.Join(t.TempDir(), "lock.json"), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Failures) != 1 || rep.Failures[0].Gone {
		t.Fatalf("failures = %+v, want one failure that is not Gone", rep.Failures)
	}
	if !rep.Failed() {
		t.Error("Failed() is false for a server error the next run might clear")
	}
}

// Presence alone is not integrity: a body that ended early leaves a file that exists
// at the recorded path and is not the pack.
func TestATruncatedCachedFileIsNotUnchanged(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatal(err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	victim := lf.Packs["polygon-pirate-pack"].Files["GENERIC_Particle_FX|Godot_4_5_1"]
	if !victim.Tracked || victim.CachePath == "" {
		t.Fatalf("no tracked file to truncate: %+v", victim)
	}
	full := filepath.Join(lib, filepath.FromSlash(victim.CachePath))
	if err := os.WriteFile(full, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}

	// status is the cheap path: it must still notice, without hashing the library.
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rep.Diffs {
		if d.FileID == victim.FileID {
			if d.Class != CacheMissing {
				t.Errorf("truncated file classified %v, want CacheMissing", d.Class)
			}
			return
		}
	}
	t.Error("the truncated file was not in the diff at all")
}

// An interrupted transfer leaves a temp beside its destination. Nothing else ever
// removes it, so a run that dies mid-download leaks its bytes for good.
func TestRunSweepsAbandonedTemps(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	dir := filepath.Join(lib, "POLYGON_Pirate_Pack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".synty-dl-abandoned")
	if err := os.WriteFile(stale, []byte("half a pack"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), filepath.Join(t.TempDir(), "lock.json"), runOpts(lib, false))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 1 {
		t.Errorf("Swept = %d, want 1", rep.Swept)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the abandoned temp survived the run")
	}
}

// A multi-gigabyte transfer that reports nothing between "download" and "done" looks
// like a hang. Progress has to come off the body as it streams.
func TestProgressReportsTransferredBytes(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	var lines []string
	opts := runOpts(lib, false)
	opts.Progress = func(m string) { lines = append(lines, m) }

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), filepath.Join(t.TempDir(), "lock.json"), opts); err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if strings.Contains(l, " B") || strings.Contains(l, "%") {
			return
		}
	}
	t.Errorf("no progress line reported bytes off the stream: %v", lines)
}

// A pack that leaves the library (refunded, delisted) is otherwise carried forward
// forever with nobody told.
func TestDeOwnedPackIsReported(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	prior := lockfile.New()
	prior.Packs["a-pack-i-no-longer-own"] = lockfile.Pack{
		DisplayName: "A Pack I No Longer Own",
		Files:       map[string]lockfile.File{"T|Godot_4_5_1": {FileToken: "T", Variant: "Godot_4_5_1", Version: "v1", FileID: 999}},
	}

	rep, err := Run(context.Background(), newClient(srv.URL), prior, filepath.Join(t.TempDir(), "lock.json"), runOpts(lib, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "a-pack-i-no-longer-own" {
		t.Errorf("Removed = %v, want the de-owned pack", rep.Removed)
	}
	if _, ok := rep.NewLockfile.Packs["a-pack-i-no-longer-own"]; !ok {
		t.Error("the de-owned pack was dropped from the lockfile; one enumeration is not enough to erase a record")
	}
}

// An enumeration that comes back empty while the lockfile holds packs is a broken
// read, not a library someone emptied. Acting on it rewrites the committed record.
func TestEmptyEnumerationWithAPopulatedLockfileIsAnError(t *testing.T) {
	srv := newServer(t, serverOpts{page1: emptyAuthPage})
	prior := lockfile.New()
	prior.Packs["polygon-pirate-pack"] = lockfile.Pack{DisplayName: "POLYGON - Pirate Pack"}

	_, err := Run(context.Background(), newClient(srv.URL), prior, filepath.Join(t.TempDir(), "lock.json"), runOpts(t.TempDir(), false))
	if err == nil {
		t.Fatal("an empty library against a populated lockfile was accepted as the truth")
	}
}

// Anyone who ran a build without the download guards has login pages sitting in their
// cache under the right filenames. Adoption is the one path into the lockfile that
// skips classify, so it has to check the bytes too.
func TestADocumentAlreadyInTheLayoutIsNotAdopted(t *testing.T) {
	srv := newServer(t, serverOpts{downloadName: func(fileID string) (string, bool) {
		if fileID == "2344711" {
			return "GENERIC_Particle_FX_Godot_4_5_1_v1_0_0.zip", true
		}
		return "", false
	}})
	lib := t.TempDir()
	dir := filepath.Join(lib, "GENERIC_Particle_FX")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "GENERIC_Particle_FX_Godot_4_5_1_v1_0_0.zip")
	if err := os.WriteFile(planted, []byte("<!doctype html><title>Log in</title>"), 0o644); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(t.TempDir(), "lock.json")
	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range rep.Adopted {
		if a.FileID == 2344711 {
			t.Fatal("a login page sitting at a cache path was adopted as the pack's content")
		}
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	f := lf.Packs["polygon-pirate-pack"].Files["GENERIC_Particle_FX|Godot_4_5_1"]
	if !f.Tracked {
		t.Fatalf("the file was neither adopted nor downloaded: %+v", f)
	}
	got, err := os.ReadFile(filepath.Join(lib, filepath.FromSlash(f.CachePath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "Log in") {
		t.Error("the lockfile points at the planted login page")
	}
}

// A file the last run verified must survive a failed update. Rebuilding the entry
// from scratch drops its path and sha while the bytes stay on disk, so nothing
// records them: the layout adopt scan looks for the new version's name and never
// matches, and the next run classifies DownloadNow rather than Changed, so the
// Changed-only prune never fires either.
func TestFailedUpdateKeepsTheVerifiedCopyRecorded(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	version := "v1_0_0"
	broken := false
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			if orderItem == "1" {
				return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", version, 999), true
			}
			return "", false
		},
		downloadStatus: func(fileID string) (int, bool) {
			if broken && fileID == "999" {
				return http.StatusInternalServerError, true
			}
			return 0, false
		},
	})

	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, opts); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	before := lf.Packs["polygon-pirate-pack"].Files[key]
	if !before.Tracked {
		t.Fatal("seed produced no tracked bundled file")
	}

	version, broken = "v2_0_0", true
	opts.Attempts, opts.Backoff = 1, 0
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, opts)
	if err != nil {
		t.Fatalf("a failed update aborted the run: %v", err)
	}
	if len(rep.Failures) != 1 {
		t.Fatalf("failures = %+v, want the one file whose update failed", rep.Failures)
	}

	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	got := after.Packs["polygon-pirate-pack"].Files[key]
	if !got.Tracked || got.CachePath != before.CachePath || got.SHA256 != before.SHA256 {
		t.Errorf("the verified copy at %s is no longer recorded: %+v", before.CachePath, got)
	}
	if got.Version != before.Version {
		t.Errorf("version = %q, want the version the recorded sha actually belongs to (%q)", got.Version, before.Version)
	}
	if !cacheFileExists(lib, before.CachePath) {
		t.Fatalf("the prior copy at %s is gone", before.CachePath)
	}
}

// The same failed update, with the file bundled under a pack this run left out of
// scope. The carried entry keeps its record, so dropping the in-scope one leaves two
// owning packs disagreeing about the same fileId.
func TestFailedUpdateKeepsOwningPacksInAgreement(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	version := "v1_0_0"
	broken := false
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			switch orderItem {
			case "1", "4": // Pirate and Dungeon both bundle fileId 999
				return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", version, 999), true
			}
			return "", false
		},
		downloadStatus: func(fileID string) (int, bool) {
			if broken && fileID == "999" {
				return http.StatusInternalServerError, true
			}
			return 0, false
		},
	})

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	version, broken = "v2_0_0", true
	only := runOpts(lib, false)
	only.Attempts, only.Backoff = 1, 0
	only.PackSelected = func(slug string) bool { return slug != "polygon-dungeon-pack" }
	if _, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, only); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	in := after.Packs["polygon-pirate-pack"].Files[key]
	out := after.Packs["polygon-dungeon-pack"].Files[key]
	if in.Tracked != out.Tracked || in.Version != out.Version || in.SHA256 != out.SHA256 || in.CachePath != out.CachePath {
		t.Errorf("owning packs diverged after a failed update:\n  in-scope %+v\n  carried  %+v", in, out)
	}
}
