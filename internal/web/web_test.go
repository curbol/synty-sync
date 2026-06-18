package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/model"
)

func TestServeRendersAndSaves(t *testing.T) {
	packs := []model.Pack{
		{Slug: "polygon-pirate-pack", DisplayName: "POLYGON - Pirate Pack", IconURL: "https://x/pirate.png"},
		{Slug: "polygon-city-zombies", DisplayName: "POLYGON - City Zombies"},
	}
	enabled := map[string]bool{"polygon-pirate-pack": true}

	type res struct {
		set map[string]bool
		err error
	}
	done := make(chan res, 1)
	go func() {
		set, err := Serve(context.Background(), "localhost:8799", packs, enabled)
		done <- res{set, err}
	}()

	base := "http://localhost:8799"
	waitUp(t, base)

	// The page lists both packs, with the enabled one pre-checked.
	body := get(t, base+"/")
	if !strings.Contains(body, "POLYGON - Pirate Pack") || !strings.Contains(body, "POLYGON - City Zombies") {
		t.Fatalf("page missing packs:\n%s", body)
	}
	if !strings.Contains(body, `value="polygon-pirate-pack" checked`) {
		t.Errorf("enabled pack not pre-checked")
	}

	// Save a new selection (only city-zombies).
	resp, err := http.PostForm(base+"/save", url.Values{"pack": {"polygon-city-zombies"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("serve: %v", r.err)
		}
		if !r.set["polygon-city-zombies"] || r.set["polygon-pirate-pack"] {
			t.Errorf("saved set = %+v, want only city-zombies", r.set)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after save")
	}
}

func waitUp(t *testing.T, base string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if resp, err := http.Get(base + "/"); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not come up")
}

func get(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
