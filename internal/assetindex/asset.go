// Package assetindex scans a local game-asset library into a searchable index of
// individual assets, seeing inside .zip and .unitypackage archives as well as
// loose files, and serves each asset's bytes and thumbnail on demand. It is pure
// of HTTP: the browse server queries the index and streams what these types open.
package assetindex

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"strings"
)

// Category is the coarse asset kind used by the type filter.
type Category string

const (
	CategoryModel     Category = "model"
	CategoryImage     Category = "image"
	CategoryUI        Category = "ui"
	CategoryTexture   Category = "texture"
	CategoryMaterial  Category = "material"
	CategoryScene     Category = "scene"
	CategoryAnimation Category = "animation"
	CategoryAudio     Category = "audio"
	CategoryScript    Category = "script"
	CategoryData      Category = "data"
	CategoryDoc       Category = "doc"
	CategoryFont      Category = "font"
	CategoryOther     Category = "other"
)

// ThumbKind tells the frontend how to render an asset's thumbnail:
//
//	image   — the asset itself is a browser image; <img src=/api/content>
//	glb/fbx — a 3D model; render client-side with three.js from /api/content
//	preview — a Unity-rendered preview.png exists; <img src=/api/thumb>
//	font    — a font file; load with FontFace from /api/content and render sample text
//	none    — no thumbnail; show a category icon
type ThumbKind string

const (
	ThumbNone    ThumbKind = ""
	ThumbImage   ThumbKind = "image"
	ThumbGLB     ThumbKind = "glb"
	ThumbFBX     ThumbKind = "fbx"
	ThumbPreview ThumbKind = "preview"
	ThumbFont    ThumbKind = "font"
)

// SourceKind discriminates where an asset's bytes live.
type SourceKind string

const (
	SourceLoose        SourceKind = "loose"
	SourceZip          SourceKind = "zip"
	SourceUnityPackage SourceKind = "unitypackage"
)

// Source locates an asset's bytes. Which fields are set depends on Kind:
// loose uses FilePath; zip uses ArchivePath+Entry; unitypackage uses
// ArchivePath+Guid (+Pathname for display, HasPreview for the thumbnail).
type Source struct {
	Kind        SourceKind `json:"kind"`
	FilePath    string     `json:"filePath,omitempty"`
	ArchivePath string     `json:"archivePath,omitempty"`
	Entry       string     `json:"entry,omitempty"`
	Guid        string     `json:"guid,omitempty"`
	Pathname    string     `json:"pathname,omitempty"`
	HasPreview  bool       `json:"hasPreview,omitempty"`
}

// Asset is one browseable item: a loose file or a single entry inside an archive.
//
// Fingerprint is the asset's stable content identity, used to attach tags. It is
// distinct from ID: ID locates the exact bytes to serve (a machine-absolute path
// and version-bearing archive name) and so is not stable across machines or pack
// updates; Fingerprint is derived from the content (zip/loose CRC32+size, Unity
// GUID) so byte-identical copies share it and a tag set once survives a resync.
// Empty when the content could not be read.
type Asset struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	RelPath     string    `json:"relPath"`
	CopyPath    string    `json:"copyPath"`
	Category    Category  `json:"category"`
	Ext         string    `json:"ext"`
	Vendor      string    `json:"vendor"`
	Pack        string    `json:"pack"`
	Variant     string    `json:"variant,omitempty"`
	Size        int64     `json:"size"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	Thumb       ThumbKind `json:"thumb"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Source      Source    `json:"source"`
}

// id derives a stable identifier from the locator so the content API can resolve
// a request to exactly one indexed asset (and nothing else).
func id(s Source) string {
	var key string
	switch s.Kind {
	case SourceLoose:
		key = "loose\x00" + s.FilePath
	case SourceZip:
		key = "zip\x00" + s.ArchivePath + "\x00" + s.Entry
	case SourceUnityPackage:
		key = "unity\x00" + s.ArchivePath + "\x00" + s.Guid
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// copyPath renders the path a user copies to paste elsewhere (e.g. into a prompt):
// a loose file's absolute path, or an archive's absolute path joined to the
// internal entry with "::" so the entry is human-locatable.
func copyPath(s Source) string {
	switch s.Kind {
	case SourceLoose:
		return s.FilePath
	case SourceZip:
		return s.ArchivePath + "::" + s.Entry
	case SourceUnityPackage:
		return s.ArchivePath + "::" + s.Pathname
	}
	return ""
}

// newAsset fills the derived fields (ID, CopyPath, Category, Thumb) from a
// locator plus the display metadata scanning has already resolved. fingerprint is
// the content identity the caller has computed for this source kind.
func newAsset(s Source, name, relPath, vendor, pack, variant string, size int64, fingerprint string) Asset {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	cat, thumb := Classify(ext)
	if cat == CategoryImage {
		cat = refineImage(relPath)
	}
	if s.Kind == SourceUnityPackage && s.HasPreview {
		thumb = ThumbPreview
	}
	return Asset{
		ID:          id(s),
		Name:        name,
		RelPath:     relPath,
		CopyPath:    copyPath(s),
		Category:    cat,
		Ext:         ext,
		Vendor:      vendor,
		Pack:        pack,
		Variant:     variant,
		Size:        size,
		Thumb:       thumb,
		Fingerprint: fingerprint,
		Source:      s,
	}
}

// archiveRel joins an archive's root-relative display path with an internal entry
// using "::" for the RelPath field (unlike CopyPath's absolute archive path).
func archiveRel(archiveDisplay, entry string) string {
	return archiveDisplay + "::" + path.Clean(entry)
}
