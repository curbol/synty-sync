// Package atomicfile replaces a committed file in one step: write a temp beside it,
// flush it to disk, then rename over the destination. The lockfile and the project
// manifest both travel with the consuming project, so a reader must never see a
// half-written one and a failure must never leave the previous one truncated.
package atomicfile

import (
	"io"
	"os"
	"path/filepath"
)

// Write replaces path with whatever write emits. The temp lives in path's own
// directory, so the rename is same-filesystem, and it is removed on every failure
// path. The file keeps the mode path already had, or 0644 when it is being created:
// os.CreateTemp opens at 0600 and the mode survives the rename, which would quietly
// narrow a file the project shares.
//
// The Sync is what makes the rename mean something. Renaming is atomic against a
// concurrent reader but says nothing about durability, so without it a crash between
// this call and writeback can leave a full-length file of zeros where the committed
// record was — and the record is what the bytes on disk are named by.
func Write(path, tempPattern string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), tempPattern)
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(modeOf(path)); err != nil {
		return cleanup(err)
	}
	if err := write(tmp); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// modeOf is the mode a rewritten committed file keeps: whatever it already had, or a
// readable default when it is being created.
func modeOf(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}
