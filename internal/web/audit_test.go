package web

import (
	"context"
	"errors"
	"html/template"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(ctx, ln, packs, nil)
	}()
	// Serve outlives the test body otherwise, and a Serve still running is a live
	// reader of the package's OpenBrowser while the next test swaps it.
	t.Cleanup(func() {
		cancel()
		<-done
	})
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
		// A browser omits the port when it is the scheme's default, so a page bound to
		// :80 arrives with a bare name and was answered 421 on every request.
		{"port 80 bind, browser omits the port", "127.0.0.1:80", "localhost", "127.0.0.1:51000", true},
		{"port 80 bind, bare loopback literal", "127.0.0.1:80", "127.0.0.1", "127.0.0.1:51000", true},
		{"port 80 IPv6 bind, bare bracketed literal", "[::1]:80", "[::1]", "[::1]:51000", true},
		{"port 80 bind, rebound bare name", "127.0.0.1:80", "evil.example", "127.0.0.1:51000", false},
		{"other port, bare name", "127.0.0.1:8787", "localhost", "127.0.0.1:51000", false},
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

// template.Must runs Parse, not html/template's escape analysis or the field lookups,
// which wait for the first Execute. A template edit that fails there used to serve a
// 200 with whatever had been written before the failure, often nothing, while the
// terminal said nothing: a blank page and no diagnostic anywhere.
func TestAPageThatFailsToRenderIsAnErrorNotABlankPage(t *testing.T) {
	prev := page
	page = template.Must(template.New("select").Parse(`<p>{{.Count}}</p>{{.NoSuchField}}`))
	t.Cleanup(func() { page = prev })

	base, _ := serving(t, []model.Pack{{Slug: "a", DisplayName: "A"}}, nil)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("a page that failed to render returned %d, want 500", resp.StatusCode)
	}
}

// The header count and the checkboxes describe the same thing, so they have to be read
// off the same list. Counting the enabled map instead lets a slug that is not in packs
// inflate the number over the boxes the page actually renders, and that count is the
// only thing telling the user how much they are about to save.
func TestPageCountsTheBoxesItRenders(t *testing.T) {
	packs := []model.Pack{
		{Slug: "pirate-pack", DisplayName: "Pirate Pack"},
		{Slug: "dungeon-pack", DisplayName: "Dungeon Pack"},
	}
	// long-gone is enabled but no longer owned, so the page never offers it.
	base, _ := serving(t, packs, map[string]bool{"pirate-pack": true, "long-gone": true})

	body := get(t, base+"/")
	if n := strings.Count(body, `type="checkbox"`); n != 2 {
		t.Fatalf("page rendered %d checkboxes, want 2:\n%s", n, body)
	}
	if !strings.Contains(body, `<span id="n">1</span>`) {
		t.Errorf("the header count is not the one box the page ticked:\n%s", body)
	}
}

