package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/synty-sync/internal/model"
)

func TestReconcilePreservesEnabledAddsNewDisabled(t *testing.T) {
	m := Manifest{Packs: []Entry{
		{Slug: "a", Name: "A", Enabled: true},
		{Slug: "gone", Name: "Gone", Enabled: true},
	}}
	m.Reconcile([]model.Pack{
		{Slug: "b", DisplayName: "B"}, // newly owned
		{Slug: "a", DisplayName: "A"}, // still owned, was enabled
	})
	if len(m.Packs) != 2 {
		t.Fatalf("packs = %d, want 2 (gone dropped): %+v", len(m.Packs), m.Packs)
	}
	got := map[string]bool{}
	for _, e := range m.Packs {
		got[e.Slug] = e.Enabled
	}
	if !got["a"] {
		t.Error("a should stay enabled")
	}
	if got["b"] {
		t.Error("newly-owned b should default disabled (opt-in)")
	}
	if _, ok := got["gone"]; ok {
		t.Error("no-longer-owned pack should drop out")
	}
}

func TestEnabledSetAndSetEnabled(t *testing.T) {
	m := Manifest{Packs: []Entry{{Slug: "a"}, {Slug: "b"}, {Slug: "c"}}}
	m.SetEnabled(map[string]bool{"a": true, "c": true})
	set := m.EnabledSet()
	if !set["a"] || set["b"] || !set["c"] {
		t.Errorf("enabled set = %+v", set)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	in := Manifest{
		VariantIncludes: []string{"Unity_*", "SourceFiles"},
		Packs: []Entry{
			{Slug: "polygon-pirate-pack", Name: "POLYGON - Pirate Pack", Enabled: true},
			{Slug: "animation-base", Name: "ANIMATION - Base", Enabled: false},
		},
	}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by slug on save: animation-base before polygon-pirate-pack.
	if out.Packs[0].Slug != "animation-base" {
		t.Errorf("not sorted by slug: %+v", out.Packs)
	}
	if !out.EnabledSet()["polygon-pirate-pack"] {
		t.Error("enabled flag lost in round-trip")
	}
	if len(out.VariantIncludes) != 2 || out.VariantIncludes[0] != "Unity_*" {
		t.Errorf("variant_includes lost in round-trip: %+v", out.VariantIncludes)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || len(m.Packs) != 0 {
		t.Errorf("missing manifest: %+v err %v", m, err)
	}
}

func TestFilter(t *testing.T) {
	m := Manifest{VariantIncludes: []string{"Godot_*", "SourceFiles", "SourceSprites"}}
	f := m.Filter()
	cases := map[model.Variant]bool{
		"Godot_4_5_1":   true,
		"SourceFiles":   true,
		"SourceSprites": true,
		"Unity_2022_3":  false,
		"Unreal_5_3":    false,
	}
	for v, want := range cases {
		if f(v) != want {
			t.Errorf("filter(%s) = %v, want %v", v, f(v), want)
		}
	}
}

func TestDiscoverWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), []byte("variant_includes = []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := Discover(sub)
	if !ok || got != filepath.Join(root, FileName) {
		t.Errorf("Discover(%q) = %q, %v; want the ancestor manifest", sub, got, ok)
	}
}

func TestDiscoverNotFound(t *testing.T) {
	// A temp dir with no manifest anywhere up to root: expect not found.
	if got, ok := Discover(t.TempDir()); ok {
		t.Errorf("Discover found %q in an empty tree; want not found", got)
	}
}

func TestLockPath(t *testing.T) {
	cases := map[string]string{
		filepath.Join("proj", "synty-sync.toml"): filepath.Join("proj", "synty-sync.lock.json"),
		"synty-sync.toml":                        "synty-sync.lock.json",
		"foo.toml":                               "foo.lock.json",
	}
	for in, want := range cases {
		if got := LockPath(in); got != want {
			t.Errorf("LockPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A misspelled key decodes to nothing and is then deleted from the user's working
// tree by the next Save, while the user sees a misleading "no variant_includes"
// error rather than "unknown key".
func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	if err := os.WriteFile(p, []byte("variant_include = [\"Godot_*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("a misspelled key must be reported, not silently dropped")
	}
	if !strings.Contains(err.Error(), "variant_include") {
		t.Errorf("error should name the offending key, got %q", err)
	}
}

// variant_includes is hand-authored, so it is the likeliest place for a glob typo.
// A bad pattern matches nothing, which looks exactly like "the store has no files
// for your engine".
func TestValidateRejectsMalformedGlob(t *testing.T) {
	if err := (Manifest{VariantIncludes: []string{"Godot_*", "Unity_[4"}}).Validate(); err == nil {
		t.Error("a malformed glob must be reported")
	} else if !strings.Contains(err.Error(), "Unity_[4") {
		t.Errorf("error should name the offending pattern, got %q", err)
	}
	if err := (Manifest{VariantIncludes: []string{"Godot_*", "SourceFiles"}}).Validate(); err != nil {
		t.Errorf("valid patterns rejected: %v", err)
	}
}

// os.CreateTemp opens at 0600 and the rename carries that mode to the destination, so
// rewriting a committed file used to narrow it to owner-only. Git does not track the
// read bits, so the change is invisible in a diff and shows up as a CI step or another
// account that can no longer read the project's manifest.
func TestSaveKeepsTheModeOfTheFileItRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synty-sync.toml")
	if err := os.WriteFile(path, []byte("variant_includes = [\"Godot_*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Manifest{VariantIncludes: []string{"Godot_*"}}); err != nil {
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

// A manifest that does not exist yet is created readable rather than owner-only, since
// it is committed and shared the moment it is written.
func TestSaveCreatesAReadableManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synty-sync.toml")
	if err := Save(path, Manifest{VariantIncludes: []string{"Godot_*"}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode of a newly created manifest = %v, want 0644", got)
	}
}

// Save sorts a copy: the value receiver shares the caller's backing array, so sorting
// in place would reorder a slice the caller still holds.
func TestSaveDoesNotReorderTheCallersSlice(t *testing.T) {
	m := Manifest{
		VariantIncludes: []string{"Godot_*"},
		Packs: []Entry{
			{Slug: "zeta", Name: "Zeta"},
			{Slug: "alpha", Name: "Alpha"},
		},
	}
	if err := Save(filepath.Join(t.TempDir(), "synty-sync.toml"), m); err != nil {
		t.Fatal(err)
	}
	if m.Packs[0].Slug != "zeta" {
		t.Errorf("Save reordered the caller's slice: %+v", m.Packs)
	}
}
