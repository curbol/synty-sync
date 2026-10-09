package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/cache"
	"github.com/curbol/synty-sync/internal/lockfile"
	"github.com/curbol/synty-sync/internal/model"
	"github.com/curbol/synty-sync/internal/portal"
)

// A rate limit clears by waiting, unlike the other 4xx statuses, so a download must
// keep its remaining attempts. The two not-a-package sentinels are the opposite case:
// no number of fetches turns a login page into a pack, so stopping on them is what
// keeps an expired session from spending every attempt and every backoff on each file
// in the library before reporting a failure it knew on the first byte.
func TestRateLimitIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"404", &portal.StatusError{Status: http.StatusNotFound, Op: "download T|Godot"}, true},
		{"401", &portal.StatusError{Status: http.StatusUnauthorized, Op: "download T|Godot"}, true},
		{"410", &portal.StatusError{Status: http.StatusGone, Op: "download T|Godot"}, true},
		// expired signature; a fresh Resolve re-signs
		{"403", &portal.StatusError{Status: http.StatusForbidden, Op: "download T|Godot"}, false},
		// rate limit; backing off clears it
		{"429", &portal.StatusError{Status: http.StatusTooManyRequests, Op: "download T|Godot"}, false},
		// the server says the request did not finish in time
		{"408", &portal.StatusError{Status: http.StatusRequestTimeout, Op: "download T|Godot"}, false},
		{"500", &portal.StatusError{Status: http.StatusInternalServerError, Op: "download T|Godot"}, false},
		// The content-type refusal and the body sniff, each as its caller wraps it.
		{"not-a-package type", fmt.Errorf("download T|Godot: %w (Content-Type text/html)", portal.ErrNotAPackage), true},
		{"not-a-package body", fmt.Errorf("T|Godot: %w", ErrNotAPackageBody), true},
		{"truncated archive", fmt.Errorf("T|Godot: %w", ErrTruncatedArchive), true},
		// A transport failure carries no status and no sentinel, and retrying is the
		// whole point of one.
		{"no status", errors.New("connection reset by peer"), false},
	} {
		if got := permanentDownloadFailure(tc.err); got != tc.permanent {
			t.Errorf("%s: permanent = %v, want %v", tc.name, got, tc.permanent)
		}
	}
}

