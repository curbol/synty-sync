// Command scrubfixtures regenerates the committed, PII-free testdata from the
// git-excluded raw portal captures.
//
//	go run ./cmd/scrubfixtures \
//	  -raw ../../.longrun/fixtures-raw \
//	  -map ../../.longrun/scrub-map.json \
//	  -out testdata/portal
//
// Raw captures and the scrub map are never committed; only the scrubbed output is.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/curbol/synty-sync/internal/fixtures"
)

func main() {
	raw := flag.String("raw", "../../.longrun/fixtures-raw", "directory of raw captured .html pages")
	mapPath := flag.String("map", "../../.longrun/scrub-map.json", "git-excluded real->fake replacement map")
	out := flag.String("out", "testdata/portal", "output directory for scrubbed fixtures")
	flag.Parse()

	if err := run(*raw, *mapPath, *out); err != nil {
		fmt.Fprintln(os.Stderr, "scrubfixtures:", err)
		os.Exit(1)
	}
}

func run(rawDir, mapPath, outDir string) error {
	sm, err := fixtures.LoadScrubMap(mapPath)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return fmt.Errorf("read raw dir: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(rawDir, e.Name()))
		if err != nil {
			return err
		}
		scrubbed := sm.Scrub(string(content))
		if err := os.WriteFile(filepath.Join(outDir, e.Name()), []byte(scrubbed), 0o644); err != nil {
			return err
		}
		n++
	}
	fmt.Printf("scrubbed %d pages into %s\n", n, outDir)
	return nil
}
