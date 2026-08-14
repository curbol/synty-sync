package assetindex

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveVariant(t *testing.T) {
	cases := []struct{ pack, file, want string }{
		{"ANIMATION_Base_Locomotion", "ANIMATION_Base_Locomotion_SourceFiles_v3.zip", "SourceFiles"},
		{"ANIMATION_Base_Locomotion", "ANIMATION_Base_Locomotion_Unity_2021_1_v1_1_3.unitypackage", "Unity_2021_1"},
		{"GENERIC_Particle_FX", "GENERIC_Particle_FX_Godot_4_5_1_v1_0_0.zip", "Godot_4_5_1"},
		{"INTERFACE_Dark_Fantasy_HUD", "INTERFACE_Dark_Fantasy_HUD_SourceSprites_v3.zip", "SourceSprites"},
		{"Human_Basic_Motions", "Human Basic Motions.zip", ""}, // convention doesn't hold
	}
	for _, c := range cases {
		if got := deriveVariant(c.pack, c.file); got != c.want {
			t.Errorf("deriveVariant(%q,%q) = %q, want %q", c.pack, c.file, got, c.want)
		}
	}
}

func TestGLBClipSplit(t *testing.T) {
	root := t.TempDir()
	mk := func(p ...string) string {
		full := filepath.Join(append([]string{root}, p...)...)
		os.MkdirAll(filepath.Dir(full), 0o755)
		return full
	}
	writeGLB(t, mk("quaternius", "UAL", "UAL1.glb"), "Walk", "Run", "Idle") // multi-anim -> split into clips
	writeGLB(t, mk("quaternius", "UAL", "UAL1_RM.glb"), "Walk", "Run")      // _RM sibling -> kept whole
	writeGLB(t, mk("quaternius", "UAL", "Prop.glb"), "Spin")                // single anim -> kept whole

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var clips, whole []Asset
	for _, a := range assets {
		if a.Source.Clip != "" {
			clips = append(clips, a)
		} else {
			whole = append(whole, a)
		}
	}

	if len(clips) != 3 {
		t.Fatalf("clip assets = %d, want 3 (Walk/Run/Idle from UAL1.glb)", len(clips))
	}
	ids, fps := map[string]bool{}, map[string]bool{}
	for _, c := range clips {
		if c.Category != CategoryAnimation {
			t.Errorf("clip %q category = %q, want animation", c.Name, c.Category)
		}
		if c.Source.Kind != SourceLoose || c.Source.FilePath == "" {
			t.Errorf("clip %q source = %+v, want loose with FilePath", c.Name, c.Source)
		}
		if c.Source.Clip != c.Name {
			t.Errorf("clip name %q != source.clip %q", c.Name, c.Source.Clip)
		}
		if c.Thumb != ThumbGLB {
			t.Errorf("clip %q thumb = %q, want glb", c.Name, c.Thumb)
		}
		ids[c.ID] = true
		fps[c.Fingerprint] = true
	}
	if len(ids) != 3 || len(fps) != 3 {
		t.Errorf("clip ids/fingerprints not distinct: ids=%d fps=%d", len(ids), len(fps))
	}

	wholeNames := map[string]bool{}
	for _, w := range whole {
		wholeNames[w.Name] = true
	}
	if wholeNames["UAL1.glb"] {
		t.Error("UAL1.glb was split; it should not also appear as a whole-file card")
	}
	if !wholeNames["UAL1_RM.glb"] {
		t.Error("UAL1_RM.glb should be kept whole (root-motion sibling), not split")
	}
	if !wholeNames["Prop.glb"] {
		t.Error("Prop.glb (single animation) should be kept whole")
	}
}

// writeZip creates a .zip with the given entry->content map.
func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// unityGUID describes one GUID dir to write into a fake .unitypackage.
type unityGUID struct {
	guid     string
	pathname string
	asset    string
	preview  bool
	folder   bool // a Unity directory entry: pathname + asset.meta, but no asset payload
}

