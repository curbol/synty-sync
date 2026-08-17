package portal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/curbol/synty-sync/internal/model"
)

// fastBackoff shortens the retry wait for the duration of one test.
func fastBackoff(t *testing.T) {
	t.Helper()
	old := httpBackoff
	httpBackoff = time.Millisecond
	t.Cleanup(func() { httpBackoff = old })
}

// A download href carries the account email (?email=…) alongside the customer id,
// so a transport failure must not put the request URL into the error text.
func TestResolveTransportErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // force a dial failure

	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, CustomerID: "9876543"}
	_, _, err := c.Resolve(context.Background(), model.FileEntry{
		FileToken:    "POLYGON_Pirate_Pack",
		Variant:      "SourceFiles",
		DownloadHref: "/apps/downloads/downloads/1624701?email=person%40example.org&order_id=1",
	})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	for _, secret := range []string{"person", "example.org", "9876543"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %q", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "POLYGON_Pirate_Pack|SourceFiles") {
		t.Errorf("error should name the file by key, got %q", err)
	}
}

// A library or item page URL carries the customer id in its path, and a connection
// refused / DNS / TLS failure arrives as a *url.Error that quotes the whole URL back.
// That is the path every real network failure takes, and it is separate from the
// status-code path a 4xx exercises.
func TestGetBodyRedactsCustomerIDOnTransportError(t *testing.T) {
	fastBackoff(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // force a dial failure

	const id = "9876543"
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL, CustomerID: id}
	_, err := c.Enumerate(context.Background())
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), id) {
		t.Errorf("customer id leaked on a transport error: %q", err)
	}
}

// A rate limit is transient: it must back off and retry, not fail the run like a
// permanent 4xx.
func TestGetBodyRetriesRateLimit(t *testing.T) {
	fastBackoff(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, "OK")
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL}
	body, err := c.getBody(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("getBody: %v", err)
	}
	if string(body) != "OK" || calls != 2 {
		t.Errorf("body=%q calls=%d; want OK after one retry", body, calls)
	}
}

// Retrying a 5xx must reuse the pooled connection, which requires the discarded
// response body to be drained before it is closed.
func TestGetBodyReusesConnectionAcrossRetries(t *testing.T) {
	fastBackoff(t)

	var calls, conns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, strings.Repeat("error page ", 100))
			return
		}
		fmt.Fprint(w, "OK")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	if _, err := c.getBody(context.Background(), srv.URL); err != nil {
		t.Fatalf("getBody: %v", err)
	}
	if conns != 1 {
		t.Errorf("opened %d connections for %d attempts; want 1 (undrained bodies break reuse)", conns, calls)
	}
}

// A markup change that stops matching the file-row selector must abort the run:
// returning zero files with no error lets the syncer rewrite the pack's lockfile
// entry as empty, discarding every tracked file it recorded.
func TestParseItemPageErrorsOnNoRows(t *testing.T) {
	html := []byte(`<div class='sky-pilot rte'><h2>Pack</h2>
		<div class='renamed-list-item'>POLYGON_Pack<br><span>SourceFiles | v3</span></div></div>`)
	if _, err := ParseItemPage(html, "pack"); err == nil {
		t.Error("a page with no recognizable file rows must be a loud error")
	}
}

// The zero-rows guard covers the row selector but not the label separator. If the
// file-heading class moves, every row parses to an empty label, is skipped as a
// versionless icon row, and the page yields zero files with no error — which lets
// the syncer rewrite the pack's lockfile entry as empty.
func TestParseItemPageErrorsWhenNoRowCarriesAVersion(t *testing.T) {
	html := []byte(`<div class='sky-pilot-list-item'>
	  <div class='sky-pilot-file-title'>POLYGON_Pirate_Godot_4_5_1 | v1_0_1 <span class='sky-pilot-file-size'>(40 MB)</span></div>
	  <div class='sky-pilot-actions'><a href='/apps/downloads/downloads/222'>Download</a></div>
	</div>`)
	files, err := ParseItemPage(html, "polygon-pirate")
	if err == nil {
		t.Errorf("rows present but none versioned must be a loud error; got %d files, nil error", len(files))
	}
	if files != nil {
		t.Errorf("a failed parse must not return partial files, got %+v", files)
	}
}

