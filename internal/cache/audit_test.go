package cache

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file token names a directory under the library root and comes from item-page
// label text, so it is exactly as untrusted as the download filename beside it.
func TestStoreRejectsUnsafeFileToken(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"..", "../escaped", "a/b", ""} {
		if _, _, _, err := Store(root, token, "pack.zip", strings.NewReader("X")); err == nil {
			t.Errorf("token %q was accepted", token)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "escaped")); err == nil {
		t.Error("a write escaped the library root")
	}
}

// A download that dies mid-stream must not leave its partial temp file behind.
func TestStoreCleansUpAfterFailedCopy(t *testing.T) {
	root := t.TempDir()
	_, _, _, err := Store(root, "TOKEN", "pack.zip", io.MultiReader(
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
			if Exists(root, rel) {
				t.Errorf("Exists reported on %q outside the root", rel)
			}
			if Verify(root, rel, "whatever") {
				t.Errorf("Verify accepted %q outside the root", rel)
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
	rel, sha, _, err := Store(root, "TOKEN", "pack.zip", strings.NewReader("bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if !Exists(root, rel) || !Verify(root, rel, sha) {
		t.Errorf("a stored file at %q is not visible to Exists/Verify", rel)
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

// Migrate considers a .ZIP (case-insensitively) but the name key stripped the
// extension case-sensitively, so it could never match one and silently left
// gigabytes to re-download.
func TestMigrateMatchesUppercaseExtension(t *testing.T) {
	lib := t.TempDir()
	if err := os.WriteFile(filepath.Join(lib, "TOK_Godot_4_5_1_v1.ZIP"), []byte("BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Migrate(lib, []Wanted{{FileID: 7, FileToken: "TOK", Variant: "Godot_4_5_1", Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("a .ZIP was not migrated: %+v", res)
	}
	if !Exists(lib, res[0].RelPath) {
		t.Errorf("migrated file missing at %s", res[0].RelPath)
	}
}
