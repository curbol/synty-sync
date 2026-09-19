package fixtures_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testdataDir is the committed fixture tree, relative to this package dir. The whole
// tree is walked rather than one directory of one extension, so a capture committed
// in a new format or a new subdirectory is guarded too.
const testdataDir = "../../testdata"

// These guards never reference real PII. They assert that any PII-shaped value in
// committed testdata is one of the synthetic placeholders, so a missed scrub of a
// real email, phone, customer id, or name fails the build instead of leaking into
// git history. Checks are context-targeted (not blanket digit-length scans, which
// would trip on Shopify's own ids/timestamps). Order ids are retained by design as
// the lockfile identity anchor, so they are deliberately not checked.
// sep matches a URL path separator in any of the spellings a captured page carries:
// plain, percent-encoded in a query string, or backslash-escaped inside inline JSON.
const sep = `(?:/|%2[Ff]|\\/)`

var (
	// No capture group: the coverage check below reads a pattern's submatch when it
	// has one and its whole match otherwise, and the alternation here is a spelling
	// of the separator, not the value.
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+(?:@|%40)[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// Key names are matched case-insensitively and allow either quote style, so a
	// capture that spells the field differently still gets checked.
	phoneRe = regexp.MustCompile(`(?i)["']phone["']\s*:\s*["']([^"']*)["']`)
	// Every context the account holder's name turns up in. Like custIDRes this is a
	// class rather than one literal: the key is quoted, bare, snake or camel, and the
	// assignment is ":" or "=" depending on whether it sits in JSON or in a script
	// blob. A quoted-key-and-colon pattern alone matched 2 of the 99 occurrences.
	//
	// The leading non-word character is what keeps a key that is merely a suffix of a
	// longer identifier out: "read_customer_name" is an OAuth scope and
	// "checkout_comment_customer_name" a setting whose value is "last_initial", and
	// neither holds a name.
	nameRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:\A|[^0-9a-z_])["']?(?:customer_?name|first_?name|last_?name)["']?\s*[:=]\s*["']([^"']*)["']`),
	}
	// Every context the customer id turns up in; an order id never does. Which contexts
	// those are is not a judgement call: TestCommittedPIIIsFakeAndFullyCovered checks
	// this set against every occurrence in the committed captures, so a shape nothing
	// here matches fails the build rather than passing quietly. A captured page carries
	// the URL forms percent-encoded inside query strings and backslash-escaped inside
	// inline JSON as well as plain, so the separator is a class: anchoring on a literal
	// "/" would leave those two spellings unchecked. The script blobs matter as much as
	// the URLs: a page reached by a URL that does not carry the id has these as its only
	// occurrences.
	custIDRes = []*regexp.Regexp{
		regexp.MustCompile(`logged_in_customer_id(?:=|%3D)(\d+)`),
		regexp.MustCompile(sep + `apps` + sep + `downloads` + sep + `customers` + sep + `(\d+)`),
		regexp.MustCompile(sep + `apps` + sep + `downloads` + sep + `orders` + sep + `(\d+)`),
		regexp.MustCompile(`"id":"(\d+)","email"`),
		// customerId / customer_id / cid, quoted or bare, assigned with ":" or "=":
		// Shopify's __st blob uses "cid", StoreCreditInit and _RSConfig assign bare
		// properties, and one inline object uses an unquoted key with a colon.
		regexp.MustCompile(`(?i)["']?c(?:ustomer_?)?id["']?\s*[:=]\s*["']?(\d+)`),
	}
	okEmailDom = "example.com"
	okPhone    = "+10000000000"
	okCustomer = "1000000000001"
	// The captures land as the full name (customer_name / customerName) and as its two
	// halves (firstName / lastName), so all three spellings are legitimate.
	okNames = map[string]bool{"Test User": true, "Test": true, "User": true, "": true}
)

// readFixtures returns every committed fixture keyed by its tree-relative path. The
// path is part of the value scanned, not just the label: a capture named after the
// URL it came from carries the customer id or email in its filename, which commits
// to git exactly as the bytes do.
func readFixtures(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(testdataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(testdataDir, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no fixtures found")
	}
	return out
}

// The guard is only worth having if its patterns actually fire. A synthetic leak of
// each shape must be caught, so the checks cannot quietly decay into no-ops.
func TestGuardCatchesASyntheticLeak(t *testing.T) {
	cases := map[string]string{
		"email":             `<a href="mailto:real.person@leaked.net">`,
		"url-encoded email": `?email=real.person%40leaked.net&order_id=1`,
		"phone":             `{"phone":"+15551234567"}`,
		"customer id":       `/apps/downloads/customers/9988776655443/orders/1`,
		// A captured page carries these URLs in all three spellings, so a leak that
		// appears only in an encoded one still has to be caught.
		"customer id percent-encoded": `%2Fapps%2Fdownloads%2Forders%2F9988776655443`,
		"customer id json-escaped":    `\/apps\/downloads\/orders\/9988776655443`,
		"customer id json":            `"customer_id": "9988776655443"`,
		// The script-blob spellings. A page reached by a URL that does not carry the id
		// has only these, so a pattern set that covers the URL forms alone would let
		// such a capture through with the build green.
		"customer id shopify __st":    `{"a":1,"cid":9988776655443};`,
		"customer id bare assignment": `window.StoreCreditInit.customer_id = '9988776655443';`,
		"customer id unquoted key":    `{ email: 'x@example.com', customer_id: '9988776655443', }`,
		"customer id camel property":  `_RSConfig.customerId = 9988776655443;`,
		// The name spellings, one per context the captures actually carry. A pattern
		// set covering only the first of these matched 2 of 99 occurrences.
		"name json":             `{"first_name":"Realperson"}`,
		"name camel json":       `"customerName": "Real Person",`,
		"name bare property":    `window.StoreCreditInit.customer_name = 'Real Person';`,
		"name unquoted key":     `{ customer_name: 'Real Person', }`,
		"name camel bare":       `_RSConfig.customerName = "Real Person";`,
		"name split bare":       `_RSConfig.firstName = "Realperson";`,
		"name split camel json": `"firstName":"Realperson","lastName":"Surname"`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			var hits int
			for _, m := range emailRe.FindAllString(content, -1) {
				if !strings.Contains(m, okEmailDom) {
					hits++
				}
			}
			for _, m := range phoneRe.FindAllStringSubmatch(content, -1) {
				if m[1] != "" && m[1] != okPhone {
					hits++
				}
			}
			for _, re := range custIDRes {
				for _, m := range re.FindAllStringSubmatch(content, -1) {
					if m[1] != okCustomer {
						hits++
					}
				}
			}
			for _, re := range nameRes {
				for _, m := range re.FindAllStringSubmatch(content, -1) {
					if !okNames[m[1]] {
						hits++
					}
				}
			}
			if hits == 0 {
				t.Errorf("no guard pattern caught %q", content)
			}
		})
	}
}

