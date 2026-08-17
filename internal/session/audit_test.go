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
	dbPath := filepath.Join(t.TempDir(), "cookies.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE moz_cookies (host TEXT, name TEXT, value TEXT);
		INSERT INTO moz_cookies VALUES ('syntystore.com','localization','US');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

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
