package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	storeCommitted(t, root, "TOKEN", "other.zip", "bytes")
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
		if e.Name() != "other.zip" {
			t.Errorf("left behind %q after a failed download", e.Name())
		}
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

	for _, rel := range []string{"../outside.key", "a/../../outside.key", "/etc/passwd", "..", "", `Z:..\..\outside.key`} {
		t.Run(rel, func(t *testing.T) {
			if Verify(root, rel, 5) {
				t.Errorf("Verify accepted %q outside the root", rel)
			}
			if VerifyDeep(context.Background(), root, rel, "whatever") {
				t.Errorf("VerifyDeep accepted %q outside the root", rel)
			}
			if _, _, err := Hash(context.Background(), root, rel); err == nil {
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
	if !Verify(root, rel, p.Size) || !VerifyDeep(context.Background(), root, rel, p.SHA256) {
		t.Errorf("a stored file at %q is not visible to Verify/VerifyDeep", rel)
	}
	if _, _, err := Hash(context.Background(), root, rel); err != nil {
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

			res, err := Migrate(context.Background(), lib, []Wanted{{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1_0_1"}}, nil)
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
	res, err := Migrate(context.Background(), lib, []Wanted{{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1"}}, nil)
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
	if rel, ok := Locate(lib, want, nil); ok {
		t.Fatalf("Locate adopted an in-flight download at %s", rel)
	}

	// The genuine file alongside it still wins.
	if err := os.WriteFile(filepath.Join(dir, name), []byte("the whole pack"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, ok := Locate(lib, want, nil)
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
	got, err := Migrate(context.Background(), root, []Wanted{{FileID: 1, FileToken: "T", Variant: "Godot_4_5_1", Version: "v1"}}, nil)
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
			moved, err := Migrate(context.Background(), root, []Wanted{w}, nil)
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
			_, locateMatched := Locate(layout, w, nil)

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
			rel, ok := Locate(root, w, nil)
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

	moved, err := Migrate(context.Background(), root, []Wanted{w}, nil)
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

	sha, size, err := Hash(context.Background(), root, pending.RelPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha != pending.SHA256 {
		t.Errorf("Hash = %s, Store recorded %s; the two disagree about the same bytes", sha, pending.SHA256)
	}
	if size != pending.Size || size != int64(len(body)) {
		t.Errorf("Hash size = %d, Store recorded %d, body is %d", size, pending.Size, len(body))
	}
	if !VerifyDeep(context.Background(), root, pending.RelPath, sha) {
		t.Error("VerifyDeep rejects the digest Hash just produced")
	}
}

// Every segment of a recorded path can be an ordinary name and still leave the library,
// when one of them is a symlink: a link is followed like any other directory. The
// lexical check sees nothing wrong with "link/secret.key", so confinement has to be the
// filesystem's, enforced by the call that acts on the path.
func TestASymlinkedSegmentCannotCarryAPathOutOfTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(outside, "secret.key")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	const rel = "link/secret.key"

	if Verify(root, rel, 6) {
		t.Error("Verify followed a symlink out of the root")
	}
	if _, _, err := Hash(context.Background(), root, rel); err == nil {
		t.Error("Hash followed a symlink out of the root")
	}
	if _, err := Head(root, rel, 16); err == nil {
		t.Error("Head followed a symlink out of the root")
	}
	if _, err := Tail(root, rel, 16); err == nil {
		t.Error("Tail followed a symlink out of the root")
	}
	if err := Remove(root, rel); err == nil {
		t.Error("Remove followed a symlink out of the root")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a file outside the library root was deleted through a link: %v", err)
	}

	// The write half has to be no weaker than the read half: a pack stored through the
	// link would be recorded, then refused by every check above, and re-download forever.
	if _, err := Store(root, "link", "pack.zip", strings.NewReader("x")); err == nil {
		t.Error("Store wrote through a symlink out of the root")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("Store left %d entries outside the root, want only the victim: %v", len(entries), entries)
	}
}

// The lexical check has to answer the same way on every platform, because the lockfile
// travels between machines. filepath.Clean keeps a Windows volume name and resolves ".."
// after it, so "Z:..\..\x" passes a leading-".." test there; and a backslash is a
// separator on one machine and an ordinary byte on another. Both are refused outright.
func TestCanonicalRefusesWhatOnlyOnePlatformWouldResolve(t *testing.T) {
	for _, rel := range []string{
		`Z:..\..\outside.key`,
		`Z:../outside.key`,
		`a\..\..\outside.key`,
		`C:/Windows/win.ini`,
		`TOK/con.zip`,
		`NUL/pack.zip`,
		"./TOK/../../outside.key",
	} {
		if got, err := Canonical(rel); err == nil {
			t.Errorf("Canonical(%q) = %q, want a refusal", rel, got)
		}
		if err := Remove(t.TempDir(), rel); err == nil {
			t.Errorf("Remove accepted %q", rel)
		}
	}
	for rel, want := range map[string]string{
		"TOK/pack.zip":      "TOK/pack.zip",
		"./TOK/pack.zip":    "TOK/pack.zip",
		"TOK//pack.zip":     "TOK/pack.zip",
		"TOK/x/../pack.zip": "TOK/pack.zip",
		"TOK/console.zip":   "TOK/console.zip",
		"TOK/con_extra.zip": "TOK/con_extra.zip",
	} {
		if got, err := Canonical(rel); err != nil || got != want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", rel, got, err, want)
		}
	}
}

// A library big enough to move onto another disk is exactly the one whose root becomes a
// symlink. WalkDir over the path Lstats the root, sees a link rather than a directory,
// and descends nothing, so abandoned temps were never reclaimed there, silently.
func TestSweepTempsDescendsASymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	stale := filepath.Join(real, "TOK", tempPrefix+"abandoned")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "library")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	if count, _ := SweepTemps(link, time.Hour); count != 1 {
		t.Errorf("SweepTemps through a symlinked root removed %d temps, want 1", count)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the abandoned temp under a symlinked root survived the sweep")
	}
}

// Store creates <fileToken>/ for the temp. A file whose download never succeeds would
// otherwise leave an empty directory behind on every attempt, in a tree quarry walks and
// nothing ever prunes.
func TestARejectedStoreLeavesNoDirectoryBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reject func(t *testing.T, root string)
	}{
		{"the copy fails", func(t *testing.T, root string) {
			_, err := Store(root, "TOKEN", "pack.zip", io.MultiReader(
				strings.NewReader("partial"), errReader{errors.New("connection reset")}))
			if err == nil {
				t.Fatal("expected the copy failure to surface")
			}
		}},
		{"the caller discards", func(t *testing.T, root string) {
			p, err := Store(root, "TOKEN", "pack.zip", strings.NewReader("rejected"))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Discard(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.reject(t, root)
			if _, err := os.Stat(filepath.Join(root, "TOKEN")); !os.IsNotExist(err) {
				t.Errorf("an empty <fileToken>/ was left behind: %v", err)
			}
		})
	}

	// A directory that already holds a cached file is not Store's to remove.
	root := t.TempDir()
	storeCommitted(t, root, "TOKEN", "kept.zip", "bytes")
	p, err := Store(root, "TOKEN", "pack.zip", strings.NewReader("rejected"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "TOKEN", "kept.zip")); err != nil {
		t.Errorf("Discard took a populated directory with it: %v", err)
	}
}

// Both matchers pick one copy out of every name that normalizes onto a wanted file, and
// the caller's checks used to run on that one copy afterwards. A truncated canonical
// name then masked an intact "(1)" beside it: the preferred copy was refused, the other
// was never examined, and the file re-downloaded in full. The checks go into the
// selection, in preference order, so the first acceptable copy wins.
func TestTheMatchersTryTheNextCopyWhenThePreferredOneIsRefused(t *testing.T) {
	w := Wanted{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1_0_1"}
	const canonical = "TOK_Godot_4_5_1_v1_0_1.zip"
	const collision = "TOK_Godot_4_5_1_v1_0_1(1).zip"
	refuseCanonical := func(rel string) bool { return path.Base(rel) != canonical }

	t.Run("Locate", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, w.FileToken)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{canonical, collision} {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if rel, ok := Locate(root, w, refuseCanonical); !ok || rel != RelPath(w.FileToken, collision) {
			t.Errorf("Locate = %q, %v; want the acceptable %q", rel, ok, collision)
		}
		if rel, ok := Locate(root, w, nil); !ok || rel != RelPath(w.FileToken, canonical) {
			t.Errorf("Locate with no check = %q, %v; want the canonical %q", rel, ok, canonical)
		}
		if rel, ok := Locate(root, w, func(string) bool { return false }); ok {
			t.Errorf("Locate returned %q though every copy was refused", rel)
		}
	})
	t.Run("Migrate", func(t *testing.T) {
		root := t.TempDir()
		for _, n := range []string{canonical, collision} {
			if err := os.WriteFile(filepath.Join(root, n), []byte(n), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		moved, err := Migrate(context.Background(), root, []Wanted{w}, refuseCanonical)
		if err != nil {
			t.Fatal(err)
		}
		if len(moved) != 1 || moved[0].From != collision {
			t.Fatalf("Migrate moved %+v, want only the acceptable %q", moved, collision)
		}
		// The refused copy is left flat, where the user can see it; moving it into the
		// layout would put bytes nothing records where the adopt scan looks.
		if _, err := os.Stat(filepath.Join(root, canonical)); err != nil {
			t.Errorf("the refused copy was moved: %v", err)
		}
	})
}

// Migrate moves files through the root like every other write here, so a <fileToken>/
// that is a symlink out of the library cannot take a flat file with it.
func TestMigrateCannotMoveAFileOutThroughASymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "TOK")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	const name = "TOK_Godot_4_5_1_v1.zip"
	if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	moved, _ := Migrate(context.Background(), root, []Wanted{{FileID: 1, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1"}}, nil)
	if len(moved) != 0 {
		t.Errorf("Migrate reported moving %+v through a link out of the root", moved)
	}
	if _, err := os.Stat(filepath.Join(outside, name)); err == nil {
		t.Error("Migrate moved a flat file out of the library through a symlink")
	}
}

// A full verify reads the whole library back, one multi-gigabyte pack per call, so an
// interrupt has to land inside a hash rather than after it.
func TestHashStopsWhenTheRunDoes(t *testing.T) {
	root := t.TempDir()
	p := storeCommitted(t, root, "TOK", "pack.zip", strings.Repeat("x", 1<<20))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Hash(ctx, root, p.RelPath); !errors.Is(err, context.Canceled) {
		t.Errorf("Hash on a cancelled run = %v, want context.Canceled", err)
	}
	if VerifyDeep(ctx, root, p.RelPath, p.SHA256) {
		t.Error("VerifyDeep vouched for a file it was told to stop reading")
	}
}
