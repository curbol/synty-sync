package session

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// "#HttpOnly_<domain>" is the Netscape-format marker every mainstream exporter
// writes, not a comment. The storefront session cookie is HttpOnly, so treating the
// line as a comment drops exactly the cookie that authenticates.
func TestFromCookiesTxtKeepsHttpOnlyLines(t *testing.T) {
	content := "# Netscape HTTP Cookie File\n" +
		"#HttpOnly_.syntystore.com\tTRUE\t/\tTRUE\t0\t_shopify_essential\tABC\n" +
		"syntystore.com\tFALSE\t/\tFALSE\t0\tlocalization\tUS\n"
	got, err := FromCookiesTxt(content)
	if err != nil {
		t.Fatal(err)
	}
	want := "_shopify_essential=ABC; localization=US"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A running browser leaves recent writes in the -wal sidecar, so reading the main
// file alone returns a stale cookie set: the user logs in, syncs, and is told the
// session expired.
func TestGeckoCookieHeaderReadsUncheckpointedWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cookies.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE moz_cookies (host TEXT, name TEXT, value TEXT);
		INSERT INTO moz_cookies VALUES ('syntystore.com','localization','US');`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	// Written after the checkpoint, so it lives only in the sidecar.
	if _, err := db.Exec(`INSERT INTO moz_cookies VALUES ('.syntystore.com','_shopify_essential','FRESH')`); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dbPath + "-wal"); err != nil || fi.Size() == 0 {
		t.Skip("sqlite driver produced no WAL sidecar")
	}

	got, err := geckoCookieHeader(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "_shopify_essential=FRESH") {
		t.Errorf("uncheckpointed cookie missing from %q", got)
	}
}

// Gecko profile folders are named inconsistently across installs, so the pick has
// to prefer default+release, then any default, then the most recently used.
func TestLocateGeckoCookieDBPrefersDefaultRelease(t *testing.T) {
	base := t.TempDir()
	mkProfile := func(name string, age time.Duration) string {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		db := filepath.Join(dir, "cookies.sqlite")
		if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(db, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return db
	}
	newest := mkProfile("zzz.scratch", 0)
	plainDefault := mkProfile("aaa.Default", time.Hour)
	want := mkProfile("bbb.Default (release)", 2*time.Hour)

	got, err := locateGeckoCookieDB(base)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("picked %q, want the default+release profile %q", got, want)
	}

	// With no release profile, any default beats the more recently used one.
	if err := os.RemoveAll(filepath.Dir(want)); err != nil {
		t.Fatal(err)
	}
	got, err = locateGeckoCookieDB(base)
	if err != nil {
		t.Fatal(err)
	}
	if got != plainDefault {
		t.Errorf("picked %q, want the default profile %q (not the newer %q)", got, plainDefault, newest)
	}
}

func TestLocateGeckoCookieDBWithNoProfiles(t *testing.T) {
	if _, err := locateGeckoCookieDB(t.TempDir()); err == nil {
		t.Error("expected an error when no profile holds a cookies.sqlite")
	}
}

// Reading must never disturb the browser's live store.
func TestGeckoCookieHeaderLeavesSourceUntouched(t *testing.T) {
	dbPath := newCookieDB(t, true, [3]string{"syntystore.com", "localization", "US"})

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := geckoCookieHeader(dbPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the source cookies.sqlite was modified")
	}
}

// FromFile decides curl-vs-cookies.txt by sniffing the content, and Resolve routes a
// browser name to the browser reader and anything else to a file. Both are real
// heuristics on the zero-paste path and neither was covered.
func TestFromFileDetectsItsFormat(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	curlPath := write("paste.curl",
		"curl 'https://syntystore.com/account' \\\n  -H 'Cookie: _shopify_essential=abc; cart=1' \\\n  --compressed\n")
	got, err := FromFile(curlPath)
	if err != nil {
		t.Fatalf("curl paste: %v", err)
	}
	if !strings.Contains(got, "_shopify_essential=abc") {
		t.Errorf("curl paste cookie = %q", got)
	}

	txtPath := write("cookies.txt",
		"# Netscape HTTP Cookie File\n.syntystore.com\tTRUE\t/\tTRUE\t0\t_shopify_essential\txyz\n")
	got, err = FromFile(txtPath)
	if err != nil {
		t.Fatalf("cookies.txt: %v", err)
	}
	if !strings.Contains(got, "_shopify_essential=xyz") {
		t.Errorf("cookies.txt cookie = %q", got)
	}

	// A cookies.txt whose comment happens to mention curl must not be misread as a
	// pasted command; the format sniff has to look at structure, not a substring.
	mixed := write("mentions-curl.txt",
		"# exported for use with curl and wget\n.syntystore.com\tTRUE\t/\tTRUE\t0\t_shopify_essential\tzzz\n")
	got, err = FromFile(mixed)
	if err != nil {
		t.Fatalf("cookies.txt mentioning curl: %v", err)
	}
	if !strings.Contains(got, "_shopify_essential=zzz") {
		t.Errorf("cookies.txt mentioning curl was misrouted: %q", got)
	}
}

func TestResolveRoutesBrowserNamesAndPaths(t *testing.T) {
	if _, err := Resolve("not-a-browser-and-not-a-file"); err == nil {
		t.Error("an unknown source must error rather than resolve to nothing")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(p, []byte(".syntystore.com\tTRUE\t/\tTRUE\t0\ts\tv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(p)
	if err != nil || !strings.Contains(got, "s=v") {
		t.Errorf("Resolve(path) = %q, %v", got, err)
	}
}

// newCookieDB creates a Gecko-shaped cookies.sqlite with the given rows, returning
// its path. It replaces the open/schema/insert block each cookie-DB test repeats;
// wal selects the journal mode a test needs.
func newCookieDB(t *testing.T, wal bool, rows ...[3]string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cookies.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if wal {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE moz_cookies (host TEXT, name TEXT, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO moz_cookies VALUES (?,?,?)`, r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath
}

// A cookie DB the tool cannot read must say so, not silently yield an empty header
// that then surfaces downstream as a confusing "expired session".
func TestUnreadableCookieDBIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.sqlite")
	if _, err := geckoCookieHeader(missing); err == nil {
		t.Error("a missing cookie DB must error, not return an empty header")
	}

	garbage := filepath.Join(t.TempDir(), "cookies.sqlite")
	if err := os.WriteFile(garbage, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := geckoCookieHeader(garbage); err == nil && got == "" {
		t.Error("a corrupt cookie DB returned an empty header with no error")
	}
}

// A profile with no syntystore.com cookies at all is a distinct, reportable state.
func TestCookieDBWithNoStoreCookies(t *testing.T) {
	db := newCookieDB(t, false, [3]string{".other.com", "junk", "XX"})
	got, err := geckoCookieHeader(db)
	if err == nil {
		// An empty header travels downstream and comes back as "expired session",
		// pointing the user at a login they have already done.
		t.Errorf("a profile with no store cookies returned header %q and no error", got)
	}
	if strings.Contains(got, "junk") {
		t.Errorf("a non-syntystore cookie was forwarded: %q", got)
	}
}
