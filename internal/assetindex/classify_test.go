package assetindex

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		ext   string
		cat   Category
		thumb ThumbKind
	}{
		{"fbx", CategoryModel, ThumbFBX},
		{"glb", CategoryModel, ThumbGLB},
		{"gltf", CategoryModel, ThumbGLB},
		{"obj", CategoryModel, ThumbNone},
		{"png", CategoryImage, ThumbImage},
		{"tga", CategoryImage, ThumbNone},
		{"mat", CategoryMaterial, ThumbNone},
		{"tres", CategoryMaterial, ThumbNone},
		{"tscn", CategoryScene, ThumbNone},
		{"prefab", CategoryScene, ThumbNone},
		{"controller", CategoryAnimation, ThumbNone},
		{"wav", CategoryAudio, ThumbNone},
		{"cs", CategoryScript, ThumbNone},
		{"pdf", CategoryDoc, ThumbNone},
		{"xyz", CategoryOther, ThumbNone},
	}
	for _, c := range cases {
		gotCat, gotThumb := Classify(c.ext)
		if gotCat != c.cat || gotThumb != c.thumb {
			t.Errorf("Classify(%q) = (%s,%s), want (%s,%s)", c.ext, gotCat, gotThumb, c.cat, c.thumb)
		}
	}
}

func TestNewAssetDerivesFields(t *testing.T) {
	// A loose fbx: model category, fbx thumb, copyPath is the absolute file path.
	a := newAsset(Source{Kind: SourceLoose, FilePath: "/lib/synty/Pack/Foo.FBX"},
		"Foo.FBX", "synty/Pack/Foo.FBX", "synty", "Pack", "SourceFiles", 123)
	if a.Category != CategoryModel || a.Thumb != ThumbFBX {
		t.Errorf("category/thumb = %s/%s", a.Category, a.Thumb)
	}
	if a.Ext != "fbx" {
		t.Errorf("ext = %q, want fbx (lowercased)", a.Ext)
	}
	if a.CopyPath != "/lib/synty/Pack/Foo.FBX" {
		t.Errorf("copyPath = %q", a.CopyPath)
	}
	if a.ID == "" {
		t.Error("empty id")
	}

	// A unitypackage fbx WITH a preview overrides the thumbnail to preview.
	b := newAsset(Source{Kind: SourceUnityPackage, ArchivePath: "/lib/p.unitypackage", Guid: "g1", Pathname: "Assets/Foo.fbx", HasPreview: true},
		"Foo.fbx", "synty/Pack/p.unitypackage::Assets/Foo.fbx", "synty", "Pack", "Unity_2022_3", 9)
	if b.Thumb != ThumbPreview {
		t.Errorf("unity+preview thumb = %s, want preview", b.Thumb)
	}
	if b.CopyPath != "/lib/p.unitypackage::Assets/Foo.fbx" {
		t.Errorf("unity copyPath = %q", b.CopyPath)
	}
}

func TestIDStableAndDistinct(t *testing.T) {
	s1 := Source{Kind: SourceZip, ArchivePath: "/a.zip", Entry: "x/Foo.fbx"}
	s2 := Source{Kind: SourceZip, ArchivePath: "/a.zip", Entry: "y/Foo.fbx"}
	if id(s1) != id(s1) {
		t.Error("id not stable")
	}
	if id(s1) == id(s2) {
		t.Error("distinct entries share an id")
	}
}
