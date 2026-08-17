package portal

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