// Gone is the one cause no re-run can clear, and it is excluded from the exit status
// for exactly that reason. Counting a 410 as actionable would make every future sync
// exit non-zero over a file the store will never serve again; missing a 5xx would hide
// a failure a re-run could fix.
func TestOnlyAPulledFileCountsAsGone(t *testing.T) {
	for _, tc := range []struct {
		status int
		gone   bool
	}{
		{http.StatusNotFound, true},
		{http.StatusGone, true},
		{http.StatusForbidden, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	} {
		err := &portal.StatusError{Status: tc.status, Op: "download T|Godot"}
		if got := goneFromTheStore(err); got != tc.gone {
			t.Errorf("status %d: gone = %v, want %v", tc.status, got, tc.gone)
		}
	}
	if goneFromTheStore(errors.New("a transport error carries no status")) {
		t.Error("an error with no status must not read as gone")
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
	assertOwnersAgree(t, in, out)
	if in.Version != "v2_0_0" {
		t.Errorf("version = %q, want the freshly downloaded v2_0_0", in.Version)
	}
}

// seedPirateLockfile runs one sync against srv and returns the lockfile it wrote
// plus how many files the pirate pack ended up holding, which is what the two
// zero-files guards below check has not shrunk.
func seedPirateLockfile(t *testing.T, srv *httptest.Server, lib, lockPath string) (lockfile.Lockfile, int) {
	t.Helper()
	seeded := seedRun(t, srv, lockPath, runOpts(lib, false))
	n := len(seeded.Packs["polygon-pirate-pack"].Files)
	if n == 0 {
		t.Fatal("seed produced no pirate files")
	}
	return seeded, n
}

// A pack whose every row carries a variant this build does not know is a Synty
// engine we have not shipped support for, not broken markup: the parser skips such
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
// often at 200, and the two guards that refuse it sit at different layers: portal
// checks the Content-Type before a byte streams, and the syncer sniffs the delivered
// bytes for the response that claims to be an archive and is not. Either has to hold
// on its own, so each case here asserts the guard it is about actually fired. Those
// bytes must never occupy a cache path and a login page's digest must never be
// recorded as a pack's verified content, or every later Verify compares them against
// themselves and finds them intact forever.
//
// A truncated archive is the third shape: a copy that stopped part way still begins
// with a zip's magic, so the sniff passes it, and only the end-of-central-directory
// check sees that it is not the whole file.
func TestARejectedBodyIsNeitherStoredNorRecorded(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        []byte // nil serves a login page
		contentType string
		wantGuard   string
	}{
		{
			name:        "a document that says it is one is refused on Content-Type",
			contentType: "text/html; charset=utf-8",
			wantGuard:   portal.ErrNotAPackage.Error(),
		},
		{
			// A document wearing an archive's Content-Type, so portal.documentMediaType
			// waves it through and only the body sniff can catch it. This is what a CDN
			// error page served as application/octet-stream looks like.
			name:        "a document wearing an archive's type is refused on its bytes",
			contentType: "application/zip",
			wantGuard:   ErrNotAPackageBody.Error(),
		},
		{
			name:        "a truncated archive is refused on its trailer",
			body:        truncatedPackageBytes("pack"),
			contentType: "application/zip",
			wantGuard:   ErrTruncatedArchive.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == nil {
				body = []byte("<!doctype html><title>Log in</title>")
			}
			srv := newServer(t, serverOpts{fileBody: func(string) ([]byte, string, bool) {
				return body, tc.contentType, true
			}})
			lib := t.TempDir()
			lockPath := filepath.Join(t.TempDir(), "lock.json")

			rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
			if err != nil {
				t.Fatalf("a rejected body must fail its file, not the run: %v", err)
			}
			if len(rep.Failures) == 0 {
				t.Fatal("no failures reported for a run where every body was rejected")
			}
			for _, f := range rep.Failures {
				if !strings.Contains(f.Err, tc.wantGuard) {
					t.Errorf("failure %q was not caught by the guard this case is about (%s)", f.Err, tc.wantGuard)
				}
			}
			if rep.ActionableFailures() == 0 {
				t.Error("no actionable failures, so the command would exit 0 on a session that is not working")
			}
			if len(rep.Downloaded) != 0 {
				t.Errorf("reported %d downloads for a run that only ever received login pages", len(rep.Downloaded))
			}
			// Nothing committed and no temp left behind: Store stops at the temp file and
			// the caller discards it, so a rejected body never occupies a real cache path
			// even briefly.
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
		})
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
//
// The record kept has to be the whole record, with its bytes: a pack carried forward as
// a key with its files stripped, or with its copy pruned, is erased in all but name.
func TestDeOwnedPackIsReported(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	body := packageBytes("T_Godot_4_5_1_v1.zip")
	p, err := cache.Store(lib, "T", "T_Godot_4_5_1_v1.zip", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	const slug, key = "a-pack-i-no-longer-own", "T|Godot_4_5_1"
	entry := lockfile.File{
		FileToken: "T", Variant: "Godot_4_5_1", Version: "v1", FileID: 999, Tracked: true,
		SHA256: p.SHA256, SizeBytes: p.Size, CachePath: p.RelPath, DownloadedAt: "2026-01-01T00:00:00Z",
	}
	prior := lockfile.New()
	prior.Packs[slug] = lockfile.Pack{DisplayName: "A Pack I No Longer Own", Files: map[string]lockfile.File{key: entry}}

	rep, err := Run(context.Background(), newClient(srv.URL), prior, filepath.Join(t.TempDir(), "lock.json"), runOpts(lib, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != slug {
		t.Errorf("Removed = %v, want the de-owned pack", rep.Removed)
	}
	kept, ok := rep.NewLockfile.Packs[slug]
	if !ok {
		t.Fatal("the de-owned pack was dropped from the lockfile; one enumeration is not enough to erase a record")
	}
	if got := kept.Files[key]; got != entry {
		t.Errorf("the de-owned pack's record changed:\n got %+v\nwant %+v", got, entry)
	}
	if !cache.Verify(lib, entry.CachePath, entry.SizeBytes) {
		t.Errorf("the de-owned pack's copy at %s is gone", entry.CachePath)
	}
}

// humanBytes formats the store's own size label inside a download goroutine, and the
// label is whatever the page says. Indexing a four-letter unit table panicked at a
// pebibyte and took the whole run down with it.
func TestHumanBytesNamesEveryInt64(t *testing.T) {
	for n, want := range map[int64]string{
		-5:            "-5 B",
		0:             "0 B",
		1023:          "1023 B",
		1 << 10:       "1.0 KB",
		1 << 40:       "1.0 TB",
		1 << 50:       "1.0 PB",
		1 << 60:       "1.0 EB",
		math.MaxInt64: "8.0 EB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// The summary is one list of classes, and String's default arm answered "unchanged",
// so a class added without a name tallied as a no-op. Classes is the list a caller
// prints from, and every class in it has to say what it is.
func TestEveryClassHasItsOwnName(t *testing.T) {
	seen := map[string]Class{}
	for _, c := range Classes() {
		name := c.String()
		if strings.HasPrefix(name, "class(") {
			t.Errorf("class %d has no name", int(c))
		}
		if other, dup := seen[name]; dup {
			t.Errorf("classes %d and %d are both called %q", int(other), int(c), name)
		}
		seen[name] = c
	}
	// Every value String names is in the list, so a new class cannot be named and still
	// drop out of the tally.
	for c := Class(0); c < 64; c++ {
		if _, listed := seen[c.String()]; !listed && !strings.HasPrefix(c.String(), "class(") {
			t.Errorf("class %q is not in Classes()", c)
		}
	}
}

// A lockfile entry with no fileId is filed under 0 by the index every lookup goes
// through, and nothing the store lists has that id: it is never classified, its pack
// rebuilds it away, and orphanedRecords then reports its copy as unreferenced while the
// same file is fetched again under its real id. It arrives by a hand edit or a merge, so
// it is refused before the run touches anything, with the entry named.
func TestAnEntryWithNoFileIDIsRefused(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	lf := seedRun(t, srv, lockPath, runOpts(lib, false))
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lf = withPirateEntry(lf, func(f *lockfile.File) { f.FileID = 0 })

	for _, dry := range []bool{true, false} {
		_, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, dry))
		if err == nil || !strings.Contains(err.Error(), pirateKey) {
			t.Errorf("dry=%v: err = %v, want a refusal naming %s", dry, err, pirateKey)
		}
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a refused run rewrote the lockfile")
	}
}

// An enumeration that comes back empty while the lockfile holds packs is a broken
// read, not a library someone emptied. Acting on it rewrites the committed record.
func TestEmptyEnumerationWithAPopulatedLockfileIsAnError(t *testing.T) {
	srv := newServer(t, serverOpts{page1: emptyAuthPage})
	prior := lockfile.New()
	prior.Packs["polygon-pirate-pack"] = lockfile.Pack{DisplayName: "POLYGON - Pirate Pack"}

	_, err := Run(context.Background(), newClient(srv.URL), prior, filepath.Join(t.TempDir(), "lock.json"), runOpts(t.TempDir(), false))
	// The sentinel, not merely non-nil: main tells this apart from an expired session
	// to say which of the two happened, and a wrap that loses errors.Is identity is
	// invisible to a test that only checks the error exists.
	if !errors.Is(err, ErrEmptyLibrary) {
		t.Fatalf("an empty library against a populated lockfile gave %v, want ErrEmptyLibrary", err)
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
//
// The update can fail at the transport or at the body checks, and the body checks are
// the case that matters most here: the new version downloads under the same filename,
// so a rejected body that reached the real path would replace the verified copy with
// the very bytes the checks refused. Each case asserts which guard refused it.
func TestFailedUpdateKeepsTheVerifiedCopyRecorded(t *testing.T) {
	const name = "GENERIC_Particle_FX_Godot_4_5_1.zip" // one name across versions
	for _, tc := range []struct {
		what    string
		status  int
		body    []byte
		wantErr string
	}{
		{what: "a server error", status: http.StatusInternalServerError, wantErr: "500"},
		{what: "a document body", body: []byte("<!doctype html><title>Log in</title>"), wantErr: ErrNotAPackageBody.Error()},
		{what: "a truncated archive", body: truncatedPackageBytes(name), wantErr: ErrTruncatedArchive.Error()},
	} {
		t.Run(tc.what, func(t *testing.T) {
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
				downloadName: func(fileID string) (string, bool) { return name, fileID == "999" },
				downloadStatus: func(fileID string) (int, bool) {
					return tc.status, broken && tc.status != 0 && fileID == "999"
				},
				fileBody: func(string) ([]byte, string, bool) {
					return tc.body, "application/zip", broken && tc.body != nil
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
			if !strings.Contains(rep.Failures[0].Err, tc.wantErr) {
				t.Errorf("failure %q was not refused by the guard this case is about (%s)", rep.Failures[0].Err, tc.wantErr)
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
			sha, _, err := cache.Hash(context.Background(), lib, before.CachePath)
			if err != nil {
				t.Fatalf("the prior copy at %s is gone: %v", before.CachePath, err)
			}
			if sha != before.SHA256 {
				t.Errorf("the bytes at %s were replaced by the rejected body", before.CachePath)
			}
		})
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
	assertOwnersAgree(t, in, out)
}

// The failed update again, with the store renaming the variant on the same fileId as
// it bumps the version. The entry keeps the bytes the last run verified, so it has to
// keep the variant those bytes are: rebuilding it under the live page's new engine
// label leaves the in-scope owner naming Godot 4.5.1 content as a 4.6.0 build, and the
// carried owner filing the same fileId under the old key. Nothing heals it while the
// download keeps failing, and a file the store has pulled never downloads again.
func TestFailedUpdateOnARenamedVariantKeepsTheBytesUnderTheirOwnName(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	variant, version := "Godot_4_5_1", "v1_0_0"
	broken := false
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			switch orderItem {
			case "1", "4": // Pirate and Dungeon both bundle fileId 999
				return itemPage("GENERIC_Particle_FX", variant, version, 999), true
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
	const oldKey = "GENERIC_Particle_FX|Godot_4_5_1"
	before := lf.Packs["polygon-pirate-pack"].Files[oldKey]
	if !before.Tracked {
		t.Fatal("seed produced no tracked bundled file")
	}

	variant, version, broken = "Godot_4_6_0", "v2_0_0", true
	only := runOpts(lib, false)
	only.Attempts, only.Backoff = 1, 0
	only.PackSelected = func(slug string) bool { return slug != "polygon-dungeon-pack" }
	if _, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, only); err != nil {
		t.Fatalf("a failed update aborted the run: %v", err)
	}

	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	in := after.Packs["polygon-pirate-pack"].Files[oldKey]
	out := after.Packs["polygon-dungeon-pack"].Files[oldKey]
	if in.FileID != 999 {
		t.Fatalf("the in-scope owner moved the entry off %q: %+v", oldKey, after.Packs["polygon-pirate-pack"].Files)
	}
	if in.Variant != before.Variant || in.Version != before.Version {
		t.Errorf("the entry names %s %s, but the recorded sha is the %s %s bytes",
			in.Variant, in.Version, before.Variant, before.Version)
	}
	assertOwnersAgree(t, in, out)
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
	assertOwnersAgree(t, in, out)
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
	assertOwnersAgree(t, in, out)
	// An owner that never held the file has no stamp of its own, so it takes the one
	// recorded for the same fileId rather than ending the run tracked, at a shared
	// path and sha, with no downloadedAt at all.
	if out.DownloadedAt == "" || in.DownloadedAt != out.DownloadedAt {
		t.Errorf("downloadedAt disagrees on fileId 999: in-scope %q vs carried %q", in.DownloadedAt, out.DownloadedAt)
	}
}

// advertisedSize is the portal's label, refreshed every run, and it describes the
// store's listing rather than the bytes, so it has to reach the owners a run did not
// fetch whatever the verdict was. Rebuilding only the in-scope entry from the live row
// leaves one fileId carrying two different advertisedSize values under two owners, at
// one version and one sha, in a committed file.
func TestAdvertisedSizeReachesAnOwnerTheRunDidNotFetch(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	size := "40 MB"
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			switch orderItem {
			case "1", "4": // Pirate and Dungeon both bundle fileId 999
				return fmt.Sprintf(`<div class='sky-pilot-list-item'>
				  <div class='sky-pilot-file-heading'>GENERIC_Particle_FX_Godot_4_5_1 | v1_0_0 <span class='sky-pilot-file-size'>(%s)</span></div>
				  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/999?x=1'>Download</a></div>
				</div>`, size), true
			}
			return "", false
		},
	})

	lf := seedRun(t, srv, lockPath, runOpts(lib, false))

	// The store re-labels the file and Dungeon drops out of scope, so its entry is
	// carried forward while Pirate is rebuilt from the live row.
	size = "41 MB"
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
	if in.AdvertisedSize != out.AdvertisedSize {
		t.Errorf("one fileId carries two advertised sizes: in-scope %d vs carried %d", in.AdvertisedSize, out.AdvertisedSize)
	}
	if in.AdvertisedSize != 41<<20 {
		t.Errorf("advertisedSize = %d, want the re-labelled 41 MB", in.AdvertisedSize)
	}
}

// The same invariant one channel over: two owners the run *did* fetch. fetchAll reads
// item pages concurrently, so a store-side re-label landing mid-run gives one owner's
// page a figure the other's does not have. Rebuilding each entry from its own row then
// commits two advertised sizes for one fileId at one version and one sha — the shape
// the test above exists to prevent, reached without any pack going out of scope.
func TestAdvertisedSizeAgreesAcrossTwoOwnersTheRunFetched(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			// Pirate's page is read before the re-label, Dungeon's after.
			size := map[string]string{"1": "40 MB", "4": "41 MB"}[orderItem]
			if size == "" {
				return "", false
			}
			return fmt.Sprintf(`<div class='sky-pilot-list-item'>
			  <div class='sky-pilot-file-heading'>GENERIC_Particle_FX_Godot_4_5_1 | v1_0_0 <span class='sky-pilot-file-size'>(%s)</span></div>
			  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/999?x=1'>Download</a></div>
			</div>`, size), true
		},
	})

	seedRun(t, srv, lockPath, runOpts(lib, false))

	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	assertOwnersAgree(t,
		after.Packs["polygon-pirate-pack"].Files[key],
		after.Packs["polygon-dungeon-pack"].Files[key])
}

// A fileId whose bytes this run did not re-fetch still has to leave every owner with
// the same downloadedAt. The in-scope entry is rebuilt with the stamp from whichever
// prior record held one; an owner that never held the file has none of its own, so
// taking only its own empty stamp leaves it tracked, at the shared path and sha, with
// no downloadedAt at all. Only an Unchanged file reaches this: a re-download stamps
// every owner with the current run's time and agrees by construction.
func TestUnfetchedBundledFileCarriesOneDownloadedAt(t *testing.T) {
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	const stamp = "2024-01-01T00:00:00Z"
	file := func(tracked bool, downloadedAt string) lockfile.File {
		f := lockfile.File{
			FileToken: "GENERIC_Particle_FX", Variant: "Godot_4_5_1", Version: "v1_0_0", FileID: 999,
		}
		if tracked {
			f.Tracked, f.CachePath, f.SHA256, f.DownloadedAt = true, "GENERIC_Particle_FX/p.zip", "sha", downloadedAt
		}
		return f
	}
	prev := lockfile.Lockfile{Packs: map[string]lockfile.Pack{
		// Pirate holds the bytes and the stamp; Dungeon is out of scope and never got
		// the file, which an earlier failed fetch is the ordinary way to reach.
		"polygon-pirate-pack":  {DisplayName: "P", Files: map[string]lockfile.File{key: file(true, stamp)}},
		"polygon-dungeon-pack": {DisplayName: "D", Files: map[string]lockfile.File{key: file(false, "")}},
	}}
	pf := []packWithFiles{{
		pack: model.Pack{Slug: "polygon-pirate-pack", DisplayName: "P"},
		files: []model.FileEntry{{
			FileToken: "GENERIC_Particle_FX", Variant: "Godot_4_5_1", Version: "v1_0_0", FileID: 999,
		}},
	}}
	// Unchanged: resolved from the prior record, so now is false and no fresh stamp
	// is minted for either owner.
	resolvedByID := map[int]resolved{999: {
		cachePath: "GENERIC_Particle_FX/p.zip", sha: "sha", version: "v1_0_0", variant: "Godot_4_5_1",
	}}

	rep := Report{NewLockfile: lockfile.Lockfile{Packs: map[string]lockfile.Pack{}}}
	opts := runOpts(t.TempDir(), false)
	vd, _, _ := readRows(pf, opts.Filter)
	vd.resolved = resolvedByID
	buildLockfile(&rep, pf, opts, vd, prev)

	in := rep.NewLockfile.Packs["polygon-pirate-pack"].Files[key]
	out := rep.NewLockfile.Packs["polygon-dungeon-pack"].Files[key]
	if !in.Tracked || !out.Tracked {
		t.Fatalf("both owners should hold the resolved file: in=%+v out=%+v", in, out)
	}
	if in.DownloadedAt != stamp || out.DownloadedAt != stamp {
		t.Errorf("downloadedAt disagrees on fileId 999: in-scope %q vs carried %q, want %q",
			in.DownloadedAt, out.DownloadedAt, stamp)
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
	vd, _, _ := readRows(pf, opts.Filter)
	vd.unresolved[999] = struct{}{}
	buildLockfile(&rep, pf, opts, vd, prev)

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
// The trailer check is keyed on the leading bytes, not the extension, and it has a
// deliberate hole: a container this build cannot read has no decidable answer without
// decompressing it, so it passes through. Synty ships Unity packs as .unitypackage
// (a gzipped tar), and every adoption fixture in this suite is a real zip, so a check
// that started demanding the zip magic before adopting anything would leave the whole
// suite green while every Unity pack re-downloaded on each run.
func TestAUnityPackageIsAdoptedWithoutAZipTrailer(t *testing.T) {
	// gzip magic: not a zip, so the end-of-central-directory search must not apply.
	body := append([]byte{0x1f, 0x8b, 0x08}, bytes.Repeat([]byte{0xab}, 512)...)
	lib := t.TempDir()
	dir := filepath.Join(lib, "POLYGON_Pirate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rel := "POLYGON_Pirate/POLYGON_Pirate_Godot_4_5_1_v1_0_1.unitypackage"
	if err := os.WriteFile(filepath.Join(lib, filepath.FromSlash(rel)), body, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := wholeArchive(rel, body, func(int) ([]byte, error) { return nil, nil }); err != nil {
		t.Errorf("a container with no zip magic was put through the zip trailer check: %v", err)
	}
	if err := adoptable(lib, rel); err != nil {
		t.Errorf("a Unity pack was refused as unadoptable, so it re-downloads every run: %v", err)
	}
}

// Adoption is the one path into the lockfile that never consults classify, and the
// head sniff cannot see a truncation: a copy that stopped part way still begins with
// an archive's magic. Taking one records its own short bytes as the file's truth,
// after which every Verify compares those bytes against themselves and finds them
// intact forever. The trailer check keys on the leading bytes rather than the name,
// because the name comes from a signed URL, a Content-Disposition, or a hand, and the
// cache deliberately matches a wanted file under any extension or none, so an
// extension check would leave unexamined exactly the names the cache is most willing
// to adopt.
func TestATruncatedArchiveIsNeverAdopted(t *testing.T) {
	// normalizeName drops extensions, so the adopt scan matches the bare name exactly
	// as it matches the .zip.
	for _, name := range []string{
		"POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip",
		"POLYGON_Pirate_Godot_4_5_1_v1_0_1",
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, serverOpts{})
			lib := t.TempDir()
			lockPath := filepath.Join(t.TempDir(), "lock.json")
			dir := filepath.Join(lib, "POLYGON_Pirate")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			cut := truncatedPackageBytes(name)
			if err := os.WriteFile(filepath.Join(dir, name), cut, 0o644); err != nil {
				t.Fatal(err)
			}

			rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			for _, d := range rep.Adopted {
				if d.FileID == 2282645 {
					t.Error("a truncated archive was adopted as the pack's content")
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
		})
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

// An interrupt is not a per-file verdict: recording every file the run had not reached
// as failed would blame each of them for something that has nothing to do with it. Nor
// is it a reason to throw the record away. A Changed file the run already downloaded has
// had its prior copy pruned, so a lockfile left as it was names a path holding nothing
// and a sha for bytes that are gone, and the new copy sits in the cache unrecorded. The
// run saves what it resolved, carries what it never reached forward unchanged, and
// still reports the interrupt as the error.
func TestAnInterruptKeepsTheRecordOfWhatTheRunDid(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	version := "v1_0_0"
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			switch orderItem {
			case "1":
				return itemPage("POLYGON_Pirate", "Godot_4_5_1", version, 4242), true
			case "4":
				return itemPage("POLYGON_Dungeon", "Godot_4_5_1", version, 5353), true
			}
			return "", false
		},
		downloadName: func(fileID string) (string, bool) {
			switch fileID {
			case "4242":
				return "POLYGON_Pirate_Godot_4_5_1_" + version + ".zip", true
			case "5353":
				return "POLYGON_Dungeon_Godot_4_5_1_" + version + ".zip", true
			}
			return "", false
		},
	})
	opts := twoPackOpts(lib)
	lf := seedRun(t, srv, lockPath, opts)
	const dungeonKey = "POLYGON_Dungeon|Godot_4_5_1"
	pirateBefore := lf.Packs["polygon-pirate-pack"].Files[pirateKey]
	dungeonBefore := lf.Packs["polygon-dungeon-pack"].Files[dungeonKey]

	version = "v2_0_0"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts.Progress = func(m string) {
		// Interrupted as the second download starts, after Pirate's has landed and its
		// prior copy has been pruned.
		if m == "download "+dungeonKey {
			cancel()
		}
	}
	rep, err := Run(ctx, newClient(srv.URL), lf, lockPath, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(rep.Failures) != 0 {
		t.Errorf("an interrupt was recorded as per-file failures: %+v", rep.Failures)
	}

	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	pirate := after.Packs["polygon-pirate-pack"].Files[pirateKey]
	if pirate.Version != "v2_0_0" || !pirate.Tracked || !cacheFileExists(lib, pirate.CachePath) {
		t.Errorf("the download the run finished is not recorded: %+v", pirate)
	}
	if cacheFileExists(lib, pirateBefore.CachePath) {
		t.Fatalf("the prior copy at %s was not pruned, so this does not test what it means to", pirateBefore.CachePath)
	}
	dungeon := after.Packs["polygon-dungeon-pack"].Files[dungeonKey]
	if dungeon != dungeonBefore {
		t.Errorf("the file the run never reached was not carried forward unchanged:\n got %+v\nwant %+v", dungeon, dungeonBefore)
	}
}

// status writes nothing, interrupted or not.
func TestAnInterruptedStatusWritesNoLockfile(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := runOpts(t.TempDir(), true)
	// The filter runs once the item pages are read, so this lands in the classify pass.
	opts.Filter = func(v model.Variant) bool { cancel(); return godotSourceFilter(v) }

	if _, err := Run(ctx, newClient(srv.URL), lockfile.New(), lockPath, opts); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(lockPath); err == nil {
		t.Error("an interrupted status wrote the lockfile")
	}
}

// main's signal handler takes SIGINT's default action away for the life of the run, so a
// pass that never looks at the context ignores Ctrl-C until it finishes. Under sync the
// classify pass re-hashes the whole library, which is minutes of reading; acting on a
// verdict an interrupted hash produced is worse, because a cancelled verify reads as a
// mismatch and the file is re-downloaded as CacheMissing.
func TestAnInterruptStopsTheClassifyPass(t *testing.T) {
	srv := newServer(t, serverOpts{})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	lf := seedRun(t, srv, lockPath, runOpts(lib, false))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := runOpts(lib, false)
	opts.Filter = func(v model.Variant) bool { cancel(); return godotSourceFilter(v) }
	rep, err := Run(ctx, newClient(srv.URL), lf, lockPath, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(rep.Diffs) != 0 || len(rep.Downloaded) != 0 {
		t.Errorf("the classify pass kept going after the interrupt: %d diffs, %d downloads", len(rep.Diffs), len(rep.Downloaded))
	}
	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Packs, lf.Packs) {
		t.Error("a run interrupted before it acted on anything changed the record")
	}
}

// The adopt pass hashes every file it takes, so it is as long as the classify pass on a
// library a lost lockfile left unrecorded. An interrupt there must stop it, and the
// adoption it cut short is the run's outcome, not a refusal to report against the file.
func TestAnInterruptStopsTheAdoptPass(t *testing.T) {
	lib := t.TempDir()
	version := "v1_0_0"
	srv := twoFileServer(t, &version, func(v string) string { return "POLYGON_Pirate_Godot_4_5_1_" + v + ".zip" })
	seedRun(t, srv, filepath.Join(t.TempDir(), "seed.json"), twoPackOpts(lib))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := twoPackOpts(lib)
	adopts := 0
	opts.Progress = func(m string) {
		if strings.HasPrefix(m, "adopt ") {
			adopts++
			cancel()
		}
	}
	rep, err := Run(ctx, newClient(srv.URL), lockfile.New(), filepath.Join(t.TempDir(), "lock.json"), opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if adopts != 1 {
		t.Errorf("the adopt pass started %d adoptions, want it to stop after the interrupt", adopts)
	}
	if w := warnContaining(rep.Warnings, "canceled"); len(w) != 0 {
		t.Errorf("an interrupt was reported as a refused adoption: %v", w)
	}
	if len(rep.Downloaded) != 0 {
		t.Errorf("an interrupted run went on to download %d files", len(rep.Downloaded))
	}
}

// An item page interrupted mid-request comes back as an error that wraps the
// cancellation. Recording that as the run's first error printed "item page for X:
// context canceled", naming a pack for what was the user's own Ctrl-C.
func TestAnInterruptedItemPageIsTheInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newServer(t, serverOpts{itemHTML: func(string) (string, bool) {
		cancel()
		time.Sleep(50 * time.Millisecond) // past the client noticing
		return "", false
	}})
	packs := []model.Pack{{Slug: "polygon-pirate-pack", ItemURL: "/apps/downloads/customers/1/orders/100/order_items/1"}}
	_, _, err := fetchAll(ctx, newClient(srv.URL), packs, 1)
	if err != context.Canceled {
		t.Errorf("err = %v, want the bare context.Canceled", err)
	}
}

// The store archiving a file and a variant_includes that stops matching one leave
// exactly the same thing behind: the pack still lists it, so the entry survives and
// orphanedRecords never sees it, while the entry is rebuilt untracked and its cache
// path and sha go with it as the bytes stay on disk. Nothing can take them back
// either, since a declined file is never an adopt candidate. Keying the report on the
// Archived label alone left a reader who dropped a variant with gigabytes nothing
// points at and no run that would ever mention them, and in that case the pack keeps
// a variant that still matches, so not even the "nothing matches the filter" warning
// fires. Both causes have to be said, and said once.
func TestDecliningAFileIsReportedNotSilentlyDropped(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		// build returns the item page order item 1 serves and the decline the second run
		// applies: a re-label by the store, or a change to the reader's own filter.
		build func() (func(string) (string, bool), func(*Options))
	}{
		{
			name: "the store archives it",
			key:  "POLYGON_Pirate|Godot_4_5_1",
			build: func() (func(string) (string, bool), func(*Options)) {
				version := "v1_0_0"
				return func(orderItem string) (string, bool) {
					if orderItem != "1" {
						return "", false
					}
					return itemPage("POLYGON_Pirate", "Godot_4_5_1", version, 4242), true
				}, func(*Options) { version = "v1_0_0_ARCHIVED" }
			},
		},
		{
			name: "variant_includes stops matching it",
			key:  "POLYGON_Pirate|SourceFiles",
			build: func() (func(string) (string, bool), func(*Options)) {
				return func(orderItem string) (string, bool) {
						if orderItem != "1" {
							return "", false
						}
						// The pack keeps a Godot file that still matches, so the pack itself has
						// nothing to warn about.
						return itemPage("POLYGON_Pirate", "Godot_4_5_1", "v1_0_0", 4242) +
							itemPage("POLYGON_Pirate", "SourceFiles", "v1_0_0", 4243), true
					}, func(o *Options) {
						o.Filter = func(v model.Variant) bool { return strings.HasPrefix(string(v), "Godot_") }
					}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, decline := tc.build()
			lib := t.TempDir()
			lockPath := filepath.Join(t.TempDir(), "lock.json")
			srv := newServer(t, serverOpts{itemHTML: items})
			opts := runOpts(lib, false)
			opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }

			seeded := seedRun(t, srv, lockPath, opts)
			before := seeded.Packs["polygon-pirate-pack"].Files[tc.key]
			if !before.Tracked || before.CachePath == "" {
				t.Fatalf("seed did not track the file the run is about to decline: %+v", before)
			}

			decline(&opts)
			rep, err := Run(context.Background(), newClient(srv.URL), seeded, lockPath, opts)
			if err != nil {
				t.Fatalf("second sync: %v", err)
			}
			if _, err := os.Stat(filepath.Join(lib, before.CachePath)); err != nil {
				t.Fatalf("the bytes this is about are not on disk: %v", err)
			}
			said := false
			for _, w := range rep.Warnings {
				if strings.Contains(w, before.CachePath) && strings.Contains(w, "unreferenced") {
					said = true
				}
			}
			if !said {
				t.Errorf("the cached copy at %s lost its record with nothing said; warnings = %q", before.CachePath, rep.Warnings)
			}

			// Said once: the run after finds nothing tracked, so repeating it would nag
			// about the same file for the life of the library.
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
					t.Errorf("the declined file was reported a second time: %q", w)
				}
			}
		})
	}
}

// A lockfile can hold one fileId under two packs at two versions (a hand merge, or a
// pack that left and came back). Which record wins decides between Unchanged and a
// multi-gigabyte refetch, and the data cannot say which is right, so the only thing
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
// gated on DryRun, a guard whose absence would make "show me what would change"
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
// path holding Godot 4.6 content: one fileId filed under two engines.
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
	assertOwnersAgree(t, in, out)
}

// Selection is opt-in, and the allowlist has to narrow what the run *fetches*, not
// just what it records. TestPackSelectedLimitsToAllowlist asserts the diff and the
// lockfile, both of which would still come out right if filterPacks ran after
// fetchAll, leaving the run reading item pages for every pack the user owns and
// declined, a request per pack against the store on every sync.
func TestADisabledPacksItemPageIsNeverRequested(t *testing.T) {
	var fetched int32
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			atomic.AddInt32(&fetched, 1)
			return "", false // fall through to the real fixture
		},
	})
	lib := t.TempDir()
	opts := runOpts(lib, true)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }

	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), "", opts)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// The library fixture lists four packs; only the enabled one may be read.
	if got := atomic.LoadInt32(&fetched); got != 1 {
		t.Errorf("read %d item pages, want 1: the allowlist has to narrow the fetch, not just the record", got)
	}
	if rep.PacksInScope != 1 {
		t.Errorf("PacksInScope = %d, want 1", rep.PacksInScope)
	}
}

