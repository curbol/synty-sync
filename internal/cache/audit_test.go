package cache

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file token names a directory under the library root and comes from item-page
// label text, so it is exactly as untrusted as the download filename beside it.
func TestStoreRejectsUnsafeFileToken(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"..", "../escaped", "a/b", ""} {
		if _, _, _, err := Store(root, token, "pack.zip", strings.NewReader("X")); err == nil {
			t.Errorf("token %q was accepted", token)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "escaped")); err == nil {
		t.Error("a write escaped the library root")
	}
}

// A download that dies mid-stream must not leave its partial temp file behind.
func TestStoreCleansUpAfterFailedCopy(t *testing.T) {
	root := t.TempDir()
	_, _, _, err := Store(root, "TOKEN", "pack.zip", io.MultiReader(
		strings.NewReader("partial"), errReader{errors.New("connection reset")}))
	if err == nil {
		t.Fatal("expected the copy failure to surface")
	}
	entries, err := os.ReadDir(filepath.Join(root, "TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left behind %q after a failed download", e.Name())
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
