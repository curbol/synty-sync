package assetindex

import (
	"hash/crc32"
	"os"
	"testing"
)

// wantCRC is the crc32+size fingerprint the scheme must produce for these bytes.
func wantCRC(content string) string {
	return crcFingerprint(crc32.ChecksumIEEE([]byte(content)), int64(len(content)))
}

func fpByName(assets []Asset) map[string]string {
	m := map[string]string{}
	for _, a := range assets {
		m[a.Name] = a.Fingerprint
	}
	return m
}

func TestFingerprintPerSourceKind(t *testing.T) {
	root, mk := libRoot(t)

	writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
		"SourceFiles/Models/Heart.fbx": "FBXHEARTDATA",
	})
	writeUnityPackage(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "aaa-guid", pathname: "Assets/Foo/Heart.prefab", asset: "PREFAB"},
	})
	os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLBSWORD"), 0o644)

	fps := fpByName(mustScan(t, root))

	if got, want := fps["Heart.fbx"], wantCRC("FBXHEARTDATA"); got != want {
		t.Errorf("zip fingerprint = %q, want %q", got, want)
	}
	if got, want := fps["Heart.prefab"], unityFingerprint("aaa-guid"); got != want {
		t.Errorf("unity fingerprint = %q, want %q", got, want)
	}
	if got, want := fps["Sword.glb"], wantCRC("GLBSWORD"); got != want {
		t.Errorf("loose fingerprint = %q, want %q", got, want)
	}
}

// Byte-identical content shares one fingerprint across packs and across the
// zip/loose boundary, so a tag set on one copy applies to every copy.
func TestFingerprintSharedForIdenticalBytes(t *testing.T) {
	root, mk := libRoot(t)
	writeZip(t, mk("synty", "A", "A.zip"), map[string]string{"Models/Tree.fbx": "TREEBYTES"})
	writeZip(t, mk("synty", "B", "B.zip"), map[string]string{"Models/Tree.fbx": "TREEBYTES"})
	os.WriteFile(mk("synty", "C", "loose", "Tree.fbx"), []byte("TREEBYTES"), 0o644)

	want := wantCRC("TREEBYTES")
	for _, a := range mustScan(t, root) {
		if a.Name == "Tree.fbx" && a.Fingerprint != want {
			t.Errorf("%s/%s fingerprint = %q, want %q (identical bytes must share)", a.Pack, a.Name, a.Fingerprint, want)
		}
	}
}

// A cold Build and a Refresh over an unchanged tree yield identical fingerprints,
// and a changed loose file's fingerprint is recomputed.
func TestFingerprintStableAndRefreshRecomputes(t *testing.T) {
	root, mk := libRoot(t)
	cacheDir := t.TempDir()
	loose := mk("explosive", "RPG", "Sword.glb")
	os.WriteFile(loose, []byte("GLBSWORD"), 0o644)
	writeZip(t, mk("synty", "A", "A.zip"), map[string]string{"Models/Tree.fbx": "TREEBYTES"})

	ix, err := Build(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.LoosePrint) == 0 {
		t.Fatal("LoosePrint not populated on Build")
	}
	before := fpByName(ix.Assets)
	if before["Sword.glb"] != wantCRC("GLBSWORD") || before["Tree.fbx"] != wantCRC("TREEBYTES") {
		t.Fatalf("cold fingerprints wrong: %+v", before)
	}

	// A second Build of the same tree is deterministic.
	ix2, err := Build(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fpByName(ix2.Assets); got["Sword.glb"] != before["Sword.glb"] || got["Tree.fbx"] != before["Tree.fbx"] {
		t.Errorf("second Build changed fingerprints: %+v vs %+v", got, before)
	}

	// Refresh over an unchanged tree preserves fingerprints.
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	if got := fpByName(ix.Assets); got["Sword.glb"] != before["Sword.glb"] || got["Tree.fbx"] != before["Tree.fbx"] {
		t.Errorf("Refresh (no change) altered fingerprints: %+v vs %+v", got, before)
	}

	// Changing the loose file's bytes (and size) recomputes its fingerprint on Refresh.
	os.WriteFile(loose, []byte("GLBSWORD-EDITED-LONGER"), 0o644)
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	if got, want := fpByName(ix.Assets)["Sword.glb"], wantCRC("GLBSWORD-EDITED-LONGER"); got != want {
		t.Errorf("Refresh after edit: fingerprint = %q, want %q", got, want)
	}
}

func mustScan(t *testing.T, root string) []Asset {
	t.Helper()
	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	return assets
}
