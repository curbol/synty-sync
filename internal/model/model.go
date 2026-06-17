// Package model holds the domain types shared across the synty tool: the packs
// and files parsed from the store, and the slug/keying rules that give them
// stable identity.
package model

import (
	"regexp"
	"strings"
)

// Variant is the engine/format token of a downloadable file, e.g. "Godot_4_5_1",
// "SourceFiles", "SourceSprites", "Unity_2022_3", "Unreal_5_3".
type Variant string

// Pack is one entry in the store's "Your Library" list. Its identity anchor is
// the order_item id; the slug is a stable, human-readable key derived from the
// display name (the file-label token is not stable within a pack).
type Pack struct {
	Slug        string
	DisplayName string
	OrderID     int
	OrderItemID int
	ItemURL     string
}

// FileEntry is one downloadable file on a pack's item page.
type FileEntry struct {
	PackSlug  string
	FileToken string
	Variant   Variant
	Version   string
	FileID    int
	// SizeBytes is derived from the rounded portal label, so it is an approximate
	// display value, not an exact integrity figure (the store shows e.g. "2.6 MB"
	// for 2,731,401 bytes).
	SizeBytes    int64
	DownloadHref string
	Archived     bool
}

// Key is the per-file lockfile key, unique within a pack even when a pack carries
// two files of the same variant (its own plus a bundled GENERIC_Particle_FX).
func (f FileEntry) Key() string { return f.FileToken + "|" + string(f.Variant) }

var nonSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

// Slug lowercases a display name and collapses every run of non-alphanumeric
// characters (spaces, ASCII hyphen, Unicode en/em dash) to a single hyphen, so
// the en-dash/hyphen mix in real names yields deterministic keys.
func Slug(displayName string) string {
	s := strings.ToLower(displayName)
	s = nonSlugRun.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
