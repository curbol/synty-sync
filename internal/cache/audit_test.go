package cache

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// Store guards the paths it writes, but Verify/Exists/Hash/Remove take a relPath
// straight from the lockfile — a committed file that travels with the consuming
// project, so its contents are not the running user's to trust. An escaping
// cachePath would make a sync delete or stat arbitrary files.
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
	p, err := Store(root, "TOKEN", "pack.zip", strings.NewReader("bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
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

// Migrate matches flat zips by name alone. When a copy is already in the layout and
// the lockfile no longer records it, moving the flat zip over it replaces verified
// bytes with unverified ones — and the caller then hashes the result and records
// that sha, so a truncated download becomes permanently "verified".
func TestMigrateDoesNotClobberLayoutCopy(t *testing.T) {
	lib := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lib, "TOK"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(lib, "TOK", "TOK_Godot_4_5_1_v1.zip")
	if err := os.WriteFile(good, []byte("GOOD-COMPLETE-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(lib, "TOK_Godot_4_5_1_v1.zip")
	if err := os.WriteFile(flat, []byte("TRUNC"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Migrate(lib, []Wanted{{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "GOOD-COMPLETE-BYTES" {
		t.Errorf("layout copy was overwritten by the flat zip: %q", got)
	}
	if len(res) != 0 {
		t.Errorf("reported a migration that did not happen: %+v", res)
	}
	if _, err := os.Stat(flat); err != nil {
		t.Errorf("the unmigrated flat zip should be left for the user, got %v", err)
	}
}

// Synty serves a Unity pack as .unitypackage and everything else as .zip, and an
// exporter can upper-case the extension. Migrate matches on the normalized name,
// which drops the extension, so a filter on one of them silently left the others
// flat at the root to re-download — gigabytes, with nothing said about it.
func TestMigrateFoldsAnyExtension(t *testing.T) {
	for _, name := range []string{
		"TOK_Godot_4_5_1_v1.zip",
		"TOK_Godot_4_5_1_v1.ZIP",
		"TOK_Godot_4_5_1_v1.unitypackage",
		"TOK_Godot_4_5_1_v1(1).zip",
	} {
		t.Run(name, func(t *testing.T) {
			lib := t.TempDir()
			if err := os.WriteFile(filepath.Join(lib, name), []byte("BYTES"), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := Migrate(lib, []Wanted{{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 {
				t.Fatalf("%s was not migrated: %+v", name, res)
			}
			if _, err := os.Stat(filepath.Join(lib, filepath.FromSlash(res[0].RelPath))); err != nil {
				t.Errorf("migrated file missing at %s", res[0].RelPath)
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
	count, bytes := SweepTemps(filepath.Join(t.TempDir(), "not-created-yet"), time.Now())
	if count != 0 || bytes != 0 {
		t.Errorf("SweepTemps = %d files, %d bytes on a missing root; want nothing", count, bytes)
	}
}