// postSave submits a selection straight to h, the way the page's form would from a
// browser on this machine.
func postSave(t *testing.T, h *handler, slugs ...string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"csrf": {h.token}, "pack": slugs}
	r := httptest.NewRequest(http.MethodPost, "/save", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Host = h.bound.String()
	r.RemoteAddr = "127.0.0.1:50000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// Two tabs on the page share the run's token, so both can submit. Serve takes one
// selection, and the second was answered "Got your selection" and then either dropped
// in a channel nobody read again or left its handler blocked on a full one: the user
// was told a selection was taken that never reached the manifest.
func TestASecondSaveIsRefusedRatherThanDropped(t *testing.T) {
	packs := []model.Pack{{Slug: "a", DisplayName: "A"}, {Slug: "b", DisplayName: "B"}}
	h, err := newHandler(boundAddr("127.0.0.1:8787"), packs, map[string]bool{"a": true})
	if err != nil {
		t.Fatal(err)
	}

	if rec := postSave(t, h, "a"); rec.Code != http.StatusOK {
		t.Fatalf("the first save returned %d, want 200", rec.Code)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postSave(t, h, "b") }()
	var second *httptest.ResponseRecorder
	select {
	case second = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second save never got an answer")
	}
	if second.Code == http.StatusOK || strings.Contains(second.Body.String(), "Got your selection") {
		t.Errorf("the second save was told it was taken: %d %q", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "select") {
		t.Errorf("the refusal does not say how to change the selection: %q", second.Body.String())
	}
	if got := <-h.result; !maps.Equal(got, map[string]bool{"a": true}) {
		t.Errorf("Serve would receive %v, want the first save", got)
	}
	select {
	case got := <-h.result:
		t.Errorf("a second selection %v was queued behind the first", got)
	default:
	}
}

// A save is queued before its handler answers the browser, so an interrupt landing in
// that window leaves Serve with both cases ready, and Go picks between ready cases at
// random. Returning the interrupt there drops a selection the page said it had.
func TestASaveAlreadyAcceptedSurvivesAnInterrupt(t *testing.T) {
	packs := []model.Pack{{Slug: "a", DisplayName: "A"}, {Slug: "b", DisplayName: "B"}}
	for round := range 50 {
		h, err := newHandler(boundAddr("127.0.0.1:8787"), packs, map[string]bool{"a": true})
		if err != nil {
			t.Fatal(err)
		}
		if rec := postSave(t, h, "b"); rec.Code != http.StatusOK {
			t.Fatalf("round %d: the save returned %d", round, rec.Code)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := serveHandler(ctx, listen(t), h)
		if err != nil || !got["b"] {
			t.Fatalf("round %d: Serve returned %v, %v; want the accepted selection", round, got, err)
		}
	}
}

// The window one step earlier: the interrupt lands while the handler is still reading
// the POST body. Serve takes the cancellation with nothing queued, its shutdown waits
// for that handler, and the handler goes on to accept the save and answer the browser.
// Returning the interrupt there tells the user their selection was taken while the
// manifest keeps the old one.
func TestASaveAcceptedWhileTheInterruptLandsIsStillReturned(t *testing.T) {
	packs := []model.Pack{{Slug: "a", DisplayName: "A"}, {Slug: "b", DisplayName: "B"}}
	ln := listen(t)
	addr := ln.Addr().String()
	h, err := newHandler(ln.Addr(), packs, map[string]bool{"a": true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		sel map[string]bool
		err error
	}
	served := make(chan result, 1)
	go func() {
		sel, err := serveHandler(ctx, ln, h)
		served <- result{sel, err}
	}()
	waitUp(t, "http://"+addr)

	// A declared length and a body that arrives in two parts, so the handler is
	// demonstrably mid-read when the interrupt lands.
	form := url.Values{"csrf": {h.token}, "pack": {"b"}}.Encode()
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/save", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(len(form))
	// The client buffers the headers and the start of a sized body until the body is
	// complete, so without this the server has not seen the request at all when the
	// interrupt lands. With 100-continue the transport reads the first part of the body
	// only once the server has asked for it, which it does when the handler reads.
	req.Header.Set("Expect", "100-continue")
	type answer struct {
		code int
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		tr := &http.Transport{DisableKeepAlives: true, ExpectContinueTimeout: time.Minute}
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			answered <- answer{err: err}
			return
		}
		resp.Body.Close()
		answered <- answer{code: resp.StatusCode}
	}()
	if _, err := pw.Write([]byte(form[:len(form)-3])); err != nil {
		t.Fatal(err)
	}

	cancel()
	// Shutdown closes the listener first, so a refused dial is the sign Serve has taken
	// the cancellation with nothing queued and is now waiting on the handler.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("Serve never began shutting down after the interrupt")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := pw.Write([]byte(form[len(form)-3:])); err != nil {
		t.Fatal(err)
	}
	pw.Close()

	if a := <-answered; a.code != http.StatusOK {
		t.Fatalf("the in-flight save was answered %d (%v); this test cannot tell us anything", a.code, a.err)
	}
	r := <-served
	if r.err != nil || !r.sel["b"] {
		t.Errorf("Serve returned %v, %v after the page told the user their selection was taken", r.sel, r.err)
	}
}

// The icon URL comes from the store and lands in an src attribute, a URL context where
// escaping the quotes is not enough: the scheme has to be filtered. html/template does
// that, rendering anything but http, https and mailto as "#ZgotmplZ". A developer who
// sees that string in a broken thumbnail finds one suggested fix, retyping the field as
// template.URL, which switches the filter off for every row; the attribute-breakout
// test above stays green through it, because quoting still works.
func TestIconURLSchemesAreFiltered(t *testing.T) {
	packs := []model.Pack{
		{Slug: "a", DisplayName: "A", IconURL: "javascript:alert(document.cookie)"},
		{Slug: "b", DisplayName: "B", IconURL: "data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg=="},
		{Slug: "c", DisplayName: "C", IconURL: "https://cdn.example/c.png"},
	}
	h, err := newHandler(boundAddr("127.0.0.1:8787"), packs, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "127.0.0.1:8787"
	r.RemoteAddr = "127.0.0.1:50000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	body := rec.Body.String()

	for _, scheme := range []string{"javascript:", "data:text/html"} {
		if strings.Contains(body, scheme) {
			t.Errorf("an icon URL with scheme %q reached the page:\n%s", scheme, body)
		}
	}
	// Present, so the assertion above is about filtering rather than about the icons
	// having been dropped altogether.
	if !strings.Contains(body, `src="https://cdn.example/c.png"`) {
		t.Errorf("an https icon URL did not reach the page:\n%s", body)
	}
}
