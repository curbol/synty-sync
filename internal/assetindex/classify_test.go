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
		{"json", CategoryDoc, ThumbNone},
		{"asset", CategoryData, ThumbNone},
		{"meta", CategoryData, ThumbNone},
		{"res", CategoryData, ThumbNone},
		{"playable", CategoryData, ThumbNone},
		{"terrainlayer", CategoryData, ThumbNone},
		{"preset", CategoryData, ThumbNone},
		{"lighting", CategoryData, ThumbNone},
		{"mesh", CategoryData, ThumbNone},
		{"sk", CategoryData, ThumbNone},
		{"ttf", CategoryFont, ThumbFont},
		{"otf", CategoryFont, ThumbFont},
		{"xyz", CategoryOther, ThumbNone},
	}
	for _, c := range cases {
		gotCat, gotThumb := Classify(c.ext)
		if gotCat != c.cat || gotThumb != c.thumb {
			t.Errorf("Classify(%q) = (%s,%s), want (%s,%s)", c.ext, gotCat, gotThumb, c.cat, c.thumb)
		}
	}
}

func TestRefineImage(t *testing.T) {
	cases := []struct {
		relPath string
		want    Category
	}{
		// UI: INTERFACE-pack sprite/icon/branding paths, and generic UI folders.
		{"synty/INTERFACE_Dark_Fantasy_HUD/x.zip::Source_Sprites/Core/Icons_Input/ICON_Input_Stick.png", CategoryUI},
		{"synty/INTERFACE_Fantasy_Menus/x.zip::Source_Sprites/Core/Branding/SPR_Logo.png", CategoryUI},
		{"pack/UI/button_01.png", CategoryUI},
		{"pack/HUD/minimap.png", CategoryUI},
		// UI wins over a texture folder when both are present in the path.
		{"pack/UI/Textures/icon.png", CategoryUI},
		// texture: /textures/ tree, sibling folders, and map suffixes.
		{"synty/POLYGON_Nature/x.zip::Textures/PolygonNature_Texture_01.png", CategoryTexture},
		{"pack/Textures/Wall_Normal.png", CategoryTexture},
		{"pack/Decals/blood_01.png", CategoryTexture},
		{"pack/Materials/rock_emissive.png", CategoryTexture},
		// image: the remainder (no UI token, no texture folder/suffix).
		{"pack/Misc/fx_circle_01.png", CategoryImage},
		{"pack/color_palette.png", CategoryImage},
		// "build" contains the substring "ui" but is not a UI token boundary.
		{"pack/Buildings/wall.png", CategoryImage},
		// The pack/archive NAME must not drive classification: POLYGON_Icons ships 3D
		// props, and a file under its Textures/ folder is a texture even though "Icons"
		// is in the pack name. Only the path inside the archive (after "::") counts.
		{"synty/POLYGON_Icons/POLYGON_Icons_Unity_2022_3_v1_2_1.unitypackage::Assets/Synty/PolygonGeneric/Textures/Alts/Generic_01_A.png", CategoryTexture},
		// A genuine UI sprite inside an archive still reads as UI from its entry path,
		// even when the pack name carries no UI token.
		{"synty/POLYGON_Kit/POLYGON_Kit.unitypackage::Assets/UI/HUD/health_bar.png", CategoryUI},
	}
	for _, c := range cases {
		if got := refineImage(c.relPath); got != c.want {
			t.Errorf("refineImage(%q) = %s, want %s", c.relPath, got, c.want)
		}
	}
}

func TestNewAssetRefinesImageCategory(t *testing.T) {
	ui := newAsset(Source{Kind: SourceZip, ArchivePath: "/x.zip", Entry: "Source_Sprites/Icons/ICON_x.png"},
		"ICON_x.png", "synty/INTERFACE_Pack/x.zip::Source_Sprites/Icons/ICON_x.png", "synty", "INTERFACE_Pack", "SourceSprites", 10, "")
	if ui.Category != CategoryUI {
		t.Errorf("ui image category = %s, want ui", ui.Category)
	}
	if ui.Thumb != ThumbImage {
		t.Errorf("ui png thumb = %s, want image (still renderable)", ui.Thumb)
	}

	tex := newAsset(Source{Kind: SourceZip, ArchivePath: "/x.zip", Entry: "Textures/Wall_Normal.png"},
		"Wall_Normal.png", "synty/POLYGON_Pack/x.zip::Textures/Wall_Normal.png", "synty", "POLYGON_Pack", "SourceFiles", 10, "")
	if tex.Category != CategoryTexture {
		t.Errorf("texture image category = %s, want texture", tex.Category)
	}
}

func TestNewAssetDerivesFields(t *testing.T) {
	// A loose fbx: model category, fbx thumb, copyPath is the absolute file path.
	a := newAsset(Source{Kind: SourceLoose, FilePath: "/lib/synty/Pack/Foo.FBX"},
		"Foo.FBX", "synty/Pack/Foo.FBX", "synty", "Pack", "SourceFiles", 123, "")
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
		"Foo.fbx", "synty/Pack/p.unitypackage::Assets/Foo.fbx", "synty", "Pack", "Unity_2022_3", 9, "")
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
