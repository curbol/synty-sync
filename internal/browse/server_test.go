package browse

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/curbol/synty-sync/internal/assetindex"
)

func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeUnity(t *testing.T, path string, guids []struct {
	guid, pathname, asset string
	preview               bool
}) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	put := func(name, content string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		tw.Write([]byte(content))
	}
	for _, g := range guids {
		put(g.guid+"/pathname", g.pathname)
		put(g.guid+"/asset.meta", "meta")
		put(g.guid+"/asset", g.asset)
		if g.preview {
			put(g.guid+"/preview.png", "PNGPREVIEW")
		}
	}
	tw.Close()
	gz.Close()
	os.WriteFile(path, buf.Bytes(), 0o644)
}

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	cache := t.TempDir()
	mk := func(p ...string) string {
		full := filepath.Join(append([]string{root}, p...)...)
		os.MkdirAll(filepath.Dir(full), 0o755)
		return full
	}

	writeZip(t, mk("synty", "Foo_Pack", "Foo_Pack_SourceFiles_v3.zip"), map[string]string{
		"SourceFiles/Heart.fbx": "FBXHEART",
		"SourceFiles/Heart.png": "PNGHEART",
	})
	writeUnity(t, mk("synty", "Foo_Pack", "Foo_Pack_Unity_2022_3_v1_0_0.unitypackage"), []struct {
		guid, pathname, asset string
		preview               bool
	}{
		{"aaa", "Assets/Foo/Heart.prefab", "PREFABBYTES", true},
		{"ccc", "Assets/Foo/Rock.fbx", "ROCKBYTES", false},
	})
	os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLBBYTES"), 0o644)

	ix, err := assetindex.Build(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(ix)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv
}

type assetsResp struct {
	Total  int
	Offset int
	Items  []struct {
		ID, Name, Category, Ext, Variant, CopyPath string
		Thumb                                      string
		Count                                      int
		Copies                                     []struct {
			Variant, Pack, CopyPath string
		}
	}
	Facets struct {
		Categories, Vendors, Variants []struct {
			Value string
			Count int
		}
	}
}

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getAssets(t *testing.T, srv *httptest.Server, qs string) assetsResp {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/assets?" + qs)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out assetsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIndexServesHTML(t *testing.T) {
	srv := testServer(t)
	resp := mustGet(t, srv.URL+"/")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") == "" || !bytes.Contains(b, []byte("importmap")) {
		t.Errorf("index html missing importmap; ct=%s", resp.Header.Get("Content-Type"))
	}
}

func TestSearchAndFacets(t *testing.T) {
	srv := testServer(t)
	r := getAssets(t, srv, "q=heart")
	if r.Total == 0 {
		t.Fatal("no heart results")
	}
	for _, it := range r.Items {
		if !bytes.Contains(bytes.ToLower([]byte(it.Name)), []byte("heart")) {
			t.Errorf("non-heart result %q", it.Name)
		}
	}
	if len(r.Facets.Categories) == 0 || len(r.Facets.Vendors) == 0 || len(r.Facets.Variants) == 0 {
		t.Error("missing facets")
	}
}

func TestTypeFilter(t *testing.T) {
	srv := testServer(t)
	r := getAssets(t, srv, "type=image")
	if r.Total == 0 {
		t.Fatal("no image results")
	}
	for _, it := range r.Items {
		if it.Category != "image" {
			t.Errorf("type=image returned %q (%s)", it.Name, it.Category)
		}
	}
}

// Repeated type params union their categories, so "model AND image" shows both.
func TestMultiValueTypeFilter(t *testing.T) {
	srv := testServer(t)
	r := getAssets(t, srv, "type=image&type=model")
	if r.Total == 0 {
		t.Fatal("no results for multi-type filter")
	}
	seen := map[string]bool{}
	for _, it := range r.Items {
		if it.Category != "image" && it.Category != "model" {
			t.Errorf("multi-type returned %q (%s)", it.Name, it.Category)
		}
		seen[it.Category] = true
	}
	if !seen["image"] || !seen["model"] {
		t.Errorf("multi-type missing a category: %v", seen)
	}
}

