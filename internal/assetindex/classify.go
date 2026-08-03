package assetindex

// Classify maps a lowercased, dotless file extension to its browse category and
// the kind of thumbnail the frontend can render for it. A Unity preview.png, when
// present, overrides the thumbnail to ThumbPreview at scan time (see newAsset).
func Classify(ext string) (Category, ThumbKind) {
	switch ext {
	case "glb", "gltf":
		return CategoryModel, ThumbGLB
	case "fbx":
		return CategoryModel, ThumbFBX
	case "obj", "blend", "dae", "stl", "ply":
		return CategoryModel, ThumbNone

	case "png", "jpg", "jpeg", "gif", "webp", "bmp":
		return CategoryImage, ThumbImage
	case "tga", "psd", "exr", "tif", "tiff", "hdr", "svg":
		return CategoryImage, ThumbNone

	case "mat", "material", "tres", "physicmaterial":
		return CategoryMaterial, ThumbNone

	case "tscn", "scn", "prefab", "unity", "scene":
		return CategoryScene, ThumbNone

	case "anim", "controller", "fbxanim":
		return CategoryAnimation, ThumbNone

	case "wav", "mp3", "ogg", "aiff", "aif", "flac":
		return CategoryAudio, ThumbNone

	case "cs", "gd", "js", "hlsl", "glsl", "shader", "shadergraph", "shadersubgraph", "gdshader", "cginc":
		return CategoryScript, ThumbNone

	case "pdf", "txt", "md", "rtf", "url", "json", "xml", "yaml", "yml", "csv", "asset", "meta":
		return CategoryDoc, ThumbNone
	}
	return CategoryOther, ThumbNone
}