func writeUnityPackage(t *testing.T, path string, guids []unityGUID) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	put := func(name, content string) {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content))
	}
	for _, g := range guids {
		put(g.guid+"/pathname", g.pathname)
		put(g.guid+"/asset.meta", "meta")
		if !g.folder { // folder entries carry no asset payload
			put(g.guid+"/asset", g.asset)
		}
		if g.preview {
			put(g.guid+"/preview.png", "PNGPREVIEW")
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func byName(assets []Asset) map[string][]Asset {
	m := map[string][]Asset{}
	for _, a := range assets {
		m[a.Name] = append(m[a.Name], a)
	}
	return m
}

func TestScanImageDimensions(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		return p
	}

	// A loose png, a png inside a zip, and a png inside a unitypackage — each a
	// distinct size so the source can't be confused. The unitypackage helper writes
	// pathname before asset (the reverse of real Unity order), so a pass here proves
	// dimension recovery is order-independent.
	os.WriteFile(mk("v", "P", "Loose.png"), encodePNG(t, 12, 5), 0o644)
	writeZip(t, mk("v", "P", "P_SourceFiles_v3.zip"), map[string]string{
		"SourceFiles/Zipped.png": string(encodePNG(t, 7, 9)),
		"SourceFiles/Model.fbx":  "FBXNOTPNG",
	})
	writeUnityPackage(t, mk("v", "P", "P_Unity_2022_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "aaa", pathname: "Assets/P/Packed.png", asset: string(encodePNG(t, 3, 4))},
	})

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	idx := byName(assets)

	want := map[string][2]int{"Loose.png": {12, 5}, "Zipped.png": {7, 9}, "Packed.png": {3, 4}}
	for name, wh := range want {
		got := idx[name]
		if len(got) != 1 {
			t.Fatalf("%s count = %d, want 1", name, len(got))
		}
		if got[0].Width != wh[0] || got[0].Height != wh[1] {
			t.Errorf("%s dims = %dx%d, want %dx%d", name, got[0].Width, got[0].Height, wh[0], wh[1])
		}
	}
	// A non-image entry carries no dimensions.
	if m := idx["Model.fbx"]; len(m) != 1 || m[0].Width != 0 || m[0].Height != 0 {
		t.Errorf("Model.fbx dims = %dx%d, want 0x0", m[0].Width, m[0].Height)
	}
}

func TestScanFixtureLibrary(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		return p
	}

	// synty zip variant (SourceFiles): a heart model + a sprite.
	writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
		"SourceFiles/Models/Heart.fbx":   "FBXHEARTDATA",
		"SourceFiles/Textures/Heart.png": "PNGDATA",
		"SourceFiles/Models/":            "", // dir entry, must be skipped
	})
	// synty unitypackage variant (Unity): one heart with preview, one folder-only phantom.
	writeUnityPackage(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "aaa", pathname: "Assets/Foo/Heart.prefab", asset: "PREFAB", preview: true},
		{guid: "bbb", pathname: "Assets/Foo/EmptyFolder", folder: true}, // folder-only
	})

	// explosive loose glb.
	os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLB"), 0o644)

	// kevdev: pack A ships zip + extracted src (dedup), plus a .meta sidecar.
	writeZip(t, mk("kevdev", "A", "A.zip"), map[string]string{"Animations/Idle.fbx": "IDLEBYTES"})
	os.WriteFile(mk("kevdev", "A", "src", "Animations", "Idle.fbx"), []byte("IDLEBYTES"), 0o644)
	os.WriteFile(mk("kevdev", "A", "src", "Animations", "Idle.fbx.meta"), []byte("sidecar"), 0o644)
	// pack B ships a zip Idle.fbx with the SAME basename+size but no loose twin.
	writeZip(t, mk("kevdev", "B", "B.zip"), map[string]string{"Animations/Idle.fbx": "IDLEBYTEX"})

	// dot-dir working files must be skipped entirely.
	os.WriteFile(mk("synty", ".ref", "junk.png"), []byte("X"), 0o644)

	assets, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	idx := byName(assets)

	// Heart.fbx from SourceFiles is present, model category, fbx thumb, variant set.
	hearts := idx["Heart.fbx"]
	if len(hearts) != 1 {
		t.Fatalf("Heart.fbx count = %d, want 1", len(hearts))
	}
	if hearts[0].Category != CategoryModel || hearts[0].Thumb != ThumbFBX || hearts[0].Variant != "SourceFiles" {
		t.Errorf("Heart.fbx = %+v", hearts[0])
	}

	// Unity prefab with preview → preview thumb.
	if p := idx["Heart.prefab"]; len(p) != 1 || p[0].Thumb != ThumbPreview || p[0].Variant != "Unity_2022_3" {
		t.Errorf("Heart.prefab = %+v", p)
	}

	// Folder-only phantom excluded.
	if _, ok := idx["EmptyFolder"]; ok {
		t.Error("folder-only unitypackage GUID leaked into the index")
	}
	// Sidecars excluded.
	if _, ok := idx["Idle.fbx.meta"]; ok {
		t.Error(".meta sidecar leaked into the index")
	}
	// Dot-dir content excluded.
	if _, ok := idx["junk.png"]; ok {
		t.Error(".ref dot-dir content leaked into the index")
	}

	// Dedup: pack A's loose Idle.fbx suppresses the A.zip entry, but B.zip's Idle.fbx
	// (same basename+size, different pack) is retained → no false merge, no data loss.
	idles := idx["Idle.fbx"]
	if len(idles) != 2 {
		t.Fatalf("Idle.fbx count = %d, want 2 (A loose + B zip)", len(idles))
	}
	var sawALoose, sawBZip bool
	for _, a := range idles {
		if a.Pack == "A" && a.Source.Kind == SourceLoose {
			sawALoose = true
		}
		if a.Pack == "B" && a.Source.Kind == SourceZip {
			sawBZip = true
		}
		if a.Pack == "A" && a.Source.Kind == SourceZip {
			t.Error("A.zip Idle.fbx should have been de-duplicated by the loose twin")
		}
	}
	if !sawALoose || !sawBZip {
		t.Errorf("dedup wrong: sawALoose=%v sawBZip=%v", sawALoose, sawBZip)
	}
}
