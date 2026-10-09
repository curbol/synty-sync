// Package cache manages the local library mirror: file-identity-keyed layout,
// hashed downloads, and folding pre-existing flat files into the layout.
// The cache is expendable; durability of used assets lives in the game repo.
//
// Writes are two-phase: Store leaves the bytes in a temp file so the caller's checks
// run before Commit renames anything into place. Nothing unverified ever occupies a
// real cache path, even briefly, because an interrupt in that window would strand a
// rejected body where the next run's adopt scan would take it for genuine.
//
// Every operation on a path under the library goes through an os.Root opened on it, so
// a symlinked segment cannot carry a read, a write or a delete out of the tree. The
// library root itself may be a symlink: it is the user's own setting, and opening it as
// a root resolves it once.
package cache

import (
	"context"
	"crypto/rand"
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

// reservedNames are the names Windows refuses as a path component, in any case and with
// any extension: it matches them before the first dot, so "nul.zip" is a device too.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

func reservedName(s string) bool {
	base, _, _ := strings.Cut(s, ".")
	return reservedNames[strings.ToLower(base)]
}

// safeName rejects a portal-supplied path component that is not a bare name, so a
// crafted one (a "../" or a nested path) cannot escape the cache root when joined to
// the library root. Both components are portal-derived — the filename from the signed
// URL or Content-Disposition, the file token from item-page label text — so both are
// checked here rather than trusting the parser upstream.
//
// A colon and a Windows device name are refused on every platform. Either is a name
// Linux will create and Windows will not, so a lockfile written on one machine would
// record a path the other cannot resolve; Canonical refuses both in a recorded path for
// the same reason, and a name Store accepted has to be one Canonical accepts.
func safeName(kind, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) {
		return fmt.Errorf("unsafe %s %q", kind, name)
	}
	if reservedName(name) {
		return fmt.Errorf("unsafe %s %q: Windows reserves this name for a device", kind, name)
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

// Canonical is the one spelling of a cache-relative path, in the forward slashes a
// lockfile records. Two values naming the same file inside the root canonicalise to the
// same string, and anything that would leave the root is refused instead.
//
// It is exported because comparing a recorded path against a derived one is not a string
// comparison. The lockfile is committed, hand-editable and travels between machines, so
// "./TOK/pack.zip" has to compare equal to the "TOK/pack.zip" a run derives; a caller that
// compares them raw decides two names for one file are two files, and deletes one.
//
// The whole check runs in slash space, so it answers identically on every platform. A
// backslash and a colon are refused rather than interpreted: filepath.Clean strips a
// Windows volume name before resolving "..", then puts it back, so "Z:..\..\x" cleans to
// itself there and slips past a leading-".." test that catches it everywhere else. The
// same committed value would then confine on the machine that wrote it and escape on the
// machine that read it.
func Canonical(rel string) (string, error) {
	if rel == "" || strings.ContainsAny(rel, `\:`) {
		return "", fmt.Errorf("unsafe cache path %q", rel)
	}
	clean := path.Clean(rel)
	if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe cache path %q", rel)
	}
	for _, seg := range strings.Split(clean, "/") {
		if reservedName(seg) {
			return "", fmt.Errorf("unsafe cache path %q: %q is a Windows device name", rel, seg)
		}
	}
	return clean, nil
}

// SamePath reports whether two cache-relative paths are spellings of one path. A value
// that cannot be canonicalised matches nothing, including another unsafe value: the
// caller is deciding whether to delete a file, and two paths it cannot resolve are not
// grounds for treating them as one.
func SamePath(a, b string) bool {
	ca, err := Canonical(a)
	if err != nil {
		return false
	}
	cb, err := Canonical(b)
	if err != nil {
		return false
	}
	return ca == cb
}

// SameFile reports whether two cache-relative paths name one file on disk.
//
// SamePath cannot answer that everywhere. On a case-insensitive filesystem (Windows, and
// macOS as it is usually configured) "TOK/Pack.zip" and "TOK/pack.zip" are one file that
// no canonical form collapses. It answers false when either path is unsafe or absent, so
// a caller pairs it with SamePath rather than replacing it.
func SameFile(libraryRoot, a, b string) bool {
	fa, err := statRel(libraryRoot, a)
	if err != nil {
		return false
	}
	fb, err := statRel(libraryRoot, b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func statRel(libraryRoot, rel string) (os.FileInfo, error) {
	rt, name, err := rooted(libraryRoot, rel)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	return rt.Stat(name)
}

// rooted opens the library as an os.Root and canonicalises rel inside it, returning the
// name to hand the root's own methods.
//
// Canonical checks a spelling, and a spelling cannot see the whole question: a path whose
// every segment is an ordinary name still leaves the tree when one of those segments is a
// symlink. These paths arrive from the lockfile, which is committed and travels with the
// consuming project, and Remove deletes what it is given, so the confinement is enforced
// by the same call that acts, leaving no window between the check and the use.
func rooted(libraryRoot, rel string) (*os.Root, string, error) {
	clean, err := Canonical(rel)
	if err != nil {
		return nil, "", err
	}
	rt, err := os.OpenRoot(libraryRoot)
	if err != nil {
		return nil, "", err
	}
	return rt, filepath.FromSlash(clean), nil
}

// Pending is a fully-written but uncommitted download.
type Pending struct {
	RelPath string
	SHA256  string
	Size    int64

	libraryRoot string
	// Both are root-relative and in slash space, so Commit and Discard go back through
	// an os.Root rather than acting on a joined path.
	tempRel string
	dirRel  string
}

// TempPath is where the bytes currently are, so the caller can inspect them before
// deciding to commit.
func (p *Pending) TempPath() string {
	return filepath.Join(p.libraryRoot, filepath.FromSlash(p.tempRel))
}

// Commit renames the pending bytes to their real cache path.
func (p *Pending) Commit() error {
	rt, err := os.OpenRoot(p.libraryRoot)
	if err != nil {
		return err
	}
	defer rt.Close()
	if err := rt.Rename(filepath.FromSlash(p.tempRel), filepath.FromSlash(p.RelPath)); err != nil {
		p.abandon(rt)
		return err
	}
	return nil
}

// Discard removes the pending bytes, and the directory Store made for them if that
// leaves it empty. Callers use it whenever a check fails, so a rejected body never
// reaches a real cache path.
func (p *Pending) Discard() error {
	rt, err := os.OpenRoot(p.libraryRoot)
	if err != nil {
		return err
	}
	defer rt.Close()
	err = rt.Remove(filepath.FromSlash(p.tempRel))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	pruneEmptyDirs(rt, p.dirRel)
	return nil
}

func (p *Pending) abandon(rt *os.Root) {
	rt.Remove(filepath.FromSlash(p.tempRel))
	pruneEmptyDirs(rt, p.dirRel)
}

// pruneEmptyDirs removes relDir and each parent it empties, walking up but never past
// the library root. A directory reached through a symlink is refused by the root rather
// than removed, so a <fileToken>/ the user moved onto another disk keeps its link.
func pruneEmptyDirs(rt *os.Root, relDir string) {
	for rel := path.Clean(relDir); rel != "." && rel != "/" && !strings.HasPrefix(rel, ".."); rel = path.Dir(rel) {
		name := filepath.FromSlash(rel)
		f, err := rt.Open(name)
		if err != nil {
			return
		}
		entries, err := f.ReadDir(1)
		f.Close()
		// ReadDir(1) reports io.EOF for a directory with nothing in it, which is the one
		// case worth acting on.
		if len(entries) > 0 || (err != nil && err != io.EOF) {
			return
		}
		if rt.Remove(name) != nil {
			return
		}
	}
}

// newTemp creates a uniquely named temp inside dirRel, through the root. os.Root has no
// CreateTemp, and the point of the write path is that it must not step outside one.
func newTemp(rt *os.Root, dirRel string) (string, *os.File, error) {
	for range 10000 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, err
		}
		rel := path.Join(dirRel, tempPrefix+hex.EncodeToString(b[:]))
		f, err := rt.OpenFile(filepath.FromSlash(rel), os.O_RDWR|os.O_CREATE|os.O_EXCL, cachedFileMode)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return rel, f, nil
	}
	return "", nil, fmt.Errorf("could not create a temp file in %s", dirRel)
}

// Store streams r into a temp file in the directory its eventual destination
// <libraryRoot>/<fileToken>/<filename> lives in, hashing while writing. It does not
// rename: the caller commits or discards.
//
// It writes through the root for the reason every read does. A <fileToken>/ that is a
// symlink out of the library would otherwise take the download, record it, and then have
// Verify, Hash and the adopt scan all refuse the file it wrote, so the file classifies
// CacheMissing and re-downloads in full on every run with nothing said. Refusing here
// fails that file once, with the reason.
func Store(libraryRoot, fileToken, filename string, r io.Reader) (*Pending, error) {
	if err := safeIdentity(fileToken, filename); err != nil {
		return nil, err
	}
	// The root itself is created over the plain path: there is no root to open until it
	// exists, and it is the user's own setting rather than a recorded path.
	if err := os.MkdirAll(libraryRoot, 0o755); err != nil {
		return nil, err
	}
	rt, err := os.OpenRoot(libraryRoot)
	if err != nil {
		return nil, err
	}
	defer rt.Close()

	if err := rt.Mkdir(fileToken, 0o755); err != nil && !os.IsExist(err) {
		return nil, err
	}
	tempRel, tmp, err := newTemp(rt, fileToken)
	if err != nil {
		pruneEmptyDirs(rt, fileToken)
		return nil, confinementError(rt, fileToken, err)
	}
	// CreateTemp-style opens are subject to the umask, and the library is a mirror: its
	// path is configurable, so it can sit on a volume more than one account reads, and a
	// pack nobody else can open is not a mirror of anything.
	p := &Pending{RelPath: RelPath(fileToken, filename), libraryRoot: libraryRoot, tempRel: tempRel, dirRel: fileToken}
	fail := func(err error) (*Pending, error) {
		tmp.Close()
		p.abandon(rt)
		return nil, err
	}
	if err := tmp.Chmod(cachedFileMode); err != nil {
		return fail(err)
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		p.abandon(rt)
		return nil, err
	}
	p.SHA256 = hex.EncodeToString(h.Sum(nil))
	p.Size = size
	return p, nil
}

// confinementError names the symlink when that is why the root refused, because os.Root
// reports only "path escapes from parent" and a <fileToken>/ linked onto another disk is a
// reasonable thing for someone to have done to a large library.
func confinementError(rt *os.Root, dirRel string, err error) error {
	fi, lerr := rt.Lstat(filepath.FromSlash(dirRel))
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		return err
	}
	return fmt.Errorf("%s is a symlink out of the library, and every cache read and write is "+
		"confined to the library, so a file stored through it could never be found again "+
		"(replace the link with the real directory or a bind mount): %w", dirRel, err)
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
//
// The walk goes through the root's own FS rather than over the path. filepath.WalkDir
// begins with an Lstat and stops at anything that is not a directory, so a library_path
// that is a symlink is visited, not descended, and nothing is ever reclaimed there. A
// symlinked directory inside the library is still not descended.
func SweepTemps(libraryRoot string, minAge time.Duration) (int, int64) {
	var count int
	var bytes int64
	olderThan := time.Now().Add(-minAge)
	rt, err := os.OpenRoot(libraryRoot)
	if err != nil {
		return 0, 0
	}
	defer rt.Close()
	fs.WalkDir(rt.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), tempPrefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.ModTime().Before(olderThan) {
			return nil
		}
		if rt.Remove(filepath.FromSlash(p)) == nil {
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
	fi, err := statRel(libraryRoot, relPath)
	return err == nil && !fi.IsDir() && fi.Size() == wantSize
}

// VerifyDeep re-hashes the file. It is opt-in because a library runs to tens of
// gigabytes, and it is the only check that sees a mid-file corruption.
//
// Through Hash rather than hashing again here: the digest this compares against is one
// Hash produced when the file was adopted, and it holds for the life of the file, so
// the two have to agree forever about what they read and how. Two copies of that only
// agree until one of them changes.
func VerifyDeep(ctx context.Context, libraryRoot, relPath, sha string) bool {
	got, _, err := Hash(ctx, libraryRoot, relPath)
	return err == nil && got == sha
}

// open opens a cached file for reading through the root.
func open(libraryRoot, relPath string) (*os.File, error) {
	rt, name, err := rooted(libraryRoot, relPath)
	if err != nil {
		return nil, err
	}
	defer rt.Close()
	return rt.Open(name)
}

// Tail returns up to the last n bytes of a cached file, for a caller that has to look
// at a trailer to tell a whole file from a truncated one.
func Tail(libraryRoot, relPath string, n int) ([]byte, error) {
	f, err := open(libraryRoot, relPath)
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
	f, err := open(libraryRoot, relPath)
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
//
// It takes a context because one call can be minutes of reading a multi-gigabyte pack,
// and main's signal handler has taken SIGINT's default action away for the life of the
// run: without it a Ctrl-C during a full verify is ignored until the file is read.
func Hash(ctx context.Context, libraryRoot, relPath string) (sha string, size int64, err error) {
	f, err := open(libraryRoot, relPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err = io.Copy(h, &ctxReader{ctx: ctx, r: f})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// ctxReader ends a long read when the run does, checking between reads, so the
// granularity is io.Copy's buffer rather than the file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Remove deletes the file at relPath (used to prune a prior version), and the
// <fileToken>/ it leaves empty. A library or a file that is not there has nothing to
// remove.
func Remove(libraryRoot, relPath string) error {
	rt, name, err := rooted(libraryRoot, relPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer rt.Close()
	err = rt.Remove(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pruneEmptyDirs(rt, path.Dir(filepath.ToSlash(name)))
	return nil
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
