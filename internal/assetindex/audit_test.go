package assetindex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func libRoot(t *testing.T) (root string, mk func(...string) string) {
	t.Helper()
	root = t.TempDir()
	return root, func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
}

// A personal library is big and accumulates the odd partial copy. One unreadable
// archive must cost that archive, not the whole index — browse treats a build
// failure as fatal and would refuse to start.
func TestBuildSkipsUnreadableArchive(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("good", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	os.WriteFile(mk("bad", "Pack", "Truncated_SourceFiles_v1.zip"), []byte("PK\x03\x04garbage"), 0o644)

	ix, err := Build(root, t.TempDir())
	if err != nil {
		t.Fatalf("one bad archive aborted the build: %v", err)
	}
	if len(ix.Assets) == 0 {
		t.Error("the readable file was dropped along with the bad archive")
	}
	if len(ix.Skipped) != 1 || !strings.Contains(ix.Skipped[0].RelPath, "Truncated") {
		t.Errorf("skipped = %+v, want the truncated zip reported", ix.Skipped)
	}
}

// The index cache is rewritten on every run; a write interrupted partway must not
// leave a half-file that forces a full rebuild of a multi-minute scan.
func TestSaveIsAtomicAndChecked(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "p", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	ix, err := Build(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "browse-index.json")
	if err := ix.Save(cachePath); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("Save left %d files behind, want just the index", len(entries))
	}

	// A directory in place of the cache file cannot be written: Save must say so.
	blocked := filepath.Join(t.TempDir(), "browse-index.json")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ix.Save(blocked); err == nil {
		t.Error("Save reported success writing over a directory")
	}
}

// Every indexed field has to survive the cache round trip; a field that serializes
// away comes back as a silently degraded asset.
func TestSaveLoadPreservesIndexedFields(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_X", "SIDEKICK_X_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Warrior/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_HEAD\n"},
		{guid: "hd1", pathname: "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX", preview: true},
	})
	os.WriteFile(mk("v", "p", "Pic.png"), encodePNG(t, 7, 11), 0o644)

	ix, err := Build(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "browse-index.json")
	if err := ix.Save(cachePath); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(cachePath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range ix.Assets {
		got, ok := loaded.Lookup(want.ID)
		if !ok {
			t.Fatalf("%s missing after reload", want.Name)
		}
		if !sameAsset(got, want) {
			t.Errorf("asset changed across the cache round trip:\n got %+v\nwant %+v", got, want)
		}
	}
}