// Repeated variant params union their buckets, including the empty (loose) bucket.
func TestMultiValueVariantFilter(t *testing.T) {
	srv := testServer(t)
	r := getAssets(t, srv, "variant=SourceFiles&variant=")
	sf, loose := false, false
	for _, it := range r.Items {
		switch it.Variant {
		case "SourceFiles":
			sf = true
		case "":
			loose = true
		default:
			t.Errorf("multi-variant returned unexpected variant %q (%s)", it.Variant, it.Name)
		}
	}
	if !sf || !loose {
		t.Errorf("multi-variant missing a bucket: sourcefiles=%v loose=%v", sf, loose)
	}
}

// Facet dropdowns are ordered alphabetically by value (case-insensitive), not by
// count, so a specific option is easy to find.
func TestFacetsSortedByName(t *testing.T) {
	srv := testServer(t)
	r := getAssets(t, srv, "")
	assertSorted := func(name string, vals []struct {
		Value string
		Count int
	}) {
		for i := 1; i < len(vals); i++ {
			if strings.ToLower(vals[i-1].Value) > strings.ToLower(vals[i].Value) {
				t.Errorf("%s facets not alphabetical: %q before %q", name, vals[i-1].Value, vals[i].Value)
			}
		}
	}
	assertSorted("category", r.Facets.Categories)
	assertSorted("vendor", r.Facets.Vendors)
	assertSorted("variant", r.Facets.Variants)
}

// Two Heart-named assets exist in different variants; each is reachable by its
// variant filter and neither is merged away.
func TestVariantFilterNonLossy(t *testing.T) {
	srv := testServer(t)
	src := getAssets(t, srv, "q=Heart&variant=SourceFiles")
	uni := getAssets(t, srv, "q=Heart&variant=Unity_2022_3")
	if src.Total == 0 {
		t.Error("SourceFiles Heart missing")
	}
	if uni.Total == 0 {
		t.Error("Unity Heart missing")
	}
	for _, it := range src.Items {
		if it.Variant != "SourceFiles" {
			t.Errorf("variant filter leaked %q", it.Variant)
		}
	}
}

