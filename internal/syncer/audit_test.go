package syncer

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/synty-sync/internal/lockfile"
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
	if err := os.WriteFile(filepath.Join(lib, flat), []byte("STRAY"), 0o644); err != nil {
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
