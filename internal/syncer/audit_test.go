package syncer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

	lf := seedRun(t, srv, lockPath, runOpts(lib, false))

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

// seedPirateLockfile runs one sync against srv and returns the lockfile it wrote
// plus how many files the pirate pack ended up holding, which is what the two
// zero-files guards below check has not shrunk.
func seedPirateLockfile(t *testing.T, srv *httptest.Server, lib, lockPath string) (lockfile.Lockfile, int) {
	t.Helper()
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	seeded, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	n := len(seeded.Packs["polygon-pirate-pack"].Files)
	if n == 0 {
		t.Fatal("seed produced no pirate files")
	}
	return seeded, n
}

// A pack whose every row carries a variant this build does not know is a Synty
// engine we have not shipped support for, not broken markup — the parser skips such
// rows by design and hands back their labels. Failing the run would take the whole
// mirror down over one future engine; rebuilding the pack from the resulting empty
// list would erase every entry it holds. Neither is acceptable, so the pack is
// dropped from the run and its record carried forward whole.
func TestPackWithOnlyUnknownVariantsIsCarriedForwardNotFailed(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	unknownOnly := false
	items := func(orderItem string) (string, bool) {
		if unknownOnly && orderItem == "1" {
			return itemPage("POLYGON_Pirate", "Ureal_5_3", "v1_0_0", 555), true
		}
		return "", false
	}
	srv := newServer(t, serverOpts{itemHTML: items})
	seeded, want := seedPirateLockfile(t, srv, lib, lockPath)

	unknownOnly = true
	rep, err := Run(context.Background(), newClient(srv.URL), seeded, lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("a future engine must not abort the run: %v", err)
	}
	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(after.Packs["polygon-pirate-pack"].Files); got != want {
		t.Errorf("lockfile lost entries: %d -> %d", want, got)
	}
	// Carried forward means untouched, not rebuilt: the entries keep the paths and
	// shas the seed run recorded.
	if !reflect.DeepEqual(after.Packs["polygon-pirate-pack"], seeded.Packs["polygon-pirate-pack"]) {
		t.Errorf("the carried pack was rewritten:\n before %+v\n after  %+v",
			seeded.Packs["polygon-pirate-pack"], after.Packs["polygon-pirate-pack"])
	}
	// And it is said out loud, naming the label that was not recognized: silence here
	// is a pack that stops updating with nothing to explain why.
	var said bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "Ureal_5_3") && strings.Contains(w, "carried forward") {
			said = true
		}
	}
	if !said {
		t.Errorf("no warning named the unrecognized variant; warnings = %q", rep.Warnings)
	}
}

