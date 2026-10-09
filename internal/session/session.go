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
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/curbol/synty-sync/internal/config"

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

// FromCurl extracts the Cookie header value from a pasted curl command.
func FromCurl(content string) (string, error) {
	if v, ok := cookieArgument(content); ok {
		return v, nil
	}
	return "", fmt.Errorf("no Cookie header found in curl command")
}

// cookieArgument returns the cookie string a pasted curl command carries. Two flags
// carry it: a browser that writes the jar as a header emits -H 'Cookie: …', and one that
// uses curl's own cookie flag emits -b '…', whose value is the cookie string itself.
func cookieArgument(content string) (string, bool) {
	args := curlArguments(content)
	for i := 0; i+1 < len(args); i++ {
		switch a := args[i]; a {
		case "-H", "--header":
			if v, ok := cutHeader(args[i+1], "Cookie"); ok {
				return v, true
			}
		case "-b", "--cookie":
			// curl reads a value holding no "=" as the name of a cookie jar to load. A
			// filename is not a cookie, and taking it as one sends the store the path.
			if strings.Contains(args[i+1], "=") {
				return strings.TrimSpace(args[i+1]), true
			}
		}
	}
	return "", false
}

func cutHeader(arg, name string) (string, bool) {
	if len(arg) <= len(name) || !strings.EqualFold(arg[:len(name)], name) || arg[len(name)] != ':' {
		return "", false
	}
	return strings.TrimSpace(arg[len(name)+1:]), true
}

// Quoting states for curlArguments, named for the shell construct each one is inside.
const (
	bare = iota
	singleQuoted
	doubleQuoted
	ansiCQuoted
	cmdQuoted
)

