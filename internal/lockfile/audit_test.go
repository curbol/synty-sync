package lockfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

// os.CreateTemp opens at 0600 and the rename carries that mode to the destination, so
// rewriting the lockfile used to narrow it to owner-only. Git does not track the read
// bits, so the change never shows in a diff — it surfaces as a CI step or another
// account that can no longer read the project's lockfile.
func TestSaveKeepsTheModeOfTheFileItRewrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports 0666 for every writable file; there are no mode bits to keep")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports 0666 for every writable file; there are no mode bits to keep")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory on Windows still accepts new files, so nothing here can fail")
	}
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
	// What makes them stable: encoding/json sorts map keys, and Save appends the
	// newline. A slice written without sorting, or a dropped newline, churns the file.
	out := string(ab)
	if strings.Index(out, "animation-base-locomotion") > strings.Index(out, "polygon-pirate-pack") {
		t.Error("pack keys are not sorted; every run would reorder the file")
	}
	if !strings.HasSuffix(out, "}\n") {
		t.Error("no trailing newline; every run would rewrite the last line")
	}
}

// encoding/json escapes &, < and > by default, which is for embedding in a script tag
// and does nothing for a file on disk. Synty names packs with ampersands, so "Forge &
// Armory" was committed as "Forge & Armory" in the one file whose diff is meant to
// read like a changelog. The rest of the formatting is the indented encoding the file
// has always had, byte for byte, or the first run after the change rewrites every line.
func TestSaveWritesNamesAsTheyAreAndKeepsItsFormatting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synty-sync.lock.json")
	lf := sample()
	lf.Packs["forge"] = Pack{DisplayName: "STYLIZED Forge & Armory <Beta>", Files: map[string]File{}}
	if err := Save(path, lf); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"STYLIZED Forge & Armory <Beta>"`) {
		t.Errorf("the display name was escaped:\n%s", raw)
	}

	want, err := json.MarshalIndent(sample(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want)+"\n" {
		t.Errorf("the encoding changed beyond the escaping:\n%s\n--- want ---\n%s\n", got, want)
	}
}

// A file the run declined has no bytes behind it, so its entry carries no digest,
// on-disk size, cache path or download time. Written as zero values they read as a
// record of an empty file at the root of the cache, and every untracked entry in the
// committed file grows lines of noise. Nothing else asserts the omitempty tags: dropping
// them keeps every round trip green, since the zero values decode to the same struct.
func TestAnUntrackedEntryCarriesOnlyItsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synty-sync.lock.json")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Packs map[string]struct {
			Files map[string]map[string]any `json:"files"`
		} `json:"packs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	files := doc.Packs["polygon-pirate-pack"].Files

	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	untracked := keys(files["POLYGON_Pirate|Unity_2022_3"])
	if want := []string{"fileId", "fileToken", "tracked", "variant", "version"}; !slices.Equal(untracked, want) {
		t.Errorf("untracked entry keys = %v, want only %v", untracked, want)
	}
	// The tracked entry still carries what it set, so the check above cannot pass by
	// reading an entry that lost everything.
	tracked := files["POLYGON_Pirate|Godot_4_5_1"]
	for _, k := range []string{"sha256", "sizeBytes", "cachePath"} {
		if _, ok := tracked[k]; !ok {
			t.Errorf("tracked entry is missing %q: %v", k, keys(tracked))
		}
	}
}
