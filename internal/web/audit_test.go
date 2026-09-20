package web

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/model"
)

// Pack display names come from the store, and the page they land on carries the token
// that authorizes rewriting the committed manifest. They are safe only because the
// template is html/template: switching the import, or wrapping a field in
// template.HTML to sidestep an escaping surprise, leaves every other test here green
// because they all assert the name is *present*, which it still is.
func TestStoreSuppliedNamesAreEscaped(t *testing.T) {
	const payload = `<script>alert(1)</script>"><img src=x onerror=alert(1)>`
	packs := []model.Pack{{Slug: "polygon-pirate-pack", DisplayName: payload, IconURL: `x" onerror="alert(1)`}}

	ln := listen(t)
	base := "http://" + ln.Addr().String()
	go Serve(context.Background(), ln, packs, nil)
	waitUp(t, base)

	body := get(t, base+"/")
	// The payload's own markup, not a bare "<script>": the page ships a legitimate
	// script block of its own, and matching that would fail whatever the template does.
	for _, raw := range []string{payload, "<script>alert(1)", "<img src=x", `onerror="alert(1)"`} {
		if strings.Contains(body, raw) {
			t.Errorf("store-supplied text reached the page unescaped (%q):\n%s", raw, body)
		}
	}
	// Present, so the assertion above is about escaping rather than about the name
	// having been dropped altogether.
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the escaped name is not on the page at all:\n%s", body)
	}
}

// proveNothingLanded confirms a rejected submission did not reach Serve. Rather than
// waiting a fixed window for nothing to arrive, which passes just as readily because
// the scheduler was slow. It sends one legitimate submission afterwards and checks
// that what Serve returns is *that* one. A forged set that had landed would have
// unblocked Serve first and be what comes back.
func proveNothingLanded(t *testing.T, base string, done chan map[string]bool, want map[string]bool) {
	t.Helper()
	form := url.Values{"csrf": {formToken(t, base)}}
	for slug := range want {
		form.Add("pack", slug)
	}
	resp, err := http.PostForm(base+"/save", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the legitimate submission returned %d; this check cannot tell us anything", resp.StatusCode)
	}
	select {
	case got := <-done:
		if !maps.Equal(got, want) {
			t.Errorf("Serve returned %v, want %v: a submission it should have refused is what landed", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve never returned after a legitimate submission")
	}
}

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

	// Neither forgery reached Serve: the selection it returns is the honest one sent
	// after them, not "other".
	proveNothingLanded(t, base, done, map[string]bool{"current": true})
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
	proveNothingLanded(t, base, done, map[string]bool{"current": true})
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
// addresses the page, so neither may be turned away by the Host check. Host is
// case-insensitive and main.loopbackHost already folds case when it vets --addr, so a
// hostname typed in any case has to reach the page rather than a 421 from the half of
// the pair that compared it exactly.
func TestHandlersAcceptTheWaysABrowserAddressesThem(t *testing.T) {
	packs := []model.Pack{{Slug: "current", DisplayName: "Current"}}
	base, _ := serving(t, packs, map[string]bool{"current": true})
	port := base[strings.LastIndex(base, ":")+1:]

	for _, host := range []string{"localhost:" + port, "LOCALHOST:" + port, "LocalHost:" + port, "127.0.0.1:" + port} {
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

// Ctrl-C during select has to end the wait. Serve blocks until the page posts, so
// without the cancellation case the command would hang with no way out but a kill.
func TestServeReturnsWhenTheContextIsCancelled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	restore := OpenBrowser
	OpenBrowser = func(string) {}
	t.Cleanup(func() { OpenBrowser = restore })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Serve(ctx, ln, []model.Pack{{Slug: "pirate", DisplayName: "Pirate"}}, nil)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

// boundAddr stands in for a listener's address so a test can ask what the handlers
// would do on a bind the suite never makes. Every real listener here is 127.0.0.1:0,
// which is exactly the shape that hides the bug below.
type boundAddr string

func (b boundAddr) Network() string { return "tcp" }
func (b boundAddr) String() string  { return string(b) }

// The Host header is the client's to claim; the peer address is not. Checking only the
// Host inverts on a wildcard bind (`select --addr :8787`): a remote client sending
// "Host: 127.0.0.1:8787" was taken for a local browser and handed the pack list — the
// account's purchase history — along with the token its form carries, which is enough
// to POST /save and rewrite the committed manifest. The browser that bind was meant to
// reach, meanwhile, can only send the machine's real address and was refused with 421.
func TestRequestsFromAnotherMachineAreRefusedWhateverHostTheyClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bound boundAddr
		host  string
		peer  string
		want  bool
	}{
		{"wildcard bind, remote peer claiming loopback", "[::]:8787", "127.0.0.1:8787", "203.0.113.9:51000", false},
		{"wildcard bind, remote peer claiming localhost", "[::]:8787", "localhost:8787", "203.0.113.9:51000", false},
		{"0.0.0.0 bind, remote peer claiming loopback", "0.0.0.0:8787", "127.0.0.1:8787", "198.51.100.4:4000", false},
		{"wildcard bind, remote peer with the real address", "[::]:8787", "192.168.1.5:8787", "192.168.1.77:51000", false},
		{"wildcard bind, local browser", "[::]:8787", "127.0.0.1:8787", "127.0.0.1:51000", true},
		{"loopback bind, local browser", "127.0.0.1:8787", "127.0.0.1:8787", "127.0.0.1:51000", true},
		{"loopback bind, rebound name", "127.0.0.1:8787", "evil.example:8787", "127.0.0.1:51000", false},
		{"loopback bind, wrong port", "127.0.0.1:8787", "127.0.0.1:9999", "127.0.0.1:51000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Host: tc.host, RemoteAddr: tc.peer}
			if got := localRequest(r, tc.bound); got != tc.want {
				t.Errorf("localRequest = %v, want %v (bound %s, Host %q, peer %q)",
					got, tc.want, tc.bound, tc.host, tc.peer)
			}
		})
	}
}