// curlArguments splits a pasted curl command into its arguments, undoing the quoting
// of the shell it was copied for.
//
// DevTools writes the command for that shell, so an argument arrives in one of four
// spellings: POSIX single quotes; ANSI-C $'…', which Firefox switches to when a value
// holds a "'", a "!" or a byte outside printable ASCII; plain double quotes; and the
// Windows cmd form, which escapes a backslash and a quote for the program's argument
// parser, prefixes ^ to every byte cmd would act on, and wraps the result in ^". A
// Cookie value holds quotes of either kind (Shopify's _consentik_cookie is JSON), so
// matching one quote style with a pattern either misses the argument or stops at the
// first escaped quote inside it.
//
// Nothing is unescaped in a context that does not escape: inside single quotes every
// byte is literal, so a cookie value holding a ^ or a \ survives as itself.
func curlArguments(content string) []string {
	var (
		args    []string
		cur     strings.Builder
		started bool
		state   = bare
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(content); i++ {
		c := content[i]
		next := byte(0)
		if i+1 < len(content) {
			next = content[i+1]
		}
		switch state {
		case singleQuoted:
			if c == '\'' {
				state = bare
				continue
			}
			cur.WriteByte(c)
		case ansiCQuoted:
			switch {
			case c == '\'':
				state = bare
			case c == '\\' && next != 0:
				decoded, n := ansiCEscape(content, i+1)
				i += n
				cur.Write(decoded)
			default:
				cur.WriteByte(c)
			}
		case doubleQuoted:
			switch {
			case c == '"':
				state = bare
			case c == '\\' && next != 0:
				i++
				cur.WriteByte(next)
			default:
				cur.WriteByte(c)
			}
		case cmdQuoted:
			i = cmdQuotedByte(content, i, &cur, &state)
		default:
			switch {
			case c == ' ' || c == '\t' || c == '\n' || c == '\r':
				flush()
			case c == '\'':
				state, started = singleQuoted, true
			case c == '"':
				state, started = doubleQuoted, true
			case c == '$' && next == '\'':
				state, started = ansiCQuoted, true
				i++
			case c == '^' && next == '"':
				state, started = cmdQuoted, true
				i++
			case c == '^' && next == '^':
				// cmd escapes a literal caret by doubling it.
				i++
				cur.WriteByte('^')
				started = true
			case c == '^' || c == '`':
				// A line continuation (cmd, PowerShell), or a prefix on the byte after
				// it, which keeps its normal meaning. Dropping the marker covers both.
			case c == '\\':
				if next == '\n' || next == '\r' {
					continue
				}
				if next != 0 {
					i++
					cur.WriteByte(next)
					started = true
				}
			default:
				cur.WriteByte(c)
				started = true
			}
		}
	}
	flush()
	return args
}

// cmdQuotedByte consumes what starts at content[i] inside a ^"…^" argument and returns
// the index of the last byte it consumed.
//
// The argument passes through two parsers. cmd goes first: the opening ^" escapes the
// quote, so cmd never counts itself as inside one and strips every caret through to the
// end, ^X becoming X. The program's own argument parser then reads what is left, where
// a quote preceded by an odd run of backslashes is a literal quote, the run halving, and
// an unescaped quote ends the argument. Undoing only the caret layer leaves a \ in front
// of every quote the value holds; undoing them in the other order reads the ^" an
// escaped quote arrives as (^\^") for the end of the argument.
func cmdQuotedByte(content string, i int, cur *strings.Builder, state *int) int {
	b, i := cmdByte(content, i)
	switch b {
	case '"':
		*state = bare
	case '\\':
		run := 1
		for {
			nb, ni := cmdByte(content, i+1)
			if ni >= len(content) || nb != '\\' {
				break
			}
			run, i = run+1, ni
		}
		nb, ni := cmdByte(content, i+1)
		if ni < len(content) && nb == '"' {
			cur.WriteString(strings.Repeat("\\", run/2))
			if run%2 == 1 {
				cur.WriteByte('"')
				i = ni
			}
			return i
		}
		cur.WriteString(strings.Repeat("\\", run))
	default:
		cur.WriteByte(b)
	}
	return i
}

// cmdByte is the byte cmd hands on for content[i], and the index of the last byte it
// read: a caret escapes the byte after it.
func cmdByte(content string, i int) (byte, int) {
	if i >= len(content) {
		return 0, i
	}
	if content[i] == '^' && i+1 < len(content) {
		return content[i+1], i + 1
	}
	return content[i], i
}

// ansiCSingle is the one-letter half of bash's $'…' table, plus the punctuation escapes.
var ansiCSingle = map[byte]byte{
	'a': 0x07, 'b': 0x08, 'e': 0x1b, 'E': 0x1b, 'f': 0x0c,
	'n': '\n', 'r': '\r', 't': '\t', 'v': 0x0b,
	'\\': '\\', '\'': '\'', '"': '"', '?': '?',
}

// ansiCEscape decodes the escape sequence starting at i, the byte after a backslash
// inside $'…', returning the bytes it stands for and how many bytes past the backslash
// it consumed.
//
// This is not the double-quoted rule of dropping the backslash and keeping the next
// byte. Firefox writes "!" as \041, a byte under 256 as \xNN and anything above as
// \uNNNN, and "!" is the first octet RFC 6265 admits in a cookie value: the
// double-quoted rule turns it into the three characters 041, and the store reads the
// mangled value as no session at all.
func ansiCEscape(src string, i int) ([]byte, int) {
	c := src[i]
	if b, ok := ansiCSingle[c]; ok {
		return []byte{b}, 1
	}
	switch c {
	case 'x':
		if v, n := digitRun(src, i+1, 16, 2); n > 0 {
			return []byte{byte(v)}, 1 + n
		}
	case 'u':
		if v, n := digitRun(src, i+1, 16, 4); n > 0 {
			return utf8.AppendRune(nil, rune(v)), 1 + n
		}
	case 'U':
		if v, n := digitRun(src, i+1, 16, 8); n > 0 {
			return utf8.AppendRune(nil, rune(v)), 1 + n
		}
	case '0', '1', '2', '3', '4', '5', '6', '7':
		// Octal has no marker, so the run starts at the digit itself.
		if v, n := digitRun(src, i, 8, 3); n > 0 {
			return []byte{byte(v)}, n
		}
	}
	// An escape bash does not recognise keeps the byte and drops the backslash.
	return []byte{c}, 1
}

// digitRun reads up to max digits in the given base starting at i, returning the value
// and how many digits it took. A run of zero digits means the sequence was not one.
func digitRun(src string, i, base, max int) (int, int) {
	v, n := 0, 0
	for n < max && i+n < len(src) {
		d := digitValue(src[i+n], base)
		if d < 0 {
			break
		}
		v = v*base + d
		n++
	}
	return v, n
}

func digitValue(c byte, base int) int {
	var v int
	switch {
	case c >= '0' && c <= '9':
		v = int(c - '0')
	case c >= 'a' && c <= 'f':
		v = int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		v = int(c-'A') + 10
	default:
		return -1
	}
	if v >= base {
		return -1
	}
	return v
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
	if _, ok := cookieArgument(content); ok {
		return true
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A Windows paste names curl.exe, and the cmd form puts a caret against the
		// first argument's quote, so neither the program name nor the separator after it
		// can be matched as a literal prefix.
		first := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '^' || r == '"' || r == '\''
		})
		if len(first) == 0 {
			return false
		}
		name := strings.ToLower(first[0])
		return name == "curl" || name == "curl.exe"
	}
	return false
}

