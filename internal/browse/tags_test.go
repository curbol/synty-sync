package browse

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/curbol/synty-sync/internal/assetindex"
	"github.com/curbol/synty-sync/internal/tagstore"
)

type paletteResp struct {
	Enabled bool
	Tags    []struct {
		ID, Color string
		Count     int
	}
}

type assignResp struct {
	Tags    []string
	Palette paletteResp
}

type taggedItem struct {
	Name         string
	Fingerprints []string
	Tags         []string
	Related      []string
}

type taggedAssetsResp struct {
	Total int
	Items []taggedItem
}

// enabledServer builds a small library and an enabled tag server over a temp tag
// store, returning the httptest server and the tag-store path on disk.
func enabledServer(t *testing.T) (*httptest.Server, string) {
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
	})
	os.WriteFile(mk("explosive", "RPG", "Sword.glb"), []byte("GLBBYTES"), 0o644)

	ix, err := assetindex.Build(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	tagsPath := filepath.Join(t.TempDir(), tagstore.FileName)
	store, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(ix, store, tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv, tagsPath
}

func doJSON(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	rq, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func taggedAssets(t *testing.T, srv *httptest.Server, qs string) taggedAssetsResp {
	t.Helper()
	resp := doJSON(t, "GET", srv.URL+"/api/assets?"+qs, nil)
	var out taggedAssetsResp
	decode(t, resp, &out)
	return out
}

func itemByName(t *testing.T, srv *httptest.Server, qs, name string) taggedItem {
	t.Helper()
	for _, it := range taggedAssets(t, srv, qs).Items {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("item %q not found (qs=%q)", name, qs)
	return taggedItem{}
}

func TestTagsDisabled(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	writeZip(t, filepath.Join(root, "synty", "P", "P_SourceFiles_v3.zip"), map[string]string{"SourceFiles/A.fbx": "AAA"})
	ix, err := assetindex.Build(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(ix, nil, "")
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	var p paletteResp
	decode(t, doJSON(t, "GET", srv.URL+"/api/tags", nil), &p)
	if p.Enabled {
		t.Error("tags should report disabled with no tagsPath")
	}
	resp := doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": []string{"x"}, "tag": "hero", "on": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("assign while disabled status = %d, want 409", resp.StatusCode)
	}
}

func TestTagCreateAndPalette(t *testing.T) {
	srv, _ := enabledServer(t)
	var p paletteResp
	decode(t, doJSON(t, "POST", srv.URL+"/api/tags", map[string]any{"id": "hero", "color": "#E11D48"}), &p)
	if !p.Enabled || len(p.Tags) != 1 || p.Tags[0].ID != "hero" || p.Tags[0].Color != "#e11d48" {
		t.Fatalf("palette after create = %+v", p)
	}
	// Invalid color rejected.
	resp := doJSON(t, "POST", srv.URL+"/api/tags", map[string]any{"id": "bad", "color": "red"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad color status = %d, want 400", resp.StatusCode)
	}
}

func TestAssignToggleAndDTO(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	if len(heart.Fingerprints) == 0 {
		t.Fatal("Heart.fbx has no fingerprints in the DTO")
	}
	if len(heart.Tags) != 0 {
		t.Fatalf("Heart.fbx starts with tags: %v", heart.Tags)
	}

	// Assign a brand-new tag via assign: it is auto-created and the response palette
	// carries its default color (so the client can render the sliver immediately).
	var ar assignResp
	decode(t, doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "fresh", "on": true}), &ar)
	if len(ar.Tags) != 1 || ar.Tags[0] != "fresh" {
		t.Fatalf("assign response tags = %v, want [fresh]", ar.Tags)
	}
	var freshColor string
	for _, tg := range ar.Palette.Tags {
		if tg.ID == "fresh" {
			freshColor = tg.Color
		}
	}
	if freshColor != tagstore.DefaultColor("fresh") {
		t.Errorf("auto-created tag color = %q, want default %q", freshColor, tagstore.DefaultColor("fresh"))
	}

	// The DTO now reports the tag on the card.
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Tags; len(got) != 1 || got[0] != "fresh" {
		t.Errorf("Heart.fbx tags after assign = %v, want [fresh]", got)
	}

	// Persisted to disk.
	reloaded, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Counts()["fresh"] != 1 {
		t.Errorf("tag not persisted: counts = %v", reloaded.Counts())
	}

	// Toggle off removes it.
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "fresh", "on": false}).Body.Close()
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Tags; len(got) != 0 {
		t.Errorf("Heart.fbx tags after toggle-off = %v, want none", got)
	}
}

