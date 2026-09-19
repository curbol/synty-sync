package atomicfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err := Write(path, ".tmp-*", func(io.Writer) error { return boom }); !errors.Is(err, boom) {
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