// A file the run declines is the third way one fileId ends up tracked under one owner
// and untracked under another, and the only one that reaches buildLockfile with no
// failure behind it: the pack in scope is rebuilt without the file while the pack left
// out of scope carries its record forward whole. Nothing else reports it, either.
// orphanedRecords sees the fileId still listed by a pack the user owns, and the filter
// case is not archived so archivedRecords says nothing at all; in the archived case the
// warning that does fire calls the cached copy unreferenced while the carried entry is
// still pointing straight at it.
func TestDeselectedBundledFileKeepsOwningPacksInAgreement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		filter  func(model.Variant) bool
	}{
		{"archived by the store", "v1_0_0_ARCHIVED", godotSourceFilter},
		{"dropped by variant_includes", "v1_0_0", func(model.Variant) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lib := t.TempDir()
			lockPath := filepath.Join(t.TempDir(), "lock.json")
			version := "v1_0_0"
			srv := newServer(t, serverOpts{
				itemHTML: func(orderItem string) (string, bool) {
					switch orderItem {
					case "1", "4": // Pirate and Dungeon both bundle fileId 999
						return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", version, 999), true
					}
					return "", false
				},
			})

			lf := seedRun(t, srv, lockPath, runOpts(lib, false))
			const key = "GENERIC_Particle_FX|Godot_4_5_1"
			if !lf.Packs["polygon-pirate-pack"].Files[key].Tracked {
				t.Fatal("seed produced no tracked bundled file")
			}

			// Dungeon goes out of scope, and the shared file stops being selected.
			version = tc.version
			only := runOpts(lib, false)
			only.Filter = tc.filter
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
			if in.Tracked {
				t.Errorf("the in-scope owner still tracks a file this run declined: %+v", in)
			}
			assertOwnersAgree(t, in, out)
		})
	}
}

