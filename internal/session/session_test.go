package session

import (
	"database/sql"
	"path/filepath"
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

func TestReadSQLiteCookies(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cookies.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE moz_cookies (host TEXT, name TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	rows := [][3]string{
		{".syntystore.com", "_shopify_essential", "ABC"},
		{"syntystore.com", "localization", "US"},
		{".other.com", "junk", "XX"},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO moz_cookies VALUES (?,?,?)`, r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	got, err := readSQLiteCookies(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "_shopify_essential=ABC; localization=US"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