// The other half: an item page the parser cannot read at all yields neither files
// nor unrecognized labels, and that is markup that moved. There is nothing to carry
// a diagnosis from and no way to tell which entries are still real, so the run has
// to stop rather than rebuild the pack from nothing.
func TestPackParsingToNothingAtAllAbortsRun(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	broken := false
	items := func(orderItem string) (string, bool) {
		if broken && orderItem == "1" {
			// Rows present, none carrying a version label: the shape a renamed
			// file-heading class leaves behind.
			return `<div class='sky-pilot-list-item'><div class='sky-pilot-file-heading'>Icon</div></div>`, true
		}
		return "", false
	}
	srv := newServer(t, serverOpts{itemHTML: items})
	seeded, want := seedPirateLockfile(t, srv, lib, lockPath)

	broken = true
	if _, err := Run(context.Background(), newClient(srv.URL), seeded, lockPath, runOpts(lib, false)); err == nil {
		t.Fatal("an item page that parses to nothing must abort the run")
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
	out, _, err := fetchAll(ctx, newClient(srv.URL), packs, 1)
	if err == nil {
		t.Fatalf("cancelled fetch returned %+v with no error; the caller would rebuild these packs as empty", out)
	}
	// As a cancellation, not as a generic fetch failure: Run tells an interrupt apart
	// from a per-file verdict by this, and a wrap that loses it turns Ctrl-C into a
	// library's worth of files recorded as failed.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to carry context.Canceled", err)
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

	lf := seedRun(t, srv, lockPath, runOpts(lib, false))
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

	lf := seedRun(t, srv, lockPath, runOpts(lib, false))
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
	if rep.ActionableFailures() == 0 {
		t.Error("no actionable failures, so the command would exit 0 on a session that is not working")
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
	if rep.ActionableFailures() != 0 {
		t.Error("a file the store no longer serves counted as an actionable failure")
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
	if rep.ActionableFailures() == 0 {
		t.Error("no actionable failures for a server error the next run might clear")
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
	lf := seedRun(t, srv, lockPath, opts)
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

	lf := seedRun(t, srv, lockPath, runOpts(lib, false))

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

// The same divergence one class over. A missing cache file whose re-download fails
// leaves the in-scope owner untracked; without carrying that verdict, the pack this
// run left out of scope keeps a record naming a cachePath the run just found gone,
// so the committed lockfile holds one fileId both tracked and untracked at once.
func TestFailedCacheMissingKeepsOwningPacksInAgreement(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	broken := false
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			switch orderItem {
			case "1", "4": // Pirate and Dungeon both bundle fileId 999
				return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", "v1_0_0", 999), true
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

	lf := seedRun(t, srv, lockPath, runOpts(lib, false))
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	seed := lf.Packs["polygon-pirate-pack"].Files[key]
	if !seed.Tracked {
		t.Fatal("seed produced no tracked bundled file")
	}

	// The cached bytes go missing, and the version has not moved: CacheMissing, not
	// Changed, so there is no prior copy to fall back to.
	if err := os.Remove(filepath.Join(lib, filepath.FromSlash(seed.CachePath))); err != nil {
		t.Fatal(err)
	}
	broken = true
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
	in := after.Packs["polygon-pirate-pack"].Files[key]
	out := after.Packs["polygon-dungeon-pack"].Files[key]
	if in.Tracked != out.Tracked || in.CachePath != out.CachePath || in.SHA256 != out.SHA256 || in.SizeBytes != out.SizeBytes {
		t.Errorf("owning packs diverged after a failed cache-missing download:\n  in-scope %+v\n  carried  %+v", in, out)
	}
	if out.Tracked {
		t.Errorf("the carried entry still records bytes the run could not find: %+v", out)
	}
}

// A pack that leaves the library is reported and keeps its lockfile record. A single
// file leaving takes its record with it — the in-scope pack is rebuilt from the live
// page alone — and the bytes stay in the cache with nothing pointing at them. Losing
// that silently is how a renamed variant keyword costs a multi-gigabyte file its
// record between two green runs.
func TestADelistedFileIsReportedNotJustDropped(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	both := true
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			if orderItem != "1" {
				return "", false
			}
			page := itemPage("EXTRA_Thing", "Godot_4_5_1", "v1_0_0", 1000)
			if both {
				page = itemPage("POLYGON_Pirate", "Godot_4_5_1", "v1_0_0", 999) + page
			}
			return page, true
		},
	})
	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }
	lf := seedRun(t, srv, lockPath, opts)
	const key = "POLYGON_Pirate|Godot_4_5_1"
	seed := lf.Packs["polygon-pirate-pack"].Files[key]
	if !seed.Tracked {
		t.Fatal("seed produced no tracked file to lose")
	}

	both = false
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, opts)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := after.Packs["polygon-pirate-pack"].Files[key]; still {
		t.Fatalf("the delisted file is still recorded; this test no longer covers what it names")
	}
	var found string
	for _, w := range rep.Warnings {
		if strings.Contains(w, key) {
			found = w
		}
	}
	if found == "" {
		t.Fatalf("a tracked file left the library with no word about it; warnings = %q", rep.Warnings)
	}
	if !strings.Contains(found, seed.CachePath) {
		t.Errorf("the warning does not name the bytes left behind at %s: %q", seed.CachePath, found)
	}
	if !cacheFileExists(lib, seed.CachePath) {
		t.Errorf("the cached copy was removed; the run reports the orphan, it does not delete it")
	}
}

// A variant the store renames to another recognized keyword moves the file's key but
// not its fileId, so the bytes stay referenced. Reporting that as a loss would fire
// the warning on an ordinary version bump.
func TestARekeyedFileIsNotReportedAsOrphaned(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	variant := "Godot_4_5_1"
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			if orderItem != "1" {
				return "", false
			}
			return itemPage("POLYGON_Pirate", variant, "v1_0_0", 999), true
		},
	})
	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }
	lf := seedRun(t, srv, lockPath, opts)

	variant = "Godot_4_6_0"
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, opts)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "no longer listed") {
			t.Errorf("a rekeyed file reported as orphaned: %q", w)
		}
	}
}

