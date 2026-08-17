package fixtures_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testdataDir is the committed portal fixtures, relative to this package dir.
const testdataDir = "../../testdata/portal"

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

func readFixtures(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(testdataDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("no fixtures found")
	}
	return out
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
