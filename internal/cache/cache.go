// Package cache manages the local library mirror: file-identity-keyed layout,
// atomic+hashed downloads, and folding pre-existing flat zips into the layout.
// The cache is expendable; durability of used assets lives in the game repo.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// RelPath is a file's cache path relative to the library root, keyed by file
// identity (not owning pack) so a bundled file shared across packs is stored once.
// Forward slashes for portable lockfile storage.
func RelPath(fileToken, filename string) string {
	return path.Join(fileToken, filename)
}

// safeName rejects a portal-supplied filename that is not a bare filename, so a
// crafted name (a "../" or a nested path) cannot escape the cache root when joined
// to the destination dir. The portal already reduces both filename sources to a
// basename; this is the hard boundary that backstops it.
func safeName(filename string) error {
	if filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, `/\`) {
		return fmt.Errorf("unsafe download filename %q", filename)
	}
	return nil
}

// Store streams r into <libraryRoot>/<fileToken>/<filename> via a temp file in the
// destination dir, hashing while writing and renaming atomically. It returns the
// relative cache path, the sha256, and the byte count.
func Store(libraryRoot, fileToken, filename string, r io.Reader) (relPath, sha string, size int64, err error) {
	if err := safeName(filename); err != nil {
		return "", "", 0, err
	}
	rel := RelPath(fileToken, filename)
	destDir := filepath.Join(libraryRoot, filepath.FromSlash(fileToken))
	if err = os.MkdirAll(destDir, 0o755); err != nil {
		return "", "", 0, err
	}
	tmp, err := os.CreateTemp(destDir, ".synty-dl-*")
	if err != nil {
		return "", "", 0, err
	}
	tmpName := tmp.Name()
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", "", 0, err
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", "", 0, err
	}
	final := filepath.Join(destDir, filename)
	if err = os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", "", 0, err
	}
	return rel, hex.EncodeToString(h.Sum(nil)), size, nil
}

// Verify reports whether the file at relPath exists and matches sha. A full hash
// is used; callers may skip it (presence-only) for cheap status checks.
func Verify(libraryRoot, relPath, sha string) bool {
	f, err := os.Open(filepath.Join(libraryRoot, filepath.FromSlash(relPath)))
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == sha
}

// Exists reports whether relPath is present (cheap presence check, no hashing).
func Exists(libraryRoot, relPath string) bool {
	_, err := os.Stat(filepath.Join(libraryRoot, filepath.FromSlash(relPath)))
	return err == nil
}

// Hash returns the sha256 and byte size of a cached file (used to adopt a file
// migrated in from a pre-existing flat zip).
func Hash(libraryRoot, relPath string) (sha string, size int64, err error) {
	f, err := os.Open(filepath.Join(libraryRoot, filepath.FromSlash(relPath)))
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err = io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// Remove deletes the file at relPath (used to prune a prior version).
func Remove(libraryRoot, relPath string) error {
	err := os.Remove(filepath.Join(libraryRoot, filepath.FromSlash(relPath)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Wanted identifies a file to look for among pre-existing flat zips.
type Wanted struct {
	FileID    int
	FileToken string
	Variant   string
	Version   string
}

// MigrateResult records one flat zip folded into the layout.
type MigrateResult struct {
	FileID  int
	From    string // original filename
	RelPath string // new cache-relative path
}

var collisionSuffix = regexp.MustCompile(`\(\d+\)`)
var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeName strips a (N) collision suffix and all non-alphanumerics and
// lowercases, so "INTERFACE_..._Source_Sprites_v3" and the item-page-derived
// "INTERFACE_..._SourceSprites_v3" compare equal.
func normalizeName(s string) string {
	s = strings.TrimSuffix(s, ".zip")
	s = collisionSuffix.ReplaceAllString(s, "")
	return nonAlnum.ReplaceAllString(strings.ToLower(s), "")
}

// Migrate folds pre-existing flat *.zip files at the library root into the
// file-identity layout, matching each wanted file by a normalized name key (so
// variant-rendering and (N) differences don't block a match). Unmatched zips are
// left untouched and will simply re-download. It is best-effort and idempotent.
func Migrate(libraryRoot string, wanted []Wanted) ([]MigrateResult, error) {
	entries, err := os.ReadDir(libraryRoot)
	if err != nil {
		return nil, err
	}
	byNorm := map[string]Wanted{}
	for _, w := range wanted {
		key := normalizeName(w.FileToken + "_" + w.Variant + "_" + w.Version)
		byNorm[key] = w
	}
	var results []MigrateResult
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".zip") {
			continue
		}
		w, ok := byNorm[normalizeName(e.Name())]
		if !ok {
			continue
		}
		rel := RelPath(w.FileToken, e.Name())
		dest := filepath.Join(libraryRoot, filepath.FromSlash(w.FileToken))
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return results, err
		}
		from := filepath.Join(libraryRoot, e.Name())
		if err := os.Rename(from, filepath.Join(dest, e.Name())); err != nil {
			return results, fmt.Errorf("migrate %s: %w", e.Name(), err)
		}
		results = append(results, MigrateResult{FileID: w.FileID, From: e.Name(), RelPath: rel})
	}
	return results, nil
}

// Locate reports the layout-relative path of a cached file already present under
// <fileToken>/ that matches the wanted file by normalized name (so variant-rendering and
// (N) collision differences don't block a match), moving nothing. It lets a sync adopt
// files already in the layout that no lockfile records, instead of re-downloading them.
// The extension is stripped before normalizing, so both .zip and .unitypackage match.
func Locate(libraryRoot string, w Wanted) (relPath string, ok bool) {
	dir := filepath.Join(libraryRoot, filepath.FromSlash(w.FileToken))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	want := normalizeName(w.FileToken + "_" + w.Variant + "_" + w.Version)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if normalizeName(base) == want {
			return RelPath(w.FileToken, e.Name()), true
		}
	}
	return "", false
}