// A flat file that Migrate moves and adopt then refuses is sitting in the layout, where
// the scan that follows finds it again. adoptAll carries a `refused` set so it is not
// turned away twice and the same reason printed twice for one file — a comment naming a
// bug that happened, with nothing behind it until now.
func TestARefusedFlatFileIsReportedOnce(t *testing.T) {
	srv := newServer(t, serverOpts{downloadName: func(fileID string) (string, bool) {
		if fileID == "2344711" {
			return "GENERIC_Particle_FX_Godot_4_5_1_v1_0_0.zip", true
		}
		return "", false
	}})
	lib := t.TempDir()
	// Flat at the library root, under the name the wanted file normalizes onto, so
	// Migrate folds it in and adopt then has to look at the bytes.
	planted := filepath.Join(lib, "GENERIC_Particle_FX_Godot_4_5_1_v1_0_0.zip")
	if err := os.WriteFile(planted, []byte("<!doctype html><title>Log in</title>"), 0o644); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(t.TempDir(), "lock.json")
	rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatal(err)
	}

	refusals := 0
	for _, w := range rep.Warnings {
		if strings.Contains(w, "not adopting") && strings.Contains(w, "GENERIC_Particle_FX") {
			refusals++
		}
	}
	if refusals != 1 {
		t.Errorf("the same refused file was reported %d times:\n%s", refusals, strings.Join(rep.Warnings, "\n"))
	}
}

