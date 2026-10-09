package atomicfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every error path in Write unlinks its temp, but a SIGKILL or a power cut between the
// create and the rename cannot, and the temp sits in the directory the user commits.
// Nothing else would ever remove it, so the next write of the same file does — sparing
// one young enough to be a concurrent run's write in flight, and anything that is not
// this file's temp.
func TestWriteSweepsTempsAKilledWriteLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "synty-sync.lock.json")
	orphan := filepath.Join(dir, ".synty-lock-123456")
	inflight := filepath.Join(dir, ".synty-lock-654321")
	otherFile := filepath.Join(dir, ".synty-sync-777777")
	for _, p := range []string{orphan, inflight, otherFile} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTempAge)
	for _, p := range []string{orphan, otherFile} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := Write(path, ".synty-lock-*", write("{}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphaned temp survived a write of the file it belonged to")
	}
	if _, err := os.Stat(inflight); err != nil {
		t.Error("a temp younger than the threshold was swept: a concurrent write in flight")
	}
	if _, err := os.Stat(otherFile); err != nil {
		t.Error("a write swept another file's temp")
	}
}

func write(s string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := w.Write([]byte(s))
		return err
	}
}

func TestWriteCreatesAReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.json")
	if err := Write(path, ".tmp-*", write("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644; CreateTemp opens 0600 and the rename carries it", fi.Mode().Perm())
	}
}

func TestWriteKeepsTheModeOfTheFileItRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.toml")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, ".tmp-*", write("new")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the 0640 the file already had", fi.Mode().Perm())
	}
}

// These files are committed and travel with the consuming project, so a failed write
// must leave the previous one exactly as it was, and must not strand a dot-file beside
// it that the project would then carry around.
func TestFailedWriteLeavesThePriorFileAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.json")
	const prior = "the previous contents"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("encoder gave up")
	if err := Write(path, ".tmp-*", func(w io.Writer) error {
		// The temp has to live in the destination's own directory, or the rename that
		// finishes the write crosses a filesystem and fails with EXDEV on the ordinary
		// Linux layout: a tmpfs /tmp and the consuming project on /home. Nothing else
		// here can see that — t.TempDir() and $TMPDIR are the same filesystem under
		// test, and the entry count below reads the same either way.
		named, ok := w.(interface{ Name() string })
		if !ok {
			t.Fatalf("Write handed the encoder a %T, which cannot be asked where it lives", w)
		}
		if got := filepath.Dir(named.Name()); got != dir {
			t.Errorf("temp is in %q, want %q; a rename out of there fails with EXDEV", got, dir)
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the encoder's own error", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != prior {
		t.Errorf("content = %q, want the prior file untouched", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the target; a temp was stranded", len(entries))
	}
}

// Sync is the step that makes the rename mean something, and it is the one step with
// no observable effect to assert: renaming is atomic against a concurrent reader but
// says nothing about durability, so the failure this prevents needs a power cut to
// reproduce. Both files this package writes are the authoritative record of bytes that
// are already on disk, so losing one to a crash costs more than the fsync does.
func TestWriteFlushesBeforeRenaming(t *testing.T) {
	src, err := os.ReadFile("atomicfile.go")
	if err != nil {
		t.Fatal(err)
	}
	sync := strings.Index(string(src), "tmp.Sync()")
	rename := strings.Index(string(src), "os.Rename(")
	if sync < 0 {
		t.Fatal("Write no longer flushes the temp before renaming it over the destination")
	}
	if rename < 0 || sync > rename {
		t.Error("the Sync must come before the Rename, or the rename can outlive the data")
	}
}