func sameAsset(a, b Asset) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// A stale cache (older index version, or a different root) must be rebuilt rather
// than served: the fingerprint scheme and the indexed fields move together.
func TestLoadOrBuildRejectsStaleCache(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "p", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	cacheDir := t.TempDir()
	cachePath := filepath.Join(t.TempDir(), "browse-index.json")

	ix, err := LoadOrBuild(root, cacheDir, cachePath, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Version != indexVersion {
		t.Fatalf("built index has version %d", ix.Version)
	}

	var raw map[string]any
	b, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = indexVersion - 1
	raw["assets"] = []any{}
	b, _ = json.Marshal(raw)
	if err := os.WriteFile(cachePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	again, err := LoadOrBuild(root, cacheDir, cachePath, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Assets) == 0 || again.Version != indexVersion {
		t.Errorf("stale cache was reused: version=%d assets=%d", again.Version, len(again.Assets))
	}
}

// A corrupt cache file must fall back to a full build, not fail the command.
func TestLoadOrBuildRebuildsFromCorruptCache(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "p", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	cachePath := filepath.Join(t.TempDir(), "browse-index.json")
	if err := os.WriteFile(cachePath, []byte(`{"assets":[{"id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	ix, err := LoadOrBuild(root, t.TempDir(), cachePath, false, nil)
	if err != nil {
		t.Fatalf("corrupt cache should rebuild, got %v", err)
	}
	if len(ix.Assets) == 0 {
		t.Error("rebuild produced no assets")
	}
}

// A Sidekick character whose part meshes are not in this archive cannot be
// assembled, so its prefab/material/mesh must stay browseable — dropping them as
// superseded byproducts leaves the character with no representation at all.
func TestUnassembledSidekickKeepsItsByproducts(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_X", "SIDEKICK_X_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Warrior/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_ABSENT\n"},
		{guid: "pf1", pathname: "Assets/S/Characters/Warrior/Warrior_01.prefab", asset: "PREFAB", preview: true},
		{guid: "mt1", pathname: "Assets/S/Characters/Warrior/Warrior_01.mat", asset: "MAT"},
	})
	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	byExt := map[string]bool{}
	for _, a := range assets {
		byExt[a.Ext] = true
	}
	if !byExt["prefab"] {
		t.Errorf("the unassembled character lost its prefab; kept only %v", byExt)
	}
}

// The assembled case still supersedes its byproducts.
func TestAssembledSidekickDropsItsByproducts(t *testing.T) {
	root, mk := libRoot(t)
	writeUnityPackage(t, mk("synty", "SIDEKICK_Y", "SIDEKICK_Y_Unity_2021_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "sk1", pathname: "Assets/S/Characters/Warrior/Warrior_01.sk", asset: "Name: Warrior_01\nParts:\n- Name: SK_HEAD\n"},
		{guid: "pf1", pathname: "Assets/S/Characters/Warrior/Warrior_01.prefab", asset: "PREFAB", preview: true},
		{guid: "hd1", pathname: "Assets/S/Resources/SK_HEAD.fbx", asset: "HEADFBX", preview: true},
	})
	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range assets {
		if a.Ext == "prefab" {
			t.Errorf("assembled character kept its superseded prefab: %s", a.RelPath)
		}
	}
}

// glTF animation names are optional and not required to be unique. Two clips with
// the same name build identical Sources, so they collide on both the id the content
// API resolves and the fingerprint tags key on — one card's tag would land on the
// other, and Lookup could only ever reach one of them.
func TestDuplicateClipNamesGetDistinctIdentities(t *testing.T) {
	root, mk := libRoot(t)
	writeGLB(t, mk("Quaternius", "AnimLib", "anims.glb"), "Walk", "Walk", "", "Run")

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 4 {
		t.Fatalf("got %d assets, want one per clip: %+v", len(assets), assets)
	}
	ids, fps, names := map[string]int{}, map[string]int{}, map[string]int{}
	for _, a := range assets {
		ids[a.ID]++
		fps[a.Fingerprint]++
		names[a.Name]++
	}
	for id, n := range ids {
		if n > 1 {
			t.Errorf("%d assets share id %s", n, id)
		}
	}
	for fp, n := range fps {
		if n > 1 {
			t.Errorf("%d assets share fingerprint %s (tagging one would tag the other)", n, fp)
		}
	}
	for name, n := range names {
		if name == "" {
			t.Error("an unnamed clip kept an empty name")
		}
		if n > 1 {
			t.Errorf("%d clips still display as %q", n, name)
		}
	}
}

// A hostile archive entry must never reach a filesystem path. Both guards are pure
// and load-bearing, and neither was covered.
func TestArchiveEntryNamesAreRejected(t *testing.T) {
	for _, name := range []string{"../escape.png", "/etc/passwd", "a/../../escape.png", "..", ""} {
		if safeEntry(name) {
			t.Errorf("zip entry %q accepted", name)
		}
	}
	for _, ok := range []string{"Assets/Sword.fbx", "a.png"} {
		if !safeEntry(ok) {
			t.Errorf("ordinary zip entry %q rejected", ok)
		}
	}
	for _, name := range []string{"../x/asset", "../asset", "a/b/asset"} {
		if guid, _, ok := splitUnityName(name); ok && (guid == ".." || strings.ContainsAny(guid, `/\`)) {
			t.Errorf("unitypackage name %q yielded an escaping guid %q", name, guid)
		}
	}
}

// A .unitypackage is a gzipped tar from an untrusted-ish archive; a malformed one
// must be skipped like any other bad archive, not panic or abort the build.
func TestBuildSkipsMalformedUnityPackage(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("good", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	// Valid gzip header, garbage tar inside.
	os.WriteFile(mk("bad", "Pack", "Broken_Unity_2022_3_v1.unitypackage"),
		[]byte("\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xffgarbage"), 0o644)

	ix, err := Build(root, t.TempDir())
	if err != nil {
		t.Fatalf("a malformed unitypackage aborted the build: %v", err)
	}
	if len(ix.Assets) == 0 {
		t.Error("the readable file was dropped along with the bad archive")
	}
	if len(ix.Skipped) != 1 || !strings.Contains(ix.Skipped[0].RelPath, "Broken") {
		t.Errorf("skipped = %+v, want the malformed unitypackage reported", ix.Skipped)
	}
}

// Every pack update writes a new extraction dir keyed on the archive's mtime.
// Nothing removed the old one, so each update stranded hundreds of MB.
func TestPruneUnpackedDropsStaleExtractions(t *testing.T) {
	root, mk := libRoot(t)
	os.WriteFile(mk("v", "Pack", "Sword.glb"), []byte("GLBBYTES"), 0o644)
	cacheDir := t.TempDir()

	ix, err := Build(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	unpacked := filepath.Join(cacheDir, "unpacked")
	stale := filepath.Join(unpacked, "deadbeefdeadbeef")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "asset"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ix.PruneUnpacked(); err != nil {
		t.Fatalf("PruneUnpacked: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale extraction %s survived the prune (err=%v)", stale, err)
	}
}
