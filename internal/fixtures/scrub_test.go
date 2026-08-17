package fixtures_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/synty-sync/internal/fixtures"
)

// The map is hand-maintained and git-excluded, so it cannot rely on the author
// ordering entries correctly: a shorter match listed first must not scrub the
// leading part of a longer one and leave the rest of a real value behind.
func TestScrubPrefersTheLongerMatch(t *testing.T) {
	m := fixtures.ScrubMap{Replacements: [][2]string{
		{"person", "user"},                          // shorter, listed first
		{"person@real.example", "user@example.com"}, // longer, would be pre-empted
		{"1000", "9999"},                            // prefix of the id below
		{"1000000000001", "1111111111111"},          //
	}}
	got := m.Scrub("mail person@real.example id 1000000000001")
	for _, leak := range []string{"@real.example", "000000001"} {
		if strings.Contains(got, leak) {
			t.Errorf("partial scrub left %q in %q", leak, got)
		}
	}
}

// An empty match would replace at every position, wrecking the fixture rather than
// scrubbing it, so the map is rejected at load.
func TestLoadScrubMapRejectsEmptyMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scrub-map.json")
	if err := os.WriteFile(path, []byte(`{"replacements":[["real","fake"],["","x"]]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtures.LoadScrubMap(path); err == nil {
		t.Error("expected an empty-match error")
	}
}

// The scrub pipeline rewrites file contents but the output name is copied from the
// raw capture. A capture saved as orders-<realid>-p1.html — the natural name for a
// capture of /apps/downloads/orders/{customerId} — would commit the id as a path,
// and the guard only ever reads contents.
func TestScrubAppliesToFilenames(t *testing.T) {
	m := fixtures.ScrubMap{Replacements: [][2]string{
		{"1234567890123", "1000000000001"},
		{"real.person@example.org", "test.user@example.com"},
	}}
	for _, tc := range []struct{ in, want string }{
		{"orders-1234567890123-p1.html", "orders-1000000000001-p1.html"},
		{"library-real.person@example.org.html", "library-test.user@example.com.html"},
		{"item_1.html", "item_1.html"},
	} {
		if got := m.Scrub(tc.in); got != tc.want {
			t.Errorf("Scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