// Archived is a per-row label, so two packs bundling one fileId can disagree about it
// in a single run while both stay in scope. The rebuild used to read each row's own
// verdict to decide whether to apply what the run resolved, so the owner whose row the
// store had archived was written untracked with no path or sha while the owner beside
// it recorded the shared copy — one fileId, one set of bytes, committed both ways, with
// no failure behind it for anything else to report. archivedRecords then named that
// same still-referenced copy as unreferenced.
func TestDivergentArchivedKeepsInScopeOwnersInAgreement(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	pirateVersion, dungeonVersion := "v1_0_0", "v1_0_0"
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			switch orderItem {
			case "1": // Pirate
				return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", pirateVersion, 999), true
			case "4": // Dungeon bundles the same fileId
				return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", dungeonVersion, 999), true
			}
			return "", false
		},
	})

	lf := seedRun(t, srv, lockPath, runOpts(lib, false))
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	if !lf.Packs["polygon-pirate-pack"].Files[key].Tracked {
		t.Fatal("seed produced no tracked bundled file")
	}

	// The store archives the file under Pirate's order item only. Both packs stay
	// enabled, so both are rebuilt from live pages on this run.
	pirateVersion = "v1_0_0_ARCHIVED"
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, false))
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}

	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	archived := after.Packs["polygon-pirate-pack"].Files[key]
	live := after.Packs["polygon-dungeon-pack"].Files[key]
	if !live.Tracked || live.CachePath == "" {
		t.Fatalf("the owner still serving the file lost its record: %+v", live)
	}
	assertOwnersAgree(t, archived, live)
	for _, w := range rep.Warnings {
		if strings.Contains(w, "unreferenced") {
			t.Errorf("a copy another owner still records was reported unreferenced: %s", w)
		}
	}
}

