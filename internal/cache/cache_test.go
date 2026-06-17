package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreAndVerify(t *testing.T) {
	lib := t.TempDir()
	rel, sha, size, err := Store(lib, "POLYGON_Pirate", "POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != "POLYGON_Pirate/POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip" {
		t.Errorf("relPath = %q", rel)
	}
	want := sha256.Sum256([]byte("hello"))
	if sha != hex.EncodeToString(want[:]) || size != 5 {
		t.Errorf("sha/size = %s/%d", sha, size)
	}
	if got, _ := os.ReadFile(filepath.Join(lib, filepath.FromSlash(rel))); string(got) != "hello" {
		t.Errorf("stored content = %q", got)
	}
	if !Verify(lib, rel, sha) {
		t.Error("Verify should pass for stored file")
	}
	if Verify(lib, rel, "deadbeef") {
		t.Error("Verify should fail on sha mismatch")
	}
	if !Exists(lib, rel) {
		t.Error("Exists should be true")
	}
	if err := Remove(lib, rel); err != nil || Exists(lib, rel) {
		t.Errorf("Remove failed: err=%v exists=%v", err, Exists(lib, rel))
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Join(lib, "POLYGON_Pirate"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".synty-dl-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestMigrateNormalizedMatch(t *testing.T) {
	lib := t.TempDir()
	flat := []string{
		"INTERFACE_Fantasy_Warrior_HUD_Source_Sprites_v3.zip", // variant Source_Sprites vs token SourceSprites
		"GENERIC_Particle_FX_Godot_4_5_1_v1_0_0(1).zip",       // collision suffix
		"POLYGON_Dungeon_Godot_4_5_1_v1_0_1.zip",
		"SOMETHING_unowned_v9.zip", // no match -> left in place
	}
	for _, name := range flat {
		if err := os.WriteFile(filepath.Join(lib, name), []byte("z"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wanted := []Wanted{
		{FileID: 1, FileToken: "INTERFACE_Fantasy_Warrior_HUD", Variant: "SourceSprites", Version: "v3"},
		{FileID: 2, FileToken: "GENERIC_Particle_FX", Variant: "Godot_4_5_1", Version: "v1_0_0"},
		{FileID: 3, FileToken: "POLYGON_Dungeon", Variant: "Godot_4_5_1", Version: "v1_0_1"},
	}
	results, err := Migrate(lib, wanted)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("matched %d, want 3: %+v", len(results), results)
	}
	for _, r := range results {
		if !Exists(lib, r.RelPath) {
			t.Errorf("migrated file missing at %s", r.RelPath)
		}
		if Exists(lib, r.From) { // no longer at flat root
			t.Errorf("source still at root: %s", r.From)
		}
	}
	// Unmatched zip stays at the root.
	if _, err := os.Stat(filepath.Join(lib, "SOMETHING_unowned_v9.zip")); err != nil {
		t.Errorf("unmatched zip should remain: %v", err)
	}
}
