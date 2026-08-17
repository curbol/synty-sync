// Package fixtures scrubs captured Synty portal pages into PII-free test data.
//
// The real-to-fake replacement map is account PII, so it is never committed: it
// lives in a git-excluded file (default .longrun/scrub-map.json) that the scrub
// command reads. This package holds only the logic, and the committed testdata
// and the PII guard test reference only synthetic values.
package fixtures

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ScrubMap is a list of literal replacements. The longest match at a position
// always wins, whatever order the entries are written in: strings.Replacer resolves
// overlaps by argument order, so a shorter entry listed first (the bare local part
// before the full email) would otherwise scrub its prefix and leave the rest of a
// real value in the fixtures. Entries of equal length keep their authored order.
type ScrubMap struct {
	Replacements [][2]string `json:"replacements"`
}

// LoadScrubMap reads an ordered replacement map from a JSON file.
func LoadScrubMap(path string) (ScrubMap, error) {
	var m ScrubMap
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read scrub map %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("parse scrub map %s: %w", path, err)
	}
	for i, r := range m.Replacements {
		if r[0] == "" {
			return m, fmt.Errorf("scrub map entry %d has an empty match", i)
		}
	}
	return m, nil
}

// Replacer builds a strings.Replacer that tries the longest match first.
// Replacement is single-pass and non-overlapping, so a fake value that contains a
// real fragment is never re-scrubbed.
func (m ScrubMap) Replacer() *strings.Replacer {
	ordered := make([][2]string, len(m.Replacements))
	copy(ordered, m.Replacements)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(ordered[i][0]) > len(ordered[j][0])
	})
	pairs := make([]string, 0, len(ordered)*2)
	for _, r := range ordered {
		pairs = append(pairs, r[0], r[1])
	}
	return strings.NewReplacer(pairs...)
}

// Scrub returns content with every mapped real value replaced by its fake.
func (m ScrubMap) Scrub(content string) string {
	return m.Replacer().Replace(content)
}