// The same divergence, on the verdicts that resolve nothing. applyResolved is what
// converged the two owners above, so every verdict that does not go through it left the
// in-scope rebuild taking version, variant and token off each owner's own row — and the
// store labels a bundled file per order item, so the two rows disagree. One fileId then
// reached the committed lockfile at two versions, under two keys, with nothing failing
// and nothing reporting it; indexByFileID picked between them by slug order on the run
// after, and drawing the stale one refetches a multi-GB file.
//
// Both halves are here because they fail for one reason and are reached two ways: a
// download that failed with no good prior copy, and a filter that declines every row.
func TestDivergentLabelsKeepInScopeOwnersInAgreementWhenNothingResolves(t *testing.T) {
	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	both := func(slug string) bool {
		return slug == "polygon-pirate-pack" || slug == "polygon-dungeon-pack"
	}
	items := func(orderItem string) (string, bool) {
		switch orderItem {
		case "1": // Pirate: the store archived this order item's copy
			return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", "v1_0_0_ARCHIVED", 999), true
		case "4": // Dungeon: the same fileId, still served
			return itemPage("GENERIC_Particle_FX", "Godot_4_5_1", "v1_0_0", 999), true
		}
		return "", false
	}

	for _, tc := range []struct {
		name string
		opts func(Options) Options
	}{
		{"the download fails", func(o Options) Options {
			o.Attempts = 1
			return o
		}},
		{"the filter declines every row", func(o Options) Options {
			o.Filter = func(model.Variant) bool { return false }
			return o
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lib := t.TempDir()
			lockPath := filepath.Join(t.TempDir(), "lock.json")
			srv := newServer(t, serverOpts{
				itemHTML:       items,
				downloadStatus: func(string) (int, bool) { return http.StatusInternalServerError, true },
			})
			opts := tc.opts(runOpts(lib, false))
			opts.PackSelected = both

			if _, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), lockPath, opts); err != nil {
				t.Fatalf("run: %v", err)
			}
			lf, err := lockfile.Load(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			var owners []lockfile.File
			for _, slug := range []string{"polygon-dungeon-pack", "polygon-pirate-pack"} {
				files := lf.Packs[slug].Files
				if len(files) != 1 {
					t.Fatalf("%s holds %d entries for one fileId: %+v", slug, len(files), files)
				}
				f, ok := files[key]
				if !ok {
					t.Fatalf("%s filed the shared file under a key of its own: %+v", slug, files)
				}
				owners = append(owners, f)
			}
			assertOwnersAgree(t, owners[0], owners[1])
		})
	}
}

// assertOwnersAgree is the fileId-dedup invariant as one assertion: two packs that own
// one fileId end a run holding the same record, field for field. Eight tests used to
// spell out their own field list and the lists had already drifted apart — SizeBytes in
// two of them, Variant in two, AdvertisedSize in none — so a divergence in a field a
// given scenario did not happen to name passed silently, and the next field added to
// lockfile.File would have been unchecked in all eight.
func assertOwnersAgree(t *testing.T, in, out lockfile.File) {
	t.Helper()
	if !reflect.DeepEqual(in, out) {
		t.Errorf("owning packs diverged over one fileId:\n  in-scope %+v\n  other    %+v", in, out)
	}
}

// The second half of the expired-session invariant. The first — Enumerate returning
// the sentinel — is covered by serving the logout shell as page 1, which kills the run
// before fetchAll is ever reached, so the wrap that carries the sentinel out of an item
// page fetch was never exercised. Turning that %w into a %v left the suite green while
// explainSession stopped firing, and a reader whose session died between the library
// page and the item pages got a bare "item page for …: expired or missing session" with
// no hint and no cookie source named. The lockfile stays safe either way, since any
// fetchAll error aborts before the save; what is lost is the diagnosis.
func TestASessionThatExpiresDuringItemPagesKeepsTheSentinel(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool { return slug == "polygon-pirate-pack" }

	// The library page still lists packs, so the walk finishes; the session is gone by
	// the time the item page behind it is asked for.
	srv := newServer(t, serverOpts{itemHTML: func(string) (string, bool) {
		return logoutShell, true
	}})

	seeded := lockfile.Lockfile{GeneratedAt: "before", Packs: map[string]lockfile.Pack{
		"polygon-pirate-pack": {DisplayName: "POLYGON - Pirate Pack", Files: map[string]lockfile.File{
			"POLYGON_Pirate|Godot_4_5_1": {
				FileToken: "POLYGON_Pirate", Variant: "Godot_4_5_1", Version: "v1_0_0", FileID: 1,
				Tracked: true, CachePath: "POLYGON_Pirate/1.zip", SHA256: "sha",
			},
		}},
	}}
	if err := lockfile.Save(lockPath, seeded); err != nil {
		t.Fatal(err)
	}

	_, err := Run(context.Background(), newClient(srv.URL), seeded, lockPath, opts)
	if !errors.Is(err, portal.ErrExpiredSession) {
		t.Fatalf("err = %v, want it to carry portal.ErrExpiredSession so explainSession can add the hint", err)
	}
	after, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.GeneratedAt != "before" {
		t.Errorf("an expired session rewrote the lockfile (generatedAt = %q)", after.GeneratedAt)
	}
}

