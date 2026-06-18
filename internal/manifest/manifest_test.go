package manifest

import (
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
	p := filepath.Join(dir, "packs.toml")
	in := Manifest{Packs: []Entry{
		{Slug: "polygon-pirate-pack", Name: "POLYGON - Pirate Pack", Enabled: true},
		{Slug: "animation-base", Name: "ANIMATION - Base", Enabled: false},
	}}
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
}

func TestLoadMissingIsEmpty(t *testing.T) {
	m, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || len(m.Packs) != 0 {
		t.Errorf("missing manifest: %+v err %v", m, err)
	}
}
