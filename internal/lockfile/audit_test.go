package lockfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// os.CreateTemp opens at 0600 and the rename carries that mode to the destination, so
// rewriting the lockfile used to narrow it to owner-only. Git does not track the read
// bits, so the change never shows in a diff — it surfaces as a CI step or another
// account that can no longer read the project's lockfile.
func TestSaveKeepsTheModeOfTheFileItRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synty-sync.lock.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode after Save = %v, want 0644", got)
	}
}

// A lockfile that does not exist yet is created readable rather than owner-only, since
// it is committed and shared the moment it is written.
func TestSaveCreatesAReadableLockfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synty-sync.lock.json")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode of a newly created lockfile = %v, want 0644", got)
	}
}

// The write is two-phase so a reader never sees a half-written record and a failure
// never leaves the previous one truncated. A Save into a directory that cannot hold
// the temp must leave the existing file exactly as it was, and leave no temp behind.
func TestFailedSaveLeavesThePriorFileAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "synty-sync.lock.json")
	const prior = "{\n  \"generatedAt\": \"before\"\n}\n"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the directory read-only here: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := Save(path, sample()); err == nil {
		t.Fatal("Save into a directory it cannot write reported success")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != prior {
		t.Errorf("the prior lockfile was disturbed by a failed Save:\n%s", got)
	}
	os.Chmod(dir, 0o700)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".synty-lock-") {
			t.Errorf("a staging file was left behind: %s", e.Name())
		}
	}
}

// The file is committed, so its diff is the changelog: two Saves of the same record
// must produce identical bytes. Any map iterated into a slice without sorting, or a
// second timestamp, would churn the file on every run.
func TestSaveIsByteStableAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	if err := Save(a, sample()); err != nil {
		t.Fatal(err)
	}
	if err := Save(b, sample()); err != nil {
		t.Fatal(err)
	}
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ab) != string(bb) {
		t.Errorf("two saves of one record differ:\n%s\n---\n%s", ab, bb)
	}
}