// Nothing creates the library root before a run reaches it: on a fresh install the
// first sync used to enumerate the whole library, fetch every item page, and then die
// in the flat-file migration because the directory did not exist yet. status was
// unaffected (it skips the block), so the tool reported what it would download and
// then refused to.
func TestFirstSyncCreatesNothingAndStillRunsOnAMissingLibraryRoot(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := filepath.Join(t.TempDir(), "not-created-yet")
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("sync on a library root that does not exist yet: %v", err)
	}
	if len(rep.Downloaded) == 0 {
		t.Error("nothing downloaded on a fresh library root")
	}
	if _, err := lockfile.Load(lockPath); err != nil {
		t.Errorf("no lockfile written: %v", err)
	}
}

// A fileId bundled across packs must agree on every owner even when the carried
// owner's entry is untracked. An earlier run that failed to fetch the file leaves
// both owners untracked; if only one is then in scope and downloads it, requiring
// the carried entry to be already-tracked before repointing it leaves the committed
// lockfile holding one fileId as both tracked and untracked.
func TestResolvedBundledFileConvergesAnUntrackedCarriedOwner(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	prev := lockfile.Lockfile{Packs: map[string]lockfile.Pack{
		"polygon-dungeon-pack": {DisplayName: "POLYGON - Dungeon Pack", Files: map[string]lockfile.File{
			"GENERIC_Particle_FX|Godot_4_5_1": {
				FileToken: "GENERIC_Particle_FX", Variant: "Godot_4_5_1",
				Version: "v1_0_0", FileID: 999, Tracked: false,
			},
		}},
	}}
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			if orderItem != "1" {
				return "", false
			}
			return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", "v1_0_1", 999), true
		},
	})
	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }

	if _, err := Run(context.Background(), newClient(srv.URL), prev, lockPath, opts); err != nil {
		t.Fatalf("sync: %v", err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	in := lf.Packs["polygon-pirate-pack"].Files[key]
	out := lf.Packs["polygon-dungeon-pack"].Files[key]
	if !in.Tracked {
		t.Fatalf("in-scope owner not tracked: %+v", in)
	}
	if in.Tracked != out.Tracked || in.CachePath != out.CachePath || in.SHA256 != out.SHA256 || in.Version != out.Version {
		t.Errorf("owning packs disagree on fileId 999:\n  in-scope  %+v\n  carried   %+v", in, out)
	}
}

// The same convergence for the losing direction: when a run proves a fileId has no
// usable copy, every owner drops the record at the version the run was looking for.
// Clearing the path but keeping the carried owner's old version records one fileId at
// two versions in a single committed file.
func TestUnresolvedBundledFileDropsEveryOwnerAtOneVersion(t *testing.T) {
	lib := t.TempDir()
	rep := Report{NewLockfile: lockfile.Lockfile{Packs: map[string]lockfile.Pack{}}}
	prev := lockfile.Lockfile{Packs: map[string]lockfile.Pack{
		"polygon-dungeon-pack": {DisplayName: "POLYGON - Dungeon Pack", Files: map[string]lockfile.File{
			"GENERIC_Particle_FX|Godot_4_5_1": {
				FileToken: "GENERIC_Particle_FX", Variant: "Godot_4_5_1", Version: "v1_0_0",
				FileID: 999, Tracked: true, CachePath: "GENERIC_Particle_FX/old.zip", SHA256: "old",
			},
		}},
	}}
	pf := []packWithFiles{{
		pack: model.Pack{Slug: "polygon-pirate-pack", DisplayName: "POLYGON - Pirate Pack"},
		files: []model.FileEntry{{
			FileToken: "GENERIC_Particle_FX", Variant: "Godot_4_5_1", Version: "v1_0_1", FileID: 999,
		}},
	}}
	opts := runOpts(lib, false)
	buildLockfile(&rep, pf, opts, map[int]resolved{}, map[int]live{999: {version: "v1_0_1", variant: "Godot_4_5_1"}}, prev)

	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	in := rep.NewLockfile.Packs["polygon-pirate-pack"].Files[key]
	out := rep.NewLockfile.Packs["polygon-dungeon-pack"].Files[key]
	if in.Tracked || out.Tracked {
		t.Errorf("an unresolved file stayed tracked: in=%+v out=%+v", in, out)
	}
	if in.Version != out.Version {
		t.Errorf("one fileId dropped at two versions: in-scope %q, carried %q", in.Version, out.Version)
	}
}

// Adoption is the one path into the lockfile that never consults classify, and the
// head sniff cannot tell a whole archive from a copy that stopped part way — both
// begin with a zip's magic. Adopting a truncated one records its own short bytes as
// the file's truth, after which every Verify compares those bytes against themselves
// and finds them intact forever.
func TestTruncatedLayoutFileIsNotAdopted(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	dir := filepath.Join(lib, "POLYGON_Pirate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cut := truncatedPackageBytes("POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip")
	layout := filepath.Join(dir, "POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip")
	if err := os.WriteFile(layout, cut, 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	for _, d := range rep.Adopted {
		if d.FileID == 2282645 {
			t.Error("a truncated zip was adopted as the pack's content")
		}
	}
	if len(warnContaining(rep.Warnings, "end-of-central-directory")) == 0 {
		t.Errorf("truncation not reported: %v", rep.Warnings)
	}
	// It is re-downloaded instead, so the short bytes never become the record.
	lf, _ := lockfile.Load(lockPath)
	f := lf.Packs["polygon-pirate-pack"].Files["POLYGON_Pirate|Godot_4_5_1"]
	if !f.Tracked {
		t.Fatalf("file not recovered by download: %+v", f)
	}
	if f.SizeBytes == int64(len(cut)) {
		t.Error("the lockfile recorded the truncated size as the file's truth")
	}
}

// status must not touch the library: the sweep is housekeeping a real run does, and a
// dry run that removed a file would make "show me what would change" destructive.
func TestStatusDoesNotSweepTemps(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	dir := filepath.Join(lib, "POLYGON_Pirate")
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

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), filepath.Join(t.TempDir(), "lock.json"), runOpts(lib, true))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Swept != 0 {
		t.Errorf("status swept %d temps; it must remove nothing", rep.Swept)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("status deleted an abandoned temp: %v", err)
	}
}

// The two size fields mean different things and only one is an integrity figure.
// advertisedSize is the store's rounded label and refreshes every run; sizeBytes is
// the count that actually landed and is what cache.Verify compares against. Folding
// them together would let a rounded display figure decide whether a file is intact.
func TestAdvertisedSizeAndSizeBytesStaySeparate(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			if orderItem != "1" {
				return "", false
			}
			return itemPage("POLYGON_Pirate", "Godot_4_5_1", "v1_0_0", 999), true
		},
	})
	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, opts); err != nil {
		t.Fatal(err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	f := lf.Packs["polygon-pirate-pack"].Files["POLYGON_Pirate|Godot_4_5_1"]
	// itemPage labels every row "(40 MB)"; the served body is a few hundred bytes.
	if f.AdvertisedSize != 40<<20 {
		t.Errorf("advertisedSize = %d, want the label's 40 MB", f.AdvertisedSize)
	}
	onDisk, err := os.Stat(filepath.Join(lib, filepath.FromSlash(f.CachePath)))
	if err != nil {
		t.Fatal(err)
	}
	if f.SizeBytes != onDisk.Size() {
		t.Errorf("sizeBytes = %d, want the %d that landed on disk", f.SizeBytes, onDisk.Size())
	}
	if f.SizeBytes == f.AdvertisedSize {
		t.Error("sizeBytes took the rounded label instead of the byte count")
	}
}

// An interrupt is not a per-file verdict. Returning a report instead of the error
// would record every file the run had not reached yet as failed, for a reason that
// has nothing to do with any of them, and write that as the committed record.
func TestInterruptDuringDownloadsIsAnErrorNotAReport(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	ctx, cancel := context.WithCancel(context.Background())
	srv := newServer(t, serverOpts{
		fileBody: func(string) ([]byte, string, bool) {
			cancel() // the run is interrupted part way through its first transfer
			return packageBytes("x"), "application/zip", true
		},
	})

	rep, err := Run(ctx, newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err == nil {
		t.Fatalf("an interrupted run reported success: %d diffs", len(rep.Diffs))
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(rep.Failures) != 0 {
		t.Errorf("an interrupt was recorded as %d per-file failures", len(rep.Failures))
	}
	if _, err := os.Stat(lockPath); err == nil {
		t.Error("an interrupted run wrote the lockfile")
	}
}

// A file the store archives keeps its lockfile entry — the pack still owns it — so
// it never reaches orphanedRecords, but the entry is rebuilt untracked and its cache
// path and sha go with it. The bytes stay on disk, and nothing can take them back:
// an archived file is never selected, so it is never an adopt candidate, and the
// adopt scan keys on the version the page now reports. Saying nothing leaves the
// user a shrinking mirror and no account of where it went.
func TestArchivingAFileIsReportedNotSilentlyDropped(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	version := "v1_0_0"
	items := func(orderItem string) (string, bool) {
		if orderItem != "1" {
			return "", false
		}
		return itemPage("POLYGON_Pirate", "Godot_4_5_1", version, 4242), true
	}
	srv := newServer(t, serverOpts{itemHTML: items})
	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, opts); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	seeded, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	before := seeded.Packs["polygon-pirate-pack"].Files["POLYGON_Pirate|Godot_4_5_1"]
	if !before.Tracked || before.CachePath == "" {
		t.Fatalf("seed did not track the file: %+v", before)
	}

	version = "v1_0_0_ARCHIVED"
	rep, err := Run(context.Background(), newClient(srv.URL), seeded, lockPath, opts)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	var said bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, before.CachePath) && strings.Contains(w, "unreferenced") {
			said = true
		}
	}
	if !said {
		t.Errorf("the cached copy at %s lost its record with nothing said; warnings = %q", before.CachePath, rep.Warnings)
	}
	// Said once. The run after finds nothing tracked, so repeating it would nag about
	// the same file for the life of the library.
	again, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := Run(context.Background(), newClient(srv.URL), again, lockPath, opts)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	for _, w := range rep2.Warnings {
		if strings.Contains(w, "unreferenced") {
			t.Errorf("the archived file was reported a second time: %q", w)
		}
	}
}

