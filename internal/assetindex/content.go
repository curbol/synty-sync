package assetindex

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNoThumbnail is returned by OpenThumbnail for an asset that has no dedicated
// thumbnail resource (only Unity preview.png assets do).
var ErrNoThumbnail = errors.New("asset has no thumbnail")

// ErrOutsideRoot guards against a stale cache pointing at a path no longer under
// the configured library root.
var ErrOutsideRoot = errors.New("asset path is outside the library root")

// Open streams an asset's bytes and its size. Loose files and zip entries are read
// directly; unitypackage assets are read from the extract-once cache. Every
// filesystem/archive path is confirmed to resolve under the library root first.
func (ix *Index) Open(a Asset) (io.ReadCloser, int64, error) {
	switch a.Source.Kind {
	case SourceLoose:
		if !ix.underRoot(a.Source.FilePath) {
			return nil, 0, ErrOutsideRoot
		}
		return openFile(a.Source.FilePath)
	case SourceZip:
		if !ix.underRoot(a.Source.ArchivePath) {
			return nil, 0, ErrOutsideRoot
		}
		return openZipEntry(a.Source.ArchivePath, a.Source.Entry)
	case SourceUnityPackage:
		if !ix.underRoot(a.Source.ArchivePath) {
			return nil, 0, ErrOutsideRoot
		}
		dir, err := ix.ensureExtracted(a.Source.ArchivePath)
		if err != nil {
			return nil, 0, err
		}
		return openFile(filepath.Join(dir, a.Source.Guid, "asset"))
	}
	return nil, 0, errors.New("unknown source kind")
}

// OpenThumbnail streams a Unity preview.png for an asset that has one.
func (ix *Index) OpenThumbnail(a Asset) (io.ReadCloser, int64, error) {
	if a.Source.Kind != SourceUnityPackage || !a.Source.HasPreview {
		return nil, 0, ErrNoThumbnail
	}
	if !ix.underRoot(a.Source.ArchivePath) {
		return nil, 0, ErrOutsideRoot
	}
	dir, err := ix.ensureExtracted(a.Source.ArchivePath)
	if err != nil {
		return nil, 0, err
	}
	return openFile(filepath.Join(dir, a.Source.Guid, "preview.png"))
}

func openFile(p string) (io.ReadCloser, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// ensureExtracted decompresses a unitypackage once into the cache and returns its
// unpacked dir. Extraction is single-flighted per archive fingerprint (concurrent
// grid fetches of the same package wait rather than each decompressing hundreds of
// MB), and is written to a temp dir renamed atomically into place so no reader ever
// sees a half-written entry.
// PruneUnpacked removes extraction directories whose archive fingerprint is no
// longer in the index. The fingerprint includes the archive's mtime, so every pack
// update writes to a new directory and would otherwise strand the previous
// extraction (hundreds of MB per Synty pack) in the cache forever.
func (ix *Index) PruneUnpacked() error {
	if ix.cacheDir == "" {
		return nil
	}
	dir := filepath.Join(ix.cacheDir, "unpacked")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(ix.ArchivePrint))
	for _, fp := range ix.ArchivePrint {
		live[fp] = true
	}
	var firstErr error
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (ix *Index) ensureExtracted(archivePath string) (string, error) {
	fp, err := fingerprint(archivePath)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(ix.cacheDir, "unpacked", fp)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	ix.extractMu.Lock()
	if ix.extractOnce == nil {
		ix.extractOnce = map[string]*sync.Once{}
		ix.extractErr = map[string]error{}
	}
	once := ix.extractOnce[fp]
	if once == nil {
		once = &sync.Once{}
		ix.extractOnce[fp] = once
	}
	ix.extractMu.Unlock()

	once.Do(func() {
		if _, err := os.Stat(dest); err == nil {
			return
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			ix.setExtractErr(fp, err)
			return
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dest), "unpack-*")
		if err != nil {
			ix.setExtractErr(fp, err)
			return
		}
		if err := extractUnityPackage(archivePath, tmp); err != nil {
			os.RemoveAll(tmp)
			ix.setExtractErr(fp, err)
			return
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.RemoveAll(tmp)
			// A racing run may have created dest first; that is success.
			if _, statErr := os.Stat(dest); statErr != nil {
				ix.setExtractErr(fp, err)
			}
		}
	})

	ix.extractMu.Lock()
	err = ix.extractErr[fp]
	if err != nil {
		// Re-arm so a later request retries: a failure (e.g. transient disk-full)
		// shouldn't poison this package for the whole process lifetime.
		delete(ix.extractOnce, fp)
		delete(ix.extractErr, fp)
	}
	ix.extractMu.Unlock()
	return dest, err
}

func (ix *Index) setExtractErr(fp string, err error) {
	ix.extractMu.Lock()
	ix.extractErr[fp] = err
	ix.extractMu.Unlock()
}

// underRoot reports whether p resolves to a location inside the library root,
// following symlinks so a symlinked entry cannot escape.
func (ix *Index) underRoot(p string) bool {
	root := resolve(ix.Root)
	rp := resolve(p)
	rel, err := filepath.Rel(root, rp)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve returns the symlink-resolved absolute path, falling back to a lexical
// clean when the path (or a parent) can't be resolved.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}
