// Package session builds the Cookie header for syntystore.com from one of several
// sources: a Gecko browser's cookie store (Firefox or Zen — default, zero-paste), a
// Netscape cookies.txt, or a pasted curl command. Only the storefront session
// matters, so every syntystore.com cookie found is forwarded rather than guessing one.
package session

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const cookieHost = "syntystore.com"

// httpOnlyPrefix marks an HttpOnly cookie in a Netscape cookies.txt.
const httpOnlyPrefix = "#HttpOnly_"

// FromCookiesTxt parses a Netscape cookies.txt and returns the Cookie header for
// syntystore.com.
func FromCookiesTxt(content string) (string, error) {
	pairs := map[string]string{}
	// How specific the record was that set the value currently in pairs, so a less
	// specific one later in the file cannot overwrite it. A cookies.txt has no
	// meaningful order, so file position must not decide which of two records setting
	// the same name wins; the same rule the sqlite reader applies with its ORDER BY.
	from := map[string]cookieRank{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		// "#HttpOnly_<domain>" is a record, not a comment: exporters mark HttpOnly
		// cookies this way, and the storefront session cookie is one of them.
		line = strings.TrimPrefix(line, httpOnlyPrefix)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		host := f[0]
		if !hostMatches(host) {
			continue
		}
		name, rank := f[5], cookieRank{host: hostRank(host), path: len(f[2])}
		if seen, ok := from[name]; ok && !rank.beats(seen) {
			continue
		}
		pairs[name], from[name] = f[6], rank
	}
	return joinCookies(pairs)
}

// cookieRank orders two records that set the same cookie name. Host specificity
// decides first; a longer path decides the rest, which is the order RFC 6265 has a
// browser send them in, so the value a server would read first is the one kept. Only
// the name is unique in a Cookie header, so one of the two has to be dropped.
type cookieRank struct {
	host int
	path int
}

func (a cookieRank) beats(b cookieRank) bool {
	if a.host != b.host {
		return a.host > b.host
	}
	return a.path > b.path
}

// hostRank orders a cookie's host by specificity: the apex beats the domain-wide
// form, which beats any subdomain.
func hostRank(host string) int {
	switch host {
	case cookieHost:
		return 2
	case "." + cookieHost:
		return 1
	default:
		return 0
	}
}

// A Cookie value can contain the opposite quote char (Shopify's _consentik_cookie
// holds JSON with double quotes), so match per outer-quote type: a single-quoted
// header captures up to the next single quote, a double-quoted one up to the next
// double quote. RE2 has no backreferences, so two patterns rather than one.
var (
	curlCookieSingle = regexp.MustCompile(`(?i)(?:-H|--header)\s+'Cookie:\s*([^']*)'`)
	curlCookieDouble = regexp.MustCompile(`(?i)(?:-H|--header)\s+"Cookie:\s*([^"]*)"`)
)

// FromCurl extracts the Cookie header value from a pasted curl command.
func FromCurl(content string) (string, error) {
	if m := curlCookieSingle.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1]), nil
	}
	if m := curlCookieDouble.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1]), nil
	}
	return "", fmt.Errorf("no Cookie header found in curl command")
}

// FromFile reads a cookie source file, auto-detecting curl vs cookies.txt.
func FromFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		// A source that is neither a browser name nor a readable file is far more often
		// a mistyped browser than a missing file, and "open firefx: no such file or
		// directory" does not say so.
		if errors.Is(err, fs.ErrNotExist) && !strings.ContainsAny(path, `/\.`) {
			return "", fmt.Errorf("no session source %q: use %s, or a path to a cookies.txt / pasted-curl file",
				path, strings.Join(browserNames, " or "))
		}
		return "", err
	}
	content := string(raw)
	if isCurlPaste(content) {
		return FromCurl(content)
	}
	return FromCookiesTxt(content)
}

// isCurlPaste distinguishes a pasted curl command from a Netscape cookies.txt by
// structure rather than by the word "curl" appearing anywhere: an exported
// cookies.txt often carries a header comment mentioning curl, and treating that as a
// command sends it to a parser that can only fail.
func isCurlPaste(content string) bool {
	if curlCookieSingle.MatchString(content) || curlCookieDouble.MatchString(content) {
		return true
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return strings.HasPrefix(line, "curl ")
	}
	return false
}