// A present-but-empty variant param filters to the loose/unknown bucket (the
// frontend's "(loose / unknown)" option), distinct from an absent param (all).
// The same file shipped in two variants collapses to one grouped item exposing
// both copy-paths; group=0 shows them separately.
func TestGroupDuplicates(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	mk := func(p ...string) string {
		full := filepath.Join(append([]string{root}, p...)...)
		os.MkdirAll(filepath.Dir(full), 0o755)
		return full
	}
	writeZip(t, mk("synty", "Dup_Pack", "Dup_Pack_SourceFiles_v3.zip"), map[string]string{
		"SourceFiles/Coin.fbx": "COINDATA",
	})
	writeUnity(t, mk("synty", "Dup_Pack", "Dup_Pack_Unity_2022_3_v1_0_0.unitypackage"), []struct {
		guid, pathname, asset string
		preview               bool
	}{
		{"g1", "Assets/Dup/Coin.fbx", "COINDATA", false}, // same name + bytes -> same size
	})
	ix, err := assetindex.Build(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(ix)
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	grouped := getAssets(t, srv, "q=Coin")
	if grouped.Total != 1 {
		t.Fatalf("grouped total = %d, want 1", grouped.Total)
	}
	it := grouped.Items[0]
	if it.Count != 2 || len(it.Copies) != 2 {
		t.Fatalf("group count = %d, copies = %d, want 2/2", it.Count, len(it.Copies))
	}
	variants := map[string]bool{}
	for _, c := range it.Copies {
		variants[c.Variant] = true
		if c.CopyPath == "" {
			t.Error("copy has no path")
		}
	}
	if !variants["SourceFiles"] || !variants["Unity_2022_3"] {
		t.Errorf("copies missing a variant: %v", variants)
	}

	ungrouped := getAssets(t, srv, "q=Coin&group=0")
	if ungrouped.Total != 2 {
		t.Errorf("ungrouped total = %d, want 2", ungrouped.Total)
	}
}

func TestEmptyVariantFilter(t *testing.T) {
	srv := testServer(t)
	all := getAssets(t, srv, "")
	loose := getAssets(t, srv, "variant=")
	if loose.Total == 0 {
		t.Fatal("empty-variant filter returned nothing (expected the loose Sword.glb)")
	}
	if loose.Total >= all.Total {
		t.Errorf("variant= did not narrow: %d vs all %d", loose.Total, all.Total)
	}
	for _, it := range loose.Items {
		if it.Variant != "" {
			t.Errorf("variant= returned a non-empty variant %q", it.Variant)
		}
	}
}

func TestPagination(t *testing.T) {
	srv := testServer(t)
	r := getAssets(t, srv, "limit=1")
	if r.Total <= 1 {
		t.Skip("fixture too small to paginate")
	}
	if len(r.Items) != 1 {
		t.Errorf("limit=1 returned %d items", len(r.Items))
	}
	r2 := getAssets(t, srv, "limit=1&offset=1")
	if r2.Offset != 1 || len(r2.Items) != 1 {
		t.Errorf("offset page wrong: offset=%d items=%d", r2.Offset, len(r2.Items))
	}
	if r.Items[0].ID == r2.Items[0].ID {
		t.Error("pages returned the same item")
	}
}

func idByName(t *testing.T, srv *httptest.Server, name string) string {
	t.Helper()
	r := getAssets(t, srv, "q="+name)
	for _, it := range r.Items {
		if it.Name == name {
			return it.ID
		}
	}
	t.Fatalf("asset %q not found", name)
	return ""
}

func TestContentBytesAndHeaders(t *testing.T) {
	srv := testServer(t)

	// zip-entry fbx: octet-stream + correct bytes + Content-Length.
	id := idByName(t, srv, "Heart.fbx")
	resp := mustGet(t, srv.URL+"/api/content?id="+id)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "FBXHEART" {
		t.Errorf("fbx content = %q", b)
	}
	if resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("fbx content-type = %s", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Content-Length") != "8" {
		t.Errorf("fbx content-length = %s", resp.Header.Get("Content-Length"))
	}

	// zip-entry png: image/png.
	pid := idByName(t, srv, "Heart.png")
	resp = mustGet(t, srv.URL+"/api/content?id="+pid)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("png content-type = %s", resp.Header.Get("Content-Type"))
	}

	// unity asset bytes.
	rid := idByName(t, srv, "Rock.fbx")
	resp = mustGet(t, srv.URL+"/api/content?id="+rid)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ROCKBYTES" {
		t.Errorf("unity content = %q", b)
	}

	// unknown id → 404.
	resp = mustGet(t, srv.URL+"/api/content?id=deadbeef")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id status = %d", resp.StatusCode)
	}
}

func TestThumbnail(t *testing.T) {
	srv := testServer(t)
	// prefab has a preview.png.
	id := idByName(t, srv, "Heart.prefab")
	resp := mustGet(t, srv.URL+"/api/thumb?id="+id)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "PNGPREVIEW" || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("thumb status=%d body=%q ct=%s", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	// Rock.fbx has no preview → 404.
	rid := idByName(t, srv, "Rock.fbx")
	resp = mustGet(t, srv.URL+"/api/thumb?id="+rid)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no-preview thumb status = %d", resp.StatusCode)
	}
}

func TestConcurrentUnityContent(t *testing.T) {
	srv := testServer(t)
	rid := idByName(t, srv, "Rock.fbx")
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/api/content?id=" + rid)
			if err != nil {
				errs <- err.Error()
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "ROCKBYTES" {
				errs <- "got " + string(b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent content: %s", e)
	}
}
