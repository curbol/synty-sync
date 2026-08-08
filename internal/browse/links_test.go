package browse

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/curbol/synty-sync/internal/assetindex"
	"github.com/curbol/synty-sync/internal/tagstore"
)

func relatedItems(t *testing.T, srv *httptest.Server, fps []string) taggedAssetsResp {
	t.Helper()
	q := url.Values{}
	for _, fp := range fps {
		q.Add("fingerprint", fp)
	}
	resp := doJSON(t, "GET", srv.URL+"/api/related?"+q.Encode(), nil)
	var out taggedAssetsResp
	decode(t, resp, &out)
	return out
}

func TestLinkRelatedAndExpansion(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	heart := itemByName(t, srv, "q=Heart", "Heart.fbx")
	sword := itemByName(t, srv, "q=Sword", "Sword.glb")

	fps := append(append([]string{}, heart.Fingerprints...), sword.Fingerprints...)
	doJSON(t, "POST", srv.URL+"/api/link", map[string]any{"fingerprints": fps, "on": true}).Body.Close()

	// The DTO now advertises each card's companion.
	if got := itemByName(t, srv, "q=Heart", "Heart.fbx").Related; len(got) == 0 {
		t.Fatalf("Heart has no related fingerprints after link")
	}

	// Tag only Heart. A plain tag filter drops the untagged, linked Sword...
	doJSON(t, "POST", srv.URL+"/api/assign", map[string]any{"fingerprints": heart.Fingerprints, "tag": "love", "on": true}).Body.Close()
	if plain := taggedAssets(t, srv, "tag=love"); plain.Total != 1 {
		t.Fatalf("tag filter total = %d, want 1 (Heart only)", plain.Total)
	}
	// ...but includeRelated pulls the companion back in.
	exp := taggedAssets(t, srv, "tag=love&includeRelated=1")
	names := map[string]bool{}
	for _, it := range exp.Items {
		names[it.Name] = true
	}
	if exp.Total != 2 || !names["Heart.fbx"] || !names["Sword.glb"] {
		t.Fatalf("includeRelated items = %v (total %d), want Heart + Sword", names, exp.Total)
	}

	// /api/related resolves a card's companions to whole cards.
	rel := relatedItems(t, srv, heart.Fingerprints)
	if len(rel.Items) != 1 || rel.Items[0].Name != "Sword.glb" {
		t.Fatalf("related items = %+v, want [Sword.glb]", rel.Items)
	}

	// Persisted as one [[group]] and preserved across reload.
	reloaded, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Groups()) != 1 {
		t.Errorf("groups after reload = %v, want 1", reloaded.Groups())
	}

	// Unlink dissolves the group; expansion no longer reaches Sword.
	doJSON(t, "POST", srv.URL+"/api/link", map[string]any{"fingerprints": fps, "on": false}).Body.Close()
	if exp := taggedAssets(t, srv, "tag=love&includeRelated=1"); exp.Total != 1 {
		t.Errorf("after unlink includeRelated total = %d, want 1", exp.Total)
	}
}

func TestLinkRequiresTwoFingerprints(t *testing.T) {
	srv, _ := enabledServer(t)
	resp := doJSON(t, "POST", srv.URL+"/api/link", map[string]any{"fingerprints": []string{"crc32:solo:1"}, "on": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("link with one fingerprint status = %d, want 400", resp.StatusCode)
	}
}

func TestLinkDisabled(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	writeZip(t, filepath.Join(root, "synty", "P", "P_SourceFiles_v3.zip"), map[string]string{"SourceFiles/A.fbx": "AAA"})
	ix, err := assetindex.Build(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(ix, nil, "") // no tags path -> tagging disabled
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	resp := doJSON(t, "POST", srv.URL+"/api/link", map[string]any{"fingerprints": []string{"a", "b"}, "on": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("link while disabled status = %d, want 409", resp.StatusCode)
	}
}
