package cache

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// storeCommitted writes body into the layout and commits it, which seven tests need
// before they can assert anything about a cached file. Store deliberately stops at a
// temp file, so every one of them otherwise repeats the same two error checks.
func storeCommitted(t *testing.T, root, token, filename, body string) *Pending {
	t.Helper()
	p, err := Store(root, token, filename, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	return p
}

// A download that dies mid-stream must not leave its partial temp file behind.
func TestStoreCleansUpAfterFailedCopy(t *testing.T) {
	root := t.TempDir()
	_, err := Store(root, "TOKEN", "pack.zip", io.MultiReader(
		strings.NewReader("partial"), errReader{errors.New("connection reset")}))
	if err == nil {
		t.Fatal("expected the copy failure to surface")
	}
	entries, err := os.ReadDir(filepath.Join(root, "TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left behind %q after a failed download", e.Name())
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// Store guards the paths it writes, but every accessor below takes a relPath straight
// from the lockfile — a committed file that travels with the consuming project, so its
// contents are not the running user's to trust. An escaping cachePath would make a sync
// delete or stat arbitrary files, and Head and Tail would hand back their contents.
func TestCachePathsCannotEscapeTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(base, "outside.key")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"../outside.key", "a/../../outside.key", "/etc/passwd", "..", ""} {
		t.Run(rel, func(t *testing.T) {
			if Verify(root, rel, 5) {
				t.Errorf("Verify accepted %q outside the root", rel)
			}
			if VerifyDeep(root, rel, "whatever") {
				t.Errorf("VerifyDeep accepted %q outside the root", rel)
			}
			if _, _, err := Hash(root, rel); err == nil {
				t.Errorf("Hash accepted %q outside the root", rel)
			}
			if err := Remove(root, rel); err == nil {
				t.Errorf("Remove accepted %q outside the root", rel)
			}
			if _, err := Head(root, rel, 16); err == nil {
				t.Errorf("Head accepted %q outside the root", rel)
			}
			if _, err := Tail(root, rel, 16); err == nil {
				t.Errorf("Tail accepted %q outside the root", rel)
			}
		})
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a file outside the library root was deleted: %v", err)
	}
}

// A relative path inside the root still works; the guard must not break the
// ordinary case.
func TestCachePathsAcceptOrdinaryRelativePaths(t *testing.T) {
	root := t.TempDir()
	p := storeCommitted(t, root, "TOKEN", "pack.zip", "bytes")
	rel := p.RelPath
	if !Verify(root, rel, p.Size) || !VerifyDeep(root, rel, p.SHA256) {
		t.Errorf("a stored file at %q is not visible to Verify/VerifyDeep", rel)
	}
	if _, _, err := Hash(root, rel); err != nil {
		t.Errorf("Hash(%q): %v", rel, err)
	}
	if err := Remove(root, rel); err != nil {
		t.Errorf("Remove(%q): %v", rel, err)
	}
}

// Migrate matches flat files through the same normalizing matcher Locate uses, so the
// layout copy has to win against every name that matcher calls equal, not just against
// the identical one. A guard that compares the exact filename leaves the (N) collision
// copy, the .unitypackage against a .zip, and the dot- against the underscore-rendered
// version all pointing at a target that does not exist: the rename proceeds, the layout
// ends up holding two copies of one file identity, and the caller hashes the one it just
// moved and records that sha while nothing ever references the other again.
func TestMigrateDoesNotClobberLayoutCopy(t *testing.T) {
	const layoutName = "TOK_Godot_4_5_1_v1_0_1.zip"
	for _, flatName := range []string{
		layoutName,                            // the identical name
		"TOK_Godot_4_5_1_v1_0_1(1).zip",       // a second browser download
		"TOK_Godot_4_5_1_v1_0_1.unitypackage", // the same file in the other container
		"TOK_Godot_4_5_1_v1.0.1.zip",          // the version rendered with dots
	} {
		t.Run(flatName, func(t *testing.T) {
			lib := t.TempDir()
			if err := os.MkdirAll(filepath.Join(lib, "TOK"), 0o755); err != nil {
				t.Fatal(err)
			}
			good := filepath.Join(lib, "TOK", layoutName)
			if err := os.WriteFile(good, []byte("GOOD-COMPLETE-BYTES"), 0o644); err != nil {
				t.Fatal(err)
			}
			flat := filepath.Join(lib, flatName)
			if err := os.WriteFile(flat, []byte("TRUNC"), 0o644); err != nil {
				t.Fatal(err)
			}

			res, err := Migrate(lib, []Wanted{{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1_0_1"}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(good)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "GOOD-COMPLETE-BYTES" {
				t.Errorf("layout copy was overwritten by the flat file: %q", got)
			}
			if len(res) != 0 {
				t.Errorf("reported a migration that did not happen: %+v", res)
			}
			if _, err := os.Stat(flat); err != nil {
				t.Errorf("the unmigrated flat file should be left for the user, got %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(lib, "TOK"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("the layout holds %d copies of one file identity, want 1: %v", len(entries), entries)
			}
		})
	}
}

// Migrate no longer filters by extension, which puts abandoned download temps in
// front of it for the first time. A partial transfer can carry enough of the name to
// normalize onto a wanted file, and moving one into the layout hands the caller a
// truncated body to hash and record as that file's truth.
func TestMigrateSkipsAnAbandonedTemp(t *testing.T) {
	lib := t.TempDir()
	temp := filepath.Join(lib, tempPrefix+"TOK_Godot_4_5_1_v1")
	if err := os.WriteFile(temp, []byte("PARTIAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Migrate(lib, []Wanted{{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("an in-flight download temp was migrated as finished content: %+v", res)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Errorf("the temp was moved out from under a running download: %v", err)
	}
}

// An abandoned download temp can carry enough of a wanted file's name to normalize
// onto it. Adopting one records a truncated body's digest as that file's truth, so
// Locate has to pass it over even when nothing else matches.
func TestLocateSkipsAnAbandonedTemp(t *testing.T) {
	lib := t.TempDir()
	dir := filepath.Join(lib, "POLYGON_Pirate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := Wanted{FileID: 1, FileToken: "POLYGON_Pirate", Variant: "Godot_4_5_1", Version: "v1_0_1"}
	name := "POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip"

	if err := os.WriteFile(filepath.Join(dir, tempPrefix+name), []byte("half a pack"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rel, ok := Locate(lib, want); ok {
		t.Fatalf("Locate adopted an in-flight download at %s", rel)
	}

	// The genuine file alongside it still wins.
	if err := os.WriteFile(filepath.Join(dir, name), []byte("the whole pack"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, ok := Locate(lib, want)
	if !ok {
		t.Fatal("Locate missed the real file sitting beside a temp")
	}
	if rel != "POLYGON_Pirate/"+name {
		t.Errorf("Locate = %q, want the completed download", rel)
	}
}

// SweepTemps is the first thing a run does, so on a fresh install it walks a root
// that does not exist yet. WalkDir hands the callback a nil DirEntry there, and only
// the short-circuit on err keeps d.IsDir() from being reached.
func TestSweepTempsOnAMissingRoot(t *testing.T) {
	count, bytes := SweepTemps(filepath.Join(t.TempDir(), "not-created-yet"), 0)
	if count != 0 || bytes != 0 {
		t.Errorf("SweepTemps = %d files, %d bytes on a missing root; want nothing", count, bytes)
	}
}

// SweepTemps tolerates a library root that does not exist yet, and Migrate has to
// agree: both run as housekeeping on the same first sync, before anything has created
// the directory. Returning the ENOENT ended the run after the whole library had been
// enumerated, so a fresh install could never download anything.
func TestMigrateOnAMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created-yet")
	got, err := Migrate(root, []Wanted{{FileID: 1, FileToken: "T", Variant: "Godot_4_5_1", Version: "v1"}})
	if err != nil {
		t.Errorf("Migrate on a root that does not exist yet: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Migrate reported %d moves against a root with no files", len(got))
	}
}

// A committed download is readable rather than owner-only: the library path is
// configurable, so it can sit on a volume more than one account reads.
func TestStoreCommitsAReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports 0666 for every writable file; there are no mode bits to keep")
	}
	root := t.TempDir()
	storeCommitted(t, root, "T", "x.zip", "PK\x03\x04data")
	fi, err := os.Stat(filepath.Join(root, "T", "x.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("committed cache file mode = %v, want 0644", got)
	}
}

// Migrate and Locate are two halves of one question (is this file on disk the one
// we want), and they have to answer it the same way. Migrate keys on the raw name;
// Locate used to trim an extension before calling normalizeName, which trims one
// itself, so the two disagreed on every name carrying a second dot. Both directions
// cost something real: a name Migrate matches and Locate does not folds into the
// layout once and is then invisible to the adopt scan forever, re-downloading
// gigabytes after a lost lockfile; a name Locate matches and Migrate does not is a
// partial transfer under a foreign extension, which slips the .zip-gated trailer
// check and gets hashed as that file's truth.
func TestMigrateAndLocateAgreeOnEveryName(t *testing.T) {
	w := Wanted{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1_0_1"}
	// A dotted version is what made the key's own extension-stripping visible: it used
	// to run filepath.Ext over "<token>_<variant>_<version>", which takes everything
	// after the last dot anywhere, so the key lost its tail while the disk name kept
	// its own. Both rows below fail without normalizeKey — the first by missing a file
	// that is right there, the second by matching one that is a different version.
	dotted := Wanted{FileID: 8, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1.0.1"}
	truncating := Wanted{FileID: 9, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1.0"}
	for _, tc := range []struct {
		name   string
		wanted Wanted
		want   bool
	}{
		{"TOK_Godot_4_5_1_v1_0_1.zip", w, true},
		{"TOK_Godot_4_5_1_v1_0_1.unitypackage", w, true},
		{"TOK_Godot_4_5_1_v1_0_1.ZIP", w, true}, // the extension's case is not part of the key
		{"TOK_Godot_4_5_1_v1_0_1", w, true},
		{"TOK_Godot_4_5_1_v1_0_1(1).zip", w, true},
		{"TOK_Godot_4_5_1_v1.0.1.zip", w, true},       // version rendered with dots
		{"TOK_Godot_4_5_1_v1_0_1.zip.part", w, false}, // an interrupted browser download
		{"TOK_Godot_4_5_1_v9_9_9.zip", w, false},      // a different version entirely
		{"OTHER_Godot_4_5_1_v1_0_1.zip", w, false},    // a different file
		{"TOK_Godot_4_5_1_v1.0.1.zip", dotted, true},  // the key keeps its own dots
		{"TOK_Godot_4_5_1_v1.zip", truncating, false}, // v1 is not v1.0
	} {
		name, w := tc.name, tc.wanted
		t.Run(name+"/"+w.Version, func(t *testing.T) {
			root := t.TempDir()
			// Migrate's view: the file sits flat at the root.
			if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			moved, err := Migrate(root, []Wanted{w})
			if err != nil {
				t.Fatal(err)
			}
			migrateMatched := len(moved) == 1

			// Locate's view: the same file already sits in the layout.
			layout := t.TempDir()
			if err := os.MkdirAll(filepath.Join(layout, w.FileToken), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(layout, w.FileToken, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, locateMatched := Locate(layout, w)

			if migrateMatched != locateMatched {
				t.Errorf("Migrate matched=%v but Locate matched=%v for %q; one key, two answers",
					migrateMatched, locateMatched, name)
			}
			// Agreement alone is not the property: two matchers that both stopped
			// matching would agree perfectly while every pack on disk re-downloaded.
			if migrateMatched != tc.want {
				t.Errorf("matched=%v, want %v for %q against version %q",
					migrateMatched, tc.want, name, w.Version)
			}
		})
	}
}

// Tail reads from the end, and until now nothing read a file bigger than the window it
// asks for: every archive the suite builds is a ~150-byte zip against the 64KiB the
// end-of-central-directory search wants, so size-n was always 0 and only the clamp ran.
// The seek is what the trailer check rests on. Reading the head instead would reject
// every genuine multi-megabyte pack's EOCD, which stops adoption silently and
// re-downloads gigabytes; worse in the other direction, a truncated pack whose first
// 64KiB happens to hold the signature would be hashed as that file's truth.
func TestTailReadsTheEndOfAFileLargerThanTheWindow(t *testing.T) {
	root := t.TempDir()
	const marker, decoy = "THE-REAL-TRAILER", "NOT-THE-TRAILER"
	body := append([]byte(decoy), make([]byte, 96<<10)...)
	body = append(body, marker...)

	p := storeCommitted(t, root, "T", "big.bin", string(body))

	const window = 1024
	tail, err := Tail(root, p.RelPath, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != window {
		t.Errorf("Tail returned %d bytes, want the %d it was asked for", len(tail), window)
	}
	if !strings.HasSuffix(string(tail), marker) {
		t.Errorf("Tail did not return the end of the file; last bytes were %q", tail[max(len(tail)-len(marker), 0):])
	}
	if strings.Contains(string(tail), decoy) {
		t.Error("Tail returned the head of the file; the seek offset is wrong")
	}
}

// Head is Tail's twin and reads the other end, so the same file pins both.
func TestHeadReadsTheStartOfAFileLargerThanTheWindow(t *testing.T) {
	root := t.TempDir()
	const marker = "PK\x03\x04THE-REAL-HEAD"
	body := append([]byte(marker), make([]byte, 96<<10)...)

	p := storeCommitted(t, root, "T", "big.bin", string(body))

	const window = 512
	head, err := Head(root, p.RelPath, window)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) != window {
		t.Errorf("Head returned %d bytes, want the %d it was asked for", len(head), window)
	}
	if !strings.HasPrefix(string(head), marker) {
		t.Errorf("Head did not return the start of the file, got %q", head[:min(len(head), len(marker))])
	}
}

// Two names in the layout can normalize onto one wanted file: Migrate folds every
// matching flat entry, and "(1)" is exactly what a second copy of one download is
// called. ReadDir is sorted and "(" sorts before ".", so the collision copy used to win
// on punctuation alone, and the adopted bytes were whichever name sorted first rather
// than the canonical one.
func TestLocatePrefersTheCanonicalNameOverACollisionCopy(t *testing.T) {
	w := Wanted{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1_0_1"}
	const canonical = "TOK_Godot_4_5_1_v1_0_1.zip"
	const collision = "TOK_Godot_4_5_1_v1_0_1(1).zip"
	// Written in both orders: a tie-break that follows creation order rather than the
	// name passes one of these by luck.
	for _, order := range [][]string{{canonical, collision}, {collision, canonical}} {
		t.Run(order[0]+" first", func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, w.FileToken)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range order {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rel, ok := Locate(root, w)
			if !ok {
				t.Fatal("Locate found neither copy")
			}
			if rel != RelPath(w.FileToken, canonical) {
				t.Errorf("Locate returned %q, want the canonical %q", rel, canonical)
			}
		})
	}
}

// Migrate used to fold in every flat name that matched a wanted file, so a library
// holding both "pack.zip" and the "pack(1).zip" that a re-download leaves behind moved
// both into the layout. The copy the lockfile does not record is then stranded for
// good — the syncer prunes only the path it recorded, and SweepTemps skips anything
// without the temp prefix — and the caller, which keys results by fileId, had two to
// choose from and took whichever ReadDir yielded last. That is the same coin-flip
// Locate refuses to let "(" sorting before "." decide.
func TestMigrateFoldsOneCopyOfACollisionPairAndLeavesTheOther(t *testing.T) {
	root := t.TempDir()
	w := Wanted{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1_0_1"}
	const canonical = "TOK_Godot_4_5_1_v1_0_1.zip"
	const collision = "TOK_Godot_4_5_1_v1_0_1(1).zip"
	for _, n := range []string{canonical, collision} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("bytes of "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	moved, err := Migrate(root, []Wanted{w})
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 {
		t.Fatalf("Migrate returned %d results for one wanted file: %+v", len(moved), moved)
	}
	if moved[0].From != canonical {
		t.Errorf("folded in %q; the canonical name should win over a (N) copy", moved[0].From)
	}
	if moved[0].RelPath != RelPath(w.FileToken, canonical) {
		t.Errorf("relPath = %q, want %q", moved[0].RelPath, RelPath(w.FileToken, canonical))
	}

	layout, err := os.ReadDir(filepath.Join(root, w.FileToken))
	if err != nil {
		t.Fatal(err)
	}
	if len(layout) != 1 {
		t.Errorf("layout holds %d files, want 1; the unrecorded copy is stranded there", len(layout))
	}
	// The one left behind stays flat, where the user can see it and delete it.
	if _, err := os.Stat(filepath.Join(root, collision)); err != nil {
		t.Errorf("the collision copy should be left flat at the root, not moved: %v", err)
	}
}

// The filename comes from a signed URL or a Content-Disposition, so the store picks it.
// safeName rejected path components but not the prefix the cache reserves for in-flight
// downloads: a file committed as ".synty-dl-*" is deleted by the next day's SweepTemps,
// skipped by Migrate, Locate and the adopt scan, and so re-downloaded on every run for
// as long as the user owns it.
func TestStoreRefusesAFilenameWearingTheTempPrefix(t *testing.T) {
	root := t.TempDir()
	_, err := Store(root, "TOK", tempPrefix+"pack.zip", strings.NewReader("x"))
	if err == nil {
		t.Fatal("Store accepted a filename in the reserved temp namespace")
	}
	if !strings.Contains(err.Error(), tempPrefix) {
		t.Errorf("error %q does not name the reserved prefix", err)
	}
}

// Hash is what adoption writes into the lockfile as a file's permanent truth, and what
// VerifyDeep compares against on every full verify afterwards. Until now it was only
// asserted to return without an error, so a digest computed over the wrong stream —
// the head, a re-opened handle, anything — would have been recorded as that file's sha
// and then agreed with itself forever.
func TestHashAgreesWithWhatStoreRecorded(t *testing.T) {
	root := t.TempDir()
	body := strings.Repeat("some package bytes ", 1000)

	pending, err := Store(root, "TOK", "pack.zip", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.Commit(); err != nil {
		t.Fatal(err)
	}

	sha, size, err := Hash(root, pending.RelPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha != pending.SHA256 {
		t.Errorf("Hash = %s, Store recorded %s; the two disagree about the same bytes", sha, pending.SHA256)
	}
	if size != pending.Size || size != int64(len(body)) {
		t.Errorf("Hash size = %d, Store recorded %d, body is %d", size, pending.Size, len(body))
	}
	if !VerifyDeep(root, pending.RelPath, sha) {
		t.Error("VerifyDeep rejects the digest Hash just produced")
	}
}
