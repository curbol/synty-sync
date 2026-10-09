package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFromCookiesTxt(t *testing.T) {
	// notsyntystore.com is the case an unrelated host cannot cover: it fails any
	// spelling of the check, while a lookalike passes a suffix test that stops
	// requiring the dot, disclosing a third-party session cookie to Synty.
	content := "# Netscape HTTP Cookie File\n" +
		".syntystore.com\tTRUE\t/\tTRUE\t0\t_shopify_essential\tABC\n" +
		"syntystore.com\tFALSE\t/\tFALSE\t0\tlocalization\tUS\n" +
		".other.com\tTRUE\t/\tTRUE\t0\tjunk\tXX\n" +
		"notsyntystore.com\tTRUE\t/\tTRUE\t0\tlookalike\tLEAK\n"
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

// Chrome on Windows writes "Copy as cURL" with double-quoted -H arguments, so the
// single-quoted form every other fixture here uses is only half the shapes a pasted
// command arrives in.
func TestFromCurlWithDoubleQuotedHeaders(t *testing.T) {
	curl := `curl "https://syntystore.com/apps/downloads/orders/1" ^
  -H "Accept: text/html" ^
  -H "Cookie: localization=US; _shopify_essential=ABC" ^
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
	// notsyntystore.com is the case an unrelated host cannot cover: it fails any
	// spelling of the check, while a lookalike passes a LIKE pattern that stops
	// requiring the dot, disclosing a third-party session cookie to Synty.
	dbPath := newCookieDB(t, false,
		[3]string{".syntystore.com", "_shopify_essential", "ABC"},
		[3]string{"syntystore.com", "localization", "US"},
		[3]string{".other.com", "junk", "XX"},
		[3]string{"notsyntystore.com", "lookalike", "LEAK"},
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

// Every spelling a browser's "Copy as cURL" writes the jar in has to yield the same
// header, compared whole: a parser that leaves a stray caret, backslash or quote in the
// value, or stops at the first escaped quote, sends a cookie the store does not
// recognise and the run reports an expired session against a login that is fine. The
// value carries double quotes on purpose, since Shopify's _consentik_cookie holds JSON.
func TestFromCurlReadsEverySpellingOfTheJar(t *testing.T) {
	const cookie = `_shopify_essential=ABC; _consentik_cookie=[{"k":"v"}]; _shopify_s=END`
	// The double-quoted spelling escapes the value's own quotes, so it is written out.
	escaped := strings.ReplaceAll(cookie, `"`, `\"`)
	for _, tc := range []struct{ name, curl string }{
		{"single-quoted header", `curl 'https://x' -H 'Cookie: ` + cookie + `'`},
		{"lower-case header name", `curl 'https://x' -H 'cookie: ` + cookie + `'`},
		{"long header flag", `curl 'https://x' --header 'Cookie: ` + cookie + `'`},
		{"double-quoted header with escaped quotes", `curl "https://x" -H "Cookie: ` + escaped + `"`},
		// Firefox switches an argument holding a "'" to ANSI-C quoting.
		{"ansi-c header", `curl 'https://x' -H $'Cookie: ` + cookie + `'`},
		// The jar can arrive as curl's own cookie flag, whose value is the cookie string
		// with no "Cookie:" prefix to cut.
		{"cookie flag", `curl 'https://x' -b '` + cookie + `'`},
		{"cookie flag long", `curl 'https://x' --cookie '` + cookie + `'`},
		{"cookie flag double-quoted", `curl "https://x" -b "` + escaped + `"`},
		// The cmd form wraps every argument in ^" and prefixes ^ to each byte cmd would
		// otherwise act on, so a quote inside the value arrives as ^\^".
		{"cmd header", "curl.exe " + cmdQuote("https://x") + " ^\n  -H " + cmdQuote("Cookie: "+cookie) + " ^\n  --compressed\n"},
		{"cmd cookie flag", "curl " + cmdQuote("https://x") + " ^\n  -b " + cmdQuote(cookie) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromCurl(tc.curl)
			if err != nil {
				t.Fatal(err)
			}
			if got != cookie {
				t.Errorf("got  %q\nwant %q", got, cookie)
			}
			// FromFile has to recognise the paste as one before FromCurl ever sees it.
			p := filepath.Join(t.TempDir(), "session.curl")
			if err := os.WriteFile(p, []byte(tc.curl), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := FromFile(p); err != nil || got != cookie {
				t.Errorf("FromFile = %q, %v; want %q", got, err, cookie)
			}
		})
	}
}

// cmdQuote is the escaper Firefox and Chrome both use for "Copy as cURL (cmd)": a
// backslash and a quote are escaped for the program's own argument parser, then every
// byte cmd would act on gets a caret, and the whole is wrapped in ^".
func cmdQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	var b strings.Builder
	b.WriteString(`^"`)
	for _, r := range s {
		safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune(" \t_-:=+~/.',?;()*`", r)
		if !safe {
			b.WriteByte('^')
		}
		b.WriteRune(r)
	}
	b.WriteString(`^"`)
	return b.String()
}

// A backslash in front of a quote in a cmd-quoted value is doubled for the argument
// parser and every byte of the result given a caret, so the value arrives as ^\^\^\^".
// Undoing only one of the two layers leaves it a byte off or ends the argument there.
func TestFromCurlUndoesBothCmdLayersOnABackslash(t *testing.T) {
	const cookie = `b=x\"y; c=END`
	got, err := FromCurl("curl " + cmdQuote("https://x") + " -H " + cmdQuote("Cookie: "+cookie))
	if err != nil {
		t.Fatal(err)
	}
	if got != cookie {
		t.Errorf("got %q, want %q", got, cookie)
	}
}

// Firefox writes an argument holding a byte outside printable ASCII, a "!" or a "'" as
// $'…', then spells "!" as \041, a byte under 256 as \xNN and anything above as \uNNNN.
// Dropping the backslash and keeping the next byte, which is right inside double quotes,
// turns \041 into the three characters 041, and "!" is a legal cookie-value octet.
func TestFromCurlDecodesAnsiCEscapes(t *testing.T) {
	got, err := FromCurl(`curl 'https://x' -H $'Cookie: bang=x\041y; q=it\'s; e=caf\u00e9; h=\x41; bs=a\\b; t=\t'`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "bang=x!y; q=it's; e=café; h=A; bs=a\\b; t="; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// curl reads a -b value holding no "=" as the name of a cookie jar to load, not as
// cookies. Taking it as the header sends the store a file path; -B is --use-ascii, so
// its argument is not a jar either.
func TestFromCurlIgnoresACookieJarFileAndOtherFlags(t *testing.T) {
	for _, curl := range []string{
		`curl -b cookies.txt 'https://x'`,
		`curl -B 'a=1' 'https://x'`,
	} {
		if got, err := FromCurl(curl); err == nil {
			t.Errorf("FromCurl(%q) = %q, want an error", curl, got)
		}
	}
}
