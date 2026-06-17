// Package session builds the Cookie header for syntystore.com from one of three
// sources: the user's Firefox cookie store (default, zero-paste), a Netscape
// cookies.txt, or a pasted curl command. Only the storefront session matters, so
// every syntystore.com cookie found is forwarded rather than guessing one.
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

// Cookie values do not contain quotes, so matching a value of non-quote chars
// between matching quotes works for both -H 'Cookie: …' and -H "Cookie: …" without
// a backreference (RE2 has none).
var curlCookieRe = regexp.MustCompile(`(?i)(?:-H|--header)\s+['"]Cookie:\s*([^'"]+)['"]`)

// FromCurl extracts the Cookie header value from a pasted curl command.
func FromCurl(content string) (string, error) {
	m := curlCookieRe.FindStringSubmatch(content)
	if m == nil {
		return "", fmt.Errorf("no Cookie header found in curl command")
	}
	return strings.TrimSpace(m[1]), nil
}

// FromFile reads a cookie source file, auto-detecting curl vs cookies.txt.
func FromFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(raw)
	if strings.Contains(content, "curl ") || curlCookieRe.MatchString(content) {
		return FromCurl(content)
	}
	return FromCookiesTxt(content)
}

// FromFirefox locates the default Firefox profile (or honours SYNTY_FIREFOX_PROFILE),
// copies its (possibly locked) cookies.sqlite, and returns the syntystore.com header.
func FromFirefox() (string, error) {
	profile, err := locateFirefoxProfile()
	if err != nil {
		return "", err
	}
	return firefoxCookieHeader(filepath.Join(profile, "cookies.sqlite"))
}

func firefoxCookieHeader(dbPath string) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return "", fmt.Errorf("firefox cookies.sqlite not found at %s: %w", dbPath, err)
	}
	// Copy first: a running Firefox holds a lock on the live DB.
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
	rows, err := db.Query(`SELECT name, value FROM moz_cookies WHERE host LIKE ? OR host = ?`,
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

func locateFirefoxProfile() (string, error) {
	if p := os.Getenv("SYNTY_FIREFOX_PROFILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	base := filepath.Join(home, ".mozilla", "firefox")
	// Prefer a *.default-release / *.default profile dir.
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", fmt.Errorf("firefox dir %s: %w (set SYNTY_FIREFOX_PROFILE)", base, err)
	}
	var fallback string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".default-release") {
			return filepath.Join(base, name), nil
		}
		if strings.Contains(name, ".default") {
			fallback = filepath.Join(base, name)
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no default Firefox profile under %s (set SYNTY_FIREFOX_PROFILE)", base)
}
