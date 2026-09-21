package session

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// copyDBToTemp retries into the same directory when it catches a torn pair, and the
// browser may have checkpointed in between, so the source's sidecar can be gone on the
// second pass. Leaving the first pass's copy behind hands SQLite a -wal holding frames
// the main file has already absorbed, which it replays over a checkpointed database:
// the wrong cookie set, reported to the user as an expired session against a login
// they have just made. The retry has no deterministic trigger; the clearing it depends
// on does.
func TestCopyDBPairClearsASidecarTheSourceNoLongerHas(t *testing.T) {
	src := filepath.Join(t.TempDir(), "cookies.sqlite")
	if err := os.WriteFile(src, []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range walSidecars {
		if err := os.WriteFile(src+suffix, []byte("frames"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	dbPath, err := copyDBPair(dir, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range walSidecars {
		if _, err := os.Stat(dbPath + suffix); err != nil {
			t.Fatalf("first copy did not bring %s across: %v", suffix, err)
		}
	}

	// The browser checkpoints and SQLite removes the sidecar; the retry copies again
	// into the directory the first attempt already populated.
	for _, suffix := range walSidecars {
		if err := os.Remove(src + suffix); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := copyDBPair(dir, src); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range walSidecars {
		if _, err := os.Stat(dbPath + suffix); !os.IsNotExist(err) {
			t.Errorf("%s survived a copy whose source had none; stale frames would replay over the checkpointed db", suffix)
		}
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
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(geckoSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO moz_cookies (name, value, host, path) VALUES ('localization','US','syntystore.com','/')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	// Written after the checkpoint, so it lives only in the sidecar.
	if _, err := db.Exec(
		`INSERT INTO moz_cookies (name, value, host, path) VALUES ('_shopify_essential','FRESH','.syntystore.com','/')`); err != nil {
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

// pickUnder is the composition FromBrowser uses: candidates from a base, then a pick
// across them. Calling it here rather than a per-base wrapper keeps these guards on the
// path production takes, since ranking per base is the thing FromBrowser deliberately
// does not do.
func pickUnder(t *testing.T, base string) (string, error) {
	t.Helper()
	cands, err := geckoCandidates(base)
	if err != nil {
		return "", err
	}
	return pickGeckoProfile(cands), nil
}

// Gecko profile folders are named inconsistently across installs, so the pick has
// to prefer default+release, then any default, then the most recently used.
func TestGeckoProfilePickPrefersDefaultRelease(t *testing.T) {
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

	got, err := pickUnder(t, base)
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
	got, err = pickUnder(t, base)
	if err != nil {
		t.Fatal(err)
	}
	if got != plainDefault {
		t.Errorf("picked %q, want the default profile %q (not the newer %q)", got, plainDefault, newest)
	}
}

// A profile base holding no cookies.sqlite is a reportable error, not an empty cookie
// header. Returning nothing here sends an empty Cookie to the store, which answers with
// a logged-out page, and the user is told their session expired when the real problem is
// that the tool never found their browser profile.
func TestGeckoProfilePickWithNoProfiles(t *testing.T) {
	if _, err := pickUnder(t, t.TempDir()); err == nil {
		t.Error("expected an error when no profile holds a cookies.sqlite")
	}
}

// Reading must never disturb the browser's live store. The whole profile directory is
// compared, not just the database's bytes: opening the source directly instead of a
// copy is the obvious simplification, mode=ro already reads like a promise, and what it
// actually does is create a -shm beside the live profile and let SQLite checkpoint the
// live -wal. On a quiescent profile that checkpoint is a no-op, so the main file is
// unchanged and only a new sidecar gives it away.
func TestGeckoCookieHeaderLeavesSourceUntouched(t *testing.T) {
	dbPath := newCookieDB(t, true, [3]string{"syntystore.com", "localization", "US"})

	before := snapshotDir(t, filepath.Dir(dbPath))
	if _, err := geckoCookieHeader(dbPath); err != nil {
		t.Fatal(err)
	}
	after := snapshotDir(t, filepath.Dir(dbPath))

	for name, content := range before {
		got, ok := after[name]
		if !ok {
			t.Errorf("reading removed %s from the profile directory", name)
			continue
		}
		if got != content {
			t.Errorf("reading modified %s in the profile directory", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("reading created %s in the profile directory; the live store must not be written to", name)
		}
	}
}

// snapshotDir returns every file in dir keyed by name, so a test can assert nothing in
// it was added, removed or rewritten.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// moz_cookies keys on (name, host, path, originAttributes), so one cookie name can have
// several rows: set at two paths, or in a container tab as well as an ordinary window.
// They tie on host, SQLite's sorter is not documented as stable, and only one value per
// name fits in a Cookie header, so without the rest of the ORDER BY the winner is
// whichever row arrived last and the user gets an intermittent expired session against
// a login they just made.
func TestDuplicateCookieRowsResolveToTheLiveOne(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cookies.sqlite")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(geckoSchema); err != nil {
		t.Fatal(err)
	}
	// Same name and host, inserted newest-first so row order and the wanted answer
	// disagree. The stale row is at the root path and was touched long ago; the live one
	// is more specific and more recent.
	for _, row := range []struct {
		value        string
		path         string
		originAttrs  string
		lastAccessed int64
	}{
		{"LIVE", "/apps/downloads", "^userContextId=4", 2000},
		{"STALE-ROOT", "/", "", 1000},
	} {
		if _, err := db.Exec(
			`INSERT INTO moz_cookies (originAttributes, name, value, host, path, lastAccessed)
			 VALUES (?, '_shopify_essential', ?, ?, ?, ?)`,
			row.originAttrs, row.value, cookieHost, row.path, row.lastAccessed); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Read repeatedly: a tie broken by arrival order can agree with the wanted answer
	// once by luck.
	for i := range 5 {
		got, err := geckoCookieHeader(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if got != "_shopify_essential=LIVE" {
			t.Fatalf("read %d returned %q, want the most specific, most recently used row", i, got)
		}
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

// Resolve takes a browser name or a file path in one string, so the two routes are told
// apart by what the value looks like. A source that is neither has to error: falling
// through to an empty header would reach the store as a logged-out request and be
// reported as an expired session, naming nothing the user mistyped.
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

// geckoSchema is Firefox's real moz_cookies table, not a three-column stand-in: the
// reader orders on path, lastAccessed and id, so a query that reads them has to be
// testable without a live profile. Every test that builds a cookie DB uses this one,
// so none of them can pass against a shape the reader could not query.
const geckoSchema = `CREATE TABLE moz_cookies (
	id INTEGER PRIMARY KEY, originAttributes TEXT NOT NULL DEFAULT '',
	name TEXT, value TEXT, host TEXT, path TEXT, expiry INTEGER,
	lastAccessed INTEGER, creationTime INTEGER, isSecure INTEGER, isHttpOnly INTEGER,
	inBrowserElement INTEGER DEFAULT 0, sameSite INTEGER DEFAULT 0,
	rawSameSite INTEGER DEFAULT 0, schemeMap INTEGER DEFAULT 0)`

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
	if _, err := db.Exec(geckoSchema); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO moz_cookies (originAttributes, name, value, host, path, expiry,
			 lastAccessed, creationTime, isSecure, isHttpOnly)
			 VALUES ('', ?, ?, ?, '/', 4102444800, 0, 0, 1, 1)`,
			r[1], r[2], r[0]); err != nil {
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
	// Any success is wrong here, not just an empty one: a corrupt DB can never
	// legitimately yield cookies, and a non-empty header out of one would be bytes
	// from nowhere presented as the user's session.
	if got, err := geckoCookieHeader(garbage); err == nil {
		t.Errorf("a corrupt cookie DB returned %q with no error", got)
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

// Releases ship macOS and Windows binaries, and browser reading is the documented
// zero-paste default. Linux-only profile bases left that default broken out of the
// box on two of the three platforms, with --cookies as the only way through.
func TestEveryReleasedPlatformHasAProfileLocation(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, name := range browserNames {
			if len(browserBases(goos, name)) == 0 {
				t.Errorf("no %s profile base for %s", name, goos)
			}
		}
	}
}

// SYNTY_BROWSER_PROFILE is the documented escape hatch for a profile in a place the
// built-in bases do not name, and it is the only route for a layout this build has
// not seen.
func TestBrowserProfileOverrideIsHonored(t *testing.T) {
	db := newCookieDB(t, false, [3]string{"syntystore.com", "session", "abc"})
	profile := filepath.Dir(db)

	for _, tc := range []struct {
		name string
		// value is the override as it is set; home is what ~ has to resolve to for it
		// to mean the profile, or "" when the value names the profile outright.
		value, home string
	}{
		{name: "an absolute path", value: profile},
		// No shell expands an environment value, so a ~ written in a systemd unit, a
		// direnv file or a quoted export arrives literally. Without expansion this
		// stats ./~/<profile> and reports a path the reader can see exists.
		{name: "a tilde path", value: filepath.Join("~", filepath.Base(profile)), home: filepath.Dir(profile)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.home != "" {
				t.Setenv("HOME", tc.home)
			}
			t.Setenv("SYNTY_BROWSER_PROFILE", tc.value)
			got, err := FromBrowser("firefox")
			if err != nil {
				t.Fatalf("FromBrowser with an explicit profile: %v", err)
			}
			if got != "session=abc" {
				t.Errorf("Cookie header = %q, want session=abc", got)
			}
		})
	}
}

// FromBrowser aggregates per-base failures and hands back errors.Join, which is nil
// for an empty slice. A base ever skipped rather than reported would return an empty
// Cookie header with no error, and the logged-out page the store then serves comes
// back as an expired session, sending the reader to log in again in a browser nothing
// ever read.
func TestFromBrowserWithNoProfileAnywhereIsAnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	t.Setenv("SYNTY_BROWSER_PROFILE", "")
	got, err := FromBrowser("firefox")
	if err == nil {
		t.Fatalf("FromBrowser = %q, nil; want an error naming the profiles it could not find", got)
	}
	if got != "" {
		t.Errorf("Cookie header = %q, want it empty alongside the error", got)
	}
}

// A source that is neither a browser name nor a path is far more often a mistyped
// browser than a missing file, and the bare open error does not say so.
func TestMistypedBrowserNamesTheOnesThatWork(t *testing.T) {
	_, err := Resolve("firefx")
	if err == nil {
		t.Fatal("a mistyped browser name resolved")
	}
	if !strings.Contains(err.Error(), "firefox") {
		t.Errorf("error does not name the browsers that work: %v", err)
	}
}

// Two hosts can set the same cookie name, and only one value can go in the header.
// The right one is always the most specific host: the apex over the domain-wide
// ".syntystore.com" form, and either over a subdomain. Deciding by anything else,
// alphabetical host order or position in a file, sends a value the store did not
// set for the apex, and the run reports an expired session against a login the user
// just completed.
func TestTheMostSpecificHostWinsACookieName(t *testing.T) {
	// Deliberately includes a subdomain sorting *after* "syntystore.com", which is
	// what an ORDER BY host alone gets wrong.
	rows := [][3]string{
		{"www." + cookieHost, "sid", "from-www"},
		{"." + cookieHost, "sid", "from-dot"},
		{cookieHost, "sid", "from-apex"},
		{"account." + cookieHost, "sid", "from-account"},
	}
	got, err := readSQLiteCookies(newCookieDB(t, false, rows...))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "sid=from-apex") {
		t.Errorf("header = %q, want the apex host's value", got)
	}

	// The same rule for a cookies.txt, where there is no order to lean on at all: the
	// apex line comes first here, so a plain last-write-wins picks the subdomain.
	txt := strings.Join([]string{
		strings.Join([]string{cookieHost, "TRUE", "/", "TRUE", "4102444800", "sid", "from-apex"}, "\t"),
		strings.Join([]string{"." + cookieHost, "TRUE", "/", "TRUE", "4102444800", "sid", "from-dot"}, "\t"),
		strings.Join([]string{"www." + cookieHost, "TRUE", "/", "TRUE", "4102444800", "sid", "from-www"}, "\t"),
	}, "\n")
	got, err = FromCookiesTxt(txt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "sid=from-apex") {
		t.Errorf("cookies.txt header = %q, want the apex host's value", got)
	}
}

// A browser that moved between layouts leaves the old profile behind: a ~/.zen beside
// the ~/.config/zen Zen now writes, or a native ~/.mozilla/firefox beside the snap
// that replaced it. Choosing the first base that holds any profile hands back the
// dead one purely because it is listed first, and its cookies are real but months
// old, so the run reports an expired session and the live profile is never opened.
// Every base's profiles have to be ranked together.
func TestALiveProfileBeatsALeftoverInAnEarlierBase(t *testing.T) {
	bases := browserBases(runtime.GOOS, "zen")
	if len(bases) < 2 {
		// A skip, not a failure: the multi-base layout this guard is about only exists
		// on Linux, and a red guard is supposed to mean a regression.
		t.Skipf("zen has %d profile base(s) on %s; ranking across bases only arises where there are two", len(bases), runtime.GOOS)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	t.Setenv("SYNTY_BROWSER_PROFILE", "")

	// The leftover goes in the base that is searched first, the live one in a later
	// base, which is the arrangement an upgrade leaves behind.
	plant := func(base, value string, age time.Duration) string {
		dir := filepath.Join(home, base, "abc.default-release")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		src := newCookieDB(t, false, [3]string{cookieHost, "sid", value})
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		db := filepath.Join(dir, "cookies.sqlite")
		if err := os.WriteFile(db, b, 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(db, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return db
	}
	plant(bases[0], "stale", 90*24*time.Hour)
	plant(bases[1], "live", 0)

	got, err := FromBrowser("zen")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "sid=live") {
		t.Errorf("header = %q, want the recently-written profile from the later base", got)
	}
}

// The zero-paste default is "read the browser you are already logged into", and on
// Linux that browser is as likely to be sandboxed as native. Without the snap and
// flatpak layouts the tool reports "no such file or directory" for a browser that is
// installed and signed in, and the user is told to export cookies by hand.
func TestLinuxBasesCoverSandboxedBrowsers(t *testing.T) {
	for _, tc := range []struct{ browser, want string }{
		{"firefox", filepath.Join("snap", "firefox", "common", ".mozilla", "firefox")},
		{"firefox", filepath.Join(".var", "app", "org.mozilla.firefox", ".mozilla", "firefox")},
		{"zen", filepath.Join(".var", "app", "app.zen_browser.zen", ".zen")},
	} {
		if !slices.Contains(browserBases("linux", tc.browser), tc.want) {
			t.Errorf("linux %s bases do not include %q: %v", tc.browser, tc.want, browserBases("linux", tc.browser))
		}
	}
}

// Ranking profiles on cookies.sqlite's own mtime reads every running browser as older
// than it is: a WAL-mode database takes its writes in the -wal sidecar and moves the
// main file only on a checkpoint. That is how the leftover from a browser that moved
// between layouts (a deb install replaced by a snap) wins the tie against the profile
// actually in use, after which the run reads real but months-old cookies — not an
// empty set, so nothing errors — and tells the user the session they just refreshed
// has expired. The existing liveness guard plants non-WAL databases and stamps them
// with Chtimes, so it cannot see this.
func TestALiveWALProfileBeatsALeftoverWithANewerMainFile(t *testing.T) {
	base := t.TempDir()

	walProfile := func(dir, value string) string {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(dir, "cookies.sqlite")
		db, err := sql.Open("sqlite", "file:"+dbPath)
		if err != nil {
			t.Fatal(err)
		}
		// Held open for the life of the test: SQLite removes the sidecars when the last
		// connection closes, and the sidecar is the whole point here.
		t.Cleanup(func() { db.Close() })
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(geckoSchema); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
		// Written after the checkpoint, so it lives only in the sidecar and the main
		// file's mtime stays at the checkpoint.
		if _, err := db.Exec(
			`INSERT INTO moz_cookies (originAttributes, name, value, host, path, expiry,
			 lastAccessed, creationTime, isSecure, isHttpOnly)
			 VALUES ('', 'sid', ?, ?, '/', 4102444800, 0, 0, 1, 1)`, value, cookieHost); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(dbPath + "-wal"); err != nil || fi.Size() == 0 {
			t.Skip("sqlite driver produced no WAL sidecar")
		}
		return dbPath
	}

	// Both names tie on default and release, so recency is what decides between them.
	live := walProfile(filepath.Join(base, "bbb.default-release"), "live")
	dead := filepath.Join(base, "aaa.default-release")
	if err := os.MkdirAll(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	deadDB := filepath.Join(dead, "cookies.sqlite")
	src, err := os.ReadFile(newCookieDB(t, false, [3]string{cookieHost, "sid", "stale"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deadDB, src, 0o600); err != nil {
		t.Fatal(err)
	}

	// The leftover's main file is newer than the live profile's main file, and older
	// than the live profile's sidecar. Only the sidecar tells them apart.
	stamp := func(path string, age time.Duration) {
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	stamp(live, 90*24*time.Hour)
	stamp(deadDB, time.Hour)

	cands, err := geckoCandidates(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := pickGeckoProfile(cands); got != live {
		t.Errorf("picked %q, want the live WAL profile %q", got, live)
	}
}