// browserNames are the Gecko browsers this tool can read cookies from. Zen is a
// Firefox fork, so its cookies.sqlite reads identically.
var browserNames = []string{"firefox", "zen"}

// browserBases maps a session source name to its Gecko profile base dirs, relative to
// home, for the platform in hand. Releases ship macOS and Windows binaries, so the
// Linux paths alone would leave the zero-paste default broken out of the box on two
// of the three.
//
// Every base is searched and the profiles found under all of them are ranked together,
// so listing one that does not exist costs nothing. On Linux a browser is as likely to
// be sandboxed as native: Ubuntu has shipped Firefox as a snap since 22.04, and a
// flatpak keeps its home under .var/app. Without those, the documented default fails
// with "no such file or directory" for a browser that is installed and logged in.
func browserBases(goos, name string) []string {
	switch goos {
	case "darwin":
		switch name {
		case "firefox":
			return []string{filepath.Join("Library", "Application Support", "Firefox")}
		case "zen":
			return []string{filepath.Join("Library", "Application Support", "zen")}
		}
	case "windows":
		switch name {
		case "firefox":
			return []string{filepath.Join("AppData", "Roaming", "Mozilla", "Firefox")}
		case "zen":
			return []string{filepath.Join("AppData", "Roaming", "zen")}
		}
	default:
		switch name {
		case "firefox":
			return []string{
				filepath.Join(".mozilla", "firefox"),
				filepath.Join("snap", "firefox", "common", ".mozilla", "firefox"),
				filepath.Join(".var", "app", "org.mozilla.firefox", ".mozilla", "firefox"),
			}
		case "zen":
			return []string{
				filepath.Join(".config", "zen"),
				filepath.Join(".zen"),
				filepath.Join(".var", "app", "app.zen_browser.zen", ".zen"),
			}
		}
	}
	return nil
}

func knownBrowser(name string) bool {
	return slices.Contains(browserNames, name)
}

// Resolve turns a session source into the syntystore.com Cookie header. The source
// is a browser name ("firefox", "zen"; "" means firefox) read from its cookie store,
// or a path to a cookies.txt / pasted-curl file.
func Resolve(src string) (string, error) {
	if src == "" {
		src = "firefox"
	}
	if knownBrowser(src) {
		return FromBrowser(src)
	}
	return FromFile(src)
}

// FromBrowser reads cookies from a Gecko browser's profile (Firefox or Zen),
// honouring SYNTY_BROWSER_PROFILE as a direct profile-dir override.
func FromBrowser(name string) (string, error) {
	if p := os.Getenv("SYNTY_BROWSER_PROFILE"); p != "" {
		return geckoCookieHeader(filepath.Join(p, "cookies.sqlite"))
	}
	if !knownBrowser(name) {
		return "", fmt.Errorf("unknown browser %q (use %s, or a cookies file path)", name, strings.Join(browserNames, ", "))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	bases := browserBases(runtime.GOOS, name)
	if len(bases) == 0 {
		return "", fmt.Errorf("no known %s profile location on %s (set SYNTY_BROWSER_PROFILE)", name, runtime.GOOS)
	}
	// Every base is collected before any is chosen. Taking the first base that holds
	// a profile would let a layout left behind by an upgrade (a ~/.zen beside the
	// ~/.config/zen the browser actually writes, or a native profile beside the snap
	// that replaced it) win over the live one purely for being listed first, and the
	// run would report an expired session against cookies that are simply months old.
	var errs []error
	var cands []geckoProfile
	for _, rel := range bases {
		found, err := geckoCandidates(filepath.Join(home, rel))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cands = append(cands, found...)
	}
	if len(cands) == 0 {
		return "", errors.Join(errs...)
	}
	return geckoCookieHeader(pickGeckoProfile(cands))
}

func geckoCookieHeader(dbPath string) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", fmt.Errorf("cookies.sqlite not found at %s: %w", dbPath, err)
	}
	// Copy first: a running browser holds a lock on the live DB.
	dir, copied, err := copyDBToTemp(dbPath)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	return readSQLiteCookies(copied)
}

