package assetindex

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// buildFixture creates a small library and returns a built index plus its root.
func buildFixture(t *testing.T) (*Index, string) {
	t.Helper()
	root := t.TempDir()
	cacheDir := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		return p
	}
	writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
		"SourceFiles/Models/Heart.fbx":   "FBXHEARTDATA",
		"SourceFiles/Textures/Heart.png": "PNGDATA",
	})
	writeUnityPackage(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []unityGUID{
		{guid: "aaa", pathname: "Assets/Foo/Heart.prefab", asset: "PREFABBYTES", preview: true},
		{guid: "ccc", pathname: "Assets/Foo/Rock.fbx", asset: "ROCKBYTES"},
	})
	os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLBBYTES"), 0o644)

	ix, err := Build(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	return ix, root
}

func find(t *testing.T, ix *Index, name string, kind SourceKind) Asset {
	t.Helper()
	for _, a := range ix.Assets {
		if a.Name == name && a.Source.Kind == kind {
			return a
		}
	}
	t.Fatalf("asset %q (%s) not found", name, kind)
	return Asset{}
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenAcrossSources(t *testing.T) {
	ix, _ := buildFixture(t)

	loose := find(t, ix, "Sword.glb", SourceLoose)
	rc, size, err := ix.Open(loose)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "GLBBYTES" || size != int64(len("GLBBYTES")) {
		t.Errorf("loose Open = %q size %d", got, size)
	}

	zipEntry := find(t, ix, "Heart.fbx", SourceZip)
	rc, size, err = ix.Open(zipEntry)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "FBXHEARTDATA" || size != int64(len("FBXHEARTDATA")) {
		t.Errorf("zip Open = %q size %d", got, size)
	}

	unity := find(t, ix, "Rock.fbx", SourceUnityPackage)
	rc, _, err = ix.Open(unity)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "ROCKBYTES" {
		t.Errorf("unity Open = %q", got)
	}
}

func TestOpenThumbnail(t *testing.T) {
	ix, _ := buildFixture(t)
	prefab := find(t, ix, "Heart.prefab", SourceUnityPackage)
	if prefab.Thumb != ThumbPreview {
		t.Fatalf("prefab thumb = %s, want preview", prefab.Thumb)
	}
	rc, _, err := ix.OpenThumbnail(prefab)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc); got != "PNGPREVIEW" {
		t.Errorf("thumbnail = %q", got)
	}

	// An asset without a preview has no thumbnail.
	rock := find(t, ix, "Rock.fbx", SourceUnityPackage)
	if _, _, err := ix.OpenThumbnail(rock); err != ErrNoThumbnail {
		t.Errorf("Rock.fbx thumbnail err = %v, want ErrNoThumbnail", err)
	}
}

// Concurrent fetches from the same unitypackage must all return complete bytes,
// exercising the single-flight + atomic extract.
func TestConcurrentUnityExtract(t *testing.T) {
	ix, _ := buildFixture(t)
	rock := find(t, ix, "Rock.fbx", SourceUnityPackage)

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, _, err := ix.Open(rock)
			if err != nil {
				errs <- err
				return
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				errs <- err
				return
			}
			if string(b) != "ROCKBYTES" {
				errs <- io.ErrUnexpectedEOF
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Open: %v", err)
	}
}

func TestOpenRejectsOutsideRoot(t *testing.T) {
	ix, root := buildFixture(t)
	outside := filepath.Join(filepath.Dir(root), "escape.txt")
	os.WriteFile(outside, []byte("SECRET"), 0o644)
	bad := Asset{Source: Source{Kind: SourceLoose, FilePath: outside}}
	if _, _, err := ix.Open(bad); err != ErrOutsideRoot {
		t.Errorf("Open outside root err = %v, want ErrOutsideRoot", err)
	}
}

func TestLookupUnknownID(t *testing.T) {
	ix, _ := buildFixture(t)
	if _, ok := ix.Lookup("deadbeef"); ok {
		t.Error("unknown id resolved")
	}
	// A known id resolves.
	first := ix.Assets[0]
	if a, ok := ix.Lookup(first.ID); !ok || a.ID != first.ID {
		t.Error("known id did not resolve")
	}
}

func TestSaveLoadRefresh(t *testing.T) {
	root := t.TempDir()
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "index.json")
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		return p
	}
	writeZip(t, mk("synty", "P", "P_SourceFiles_v1.zip"), map[string]string{"A/x.fbx": "X"})

	ix, err := LoadOrBuild(root, cacheDir, cachePath, false)
	if err != nil {
		t.Fatal(err)
	}
	n0 := len(ix.Assets)
	if n0 == 0 {
		t.Fatal("no assets built")
	}

	// Reload from cache and refresh: same count, ids resolve.
	ix2, err := LoadOrBuild(root, cacheDir, cachePath, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix2.Assets) != n0 {
		t.Errorf("reload count = %d, want %d", len(ix2.Assets), n0)
	}
	if _, ok := ix2.Lookup(ix.Assets[0].ID); !ok {
		t.Error("id lookup broken after reload")
	}

	// Add a loose file, refresh picks it up.
	os.WriteFile(mk("explosive", "R", "new.glb"), []byte("G"), 0o644)
	ix3, err := LoadOrBuild(root, cacheDir, cachePath, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix3.Assets) != n0+1 {
		t.Errorf("after add, count = %d, want %d", len(ix3.Assets), n0+1)
	}
}
