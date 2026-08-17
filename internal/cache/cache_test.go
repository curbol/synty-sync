package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both components of a cache path are portal-derived: the filename from the signed
// URL, the token from item-page label text. They go through one guard, so they are
// tested as one table — a third component added to safeIdentity cannot then be
// covered in only half the cases.
func TestStoreRejectsUnsafePathComponents(t *testing.T) {
	for _, tc := range []struct{ what, token, filename string }{
		{"filename traversal", "TOKEN", "../evil.zip"},
		{"filename nested", "TOKEN", "sub/evil.zip"},
		{"filename dotdot", "TOKEN", ".."},
		{"filename dot", "TOKEN", "."},
		{"filename empty", "TOKEN", ""},
		{"token dotdot", "..", "pack.zip"},
		{"token traversal", "../escaped", "pack.zip"},
		{"token nested", "a/b", "pack.zip"},
		{"token empty", "", "pack.zip"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "library")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := Store(root, tc.token, tc.filename, strings.NewReader("x")); err == nil {
				t.Errorf("Store accepted token=%q filename=%q", tc.token, tc.filename)
			}
			for _, escaped := range []string{filepath.Join(base, "evil.zip"), filepath.Join(base, "escaped")} {
				if _, err := os.Stat(escaped); err == nil {
					t.Errorf("a write escaped the library root to %s", escaped)
				}
			}
		})
	}

	// A bare token and filename still store normally.
	root := t.TempDir()
	rel, _, _, err := Store(root, "TOKEN", "pack.zip", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Store rejected a valid path: %v", err)
	}
	if rel != "TOKEN/pack.zip" {
		t.Errorf("relPath = %q, want TOKEN/pack.zip", rel)
	}
}

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

func TestLocate(t *testing.T) {
	lib := t.TempDir()
	layout := map[string]string{ // fileToken -> filename already under <fileToken>/
		"POLYGON_Dungeon":               "POLYGON_Dungeon_Godot_4_5_1_v1_0_1.zip",
		"POLYGON_Pirate":                "POLYGON_Pirate_Unity_2022_3_v1_6_1.unitypackage",     // Unity => .unitypackage
		"INTERFACE_Fantasy_Warrior_HUD": "INTERFACE_Fantasy_Warrior_HUD_Source_Sprites_v3.zip", // Source_Sprites vs SourceSprites
	}
	for token, name := range layout {
		dir := filepath.Join(lib, token)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("z"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		w    Wanted
		want string // expected relPath; "" = not found
	}{
		{"zip", Wanted{FileToken: "POLYGON_Dungeon", Variant: "Godot_4_5_1", Version: "v1_0_1"}, "POLYGON_Dungeon/POLYGON_Dungeon_Godot_4_5_1_v1_0_1.zip"},
		{"unitypackage", Wanted{FileToken: "POLYGON_Pirate", Variant: "Unity_2022_3", Version: "v1_6_1"}, "POLYGON_Pirate/POLYGON_Pirate_Unity_2022_3_v1_6_1.unitypackage"},
		{"variant rendering diff", Wanted{FileToken: "INTERFACE_Fantasy_Warrior_HUD", Variant: "SourceSprites", Version: "v3"}, "INTERFACE_Fantasy_Warrior_HUD/INTERFACE_Fantasy_Warrior_HUD_Source_Sprites_v3.zip"},
		{"wrong version", Wanted{FileToken: "POLYGON_Dungeon", Variant: "Godot_4_5_1", Version: "v2_0_0"}, ""},
		{"missing dir", Wanted{FileToken: "NOPE", Variant: "Godot_4_5_1", Version: "v1"}, ""},
	}
	for _, c := range cases {
		rel, ok := Locate(lib, c.w)
		if c.want == "" {
			if ok {
				t.Errorf("%s: located %q, want not found", c.name, rel)
			}
			continue
		}
		if !ok || rel != c.want {
			t.Errorf("%s: Locate = %q,%v; want %q,true", c.name, rel, ok, c.want)
		}
	}
}