// The second of the two not-a-package guards. portal refuses a document by
// Content-Type before a byte streams; this one catches the response that claims to
// be an archive and is not, which is what a CDN error page served as
// application/octet-stream looks like. Without it those bytes are hashed, committed,
// and recorded as the pack's verified content, after which every Verify compares
// them against themselves and finds them intact forever.
func TestABodyThatIsNotAPackageIsRefusedEvenWhenTheTypeSaysItIs(t *testing.T) {
	srv := newServer(t, serverOpts{fileBody: func(string) ([]byte, string, bool) {
		// A document wearing an archive's Content-Type, so portal.documentMediaType
		// waves it through and only the body sniff can catch it.
		return []byte("<!doctype html><title>Log in</title>"), "application/zip", true
	}})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("a rejected body must fail its file, not the run: %v", err)
	}
	if len(rep.Failures) == 0 {
		t.Fatal("no failures reported for a run where every body was a document")
	}
	for _, f := range rep.Failures {
		if !strings.Contains(f.Err, ErrNotAPackageBody.Error()) {
			t.Errorf("failure %q does not name the body sniff; the Content-Type guard cannot have caught this", f.Err)
		}
	}
	if len(rep.Downloaded) != 0 {
		t.Errorf("reported %d downloads for a run that only ever received login pages", len(rep.Downloaded))
	}
	// Nothing committed and no temp left behind: Store stops at the temp file and the
	// caller discards it, so a rejected body never occupies a real cache path even
	// briefly.
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

