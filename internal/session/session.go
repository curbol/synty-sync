// Package session builds the Cookie header for syntystore.com from one of several
// sources: a Gecko browser's cookie store (Firefox or Zen — default, zero-paste), a
// Netscape cookies.txt, or a pasted curl command. Only the storefront session
// matters, so every syntystore.com cookie found is forwarded rather than guessing one.
package session

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const cookieHost = "syntystore.com"

// FromCookiesTxt parses a Netscape cookies.txt and returns the Cookie header for
// syntystore.com.
func FromCookiesTxt(content string) (string, error) {
	pairs := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
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
		return "", err
	}
	content := string(raw)
	if strings.Contains(content, "curl ") || curlCookieSingle.MatchString(content) || curlCookieDouble.MatchString(content) {
		return FromCurl(content)
	}
	return FromCookiesTxt(content)
}

// browserBases maps a session source name to its Gecko profile base dir (relative
// to home). Zen is a Firefox fork, so its cookies.sqlite reads identically.
var browserBases = map[string]string{
	"firefox": filepath.Join(".mozilla", "firefox"),
	"zen":     filepath.Join(".config", "zen"),
}

// Resolve turns a session source into the syntystore.com Cookie header. The source
// is a browser name ("firefox", "zen"; "" means firefox) read from its cookie store,
// or a path to a cookies.txt / pasted-curl file.
func Resolve(src string) (string, error) {
	if src == "" {
		src = "firefox"
	}
	if _, ok := browserBases[src]; ok {
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
	rel, ok := browserBases[name]
	if !ok {
		return "", fmt.Errorf("unknown browser %q (use firefox, zen, or a cookies file path)", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	db, err := locateGeckoCookieDB(filepath.Join(home, rel))
	if err != nil {
		return "", err
	}
	return geckoCookieHeader(db)
}

func geckoCookieHeader(dbPath string) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", fmt.Errorf("cookies.sqlite not found at %s: %w", dbPath, err)
	}
	// Copy first: a running browser holds a lock on the live DB.
	tmp, err := copyToTemp(dbPath)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	return readSQLiteCookies(tmp)
}

func readSQLiteCookies(dbPath string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&immutable=1")
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

func copyToTemp(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp("", "synty-cookies-*.sqlite")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// locateGeckoCookieDB finds the best cookies.sqlite under a Gecko profile base dir.
// Profile folder names vary ("x.default-release", "y.Default (release)", …), so it
// prefers a default+release profile, then any default, then the most-recently-used.
func locateGeckoCookieDB(base string) (string, error) {
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
