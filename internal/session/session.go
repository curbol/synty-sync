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
		pairs[f[5]] = f[6]
	}
	return joinCookies(pairs)
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
			return []string{filepath.Join(".mozilla", "firefox")}
		case "zen":
			return []string{filepath.Join(".config", "zen"), filepath.Join(".zen")}
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
	var errs []error
	for _, rel := range bases {
		db, err := locateGeckoCookieDB(filepath.Join(home, rel))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return geckoCookieHeader(db)
	}
	return "", errors.Join(errs...)
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
	// ORDER BY host so that for a name present on both ".syntystore.com" and
	// "syntystore.com", the exact host is scanned last and wins the map.
	rows, err := db.Query(`SELECT name, value FROM moz_cookies WHERE host LIKE ? OR host = ? ORDER BY host`,
		"%."+cookieHost, cookieHost)
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

// copyDBToTemp copies a SQLite database and its WAL sidecars into a fresh temp
// directory, keeping the basename so SQLite finds the sidecars on open. It returns
// the directory to remove and the path of the copied database.
func copyDBToTemp(src string) (dir, dbPath string, err error) {
	dir, err = os.MkdirTemp("", "synty-cookies-")
	if err != nil {
		return "", "", err
	}
	base := filepath.Base(src)
	dbPath = filepath.Join(dir, base)
	if err := copyFile(src, dbPath); err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	for _, suffix := range walSidecars {
		if _, err := os.Stat(src + suffix); err != nil {
			continue
		}
		if err := copyFile(src+suffix, dbPath+suffix); err != nil {
			os.RemoveAll(dir)
			return "", "", err
		}
	}
	return dir, dbPath, nil
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

// locateGeckoCookieDB finds the best cookies.sqlite under a Gecko profile base dir.
// Profile folder names vary ("x.default-release", "y.Default (release)", …), so it
// prefers a default+release profile, then any default, then the most-recently-used.
func locateGeckoCookieDB(base string) (string, error) {
	// macOS and Windows nest the profiles one level further down than Linux does.
	if entries, err := os.ReadDir(filepath.Join(base, "Profiles")); err == nil && len(entries) > 0 {
		base = filepath.Join(base, "Profiles")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", fmt.Errorf("browser profile dir %s: %w (set SYNTY_BROWSER_PROFILE)", base, err)
	}
	type cand struct {
		path      string
		mod       time.Time
		isDefault bool
		isRelease bool
	}
	var cands []cand
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
		cands = append(cands, cand{db, fi.ModTime(), strings.Contains(low, "default"), strings.Contains(low, "release")})
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("no cookies.sqlite under %s (set SYNTY_BROWSER_PROFILE)", base)
	}
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
	return cands[0].path, nil
}