func readSQLiteCookies(dbPath string) (string, error) {
	// mode=ro, not immutable=1: immutable tells SQLite the file cannot change and so
	// skips WAL recovery, which would hide every write the browser has not yet
	// checkpointed into the main file. The path is a private copy, so ro is enough.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	// Scan in increasing order of specificity, so the last write into the map for a
	// given name is the most specific record that set it: a subdomain first, then the
	// domain-wide ".syntystore.com", then the apex itself. Ordering by host alone
	// would decide that alphabetically: every subdomain sorting after "syntystore"
	// (www, for one) would beat the apex and send the wrong value, which arrives as an
	// expired session against cookies the user just refreshed.
	//
	// Host is not unique: moz_cookies keys on (name, host, path, originAttributes), so
	// a name set at two paths, or in a container tab as well as an ordinary window,
	// gives two rows that tie on host. SQLite's sorter is not documented as stable, so
	// without the rest of this the winner is whichever row happened to arrive last.
	// path length is the order RFC 6265 has a browser send them in, and lastAccessed
	// then picks the session actually in use over one left behind by an earlier login;
	// id is there only to make the order total.
	rows, err := db.Query(
		`SELECT name, value FROM moz_cookies WHERE host LIKE ? OR host = ?
		 ORDER BY CASE host WHEN ? THEN 2 WHEN ? THEN 1 ELSE 0 END,
		          LENGTH(path), lastAccessed, id`,
		"%."+cookieHost, cookieHost, cookieHost, "."+cookieHost)
	if err != nil {
		return "", fmt.Errorf("query moz_cookies: %w", err)
	}
	defer rows.Close()
	pairs := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return "", err
		}
		pairs[name] = value
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return joinCookies(pairs)
}

func hostMatches(host string) bool {
	host = strings.TrimPrefix(host, ".")
	return host == cookieHost || strings.HasSuffix(host, "."+cookieHost)
}

func joinCookies(pairs map[string]string) (string, error) {
	if len(pairs) == 0 {
		return "", fmt.Errorf("no %s cookies found (is the session present / logged in?)", cookieHost)
	}
	names := make([]string, 0, len(pairs))
	for n := range pairs {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic header
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(pairs[n])
	}
	return b.String(), nil
}

// walSidecars are the files SQLite keeps beside a WAL-mode database that the copy has
// to bring along. A running browser can hold recent cookie writes in -wal for the
// whole session, so without it the copy reads a stale snapshot and the user is told
// the session they just refreshed has expired. The -shm wal-index is deliberately not
// copied: SQLite rebuilds it from the -wal, and copying it second would let a write
// landing between the two describe frames the copied -wal does not have.
var walSidecars = []string{"-wal"}

// copyAttempts is how many times a torn copy is retried before the pair is used as
// taken. A checkpoint is a rare event in the millisecond or two a copy takes, so one
// retry practically always suffices; the cap is here so a browser writing constantly
// cannot spin.
const copyAttempts = 3

// copyDBToTemp copies a SQLite database and its WAL sidecars into a fresh temp
// directory, keeping the basename so SQLite finds the sidecars on open. It returns
// the directory to remove and the path of the copied database.
//
// The main file and the -wal are two separate copies, and a checkpoint landing between
// them folds the -wal into the main file and resets it: the main copy is then missing
// the pages the -wal copy no longer describes, and WAL recovery applies those frames to
// a state that never existed. Nothing cross-checks the pair, so the read comes back
// wrong or empty, and the user is told the session they just refreshed has expired,
// which is the failure copying the -wal at all was meant to prevent. A checkpoint
// always writes the main file, so the source is stat'd either side and a copy that
// raced one is taken again.
func copyDBToTemp(src string) (dir, dbPath string, err error) {
	dir, err = os.MkdirTemp("", "synty-cookies-")
	if err != nil {
		return "", "", err
	}
	for attempt := range copyAttempts {
		before, statErr := os.Stat(src)
		if statErr != nil {
			os.RemoveAll(dir)
			return "", "", statErr
		}
		dbPath, err = copyDBPair(dir, src)
		if err != nil {
			os.RemoveAll(dir)
			return "", "", err
		}
		after, statErr := os.Stat(src)
		if statErr == nil && after.Size() == before.Size() && after.ModTime().Equal(before.ModTime()) {
			return dir, dbPath, nil
		}
		// Last time round: the pair may be torn, but a stale read is still better than
		// refusing to read a cookie store the browser happens to be busy with.
		if attempt == copyAttempts-1 {
			return dir, dbPath, nil
		}
	}
	// Unreachable: the loop returns on every path.
	return dir, dbPath, nil
}

