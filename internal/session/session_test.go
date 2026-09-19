package session

import (
	"strings"
	"testing"
)

func TestFromCookiesTxt(t *testing.T) {
	content := "# Netscape HTTP Cookie File\n" +
		".syntystore.com\tTRUE\t/\tTRUE\t0\t_shopify_essential\tABC\n" +
		"syntystore.com\tFALSE\t/\tFALSE\t0\tlocalization\tUS\n" +
		".other.com\tTRUE\t/\tTRUE\t0\tjunk\tXX\n"
	got, err := FromCookiesTxt(content)
	if err != nil {
		t.Fatal(err)
	}
	want := "_shopify_essential=ABC; localization=US"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFromCurl(t *testing.T) {
	curl := `curl 'https://syntystore.com/apps/downloads/orders/1' \
  -H 'Accept: text/html' \
  -H 'Cookie: localization=US; _shopify_essential=ABC' \
  --compressed`
	got, err := FromCurl(curl)
	if err != nil {
		t.Fatal(err)
	}
	if got != "localization=US; _shopify_essential=ABC" {
		t.Errorf("got %q", got)
	}
}

func TestFromCurlWithQuotedJSONCookie(t *testing.T) {
	// Single-quoted -H whose Cookie value contains double quotes (Shopify's
	// _consentik_cookie holds JSON). The whole value, including later cookies,
	// must survive.
	curl := `curl 'https://x' -H 'Cookie: _shopify_essential=ABC; _consentik_cookie=[{"k":"v"}]; _shopify_s=END'`
	got, err := FromCurl(curl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "_shopify_essential=ABC") || !strings.HasSuffix(got, "_shopify_s=END") {
		t.Errorf("truncated cookie: %q", got)
	}
}

func TestFromCurlMissing(t *testing.T) {
	if _, err := FromCurl(`curl 'https://x' -H 'Accept: text/html'`); err == nil {
		t.Error("expected error when no Cookie header present")
	}
}

// Every syntystore.com cookie is forwarded and nothing else is, on Firefox's real
// table rather than a three-column stand-in: the query reads named columns, so a
// stand-in schema stops representing the thing under test the moment it reads one
// more of them.
func TestReadSQLiteCookies(t *testing.T) {
	dbPath := newCookieDB(t, false,
		[3]string{".syntystore.com", "_shopify_essential", "ABC"},
		[3]string{"syntystore.com", "localization", "US"},
		[3]string{".other.com", "junk", "XX"},
	)
	got, err := readSQLiteCookies(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "_shopify_essential=ABC; localization=US"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
