package browse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/curbol/synty-sync/internal/tagstore"
)

// The paging window is request-controlled, so it has to survive values the UI never
// sends: a negative offset used to slice out of range and take the handler down.
func TestAssetsPagingClampsOutOfRangeOffsets(t *testing.T) {
	srv := testServer(t)
	total := getAssets(t, srv, "").Total

	for _, q := range []string{"offset=-1", "offset=-1000&limit=5", fmt.Sprintf("offset=%d", total+50)} {
		resp, err := http.Get(srv.URL + "/api/assets?" + q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		body := resp.Body
		code := resp.StatusCode
		body.Close()
		if code != http.StatusOK {
			t.Errorf("%s: status %d", q, code)
		}
	}
	if got := getAssets(t, srv, "offset=-1&limit=1").Offset; got != 0 {
		t.Errorf("negative offset resolved to %d, want 0", got)
	}
}

// Every tag write goes through one mutex and re-saves the store, so concurrent
// assignments must all land and the file on disk must match what the server holds.
func TestConcurrentTagAssignmentsAllPersist(t *testing.T) {
	srv, tagsPath := enabledServer(t)
	fps := []string{"crc32:aa:1", "crc32:bb:2", "crc32:cc:3", "crc32:dd:4"}
	tags := []string{"hero", "prop", "vfx", "wip"}

	var wg sync.WaitGroup
	for _, fp := range fps {
		for _, tag := range tags {
			wg.Add(1)
			go func(fp, tag string) {
				defer wg.Done()
				body, _ := json.Marshal(map[string]any{
					"fingerprints": []string{fp}, "tag": tag, "on": true,
				})
				resp, err := http.Post(srv.URL+"/api/assign", "application/json", bytes.NewReader(body))
				if err != nil {
					t.Errorf("assign %s/%s: %v", fp, tag, err)
					return
				}
				resp.Body.Close()
			}(fp, tag)
		}
	}
	wg.Wait()

	saved, err := tagstore.Load(tagsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, fp := range fps {
		got := saved.TagsFor(fp)
		if len(got) != len(tags) {
			t.Errorf("%s has tags %v, want all %v", fp, got, tags)
		}
	}
}