// A lockfile can hold one fileId under two packs at two versions (a hand merge, or a
// pack that left and came back). Which record wins decides between Unchanged and a
// multi-gigabyte refetch, and the data cannot say which is right — so the only thing
// that matters is that two runs over the same file agree. Both halves of the rule
// are load-bearing: a tracked record beats an untracked one, and slug order breaks
// the remaining tie instead of Go's map iteration.
func TestIndexByFileIDPicksTheSameRecordEveryTime(t *testing.T) {
	file := func(version, path string, tracked bool) lockfile.File {
		return lockfile.File{
			FileToken: "TOK", Variant: "Godot_4_5_1", Version: version, FileID: 7,
			Tracked: tracked, CachePath: path, SHA256: version,
		}
	}
	pack := func(f lockfile.File) lockfile.Pack {
		return lockfile.Pack{Files: map[string]lockfile.File{"TOK|Godot_4_5_1": f}}
	}

	for _, tc := range []struct {
		name  string
		packs map[string]lockfile.Pack
		want  string
	}{
		{
			// Tracked wins wherever it sits, so a record naming real bytes is never
			// passed over for one that names none.
			name:  "tracked beats untracked under a lexically earlier slug",
			packs: map[string]lockfile.Pack{"aaa": pack(file("v1", "", false)), "zzz": pack(file("v2", "p2", true))},
			want:  "v2",
		},
		{
			name:  "tracked beats untracked under a lexically later slug",
			packs: map[string]lockfile.Pack{"aaa": pack(file("v2", "p2", true)), "zzz": pack(file("v1", "", false))},
			want:  "v2",
		},
		{
			// Both tracked: the first slug in sort order wins, whichever order the
			// map hands them over in.
			name:  "two tracked records break the tie on slug order",
			packs: map[string]lockfile.Pack{"aaa": pack(file("v1", "p1", true)), "zzz": pack(file("v2", "p2", true))},
			want:  "v1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Repeated, because a map-order dependency passes most of the time.
			for i := 0; i < 20; i++ {
				got := indexByFileID(lockfile.Lockfile{Packs: tc.packs})[7]
				if got.Version != tc.want {
					t.Fatalf("picked %q, want %q (run %d)", got.Version, tc.want, i)
				}
			}
		})
	}
}