func TestRenameRecolorDelete(t *testing.T) {
	srv, _ := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "wip", "on": true}).Body.Close()

	// Rename wip -> in-progress and recolor in one PATCH.
	var p paletteResp
	decode(t, doJSON(t, "PATCH", srv.URL+"/api/tags", map[string]any{"id": "wip", "newId": "in-progress", "color": "#00ff00"}), &p)
	if len(p.Tags) != 1 || p.Tags[0].ID != "in-progress" || p.Tags[0].Color != "#00ff00" {
		t.Fatalf("palette after rename+recolor = %+v", p)
	}
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Tags; len(got) != 1 || got[0] != "in-progress" {
		t.Errorf("assignment not rewritten by rename: %v", got)
	}

	// Delete removes it from the palette and the card.
	decode(t, doJSON(t, "DELETE", srv.URL+"/api/tags", map[string]any{"id": "in-progress"}), &p)
	if len(p.Tags) != 0 {
		t.Errorf("palette after delete = %+v", p)
	}
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Tags; len(got) != 0 {
		t.Errorf("Heart.fbx still tagged after delete: %v", got)
	}
}

func TestTagFilterAndOr(t *testing.T) {
	srv, _ := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	sword := itemByName(t, srv, "q=Sword", "Sword.glb")
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "a", "on": true}).Body.Close()
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": sword.Fingerprints, "tag": "b", "on": true}).Body.Close()

	// OR: either tag matches → both.
	or := taggedAssets(t, srv, "tag=a&tag=b&tagmode=or")
	if or.Total != 2 {
		t.Errorf("OR filter total = %d, want 2", or.Total)
	}
	// AND: needs both on one card → neither (each has only one).
	and := taggedAssets(t, srv, "tag=a&tag=b&tagmode=and")
	if and.Total != 0 {
		t.Errorf("AND filter total = %d, want 0", and.Total)
	}
	// Single tag narrows to its asset.
	one := taggedAssets(t, srv, "tag=a")
	if one.Total != 1 || one.Items[0].Name != "Heart.fbx" {
		t.Errorf("single-tag filter = %+v", one)
	}
}

// A card that groups two distinct fingerprints (same normalized name + size,
// different bytes) is a single tag unit: its Tags is the union over both, so an AND
// filter matches on the union even though no single file carries both tags.
func TestCardUnionAndFilter(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	mk := func(p ...string) string {
		full := filepath.Join(append([]string{root}, p...)...)
		os.MkdirAll(filepath.Dir(full), 0o755)
		return full
	}
	// Two "Coin.fbx" of equal byte length but different content in different packs:
	// same group key (name+size), different fingerprints (crc differs).
	writeZip(t, mk("synty", "A", "A_SourceFiles_v3.zip"), map[string]string{"SourceFiles/Coin.fbx": "COINDAT1"})
	writeZip(t, mk("synty", "B", "B_SourceFiles_v3.zip"), map[string]string{"SourceFiles/Coin.fbx": "COINDAT2"})
	ix, err := assetindex.Build(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	tagsPath := filepath.Join(t.TempDir(), tagstore.FileName)
	store, _ := tagstore.Load(tagsPath)
	s, _ := newServer(ix, store, tagsPath)
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	coin := itemByName(t, srv, "q=Coin", "Coin.fbx")
	if len(coin.Fingerprints) != 2 {
		t.Fatalf("Coin card fingerprints = %v, want 2 distinct", coin.Fingerprints)
	}
	// hero on fp1 only, wip on fp2 only.
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": []string{coin.Fingerprints[0]}, "tag": "hero", "on": true}).Body.Close()
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": []string{coin.Fingerprints[1]}, "tag": "wip", "on": true}).Body.Close()

	// The card's union carries both, so hero AND wip matches it.
	and := taggedAssets(t, srv, "q=Coin&tag=hero&tag=wip&tagmode=and")
	if and.Total != 1 {
		t.Fatalf("card-union AND total = %d, want 1", and.Total)
	}
	got := and.Items[0].Tags
	if len(got) != 2 || got[0] != "hero" || got[1] != "wip" {
		t.Errorf("card union tags = %v, want [hero wip]", got)
	}
}