// longDigitRun is a name-only rule. The content checks are context-targeted (a
// blanket digit scan would trip on Shopify's own ids and timestamps), but a fixture
// filename is ours to choose and legitimately carries only short counters like
// "item_1" or "library_p5", so any long digit run in one is an unscrubbed id.
var longDigitRun = regexp.MustCompile(`\d{7,}`)

// A capture of /apps/downloads/orders/{customerId} is naturally saved under a name
// carrying that id, and a filename commits to git exactly as its bytes do. No
// content pattern can catch it: they all anchor on a URL path or a JSON key that a
// filename does not have.
func TestFixtureNamesCarryNoPII(t *testing.T) {
	for name := range readFixtures(t) {
		// Every run, not the first: a name carrying the scrubbed id followed by a real
		// one would otherwise pass on the first match.
		for _, m := range longDigitRun.FindAllString(name, -1) {
			if m != okCustomer {
				t.Errorf("fixture name %q carries a long digit run %q; scrub the filename too", name, m)
			}
		}
		for _, m := range emailRe.FindAllString(name, -1) {
			if !strings.Contains(m, okEmailDom) {
				t.Errorf("fixture name %q carries an email %q", name, m)
			}
		}
	}
}

func TestNoEmailExceptFake(t *testing.T) {
	for name, content := range readFixtures(t) {
		for _, m := range emailRe.FindAllString(content, -1) {
			if !strings.Contains(m, okEmailDom) {
				t.Errorf("%s: non-fixture email leaked: %q", name, m)
			}
		}
	}
}

// TestScrubActuallyRan guards against an empty/no-op scrub silently passing the
// other checks: the fake customer id must appear somewhere in the corpus.
func TestScrubActuallyRan(t *testing.T) {
	joined := strings.Join(maps(readFixtures(t)), "\n")
	if !strings.Contains(joined, okCustomer) {
		t.Fatalf("fake customer id %q not present; was the scrub run?", okCustomer)
	}
}