// sync re-hashes the cache and status does not, and a body corrupted in place keeps
// the length that was recorded for it, so the cheap check cannot see it. Drop the
// VerifyDeep half of cacheChecker and every test still passes while a sync calls that
// file Unchanged for good, leaving the recorded sha describing bytes nothing reads
// again.
func TestOnlySyncSeesACorruptionThatKeptTheSize(t *testing.T) {
	// The bundled file: one set of bytes, so corrupting it is unambiguous.
	const bundledFileID = 2344711
	for _, tc := range []struct {
		name string
		dry  bool
		want Class
	}{
		{name: "status compares the recorded size", dry: true, want: Unchanged},
		{name: "sync re-hashes", dry: false, want: CacheMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, serverOpts{})
			lib := t.TempDir()
			lockPath := filepath.Join(t.TempDir(), "lock.json")
			lf := seedRun(t, srv, lockPath, runOpts(lib, false))

			rel := ""
			for _, p := range lf.Packs {
				for _, f := range p.Files {
					if f.FileID == bundledFileID && f.Tracked {
						rel = f.CachePath
					}
				}
			}
			if rel == "" {
				t.Fatalf("the seed run tracked no file %d; this test no longer corrupts anything", bundledFileID)
			}
			full := filepath.Join(lib, filepath.FromSlash(rel))
			was, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			// Same byte count, different content: exactly what Verify cannot see.
			corrupt := append([]byte(nil), was...)
			corrupt[len(corrupt)-1] ^= 0xff
			if err := os.WriteFile(full, corrupt, 0o644); err != nil {
				t.Fatal(err)
			}

			rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, runOpts(lib, tc.dry))
			if err != nil {
				t.Fatalf("second run: %v", err)
			}
			got, found := Class(-1), false
			for _, d := range rep.Diffs {
				if d.FileID == bundledFileID {
					got, found = d.Class, true
				}
			}
			if !found {
				t.Fatalf("file %d was not classified at all", bundledFileID)
			}
			if got != tc.want {
				t.Errorf("class = %v, want %v", got, tc.want)
			}
		})
	}
}

// The store labels a bundled file per order item, so which owner's row a run records
// it under decides its version, variant and advertised size. fetchAll collects into
// slots indexed by the pack's position, so that is enumeration order whatever order
// the item pages come back in. Collect with an append instead and the first page to
// answer names the file: two runs over unchanged data then disagree, and when the
// differing label is the version they re-download it on every other run.
func TestABundledFileTakesItsLabelsFromEnumerationOrderNotResponseOrder(t *testing.T) {
	const row = `<div class='sky-pilot-list-item'>
	  <div class='sky-pilot-file-heading'>GENERIC_Particle_FX_Godot_4_5_1 | v1_0_0 <span class='sky-pilot-file-size'>(%s)</span></div>
	  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/999?x=1'>Download</a></div>
	</div>`
	// Dungeon is second in the library page and is made to answer first.
	dungeonServed := make(chan struct{})
	srv := newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			switch orderItem {
			case "4":
				close(dungeonServed)
				return fmt.Sprintf(row, "41 MB"), true
			case "1":
				select {
				case <-dungeonServed:
				case <-time.After(5 * time.Second):
					// t.Error, not t.Fatal: this runs on the server's goroutine.
					t.Error("the two item pages were not fetched concurrently, so this proves nothing")
				}
				return fmt.Sprintf(row, "40 MB"), true
			}
			return "", false
		},
	})
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	after := seedRun(t, srv, lockPath, runOpts(lib, false))

	const key = "GENERIC_Particle_FX|Godot_4_5_1"
	const wantSize = 40 << 20 // Pirate's label, the pack listed first
	got := after.Packs["polygon-pirate-pack"].Files[key].AdvertisedSize
	if got != wantSize {
		t.Errorf("advertisedSize = %d, want %d: the file was labelled by the page that answered first, not the pack listed first", got, wantSize)
	}
}

// twoFileServer serves Pirate (fileId 4242, at *pirateVersion) and Dungeon (fileId 5353,
// fixed at v1_0_0) as one-file item pages, each file downloading under the name
// pirateName or its Synty-style default gives it. A test changes *pirateVersion between
// runs to make Pirate's file classify Changed.
func twoFileServer(t *testing.T, pirateVersion *string, pirateName func(version string) string) *httptest.Server {
	t.Helper()
	return newServer(t, serverOpts{
		itemHTML: func(orderItem string) (string, bool) {
			switch orderItem {
			case "1":
				return itemPage("POLYGON_Pirate", "Godot_4_5_1", *pirateVersion, 4242), true
			case "4":
				return itemPage("POLYGON_Dungeon", "Godot_4_5_1", "v1_0_0", 5353), true
			}
			return "", false
		},
		downloadName: func(fileID string) (string, bool) {
			switch fileID {
			case "4242":
				return pirateName(*pirateVersion), true
			case "5353":
				return "POLYGON_Dungeon_Godot_4_5_1_v1_0_0.zip", true
			}
			return "", false
		},
	})
}

func twoPackOpts(lib string) Options {
	opts := runOpts(lib, false)
	opts.PackSelected = func(slug string) bool {
		return slug == "polygon-pirate-pack" || slug == "polygon-dungeon-pack"
	}
	return opts
}

const pirateKey = "POLYGON_Pirate|Godot_4_5_1"

// withPirateEntry returns lf with the Pirate pack's one entry rewritten by edit.
func withPirateEntry(lf lockfile.Lockfile, edit func(*lockfile.File)) lockfile.Lockfile {
	pirate := lf.Packs["polygon-pirate-pack"]
	f := pirate.Files[pirateKey]
	edit(&f)
	pirate.Files[pirateKey] = f
	lf.Packs["polygon-pirate-pack"] = pirate
	return lf
}

// The prior copy of a Changed file is pruned when the new one lands elsewhere, and
// "elsewhere" was decided by comparing the lockfile's string against the derived one.
// The lockfile is committed and hand-editable, so "./TOK/f.zip" names the file
// "TOK/f.zip" does; compared raw, a re-download to the same filename was deleted moments
// after it was committed, recorded with a digest for a path holding nothing, and
// re-fetched in full on the next run.
func TestAPruneNeverDeletesTheFileItJustDownloaded(t *testing.T) {
	const name = "POLYGON_Pirate_Godot_4_5_1.zip" // one name across versions
	const rel = "POLYGON_Pirate/" + name
	for _, spelling := range []string{"./" + rel, "POLYGON_Pirate//" + name, "POLYGON_Pirate/x/../" + name} {
		t.Run(spelling, func(t *testing.T) {
			lib := t.TempDir()
			lockPath := filepath.Join(t.TempDir(), "lock.json")
			version := "v1_0_0"
			srv := twoFileServer(t, &version, func(string) string { return name })
			opts := twoPackOpts(lib)
			lf := withPirateEntry(seedRun(t, srv, lockPath, opts), func(f *lockfile.File) { f.CachePath = spelling })

			version = "v2_0_0"
			rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Downloaded) != 1 {
				t.Fatalf("downloaded %+v, want the one Changed file", rep.Downloaded)
			}
			got := rep.NewLockfile.Packs["polygon-pirate-pack"].Files[pirateKey]
			if !cacheFileExists(lib, got.CachePath) {
				t.Errorf("the run deleted the file it just downloaded to %s, recorded as %s", got.CachePath, spelling)
			}
		})
	}
}