// copyDBPair copies the database and its sidecars into dir, returning the copy's path.
func copyDBPair(dir, src string) (dbPath string, err error) {
	base := filepath.Base(src)
	dbPath = filepath.Join(dir, base)
	if err := copyFile(src, dbPath); err != nil {
		return "", err
	}
	for _, suffix := range walSidecars {
		// A retry after a checkpoint finds the sidecar gone from the source. Clearing the
		// earlier attempt's copy is what keeps it from being applied to a main file that
		// has already absorbed it.
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, err := os.Stat(src + suffix); err != nil {
			continue
		}
		// The browser can exit between the stat and the copy, and SQLite deletes the
		// sidecars as the last connection closes. That is the same race the retry above
		// this exists for, so it reads as "no sidecar" rather than ending the run with
		// an ENOENT on a file the user never asked about.
		if err := copyFile(src+suffix, dbPath+suffix); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", err
		}
	}
	return dbPath, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// livenessSidecars are the files whose timestamps say when a profile was last written
// to. It is a superset of walSidecars: -shm is not worth copying (SQLite rebuilds it)
// but its presence and mtime are the strongest signal a profile gives that a browser
// is holding the database open right now.
var livenessSidecars = []string{"-wal", "-shm"}

// lastWritten is when a profile's cookie store was last touched, which is not the main
// file's mtime. A WAL-mode database — which is every running Firefox or Zen — takes its
// writes in cookies.sqlite-wal and moves the main file only on a checkpoint, so ranking
// on the main file alone reads a live profile as older than it is. That is how a dead
// profile left behind by a browser that moved between layouts (a deb install replaced
// by a snap) wins the tie against the one in use, after which the run reads real but
// months-old cookies and reports the session the user just refreshed as expired.
func lastWritten(db string, fi os.FileInfo) time.Time {
	mod := fi.ModTime()
	for _, suffix := range livenessSidecars {
		if si, err := os.Stat(db + suffix); err == nil && si.ModTime().After(mod) {
			mod = si.ModTime()
		}
	}
	return mod
}

// geckoProfile is one cookies.sqlite found under a profile base, with the facts that
// decide between several.
type geckoProfile struct {
	path      string
	mod       time.Time
	isDefault bool
	isRelease bool
}

// geckoCandidates lists every cookies.sqlite under one Gecko profile base dir.
func geckoCandidates(base string) ([]geckoProfile, error) {
	// macOS and Windows nest the profiles one level further down than Linux does.
	if entries, err := os.ReadDir(filepath.Join(base, "Profiles")); err == nil && len(entries) > 0 {
		base = filepath.Join(base, "Profiles")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("browser profile dir %s: %w (set SYNTY_BROWSER_PROFILE)", base, err)
	}
	var cands []geckoProfile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		db := filepath.Join(base, e.Name(), "cookies.sqlite")
		fi, err := os.Stat(db)
		if err != nil {
			continue
		}
		low := strings.ToLower(e.Name())
		cands = append(cands, geckoProfile{db, lastWritten(db, fi), strings.Contains(low, "default"), strings.Contains(low, "release")})
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("no cookies.sqlite under %s (set SYNTY_BROWSER_PROFILE)", base)
	}
	return cands, nil
}

// pickGeckoProfile chooses between candidates. Profile folder names vary
// ("x.default-release", "y.Default (release)", …), so it prefers a default+release
// profile, then any default, then the most-recently-used. It ranks across every base
// at once: a browser that moved between layouts leaves the old profile in place, and
// deciding per-base would let whichever base was listed first supply a dead one.
func pickGeckoProfile(cands []geckoProfile) string {
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.isDefault != b.isDefault {
			return a.isDefault
		}
		if a.isRelease != b.isRelease {
			return a.isRelease
		}
		return a.mod.After(b.mod)
	})
	return cands[0].path
}
