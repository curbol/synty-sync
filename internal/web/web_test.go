package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/model"
)

// Serve launches a browser at the address it binds. Left alone, every test here
// would open a real tab at a URL that dies with the test.
func TestMain(m *testing.M) {
	OpenBrowser = func(string) {}
	os.Exit(m.Run())
}

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

// /save persists the whole pack selection, and browse-adjacent pages run on
// localhost too. Without a method guard, any page the user visits while `select` is
// open can fire a GET at it, submit an empty form, and disable every pack.
func TestSaveRejectsNonPost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan map[string]bool, 1)
	go func() {
		chosen, _ := Serve(ctx, "localhost:18789", []model.Pack{{Slug: "a", DisplayName: "A"}}, map[string]bool{"a": true})
		done <- chosen
	}()
	waitForListener(t, "localhost:18789")

	resp, err := http.Get("http://localhost:18789/save")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("GET /save returned %d; a drive-by request must not submit a selection", resp.StatusCode)
	}
	select {
	case chosen := <-done:
		t.Errorf("GET /save unblocked Serve with %v; the caller would disable every pack", chosen)
	case <-time.After(300 * time.Millisecond):
	}
}

// A tab left open from an earlier run posts the slugs it was rendered with. Those
// no longer name anything owned, and returning them would hand back a set that looks
// like a deliberate choice while selecting nothing, instead of the empty submission
// the caller knows how to refuse.
func TestSaveIgnoresUnknownSlugs(t *testing.T) {
	packs := []model.Pack{
		{Slug: "current", DisplayName: "Current"},
		{Slug: "other", DisplayName: "Other"},
	}
	for _, tc := range []struct {
		name string
		addr string
		post []string
		want map[string]bool
	}{
		{"every slug is stale", "localhost:18791", []string{"long-gone", "also-gone"}, map[string]bool{}},
		{"a stale slug alongside a real one", "localhost:18792", []string{"current", "long-gone"}, map[string]bool{"current": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan map[string]bool, 1)
			go func() {
				chosen, _ := Serve(ctx, tc.addr, packs, map[string]bool{"current": true})
				done <- chosen
			}()
			waitForListener(t, tc.addr)

			resp, err := http.PostForm("http://"+tc.addr+"/save", url.Values{"pack": tc.post})
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			select {
			case chosen := <-done:
				if len(chosen) != len(tc.want) {
					t.Fatalf("chosen = %v, want %v", chosen, tc.want)
				}
				for slug := range tc.want {
					if !chosen[slug] {
						t.Errorf("chosen = %v, want it to keep %q", chosen, slug)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Serve did not return after save")
			}
		})
	}
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never came up on %s", addr)
}
