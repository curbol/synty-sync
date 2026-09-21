// Package cache manages the local library mirror: file-identity-keyed layout,
// hashed downloads, and folding pre-existing flat files into the layout.
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
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// cachedFileMode is what a committed download ends up with. Directories the layout
// creates are already 0755, so this keeps the files readable to match them.
const cachedFileMode = 0o644

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
	// tempPrefix is a reserved namespace, not just a convention: Migrate, Locate and
	// the adopt scan all skip it, and SweepTemps deletes anything wearing it once it is
	// old enough. A portal-supplied name carrying it would commit to a real cache path
	// that the next day's sweep removes and no scan can ever take back, so the file
	// re-downloads on every run forever.
	if strings.HasPrefix(name, tempPrefix) {
		return fmt.Errorf("unsafe %s %q (%q is reserved for in-flight downloads)", kind, name, tempPrefix)
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
	// CreateTemp opens owner-only and the mode survives the rename, but the library is
	// a mirror: its path is configurable, so it can sit on a volume more than one
	// account reads, and a pack nobody else can open is not a mirror of anything.
	if err := tmp.Chmod(cachedFileMode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
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

// SweepTemps removes download temps that have gone untouched for at least minAge,
// anywhere in the tree, returning how many and how many bytes. It walks rather than
// scanning the root, because temps live beside their destinations, and an in-flight
// transfer touches its temp on every write, so any age long enough to clear a stall
// spares a concurrent run's.
//
// An age rather than a cutoff instant: the caller's only sensible cutoff is
// now-minus-something, and a sign the wrong way round there compiles, runs, reports a
// large sweep, and deletes every in-flight transfer on the machine.
//
// Nothing here fails: a subtree that cannot be read, or a root that does not exist yet
// on a first run, is skipped. One unreadable directory must not stop a mirror over a
// housekeeping pass.
func SweepTemps(libraryRoot string, minAge time.Duration) (int, int64) {
	var count int
	var bytes int64
	olderThan := time.Now().Add(-minAge)
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
//
// Through Hash rather than hashing again here: the digest this compares against is one
// Hash produced when the file was adopted, and it holds for the life of the file, so
// the two have to agree forever about what they read and how. Two copies of that only
// agree until one of them changes.
func VerifyDeep(libraryRoot, relPath, sha string) bool {
	got, _, err := Hash(libraryRoot, relPath)
	return err == nil && got == sha
}

// Tail returns up to the last n bytes of a cached file, for a caller that has to look
// at a trailer to tell a whole file from a truncated one.
func Tail(libraryRoot, relPath string, n int) ([]byte, error) {
	full, err := resolve(libraryRoot, relPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if int64(n) > size {
		n = int(size)
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, size-int64(n)); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// Head returns up to n leading bytes of a cached file, for a caller that has to look
// at what a file is before trusting it.
func Head(libraryRoot, relPath string, n int) ([]byte, error) {
	full, err := resolve(libraryRoot, relPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:read], nil
}

// Hash returns the sha256 and byte size of a cached file (used to adopt a file
// migrated in from a pre-existing flat file).
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

// Wanted identifies a file to look for among pre-existing flat files.
type Wanted struct {
	FileID    int
	FileToken string
	Variant   string
	Version   string
}

// MigrateResult records one flat file folded into the layout.
type MigrateResult struct {
	FileID  int
	From    string // original filename
	RelPath string // new cache-relative path
}

var collisionSuffix = regexp.MustCompile(`\(\d+\)`)
var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeKey strips a (N) collision suffix and all non-alphanumerics and
// lowercases, so "INTERFACE_..._Source_Sprites_v3" and the item-page-derived
// "INTERFACE_..._SourceSprites_v3" compare equal.
func normalizeKey(s string) string {
	s = collisionSuffix.ReplaceAllString(s, "")
	return nonAlnum.ReplaceAllString(strings.ToLower(s), "")
}

// normalizeName is normalizeKey for a name that came off the disk, which carries an
// extension the wanted-file key does not. Only a real filename goes through here:
// filepath.Ext takes everything after the last dot anywhere in the string, so running
// it over "<token>_<variant>_<version>" would truncate the key at the first version
// rendered with a dot — dropping "v1.0.1" to "v1.0", and "v1.0" onto the genuinely
// different "v1", which would fold one version's bytes in as another's and record that
// sha as the file's truth.
func normalizeName(s string) string {
	return normalizeKey(strings.TrimSuffix(s, filepath.Ext(s)))
}

// preferredMatch reports whether name should displace best as the flat file standing
// in for a wanted one. Several names can normalize onto a single wanted file: the
// "(N)" suffix normalizeKey strips is exactly what a second copy of one download is
// named. ReadDir is sorted, and "(" sorts before ".", so the collision copy comes back
// ahead of the canonical name for no reason but its punctuation. Prefer the name that
// needed the least normalizing, so the choice follows the file rather than the order.
func preferredMatch(best, name string) bool {
	if best == "" {
		return true
	}
	return collisionSuffix.FindStringIndex(name) == nil && collisionSuffix.FindStringIndex(best) != nil
}

// Migrate folds pre-existing flat files at the library root into the file-identity
// layout, matching each wanted file by a normalized name key (so variant-rendering,
// extension and (N) differences don't block a match). Unmatched files are left
// untouched and will simply re-download. It is best-effort and idempotent.
func Migrate(libraryRoot string, wanted []Wanted) ([]MigrateResult, error) {
	entries, err := os.ReadDir(libraryRoot)
	if errors.Is(err, fs.ErrNotExist) {
		// A root that does not exist yet holds no flat files to fold in. The first run
		// on a fresh install reaches here before anything has created it.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byNorm := map[string]Wanted{}
	for _, w := range wanted {
		if safeName("file token", w.FileToken) != nil {
			continue
		}
		byNorm[normalizeKey(w.FileToken+"_"+w.Variant+"_"+w.Version)] = w
	}
	// One flat file per wanted file, decided before anything moves. Folding in every
	// name that matches would leave the copies the lockfile does not record sitting in
	// the layout for good: nothing prunes them (the syncer only knows the path it
	// recorded) and nothing sweeps them (they carry no temp prefix). It would also hand
	// the caller two results for one fileId, letting ReadDir's order pick which copy
	// gets hashed and recorded — the decision Locate already refuses to let punctuation
	// make.
	pick := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		key := normalizeName(e.Name())
		if _, ok := byNorm[key]; !ok {
			continue
		}
		if preferredMatch(pick[key], e.Name()) {
			pick[key] = e.Name()
		}
	}
	var results []MigrateResult
	for _, e := range entries {
		// Matching on the normalized name, which drops the extension, is what lets a
		// Unity pack's .unitypackage fold in beside a .zip. An abandoned download temp
		// is skipped outright: a partial transfer can carry enough of the name to
		// normalize onto a wanted file, and the caller hashes whatever lands here.
		if e.IsDir() || strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		key := normalizeName(e.Name())
		w, ok := byNorm[key]
		if !ok || pick[key] != e.Name() {
			continue
		}
		// The layout copy wins, and the question is whether the layout holds this
		// wanted file at all, not whether it holds this exact name. Asking the name
		// leaves every equivalence the matcher grants — a (N) collision copy, a
		// .unitypackage against a .zip, a dot- against an underscore-rendered version —
		// pointing at a target that does not exist, so the rename proceeds and the
		// layout ends up holding two copies of one file identity. The caller then
		// hashes the one it just moved and records that sha, and nothing ever
		// references the other again: the syncer prunes only the path it recorded and
		// the sweep spares anything without the temp prefix.
		if _, already := Locate(libraryRoot, w); already {
			continue
		}
		rel := RelPath(w.FileToken, e.Name())
		dest := filepath.Join(libraryRoot, filepath.FromSlash(w.FileToken))
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return results, err
		}
		target := filepath.Join(dest, e.Name())
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
	want := normalizeKey(w.FileToken + "_" + w.Variant + "_" + w.Version)
	// Several names can normalize onto one wanted file, so both matchers resolve that
	// the same way, through preferredMatch.
	best := ""
	for _, e := range entries {
		// An abandoned download temp is skipped outright: a partial transfer can carry
		// enough of the name to normalize onto a wanted file, and adopting it would
		// record a truncated body's digest as that file's truth.
		if e.IsDir() || strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		// The raw name, exactly as Migrate keys it. normalizeName already drops one
		// extension, so trimming one here first would make these two matchers disagree
		// on every name carrying a second dot.
		if normalizeName(e.Name()) != want {
			continue
		}
		if preferredMatch(best, e.Name()) {
			best = e.Name()
		}
	}
	if best == "" {
		return "", false
	}
	return RelPath(w.FileToken, best), true
}
