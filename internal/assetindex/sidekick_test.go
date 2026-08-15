package assetindex

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseSidekick(t *testing.T) {
	sk := "" +
		"Name: ElvenWarriors_01\n" +
		"Species: 5\n" +
		"Parts:\n" +
		"- Name: SK_ELVN_BASE_01_01HEAD_EV01\n" +
		"  PartType: Head\n" +
		"  PartVersion: 1\n" +
		"- Name: SK_ELVN_WARR_01_10TORS_HU01\n" +
		"  PartType: Torso\n" +
		"  PartVersion: 1\n" +
		"ColorSet:\n" +
		"- Name: NotAPart\n" +
		"BlendShapes:\n"

	name, parts := parseSidekick([]byte(sk))
	if name != "ElvenWarriors_01" {
		t.Errorf("name = %q, want ElvenWarriors_01", name)
	}
	want := []string{"SK_ELVN_BASE_01_01HEAD_EV01", "SK_ELVN_WARR_01_10TORS_HU01"}
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("parts = %v, want %v (a Name under a later top-level key must not leak in)", parts, want)
	}
}

func TestParseSidekickEmpty(t *testing.T) {
	name, parts := parseSidekick([]byte("Name: Foo\nSpecies: 1\n"))
	if name != "Foo" || len(parts) != 0 {
		t.Errorf("parseSidekick(no Parts) = %q,%v; want Foo,[]", name, parts)
	}
}

// A Sidekick package's .sk data entries become assembled-character assets: model
// category, sidekick thumb, and Source.Parts listing the FBX parts' ids in the same
// package. The character's own id/fingerprint stay tied to the .sk guid (stable).
func TestScanSidekickCharacter(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		return p
	}

	sk := "Name: Warrior_01\nParts:\n- Name: SK_HEAD\n- Name: SK_TORS\n- Name: SK_MISSING\n"
	writeUnityPackage(t, mk("synty", "SIDEKICK_X", "SIDEKICK_X_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/Synty/SidekickCharacters/Characters/Warrior/Warrior_01/Warrior_01.sk", asset: sk},
		{guid: "hd1", pathname: "Assets/Synty/SidekickCharacters/Resources/Meshes/SK_HEAD.fbx", asset: "HEADFBX", preview: true},
		{guid: "to1", pathname: "Assets/Synty/SidekickCharacters/Resources/Meshes/SK_TORS.fbx", asset: "TORSFBX", preview: true},
	})

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var ch *Asset
	for i := range assets {
		if assets[i].Name == "Warrior_01" {
			ch = &assets[i]
		}
	}
	if ch == nil {
		t.Fatal("assembled character Warrior_01 not found")
	}
	if ch.Category != CategoryModel || ch.Thumb != ThumbSidekick {
		t.Errorf("character = %s/%s, want model/sidekick", ch.Category, ch.Thumb)
	}
	if ch.Fingerprint != unityFingerprint("sk1") {
		t.Errorf("fingerprint = %q, want the .sk guid print (stable identity)", ch.Fingerprint)
	}
	// Parts resolve to the two present FBX ids, in .sk order; the missing part is dropped.
	want := []string{
		id(Source{Kind: SourceUnityPackage, ArchivePath: ch.Source.ArchivePath, Guid: "hd1"}),
		id(Source{Kind: SourceUnityPackage, ArchivePath: ch.Source.ArchivePath, Guid: "to1"}),
	}
	if !reflect.DeepEqual(ch.Source.Parts, want) {
		t.Errorf("parts = %v, want %v", ch.Source.Parts, want)
	}
}