// browserNames are the Gecko browsers this tool can read cookies from. Zen is a
// Firefox fork, so its cookies.sqlite reads identically.
var browserNames = []string{"firefox", "zen"}

// browserBases maps a session source name to its Gecko profile base dirs for the
// platform in hand. Releases ship macOS and Windows binaries, so the Linux paths alone
// would leave the zero-paste default broken out of the box on two of the three.
//
// Every base is searched and the profiles found under all of them are ranked together,
// so listing one that does not exist costs nothing. On Linux a browser is as likely to
// be sandboxed as native: Ubuntu has shipped Firefox as a snap since 22.04, and a
// flatpak keeps its home under .var/app. Without those, the documented default fails
// with "no such file or directory" for a browser that is installed and logged in. The
// Linux and macOS paths hang off home because the browsers hardcode them there.
//
// The Windows ones do not. %APPDATA% and %LOCALAPPDATA% are known folders, and Folder
// Redirection, ordinary on a domain-joined machine, moves them off the profile
// entirely, so they are taken from the variables and rebuilt under home only when one
// is unset. The Microsoft Store build of Firefox is a packaged app whose %APPDATA%
// writes are redirected into its package container, so it has nothing under
// %APPDATA%\Mozilla\Firefox; its base is a pattern (see expandBases) because the
// container is named for a publisher hash Mozilla documents nowhere.
func browserBases(goos, name, home, appData, localAppData string) []string {
	switch goos {
	case "darwin":
		switch name {
		case "firefox":
			return []string{filepath.Join(home, "Library", "Application Support", "Firefox")}
		case "zen":
			return []string{filepath.Join(home, "Library", "Application Support", "zen")}
		}
	case "windows":
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		if localAppData == "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
		switch name {
		case "firefox":
			return []string{
				filepath.Join(appData, "Mozilla", "Firefox"),
				filepath.Join(localAppData, "Packages", "Mozilla.Firefox_*", "LocalCache", "Roaming", "Mozilla", "Firefox"),
			}
		case "zen":
			return []string{filepath.Join(appData, "zen")}
		}
	default:
		switch name {
		case "firefox":
			return []string{
				filepath.Join(home, ".mozilla", "firefox"),
				filepath.Join(home, "snap", "firefox", "common", ".mozilla", "firefox"),
				filepath.Join(home, ".var", "app", "org.mozilla.firefox", ".mozilla", "firefox"),
			}
		case "zen":
			return []string{
				filepath.Join(home, ".config", "zen"),
				filepath.Join(home, ".zen"),
				filepath.Join(home, ".var", "app", "app.zen_browser.zen", ".zen"),
			}
		}
	}
	return nil
}

