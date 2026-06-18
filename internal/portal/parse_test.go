package portal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/curbol/synty-sync/internal/model"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "portal", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func findFile(files []model.FileEntry, token string, variant model.Variant) (model.FileEntry, bool) {
	for _, f := range files {
		if f.FileToken == token && f.Variant == variant {
			return f, true
		}
	}
	return model.FileEntry{}, false
}

func findByID(files []model.FileEntry, id int) (model.FileEntry, bool) {
	for _, f := range files {
		if f.FileID == id {
			return f, true
		}
	}
	return model.FileEntry{}, false
}

func TestHasLibrarySentinel(t *testing.T) {
	cases := map[string]bool{
		"library_p1.html":                  true,
		"library_p5.html":                  true,
		"library_empty_authenticated.html": true,  // empty overflow page: search box, no heading
		"library_logout_shell.html":        false, // logged out: no Sky Pilot UI
		"item_1.html":                      true,  // item pages are authenticated too (not enumerated)
	}
	for name, want := range cases {
		got, err := HasLibrarySentinel(read(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: sentinel = %v, want %v", name, got, want)
		}
	}
}

func TestParseLibraryAllPages(t *testing.T) {
	total := 0
	var all []model.Pack
	for i := 1; i <= 5; i++ {
		packs, err := ParseLibraryPage(read(t, fmt.Sprintf("library_p%d.html", i)))
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		total += len(packs)
		all = append(all, packs...)
	}
	if total != 61 {
		t.Errorf("total packs = %d, want 61", total)
	}
	// Empty authenticated page yields zero packs (terminator), no error.
	empty, err := ParseLibraryPage(read(t, "library_empty_authenticated.html"))
	if err != nil || len(empty) != 0 {
		t.Errorf("empty page: got %d packs, err %v; want 0, nil", len(empty), err)
	}
	// Slug derivation handles the Unicode en-dash names deterministically.
	wantSlugs := map[string]bool{
		"polygon-pirate-pack":                         false,
		"fantasy-knights-sidekick-modular-characters": false, // uses en-dash in display name
		"elven-warriors-sidekick-modular-characters":  false,
	}
	for _, p := range all {
		if _, ok := wantSlugs[p.Slug]; ok {
			wantSlugs[p.Slug] = true
		}
	}
	for slug, seen := range wantSlugs {
		if !seen {
			t.Errorf("expected pack slug %q not found", slug)
		}
	}
}

func TestParseItemPirate(t *testing.T) {
	files, err := ParseItemPage(read(t, "item_1.html"), "polygon-pirate-pack")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Five asset files; the .png icon row is skipped.
	if len(files) != 5 {
		t.Fatalf("got %d files, want 5: %+v", len(files), files)
	}
	godot, ok := findFile(files, "POLYGON_Pirate", "Godot_4_5_1")
	if !ok {
		t.Fatal("POLYGON_Pirate Godot_4_5_1 not found")
	}
	if godot.Version != "v1_0_1" || godot.FileID != 2282645 {
		t.Errorf("godot entry = %+v, want version v1_0_1 fileId 2282645", godot)
	}
	if godot.SizeBytes <= 0 {
		t.Errorf("godot size not parsed: %d", godot.SizeBytes)
	}
	bundled, ok := findFile(files, "GENERIC_Particle_FX", "Godot_4_5_1")
	if !ok {
		t.Fatal("bundled GENERIC_Particle_FX not found")
	}
	if bundled.FileID != 2344711 || bundled.Version != "v1_0_0" {
		t.Errorf("bundled entry = %+v, want fileId 2344711 version v1_0_0", bundled)
	}
}

func TestParseItemSourceSpritesSplitLess(t *testing.T) {
	files, err := ParseItemPage(read(t, "item_5.html"), "interface-dark-fantasy-hud")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f, ok := findFile(files, "INTERFACE_Dark_Fantasy_HUD", "SourceSprites")
	if !ok {
		t.Fatalf("split-less SourceSprites row not recovered: %+v", files)
	}
	if f.Version == "" {
		t.Errorf("SourceSprites version empty: %+v", f)
	}
}

func TestParseItemArchivedFlag(t *testing.T) {
	files, err := ParseItemPage(read(t, "item_2.html"), "polygon-alpine-mountain-nature-biome")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var sawArchived bool
	for _, f := range files {
		if f.Archived {
			sawArchived = true
			if f.Version == "" {
				t.Errorf("archived entry has empty version: %+v", f)
			}
		}
	}
	if !sawArchived {
		t.Errorf("expected an ARCHIVED variant in Alpine Mountain: %+v", files)
	}
}

func TestParseItemSkipsUnknownVariant(t *testing.T) {
	// Synty has a real typo "Ureal_5_3" (missing the n). An unrecognized variant
	// keyword is skipped, not fatal; the recognizable row still parses.
	html := []byte(`<div class='sky-pilot-list-item'>
	  <div class='sky-pilot-file-heading'>POLYGON_Holiday_2025_Ureal_5_3 | v1_0_0 <span class='sky-pilot-file-size'>(50 MB)</span></div>
	  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/111?x=1'>Download</a></div>
	</div>
	<div class='sky-pilot-list-item'>
	  <div class='sky-pilot-file-heading'>POLYGON_Holiday_2025_Godot_4_5_1 | v1_0_0 <span class='sky-pilot-file-size'>(40 MB)</span></div>
	  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/222?x=1'>Download</a></div>
	</div>`)
	files, err := ParseItemPage(html, "polygon-holiday-2025")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1 (Ureal skipped): %+v", len(files), files)
	}
	if files[0].Variant != "Godot_4_5_1" || files[0].FileID != 222 {
		t.Errorf("kept wrong row: %+v", files[0])
	}
}

func TestBundledFileSharedAcrossPacks(t *testing.T) {
	// The same fileId 2344711 is bundled under Pirate (item_1), Dungeon (item_4),
	// and Fantasy Kingdom (item_6); dedup keys on this.
	for _, name := range []string{"item_1.html", "item_4.html", "item_6.html"} {
		files, err := ParseItemPage(read(t, name), "x")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := findByID(files, 2344711); !ok {
			t.Errorf("%s: expected bundled fileId 2344711", name)
		}
	}
}
