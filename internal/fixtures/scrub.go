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
	"strings"
)

// ScrubMap is an ordered list of literal replacements. Order matters: longer,
// more specific strings come first so a substring replacement cannot pre-empt a
// fuller match (e.g. the full email before the bare local part).
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

// Replacer builds a strings.Replacer that applies the map in order. Replacement
// is single-pass and non-overlapping, so a fake value that contains a real
// fragment is never re-scrubbed.
func (m ScrubMap) Replacer() *strings.Replacer {
	pairs := make([]string, 0, len(m.Replacements)*2)
	for _, r := range m.Replacements {
		pairs = append(pairs, r[0], r[1])
	}
	return strings.NewReplacer(pairs...)
}

// Scrub returns content with every mapped real value replaced by its fake.
func (m ScrubMap) Scrub(content string) string {
	return m.Replacer().Replace(content)
}
