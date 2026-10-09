// Package atomicfile replaces a committed file in one step: write a temp beside it,
// flush it to disk, then rename over the destination. The lockfile and the project
// manifest both travel with the consuming project, so a reader must never see a
// half-written one and a failure must never leave the previous one truncated.
package atomicfile

import (
	"io"
	"os"
	"path/filepath"
	"time"
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
//
// It first sweeps stale temps of the same pattern beside path: see sweepTemps.
func Write(path, tempPattern string, write func(io.Writer) error) error {
	sweepTemps(filepath.Dir(path), tempPattern, staleTempAge)
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
	SyncDir(filepath.Dir(path))
	return nil
}

// SyncDir flushes the directory entry a rename created. Syncing the file makes the
// new bytes durable; this is what makes the name pointing at them durable, so a crash
// cannot leave the directory still naming the file the rename replaced.
//
// Exported because Write is not the only place that finishes with a rename: selfupdate
// renames an extracted binary over the running one, and the durability half of that
// contract belongs here with the rest of it rather than in a second copy that can drift.
//
// Best-effort on purpose: Windows does not hand out a syncable handle for a directory,
// and a write that genuinely landed must not be reported as failed there.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// staleTempAge is how old a temp must be before a write treats it as orphaned. A write
// holds its temp only for an encode and a flush, so this clears any concurrent one by a
// wide margin.
const staleTempAge = time.Hour

// sweepTemps removes temps matching tempPattern in dir that have gone untouched for at
// least minAge. Every error path in Write unlinks its own, but a SIGKILL or a power cut
// between the create and the rename cannot, and the leftover sits in the directory the
// user commits with nothing else ever removing it.
//
// Housekeeping only, so nothing here fails: a write must not be refused over a temp it
// could not read or remove.
func sweepTemps(dir, tempPattern string, minAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	olderThan := time.Now().Add(-minAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ok, _ := filepath.Match(tempPattern, e.Name()); !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().Before(olderThan) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// modeOf is the mode a rewritten committed file keeps: whatever it already had, or a
// readable default when it is being created.
func modeOf(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}
