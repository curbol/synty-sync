package assetindex

import (
	"archive/zip"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
)

// safeEntry rejects archive entry names that are absolute or escape their archive
// via "..". Such names never enter the index, so the content API can never be
// tricked into serving a path outside the archive.
func safeEntry(name string) bool {
	if name == "" || path.IsAbs(name) || strings.HasPrefix(name, "/") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// zipAssets enumerates the files inside a .zip as assets. Directory entries and
// unsafe names are skipped. displayRel is the archive's path relative to the
// library root (for RelPath); archivePath is absolute (for CopyPath and Open).
func zipAssets(archivePath, displayRel, vendor, pack, variant string) ([]Asset, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", archivePath, err)
	}
	defer zr.Close()

	var assets []Asset
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !safeEntry(f.Name) || skipEntry(f.Name) {
			continue
		}
		src := Source{Kind: SourceZip, ArchivePath: archivePath, Entry: f.Name}
		assets = append(assets, newAsset(src,
			path.Base(f.Name),
			archiveRel(displayRel, f.Name),
			vendor, pack, variant,
			int64(f.UncompressedSize64),
			crcFingerprint(f.CRC32, int64(f.UncompressedSize64)),
		))
	}
	return assets, nil
}

// openZipEntry streams one entry's bytes by exact-name match. The name comes from
// an indexed asset (never raw client input), and is re-validated defensively.
func openZipEntry(archivePath, entry string) (io.ReadCloser, int64, error) {
	if !safeEntry(entry) {
		return nil, 0, fmt.Errorf("unsafe zip entry %q", entry)
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range zr.File {
		if f.Name == entry {
			rc, err := f.Open()
			if err != nil {
				zr.Close()
				return nil, 0, err
			}
			return &zipEntryReader{rc: rc, zr: zr}, int64(f.UncompressedSize64), nil
		}
	}
	zr.Close()
	return nil, 0, fmt.Errorf("entry %q not found in %s", entry, filepath.Base(archivePath))
}

// zipEntryReader keeps the zip.ReadCloser alive for the entry's lifetime, closing
// both together.
type zipEntryReader struct {
	rc io.ReadCloser
	zr *zip.ReadCloser
}

func (r *zipEntryReader) Read(p []byte) (int, error) { return r.rc.Read(p) }
func (r *zipEntryReader) Close() error {
	err := r.rc.Close()
	if zerr := r.zr.Close(); err == nil {
		err = zerr
	}
	return err
}