// Nothing stops a hand-merged lockfile recording one file's bytes as another fileId's
// prior copy. The prune then deletes a file the lockfile still records for its real
// owner, which classifies Unchanged this run and CacheMissing the next.
func TestAPruneNeverDeletesAPathAnotherFileRecords(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	version := "v1_0_0"
	srv := twoFileServer(t, &version, func(v string) string { return "POLYGON_Pirate_Godot_4_5_1_" + v + ".zip" })
	opts := twoPackOpts(lib)
	lf := seedRun(t, srv, lockPath, opts)

	dungeon := lf.Packs["polygon-dungeon-pack"].Files["POLYGON_Dungeon|Godot_4_5_1"]
	if !dungeon.Tracked || !cacheFileExists(lib, dungeon.CachePath) {
		t.Fatalf("seed did not leave Dungeon's file on disk: %+v", dungeon)
	}
	// A second spelling, so a raw comparison against Dungeon's path cannot see it either.
	lf = withPirateEntry(lf, func(f *lockfile.File) { f.CachePath = "./" + dungeon.CachePath })

	version = "v2_0_0"
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Asked of the run, not only of the disk: Dungeon classifies after Pirate, so a
	// deleted copy is re-downloaded as CacheMissing and is back by the time this looks.
	for _, d := range rep.Downloaded {
		if d.FileID == dungeon.FileID {
			t.Errorf("Dungeon's file was re-downloaded: pruning Pirate's prior copy deleted %s", dungeon.CachePath)
		}
	}
	if !cacheFileExists(lib, dungeon.CachePath) {
		t.Fatalf("pruning Pirate's prior copy deleted %s, which fileId %d still records", dungeon.CachePath, dungeon.FileID)
	}
	if len(warnContaining(rep.Warnings, dungeon.CachePath)) == 0 {
		t.Errorf("the refused prune was not reported: %v", rep.Warnings)
	}
}

// The preferred copy of a file is the canonical name, and when it was a truncated one
// the adopt checks refused it after the matcher had already chosen it, so an intact
// "(1)" copy beside it was never examined and the pack re-downloaded in full. Asked of
// both places a copy can sit: the layout, and flat at the root where Migrate finds it.
func TestAnIntactCopyBesideARefusedOneIsAdopted(t *testing.T) {
	const canonical = "POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip"
	const collision = "POLYGON_Pirate_Godot_4_5_1_v1_0_1(1).zip"
	for _, where := range []string{"POLYGON_Pirate", "."} {
		t.Run(where, func(t *testing.T) {
			srv := newServer(t, serverOpts{})
			lib := t.TempDir()
			dir := filepath.Join(lib, where)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, canonical), truncatedPackageBytes(canonical), 0o644); err != nil {
				t.Fatal(err)
			}
			whole := packageBytes(collision)
			if err := os.WriteFile(filepath.Join(dir, collision), whole, 0o644); err != nil {
				t.Fatal(err)
			}

			rep, err := Run(context.Background(), newClient(srv.URL), lockfile.New(), filepath.Join(t.TempDir(), "lock.json"), runOpts(lib, false))
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range rep.Downloaded {
				if d.FileID == 2282645 {
					t.Error("the pack was re-downloaded though an intact copy sat beside the refused one")
				}
			}
			got := rep.NewLockfile.Packs["polygon-pirate-pack"].Files[pirateKey]
			if got.CachePath != "POLYGON_Pirate/"+collision || got.SizeBytes != int64(len(whole)) {
				t.Errorf("recorded %+v, want the intact copy %s", got, collision)
			}
		})
	}
}

// library_path is user-scoped while the lockfile is project-scoped, so two projects share
// one library. Once one of them has synced a file to v2, the other, whose lockfile still
// says v1, classified Changed and re-transferred the whole pack over a v2 copy already
// sitting at <fileToken>/ under the name the store gives v2. New and DownloadNow already
// asked the layout first; Changed was the one class that never did.
func TestAChangedFileAdoptsTheNewVersionAnotherProjectFetched(t *testing.T) {
	lib := t.TempDir()
	version := "v1_0_0"
	srv := twoFileServer(t, &version, func(v string) string { return "POLYGON_Pirate_Godot_4_5_1_" + v + ".zip" })
	opts := twoPackOpts(lib)
	projectA := filepath.Join(t.TempDir(), "a.lock.json")
	projectB := filepath.Join(t.TempDir(), "b.lock.json")
	lfA := seedRun(t, srv, projectA, opts)
	lfB := seedRun(t, srv, projectB, opts)

	version = "v2_0_0"
	if _, err := Run(context.Background(), newClient(srv.URL), lfA, projectA, opts); err != nil {
		t.Fatal(err)
	}
	afterA, err := lockfile.Load(projectA)
	if err != nil {
		t.Fatal(err)
	}
	fetched := afterA.Packs["polygon-pirate-pack"].Files[pirateKey]

	rep, err := Run(context.Background(), newClient(srv.URL), lfB, projectB, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Downloaded) != 0 {
		t.Errorf("project B re-downloaded %+v though project A had already fetched v2 into the shared library", rep.Downloaded)
	}
	got := rep.NewLockfile.Packs["polygon-pirate-pack"].Files[pirateKey]
	if got.Version != "v2_0_0" || got.CachePath != fetched.CachePath || got.SHA256 != fetched.SHA256 {
		t.Errorf("project B recorded %+v, want the v2 copy project A fetched: %+v", got, fetched)
	}
}

// The adopt probe matches on name, and the prior record's own path is the one copy whose
// bytes are known to be another version: the lockfile hashed them as that version. A
// record whose path already carries the new version's name is a hand edit or a stale
// merge, and taking those bytes as the new version on the strength of their name would
// record the old version's content under the new version's number.
func TestAChangedFileNeverAdoptsItsOwnPriorCopy(t *testing.T) {
	lib := t.TempDir()
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	version := "v1_0_0"
	srv := twoFileServer(t, &version, func(v string) string { return "POLYGON_Pirate_Godot_4_5_1_" + v + ".zip" })
	opts := twoPackOpts(lib)
	lf := seedRun(t, srv, lockPath, opts)

	// Move the v1 bytes to the name v2 would have, and record them there, still as v1.
	prior := lf.Packs["polygon-pirate-pack"].Files[pirateKey]
	renamed := "POLYGON_Pirate/POLYGON_Pirate_Godot_4_5_1_v2_0_0.zip"
	if err := os.Rename(filepath.Join(lib, filepath.FromSlash(prior.CachePath)), filepath.Join(lib, filepath.FromSlash(renamed))); err != nil {
		t.Fatal(err)
	}
	lf = withPirateEntry(lf, func(f *lockfile.File) { f.CachePath = "./" + renamed })

	version = "v2_0_0"
	rep, err := Run(context.Background(), newClient(srv.URL), lf, lockPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range rep.Adopted {
		if a.FileID == prior.FileID {
			t.Fatal("the prior copy was adopted as the new version on the strength of its name")
		}
	}
	if got := rep.NewLockfile.Packs["polygon-pirate-pack"].Files[pirateKey]; got.SHA256 == prior.SHA256 {
		t.Errorf("v2 is recorded with v1's sha: %+v", got)
	}
}

// SamePath cannot see two spellings that a case-insensitive filesystem calls one file,
// so the prune asks the filesystem too. A hard link stands in for that here: two names
// the filesystem reports as one file, on a platform where case alone would not be.
func TestRemoveSupersededSparesAPathTheFilesystemCallsTheSameFile(t *testing.T) {
	lib := t.TempDir()
	dir := filepath.Join(lib, "TOK")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.zip"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "new.zip"), filepath.Join(dir, "Old.zip")); err != nil {
		t.Skipf("cannot hard-link here: %v", err)
	}
	if w := removeSuperseded(lib, "TOK/Old.zip", "TOK/new.zip", nil); w != "" {
		t.Errorf("warning = %q", w)
	}
	if !cacheFileExists(lib, "TOK/Old.zip") {
		t.Error("removeSuperseded deleted a name the filesystem reports as the current file")
	}
}
