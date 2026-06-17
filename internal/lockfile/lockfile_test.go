package lockfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample() Lockfile {
	return Lockfile{
		GeneratedAt: "2026-06-17T00:00:00Z",
		Packs: map[string]Pack{
			"polygon-pirate-pack": {
				DisplayName: "POLYGON - Pirate Pack", OrderID: 95580704, OrderItemID: 166480940,
				Files: map[string]File{
					"POLYGON_Pirate|Godot_4_5_1": {
						FileToken: "POLYGON_Pirate", Variant: "Godot_4_5_1", Version: "v1_0_1",
						FileID: 2282645, Tracked: true, SHA256: "abc", SizeBytes: 41700000,
						CachePath: "POLYGON_Pirate/POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip",
					},
					"POLYGON_Pirate|Unity_2022_3": {
						FileToken: "POLYGON_Pirate", Variant: "Unity_2022_3", Version: "v1_6_1",
						FileID: 1164794, Tracked: false,
					},
				},
			},
			"animation-base-locomotion": {
				DisplayName: "ANIMATION - Base Locomotion", OrderID: 1, OrderItemID: 2,
				Files: map[string]File{},
			},
		},
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "synty-library.lock.json")
	in := sample()
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Packs) != 2 {
		t.Fatalf("packs = %d, want 2", len(out.Packs))
	}
	got := out.Packs["polygon-pirate-pack"].Files["POLYGON_Pirate|Godot_4_5_1"]
	if got.FileID != 2282645 || got.SHA256 != "abc" || !got.Tracked {
		t.Errorf("round-trip lost data: %+v", got)
	}
}

func TestStableFormatting(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.json")
	p2 := filepath.Join(dir, "b.json")
	if err := Save(p1, sample()); err != nil {
		t.Fatal(err)
	}
	if err := Save(p2, sample()); err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(p1)
	b2, _ := os.ReadFile(p2)
	if string(b1) != string(b2) {
		t.Error("two saves of identical data differ")
	}
	// Keys are sorted: animation pack appears before polygon pack.
	s := string(b1)
	if strings.Index(s, "animation-base-locomotion") > strings.Index(s, "polygon-pirate-pack") {
		t.Error("pack keys not sorted")
	}
	if !strings.HasSuffix(s, "}\n") {
		t.Error("missing trailing newline")
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	lf, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if lf.Packs == nil || len(lf.Packs) != 0 {
		t.Errorf("missing lockfile should load empty, got %+v", lf)
	}
}
