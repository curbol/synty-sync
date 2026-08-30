package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/model"
)

// serving starts a selection page on an ephemeral port and hands back its base URL
// and the channel Serve's result arrives on.
func serving(t *testing.T, packs []model.Pack, enabled map[string]bool) (base string, done chan map[string]bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ln := listen(t)
	base = "http://" + ln.Addr().String()
	done = make(chan map[string]bool, 1)
	go func() {
		chosen, _ := Serve(ctx, ln, packs, enabled)
		done <- chosen
	}()
	waitUp(t, base)
	return base, done
}

// A cross-origin form POST is a CORS "simple request": no preflight, nothing in the
// browser stops it. Any page open in another tab while `select` is running could
// otherwise post a set of guessed slugs — they come from public display names — and
// both throw away the real selection and enable packs the user never chose.
func TestSaveRejectsAFormItDidNotRender(t *testing.T) {
	packs := []model.Pack{{Slug: "current", DisplayName: "Current"}, {Slug: "other", DisplayName: "Other"}}
	base, done := serving(t, packs, map[string]bool{"current": true})

	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"no token at all", url.Values{"pack": {"other"}}},
		{"a guessed token", url.Values{"pack": {"other"}, "csrf": {strings.Repeat("a", 64)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.PostForm(base+"/save", tc.form)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("POST /save returned %d, want 403", resp.StatusCode)
			}
		})
	}

	select {
	case chosen := <-done:
		t.Errorf("a forged submission unblocked Serve with %v; the committed allowlist would be rewritten", chosen)
	case <-time.After(300 * time.Millisecond):
	}
}

// The token must not be reachable from the query string: a link would then stand in
// for the form this server rendered, and a link is something a page can navigate to.
func TestSaveIgnoresATokenFromTheQueryString(t *testing.T) {
	packs := []model.Pack{{Slug: "current", DisplayName: "Current"}}
	base, done := serving(t, packs, map[string]bool{"current": true})
	token := formToken(t, base)

	resp, err := http.PostForm(base+"/save?csrf="+token, url.Values{"pack": {"current"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /save returned %d for a query-string token, want 403", resp.StatusCode)
	}
	select {
	case chosen := <-done:
		t.Errorf("a query-string token unblocked Serve with %v", chosen)
	case <-time.After(300 * time.Millisecond):
	}
}

// A page that points its own name at 127.0.0.1 reaches this server with that name in
// Host. Without the check it is same-origin with the selection page and can read the
// whole pack list, which is the user's purchase history.
func TestHandlersRefuseAForeignHost(t *testing.T) {
	packs := []model.Pack{{Slug: "current", DisplayName: "Current"}}
	base, _ := serving(t, packs, map[string]bool{"current": true})
	port := base[strings.LastIndex(base, ":")+1:]

	for _, path := range []string{"/", "/save"} {
		method := http.MethodGet
		if path == "/save" {
			method = http.MethodPost
		}
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "attacker.example:" + port
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("%s %s returned %d, want 421", method, path, resp.StatusCode)
		}
		if strings.Contains(string(body[:n]), "Current") {
			t.Errorf("%s %s leaked the pack list to a rebound host", method, path)
		}
	}
}

// localhost and a loopback literal are how a browser on this machine actually
// addresses the page, so neither may be turned away by the Host check.
func TestHandlersAcceptTheWaysABrowserAddressesThem(t *testing.T) {
	packs := []model.Pack{{Slug: "current", DisplayName: "Current"}}
	base, _ := serving(t, packs, map[string]bool{"current": true})
	port := base[strings.LastIndex(base, ":")+1:]

	for _, host := range []string{"localhost:" + port, "127.0.0.1:" + port} {
		req, err := http.NewRequest(http.MethodGet, base+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Host %q returned %d, want the page", host, resp.StatusCode)
		}
	}
}
