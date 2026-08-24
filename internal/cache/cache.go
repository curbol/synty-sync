// Package cache manages the local library mirror: file-identity-keyed layout,
// hashed downloads, and folding pre-existing flat zips into the layout.
// The cache is expendable; durability of used assets lives in the game repo.
//
// Writes are two-phase: Store leaves the bytes in a temp file so the caller's checks
// run before Commit renames anything into place. Nothing unverified ever occupies a
// real cache path, even briefly, because an interrupt in that window would strand a
// rejected body where the next run's adopt scan would take it for genuine.
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
	"time"
)

// tempPrefix marks an in-flight download. SweepTemps looks for it, and the adopt
// scan deliberately does not consider it.
const tempPrefix = ".synty-dl-"

// RelPath is a file's cache path relative to the library root, keyed by file
// identity (not owning pack) so a bundled file shared across packs is stored once.
// Forward slashes for portable lockfile storage.
func RelPath(fileToken, filename string) string {
	return path.Join(fileToken, filename)
}

// safeName rejects a portal-supplied path component that is not a bare name, so a
// crafted one (a "../" or a nested path) cannot escape the cache root when joined to
// the library root. Both components are portal-derived — the filename from the signed
// URL or Content-Disposition, the file token from item-page label text — so both are
// checked here rather than trusting the parser upstream.
func safeName(kind, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("unsafe %s %q", kind, name)
	}
	return nil
}

// safeIdentity checks both components of a cache path together.
func safeIdentity(fileToken, filename string) error {
	if err := safeName("file token", fileToken); err != nil {
		return err
	}
	return safeName("download filename", filename)
}

// resolve turns a cache-relative path into an absolute one, refusing anything that
// would leave the library root. Unlike the components Store writes, these paths
// arrive from the lockfile, which is committed and travels with the consuming
// project, so they are validated rather than trusted.
func resolve(libraryRoot, relPath string) (string, error) {
	if relPath == "" || path.IsAbs(relPath) || filepath.IsAbs(relPath) {
		return "", fmt.Errorf("unsafe cache path %q", relPath)
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == ".." || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe cache path %q", relPath)
	}
	return filepath.Join(libraryRoot, clean), nil
}

// Pending is a fully-written but uncommitted download.
type Pending struct {
	RelPath string
	SHA256  string
	Size    int64

	tempPath string
	final    string
}

// TempPath is where the bytes currently are, so the caller can inspect them before
// deciding to commit.
func (p *Pending) TempPath() string { return p.tempPath }

// Commit renames the pending bytes to their real cache path.
func (p *Pending) Commit() error {
	if err := os.Rename(p.tempPath, p.final); err != nil {
		os.Remove(p.tempPath)
		return err
	}
	return nil
}

// Discard removes the pending bytes. Callers use it whenever a check fails, so a
// rejected body never reaches a real cache path.
func (p *Pending) Discard() error {
	err := os.Remove(p.tempPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Store streams r into a temp file in the directory its eventual destination
// <libraryRoot>/<fileToken>/<filename> lives in, hashing while writing. It does not
// rename: the caller commits or discards.
func Store(libraryRoot, fileToken, filename string, r io.Reader) (*Pending, error) {
	if err := safeIdentity(fileToken, filename); err != nil {
		return nil, err
	}
	destDir := filepath.Join(libraryRoot, filepath.FromSlash(fileToken))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(destDir, tempPrefix+"*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	return &Pending{
		RelPath:  RelPath(fileToken, filename),
		SHA256:   hex.EncodeToString(h.Sum(nil)),
		Size:     size,
		tempPath: tmpName,
		final:    filepath.Join(destDir, filename),
	}, nil
}

// SweepTemps removes abandoned download temps anywhere in the tree, returning how
// many and how many bytes. It walks rather than scanning the root, because temps live
// beside their destinations, and it spares anything newer than the cutoff so a
// concurrent run's in-flight transfer survives.
//
// Nothing here fails: a subtree that cannot be read, or a root that does not exist yet
// on a first run, is skipped. One unreadable directory must not stop a mirror over a
// housekeeping pass.
func SweepTemps(libraryRoot string, olderThan time.Time) (int, int64) {
	var count int
	var bytes int64
	filepath.WalkDir(libraryRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), tempPrefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.ModTime().Before(olderThan) {
			return nil
		}
		if os.Remove(p) == nil {
			count++
			bytes += fi.Size()
		}
		return nil
	})
	return count, bytes
}

// Verify is the cheap check: the file exists and its size is exactly what was
// recorded. Exact size is what makes a truncated or replaced-by-an-error-page body
// detectable without reading tens of gigabytes back off the disk.
func Verify(libraryRoot, relPath string, wantSize int64) bool {
	full, err := resolve(libraryRoot, relPath)
	if err != nil {
		return false
	}
	fi, err := os.Stat(full)
	return err == nil && !fi.IsDir() && fi.Size() == wantSize
}

// VerifyDeep re-hashes the file. It is opt-in because a library runs to tens of
// gigabytes, and it is the only check that sees a mid-file corruption.
func VerifyDeep(libraryRoot, relPath, sha string) bool {
	full, err := resolve(libraryRoot, relPath)
	if err != nil {
		return false
	}
	f, err := os.Open(full)
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

// Hash returns the sha256 and byte size of a cached file (used to adopt a file
// migrated in from a pre-existing flat zip).
func Hash(libraryRoot, relPath string) (sha string, size int64, err error) {
	full, err := resolve(libraryRoot, relPath)
	if err != nil {
		return "", 0, err
	}
	f, err := os.Open(full)
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
	full, err := resolve(libraryRoot, relPath)
	if err != nil {
		return err
	}
	err = os.Remove(full)
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
	s = strings.TrimSuffix(s, filepath.Ext(s))
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
		if safeName("file token", w.FileToken) != nil {
			continue
		}
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
		target := filepath.Join(dest, e.Name())
		if _, err := os.Stat(target); err == nil {
			// The layout copy wins. os.Rename would replace it silently, and the caller
			// hashes whatever lands here and records that sha, so a stale flat zip would
			// be adopted as verified content.
			continue
		}
		from := filepath.Join(libraryRoot, e.Name())
		if err := os.Rename(from, target); err != nil {
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
	if safeName("file token", w.FileToken) != nil {
		return "", false
	}
	dir := filepath.Join(libraryRoot, filepath.FromSlash(w.FileToken))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	want := normalizeName(w.FileToken + "_" + w.Variant + "_" + w.Version)
	for _, e := range entries {
		// An abandoned download temp is skipped outright: a partial transfer can carry
		// enough of the name to normalize onto a wanted file, and adopting it would
		// record a truncated body's digest as that file's truth.
		if e.IsDir() || strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if normalizeName(base) == want {
			return RelPath(w.FileToken, e.Name()), true
		}
	}
	return "", false
}