// status must not move a user's files. The adopt scan itself is read-only and runs
// for status on purpose, but the flat-file migration ahead of it renames, so it is
// gated on DryRun — a guard whose absence would make "show me what would change"
// change something.
func TestStatusDoesNotMigrateFlatFiles(t *testing.T) {
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			if orderItem != "1" {
				return "", false
			}
			return itemPage("POLYGON_Pirate", "Godot_4_5_1", "v1_0_0", 4242), true
		},
	})
	lib := t.TempDir()
	flat := filepath.Join(lib, "POLYGON_Pirate_Godot_4_5_1_v1_0_0.zip")
	if err := os.WriteFile(flat, packageBytes("EXISTING-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := runOpts(lib, true)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), "", opts); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := os.Stat(flat); err != nil {
		t.Errorf("status moved a flat file out of the library root: %v", err)
	}
}

// PacksInScope is how many packs the run actually read item pages for. The lockfile
// carries every pack the record holds, in scope or not, so reporting its size in
// that slot would tell a user narrowing with --only that the narrowing did nothing.
func TestPacksInScopeCountsWhatTheRunRead(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	seeded, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	opts := runOpts(lib, true)
	opts.OnlyGlob = "polygon-pirate-pack"
	rep, err := Run(context.Background(), newClient(srv.URL), seeded, lockPath, opts)
	if err != nil {
		t.Fatalf("narrowed status: %v", err)
	}
	if rep.PacksInScope != 1 {
		t.Errorf("PacksInScope = %d, want 1 (the one pack --only selected)", rep.PacksInScope)
	}
	if len(rep.NewLockfile.Packs) <= rep.PacksInScope {
		t.Errorf("the lockfile holds %d packs and the run read %d; this test proves nothing unless they differ",
			len(rep.NewLockfile.Packs), rep.PacksInScope)
	}
}