// A pack whose item page legitimately carries only its versionless icon row is
// still an error: every owned pack ships at least one downloadable file, so zero
// means the markup moved.
func TestParseItemPageErrorsOnIconOnlyPage(t *testing.T) {
	html := []byte(`<div class='sky-pilot-list-item'>
	  <div class='sky-pilot-file-heading'>POLYGON_Pirate_icon.png</div>
	</div>`)
	if _, err := ParseItemPage(html, "polygon-pirate"); err == nil {
		t.Error("an item page yielding no versioned rows must be a loud error")
	}
}

// Library anchors embed the customer id, so the parser's own error — which fires
// exactly when the anchor shape changes — must not quote the href.
func TestParseLibraryPageErrorHidesCustomerID(t *testing.T) {
	html := []byte(`<a class='sky-pilot-list-item'
	  href='/apps/downloads/customers/1000000000001/orders/114478945/nope'>POLYGON - Pirate Pack</a>`)
	packs, err := ParseLibraryPage(html)
	if err == nil {
		t.Fatal("expected an error for an anchor without an order_item url")
	}
	if strings.Contains(err.Error(), "1000000000001") {
		t.Errorf("error leaks the customer id: %q", err)
	}
	if !strings.Contains(err.Error(), "POLYGON - Pirate Pack") {
		t.Errorf("error should identify the anchor by its link text, got %q", err)
	}
	if packs != nil {
		t.Errorf("a failed parse must not return partial packs, got %+v", packs)
	}
}

// A logged-out shell served part way through the walk must abort, not read as the
// terminator: a truncated pack list makes `select` drop the user's enabled flags.
// The real terminator page carries the sentinel, so it is still distinguishable.
func TestEnumerateExpiredSessionMidWalk(t *testing.T) {
	const page = `<div class='sky-pilot'><input class='sky-pilot-search-input'>
	  <a href='/apps/downloads/customers/1/orders/100/order_items/%d' class='sky-pilot-list-item'>Pack %d</a></div>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("line_items_page") {
		case "1":
			fmt.Fprintf(w, page, 1, 1)
		default:
			fmt.Fprint(w, `<html><body><h1>Login</h1></body></html>`)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1"}
	packs, err := c.Enumerate(context.Background())
	if !errors.Is(err, ErrExpiredSession) {
		t.Errorf("mid-walk logout: err = %v, want ErrExpiredSession (got %d packs)", err, len(packs))
	}
}

// A paginator that clamps an out-of-range page to the last one would otherwise
// loop forever, re-appending the same packs and hammering the store.
func TestEnumerateStopsWhenPaginationRepeats(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, `<div class='sky-pilot'><input class='sky-pilot-search-input'>
		  <a href='/apps/downloads/customers/1/orders/100/order_items/7' class='sky-pilot-list-item'>Pack</a></div>`)
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1"}
	_, err := c.Enumerate(context.Background())
	if err == nil {
		t.Fatal("a paginator that never advances must error, not loop forever")
	}
	if calls > 4 {
		t.Errorf("made %d requests before giving up; want it to notice on the second page", calls)
	}
}

// A 408 is the server saying the request did not complete in time, which is
// retryable. Failing fast on it aborts the whole run over one blip.
func TestGetBodyRetriesRequestTimeout(t *testing.T) {
	fastBackoff(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		fmt.Fprint(w, "OK")
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL}
	body, err := c.getBody(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("getBody: %v", err)
	}
	if string(body) != "OK" || calls != 2 {
		t.Errorf("body=%q calls=%d; want OK after one retry", body, calls)
	}
}

// An oversized page must error rather than truncate: a silently shortened library
// page parses as a valid short page and quietly ends enumeration early.
func TestGetBodyRejectsOversizedPage(t *testing.T) {
	fastBackoff(t)
	old := maxPageBytes
	maxPageBytes = 1024
	t.Cleanup(func() { maxPageBytes = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 4096))
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL}
	if _, err := c.getBody(context.Background(), srv.URL); err == nil {
		t.Error("a page past the size bound must error, not truncate")
	}
}

// Headers arrive and then the body stalls. Without a per-attempt read deadline the
// run blocks forever: ResponseHeaderTimeout has already been satisfied.
func TestGetBodyTimesOutOnStalledBody(t *testing.T) {
	fastBackoff(t)
	old := pageTimeout
	pageTimeout = 150 * time.Millisecond
	t.Cleanup(func() { pageTimeout = old })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL}
	done := make(chan error, 1)
	go func() { _, err := c.getBody(context.Background(), srv.URL); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stalled body must fail, not return successfully")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("getBody hung on a stalled body: no per-attempt read deadline")
	}
}

// An authenticated page-1 with no packs is a genuinely empty library, not an
// expired session.
func TestEnumerateEmptyLibrary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div class='sky-pilot'><input class='sky-pilot-search-input'></div>`)
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: "1"}
	packs, err := c.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("empty library should not error: %v", err)
	}
	if len(packs) != 0 {
		t.Errorf("packs = %+v, want none", packs)
	}
}