func maps(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// piiClass is one kind of account PII: the patterns that check it, and the scans that
// find where its placeholders actually landed in the corpus.
type piiClass struct {
	kind string
	// scans locate each placeholder occurrence; submatch 1 is the value. They are
	// patterns rather than literals because a placeholder is not always distinctive on
	// its own: "Test" is also 880 occurrences of ordinary page text, and is only a
	// value where a quote says so. One class holds every scan that shares a pattern
	// set, so the patterns are run over each fixture once rather than once per scan.
	scans    []*regexp.Regexp
	patterns []*regexp.Regexp
	// ok reports whether a value the patterns captured is one of the synthetic
	// placeholders rather than something a scrub missed.
	ok func(string) bool
}

// quoted matches a placeholder only where it sits between quotes as a value.
func quoted(value string) *regexp.Regexp {
	return regexp.MustCompile(`["'](` + regexp.QuoteMeta(value) + `)["']`)
}

// bare matches a placeholder distinctive enough that any occurrence of it is PII.
func bare(value string) *regexp.Regexp {
	return regexp.MustCompile(`(` + regexp.QuoteMeta(value) + `)`)
}

// piiClasses is what TestCommittedPIIIsFakeAndFullyCovered walks. Email is deliberately
// absent: emailRe matches the shape of an address anywhere, with no key context to
// miss, so asserting its own matches are covered says nothing.
func piiClasses() []piiClass {
	is := func(want string) func(string) bool {
		return func(got string) bool { return got == want }
	}
	return []piiClass{
		{"customer id", []*regexp.Regexp{bare(okCustomer)}, custIDRes, is(okCustomer)},
		{
			"phone", []*regexp.Regexp{bare(okPhone)}, []*regexp.Regexp{phoneRe},
			// An empty capture is a field present with no number in it.
			func(got string) bool { return got == "" || got == okPhone },
		},
		// The full name is distinctive enough to scan for bare. Its two halves are not,
		// so those are scanned only where a quote makes them a value.
		{
			"name", []*regexp.Regexp{bare("Test User"), quoted("Test"), quoted("User")}, nameRes,
			func(got string) bool { return okNames[got] },
		},
	}
}

// found is one value a pattern set captured, and where it sits.
type found struct {
	value    string
	start    int
	end      int
	byModule string // the pattern that claimed it, for the failure message
}

// claimed returns every value a pattern set says it checks: the captured group where
// a pattern has one, the whole match where it does not. Both halves of the guard read
// this, so the patterns run over each fixture once.
func claimed(patterns []*regexp.Regexp, body string) []found {
	var out []found
	for _, re := range patterns {
		for _, m := range re.FindAllStringSubmatchIndex(body, -1) {
			lo, hi := m[0], m[1]
			if re.NumSubexp() > 0 {
				lo, hi = m[2], m[3]
			}
			out = append(out, found{value: body[lo:hi], start: lo, end: hi, byModule: re.String()})
		}
	}
	return out
}

// The other half of "the guard actually works". TestGuardCatchesASyntheticLeak
// proves each pattern fires; this proves the set covers everywhere the value actually
// appears. Without it a pattern list is a guess about the corpus, and it was wrong
// twice: four script-blob spellings accounted for 55 of 414 customer-id occurrences,
// and a quoted-key-and-colon name pattern covered 2 of 99 name occurrences. That is
// only harmless while some *other* occurrence in the same file is covered; a capture
// whose value appears in the uncovered shapes alone would commit real PII with every
// check green.
//
// This runs against the placeholder, not against real PII: the scrub map replaces
// every occurrence at once, so where the placeholder lands is exactly where a missed
// scrub would have left the real value.
func TestCommittedPIIIsFakeAndFullyCovered(t *testing.T) {
	fixtures := readFixtures(t)
	for _, class := range piiClasses() {
		t.Run(class.kind, func(t *testing.T) {
			hits := map[string]int{}
			for name, body := range fixtures {
				captures := claimed(class.patterns, body)
				// Half one: nothing the patterns see is a real value.
				for _, c := range captures {
					if !class.ok(c.value) {
						t.Errorf("%s: non-fixture %s in %q context: %q", name, class.kind, c.byModule, c.value)
					}
				}
				// Half two: the patterns see every place a placeholder landed.
				for _, scan := range class.scans {
					for _, m := range scan.FindAllStringSubmatchIndex(body, -1) {
						hits[scan.String()]++
						at := m[2]
						seen := false
						for _, c := range captures {
							if c.start <= at && at < c.end {
								seen = true
								break
							}
						}
						if seen {
							continue
						}
						lo := max(at-60, 0)
						hi := min(m[3]+20, len(body))
						t.Errorf("%s: the %s at offset %d is in a context no pattern checks; "+
							"real PII here would not fail the build. Context: …%s…",
							name, class.kind, at, strings.Join(strings.Fields(body[lo:hi]), " "))
					}
				}
			}
			// A scan that finds nothing proves nothing, and would hide a rename of the
			// placeholder it looks for.
			for _, scan := range class.scans {
				if hits[scan.String()] == 0 {
					t.Errorf("no %s placeholder found by %s; the scan no longer matches the corpus", class.kind, scan)
				}
			}
		})
	}
}

// The raw captures and the real-to-fake scrub map are account PII, and the tooling's
// own defaults write them here (cmd/scrubfixtures -raw / -map). A per-clone
// .git/info/exclude does not travel, so the committed .gitignore is what keeps a
// fresh clone from committing them; the guards above only ever look at testdata.
func TestRawCaptureDirIsGitIgnored(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	for _, want := range []string{".longrun/"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf(".gitignore does not cover %q; a capture run on a fresh clone would commit real PII", want)
		}
	}
}