// The key is half variant, so a variant the store renames on an unchanged fileId
// moves the file's key. The in-scope pack is rebuilt from the live page and moves
// with it; a pack the run did not fetch has no live page to rebuild from, and if the
// carried entry keeps its old key while being repointed at the new bytes, the
// committed lockfile ends up telling a consumer that a Godot 4.5.1 file lives at a
// path holding Godot 4.6 content — one fileId filed under two engines.
func TestRenamedVariantMovesTheKeyForCarriedOwnersToo(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	variant, version := "Godot_4_5_1", "v1_0_0"
	items := func(orderItem string) (string, bool) {
		switch orderItem {
		case "1", "4": // Pirate and Dungeon both bundle fileId 999
			return itemPage("GENERIC_Particle_FX", variant, version, 999), true
		}
		return "", false
	}
	srv := newServer(t, serverOpts{itemHTML: items})

	if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false)); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	// The store renames the variant on the same fileId, and Dungeon is out of scope
	// so its entry is carried rather than rebuilt.
	variant, version = "Godot_4_6_0", "v2_0_0"
	only := runOpts(lib, false)
	only.PackSelected = func(slug string) bool { return slug != "polygon-dungeon-pack" }
	if _, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, only); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	const newKey = "GENERIC_Particle_FX|Godot_4_6_0"
	const oldKey = "GENERIC_Particle_FX|Godot_4_5_1"
	carried := after.Packs["polygon-dungeon-pack"]
	if _, stale := carried.Files[oldKey]; stale {
		t.Errorf("the carried owner kept %q after the variant was renamed: %+v", oldKey, carried.Files[oldKey])
	}
	in := after.Packs["polygon-pirate-pack"].Files[newKey]
	out := carried.Files[newKey]
	if out.FileID != 999 {
		t.Fatalf("the carried owner has no entry at %q: %+v", newKey, carried.Files)
	}
	if in.Variant != out.Variant {
		t.Errorf("one fileId filed under two variants: in-scope %q vs carried %q", in.Variant, out.Variant)
	}
	if in.Version != out.Version || in.SHA256 != out.SHA256 || in.CachePath != out.CachePath {
		t.Errorf("owning packs diverged:\n  in-scope %+v\n  carried  %+v", in, out)
	}
}
