package assetindex

import (
	"regexp"
	"strings"
)

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

	case "ttf", "otf", "woff", "woff2":
		return CategoryFont, ThumbFont

	case "asset", "meta", "res", "playable", "terrainlayer", "preset", "lighting", "mesh", "sk":
		return CategoryData, ThumbNone

	case "pdf", "txt", "md", "rtf", "url", "json", "xml", "yaml", "yml", "csv":
		return CategoryDoc, ThumbNone
	}
	return CategoryOther, ThumbNone
}

// UI containers, texture folders, and material-map filename suffixes. Anchored to
// path boundaries (/, _, :) so the archive "::" separator and folder joins both act
// as boundaries, and a substring like the "ui" inside "building" never matches.
var (
	uiTokenRe   = regexp.MustCompile(`(^|[/_:])(ui|hud|gui|interface|menus?|icons?|sprites?|branding|widgets?|cursor|minimap)([/_.:]|$)`)
	texFolderRe = regexp.MustCompile(`(^|[/_:])(textures?|decals?|emissive|normals?)([/:]|$)`)
	texSuffixRe = regexp.MustCompile(`_(albedo|basecolor|diffuse|normals?|metallic(smoothness)?|roughness|specular|emissive|emission|occlusion|ao|height|orm|gloss|opacity|mask|texture)([._]|$|[0-9])`)
)

// refineImage narrows a file already classified as an image to ui, texture, or plain
// image using its path. UI containers win (a HUD sprite is UI even if the pack also
// ships textures), then texture folders and material-map suffixes, else a plain image.
func refineImage(relPath string) Category {
	p := strings.ToLower(relPath)
	switch {
	case uiTokenRe.MatchString(p):
		return CategoryUI
	case texFolderRe.MatchString(p) || texSuffixRe.MatchString(p):
		return CategoryTexture
	default:
		return CategoryImage
	}
}