// expandBases replaces any base that is a pattern with the directories it matches. A
// pattern matching nothing contributes nothing, which is what a missing directory
// already does.
func expandBases(bases []string) []string {
	var out []string
	for _, b := range bases {
		if !strings.ContainsRune(b, '*') {
			out = append(out, b)
			continue
		}
		matches, err := filepath.Glob(b)
		if err != nil {
			continue
		}
		out = append(out, matches...)
	}
	return out
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
		// No shell expands an environment value, so a ~ written in a systemd unit, a
		// direnv file or a quoted export arrives literally. Every other path input this
		// tool takes is expanded; leaving this one out made the documented escape hatch
		// for an unrecognized profile layout fail with the path the reader can see
		// exists, and nothing saying the ~ was taken at face value.
		return geckoCookieHeader(filepath.Join(config.ExpandHome(p), "cookies.sqlite"))
	}
	if !knownBrowser(name) {
		return "", fmt.Errorf("unknown browser %q (use %s, or a cookies file path)", name, strings.Join(browserNames, ", "))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	bases := browserBases(runtime.GOOS, name, home, os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"))
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
	for _, base := range expandBases(bases) {
		found, err := geckoCandidates(base)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cands = append(cands, found...)
	}
	if len(cands) == 0 {
		// errors.Join is nil for an empty slice, so returning it alone would hand back an
		// empty header and no error the moment a base is ever skipped rather than
		// reported. main puts that straight in the Cookie header, the store answers with a
		// logged-out page, and the run blames an expired session for a profile it never
		// found. Say what actually happened instead.
		if err := errors.Join(errs...); err != nil {
			return "", err
		}
		return "", fmt.Errorf("no %s profile found under %s (set SYNTY_BROWSER_PROFILE)", name, strings.Join(bases, ", "))
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
	// Within one cookie jar, scan in increasing order of specificity, so the last write
	// for a given name is the most specific record that set it: a subdomain first, then
	// the domain-wide ".syntystore.com", then the apex itself. Ordering by host alone
	// would decide that alphabetically: every subdomain sorting after "syntystore" (www,
	// for one) would beat the apex and send the wrong value, which arrives as an expired
	// session against cookies the user just refreshed.
	//
	// Host is not unique: moz_cookies keys on (name, host, path, originAttributes), so a
	// name set at two paths gives two rows that tie on host. SQLite's sorter is not
	// documented as stable, so without the rest of this the winner is whichever row
	// happened to arrive last. path length is the order RFC 6265 has a browser send them
	// in, and lastAccessed then picks the session actually in use over one left behind by
	// an earlier login; id is there only to make the order total.
	rows, err := db.Query(
		`SELECT originAttributes, name, value, COALESCE(lastAccessed, 0) FROM moz_cookies
		 WHERE host LIKE ? OR host = ?
		 ORDER BY CASE host WHEN ? THEN 2 WHEN ? THEN 1 ELSE 0 END,
		          LENGTH(path), lastAccessed, id`,
		"%."+cookieHost, cookieHost, cookieHost, "."+cookieHost)
	if err != nil {
		return "", fmt.Errorf("query moz_cookies: %w", err)
	}
	defer rows.Close()
	jars := map[string]*cookieJar{}
	for rows.Next() {
		var attrs, name, value string
		var lastAccessed int64
		if err := rows.Scan(&attrs, &name, &value, &lastAccessed); err != nil {
			return "", err
		}
		if privateBrowsing(attrs) {
			continue
		}
		jar := jars[attrs]
		if jar == nil {
			jar = &cookieJar{attrs: attrs, pairs: map[string]string{}}
			jars[attrs] = jar
		}
		jar.pairs[name] = value
		jar.lastAccessed = max(jar.lastAccessed, lastAccessed)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return joinCookies(mergeJars(jars))
}

// cookieJar is the syntystore.com cookies one originAttributes value holds.
type cookieJar struct {
	attrs        string
	pairs        map[string]string
	lastAccessed int64
}

// mergeJars builds one Cookie header's worth of pairs out of several cookie jars, taking
// each name from the highest-ranked jar that holds it.
//
// Multi-Account Containers gives a container tab its own jar, so one profile can hold two
// syntystore.com sessions for two accounts. Ranking every row together by recency lets a
// container left open against the other account outrank the session the rest of the
// browser uses, or supply half the header. The default jar (an empty originAttributes)
// decides every name it holds; the other jars, most recently used first, supply only the
// names it lacks. That keeps a sign-in made only inside a container resolving, and one
// made under first-party isolation, which gives every cookie a non-empty originAttributes.
func mergeJars(jars map[string]*cookieJar) map[string]string {
	order := make([]*cookieJar, 0, len(jars))
	for _, j := range jars {
		order = append(order, j)
	}
	sort.Slice(order, func(i, k int) bool {
		a, b := order[i], order[k]
		if (a.attrs == "") != (b.attrs == "") {
			return a.attrs == ""
		}
		if a.lastAccessed != b.lastAccessed {
			return a.lastAccessed > b.lastAccessed
		}
		return a.attrs < b.attrs
	})
	pairs := map[string]string{}
	for _, j := range order {
		for name, value := range j.pairs {
			if _, have := pairs[name]; !have {
				pairs[name] = value
			}
		}
	}
	return pairs
}

// privateBrowsing reports whether an originAttributes value names a private window's
// jar. Those cookies are meant to die with the window, so one that reached the disk is
// not a session the user chose to keep.
func privateBrowsing(attrs string) bool {
	for _, kv := range strings.Split(strings.TrimPrefix(attrs, "^"), "&") {
		if v, ok := strings.CutPrefix(kv, "privateBrowsingId="); ok && v != "0" {
			return true
		}
	}
	return false
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