// New supplies the two things a zero-value Client gets wrong.
func TestNewNormalizesTheClient(t *testing.T) {
	c := New(nil, "https://syntystore.com/", "42", "a=b")
	if c.HTTP == nil {
		t.Error("HTTP left nil; a zero-value client panics on first use")
	}
	if c.BaseURL != "https://syntystore.com" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed (URLs are concatenated)", c.BaseURL)
	}
}

// The whole committed capture set walked end to end: five real library pages, the
// real overflow terminator, and the real anchor shape. The synthetic terminator used
// elsewhere happens to include the sentinel, which is exactly the fact the paging
// logic turns on, so only the real pages prove the walk.
func TestEnumerateWalksTheRealCaptures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("line_items_page")
		name := "library_empty_authenticated.html"
		if n, err := strconv.Atoi(page); err == nil && n >= 1 && n <= 5 {
			name = fmt.Sprintf("library_p%d.html", n)
		}
		w.Write(read(t, name))
	}))
	defer srv.Close()

	packs, err := New(nil, srv.URL, "1000000000001", "x=y").Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate over the real captures: %v", err)
	}
	if len(packs) != 61 {
		t.Errorf("enumerated %d packs, want the 61 in the captures", len(packs))
	}
	seen := map[int]bool{}
	for _, p := range packs {
		if p.Slug == "" || p.OrderItemID == 0 || p.ItemURL == "" {
			t.Errorf("incomplete pack: %+v", p)
		}
		if seen[p.OrderItemID] {
			t.Errorf("duplicate order item %d", p.OrderItemID)
		}
		seen[p.OrderItemID] = true
	}
}

// A logged-out shell on page 1, using the real capture rather than a stub.
func TestEnumerateRealLogoutShell(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(read(t, "library_logout_shell.html"))
	}))
	defer srv.Close()

	_, err := New(nil, srv.URL, "1000000000001", "x=y").Enumerate(context.Background())
	if !errors.Is(err, ErrExpiredSession) {
		t.Errorf("err = %v, want ErrExpiredSession", err)
	}
}

// The syncer classifies a download failure by the status it recovers with StatusOf,
// so the code has to survive the retry.Stop wrapper. Nothing covered that path: the
// only StatusOf test goes through Resolve, which never touches retry.
func TestFailFastErrorKeepsItsStatusAndBound(t *testing.T) {
	fastBackoff(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL}
	_, err := c.getBody(context.Background(), srv.URL)
	if code, ok := StatusOf(err); !ok || code != http.StatusNotFound {
		t.Errorf("StatusOf = (%d, %v), want (404, true) through the retry wrapper", code, ok)
	}
	if calls != 1 {
		t.Errorf("a 4xx was attempted %d times, want 1", calls)
	}
}

// Exhausting the retries must stop at the bound and still surface the status.
func TestRetriesAreBounded(t *testing.T) {
	fastBackoff(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL}
	_, err := c.getBody(context.Background(), srv.URL)
	if code, ok := StatusOf(err); !ok || code != http.StatusInternalServerError {
		t.Errorf("StatusOf = (%d, %v), want (500, true)", code, ok)
	}
	if int(calls) != httpAttempts {
		t.Errorf("made %d attempts, want exactly httpAttempts (%d)", calls, httpAttempts)
	}
}
