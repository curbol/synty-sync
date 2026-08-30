package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
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

// listen binds an ephemeral loopback port, so the tests carry no fixed port numbers
// to collide over and can run alongside each other.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

var tokenRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// formToken fetches the page and returns the token its form carries, the way a
// browser submitting that form would.
func formToken(t *testing.T, base string) string {
	t.Helper()
	m := tokenRe.FindStringSubmatch(get(t, base+"/"))
	if m == nil {
		t.Fatal("the rendered page carries no form token")
	}
	return m[1]
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
	ln := listen(t)
	base := "http://" + ln.Addr().String()
	done := make(chan res, 1)
	go func() {
		set, err := Serve(context.Background(), ln, packs, enabled)
		done <- res{set, err}
	}()
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
	resp, err := http.PostForm(base+"/save", url.Values{
		"pack": {"polygon-city-zombies"},
		"csrf": {formToken(t, base)},
	})
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

	ln := listen(t)
	base := "http://" + ln.Addr().String()
	done := make(chan map[string]bool, 1)
	go func() {
		chosen, _ := Serve(ctx, ln, []model.Pack{{Slug: "a", DisplayName: "A"}}, map[string]bool{"a": true})
		done <- chosen
	}()
	waitUp(t, base)

	resp, err := http.Get(base + "/save")
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
		post []string
		want map[string]bool
	}{
		{"every slug is stale", []string{"long-gone", "also-gone"}, map[string]bool{}},
		{"a stale slug alongside a real one", []string{"current", "long-gone"}, map[string]bool{"current": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ln := listen(t)
			base := "http://" + ln.Addr().String()
			done := make(chan map[string]bool, 1)
			go func() {
				chosen, _ := Serve(ctx, ln, packs, map[string]bool{"current": true})
				done <- chosen
			}()
			waitUp(t, base)

			resp, err := http.PostForm(base+"/save", url.Values{
				"pack": tc.post,
				"csrf": {formToken(t, base)},
			})
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
