package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sample is a manifest with more than one pack and more than one include, so a write
// that loses ordering or drops a field has somewhere to show it.
func sample() Manifest {
	return Manifest{
		VariantIncludes: []string{"Godot_*", "SourceFiles"},
		Packs: []Entry{
			{Slug: "polygon-pirate-pack", Name: "POLYGON - Pirate Pack", Enabled: true},
			{Slug: "elven-warriors", Name: "Elven Warriors", Enabled: false},
			{Slug: "polygon-dungeon-pack", Name: "POLYGON - Dungeon Pack", Enabled: true},
		},
	}
}

// The manifest is committed and travels with the consuming project, exactly like the
// lockfile, so its write is two-phase for the same reason: a reader never sees a
// half-written allowlist, and a failure never leaves the previous one truncated. This
// file holds the user's pack selection, so losing it means choosing again by hand.
// The lockfile has had this guard; its twin did not.
func TestFailedSaveLeavesThePriorFileAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	const prior = "variant_includes = [\"Godot_4_5_1\"]\n"
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
		t.Errorf("the prior manifest was disturbed by a failed Save:\n%s", got)
	}
	os.Chmod(dir, 0o700)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".synty-sync-") {
			t.Errorf("a staging file was left behind: %s", e.Name())
		}
	}
}

// The file is committed, so its diff is the changelog: saving the same selection twice
// must produce identical bytes, whatever order the caller happened to hold the packs
// in. Save sorts for exactly this reason, and a set rendered in encounter order would
// churn every line on every `select`, burying the one the user actually changed.
func TestSaveIsByteStableAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.toml"), filepath.Join(dir, "b.toml")
	if err := Save(a, sample()); err != nil {
		t.Fatal(err)
	}
	reversed := sample()
	for i, j := 0, len(reversed.Packs)-1; i < j; i, j = i+1, j-1 {
		reversed.Packs[i], reversed.Packs[j] = reversed.Packs[j], reversed.Packs[i]
	}
	if err := Save(b, reversed); err != nil {
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
		t.Errorf("two Saves of the same selection differ:\n--- a ---\n%s\n--- b ---\n%s", ab, bb)
	}
}

// This file is committed and hand-edited, so a duplicate [[pack]] arrives from a merge
// that kept both sides. The two readers then disagree: EnabledSet enables the pack if
// either block says so, so sync mirrors it, while Reconcile keeps the last block, so the
// select page renders it unchecked. Saving from that page collapses the pair to disabled
// and the pack silently stops being mirrored.
func TestTwoEntriesForOnePackAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	body := "[[pack]]\n  slug = \"polygon-pirate-pack\"\n  name = \"Pirate\"\n  enabled = true\n\n" +
		"[[pack]]\n  slug = \"polygon-pirate-pack\"\n  name = \"Pirate\"\n  enabled = false\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("a manifest with two entries for one pack loaded without complaint")
	}
	if !strings.Contains(err.Error(), "polygon-pirate-pack") {
		t.Errorf("error %q does not name the duplicated slug", err)
	}
}
