package manifest

import (
	"os"
	"path/filepath"
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
