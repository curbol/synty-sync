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
var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+(@|%40)[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// Key names are matched case-insensitively and allow either quote style, so a
	// capture that spells the field differently still gets checked.
	phoneRe    = regexp.MustCompile(`(?i)["']phone["']\s*:\s*["']([^"']*)["']`)
	nameJSONRe = regexp.MustCompile(`(?i)["'](?:first_?name|last_?name)["']\s*:\s*["']([^"']*)["']`)
	// Customer id appears in these contexts only; an order id never does. The
	// storefront also emits it as a bare JSON number, which no URL pattern covers.
	custIDRes = []*regexp.Regexp{
		regexp.MustCompile(`logged_in_customer_id(?:=|%3D)(\d+)`),
		regexp.MustCompile(`/apps/downloads/customers/(\d+)`),
		regexp.MustCompile(`/apps/downloads/orders/(\d+)`),
		regexp.MustCompile(`"id":"(\d+)","email"`),
		regexp.MustCompile(`(?i)["']customer_?id["']\s*:\s*["']?(\d+)`),
	}
	okEmailDom = "example.com"
	okPhone    = "+10000000000"
	okCustomer = "1000000000001"
	okNames    = map[string]bool{"Test": true, "User": true, "": true}
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
		"customer id json":  `"customer_id": "9988776655443"`,
		"name":              `{"first_name":"Realperson"}`,
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
			for _, m := range nameJSONRe.FindAllStringSubmatch(content, -1) {
				if !okNames[m[1]] {
					hits++
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
		if m := longDigitRun.FindString(name); m != "" && m != okCustomer {
			t.Errorf("fixture name %q carries a long digit run %q; scrub the filename too", name, m)
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

func TestNoPhoneExceptFake(t *testing.T) {
	for name, content := range readFixtures(t) {
		for _, m := range phoneRe.FindAllStringSubmatch(content, -1) {
			if m[1] != "" && m[1] != okPhone {
				t.Errorf("%s: non-fixture phone leaked: %q", name, m[1])
			}
		}
	}
}

func TestCustomerIdContextsAreFake(t *testing.T) {
	for name, content := range readFixtures(t) {
		for _, re := range custIDRes {
			for _, m := range re.FindAllStringSubmatch(content, -1) {
				if m[1] != okCustomer {
					t.Errorf("%s: non-fixture customer id in %q context: %q", name, re.String(), m[1])
				}
			}
		}
	}
}

func TestNamesAreFake(t *testing.T) {
	for name, content := range readFixtures(t) {
		for _, m := range nameJSONRe.FindAllStringSubmatch(content, -1) {
			if !okNames[m[1]] {
				t.Errorf("%s: non-fixture name leaked: %q", name, m[1])
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
