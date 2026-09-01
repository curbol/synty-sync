package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run is the only place the scrub is applied to a fixture's *filename*, and it is the
// only place the .html filter lives. internal/fixtures tests the string transform, so
// dropping the sm.Scrub around e.Name() here — or widening the filter — leaves the
// whole suite green until someone regenerates fixtures and commits a name carrying the
// customer id.
func TestRunScrubsContentAndFilenamesAndSkipsNonHTML(t *testing.T) {
	rawDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "portal")
	mapPath := filepath.Join(t.TempDir(), "scrub-map.json")

	if err := os.WriteFile(mapPath, []byte(
		`{"replacements":[["9988776655443","1000000000001"],["real.person@leaked.net","test.user@example.com"]]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A capture named after the URL it came from, so its name carries the customer id.
	if err := os.WriteFile(filepath.Join(rawDir, "orders_9988776655443.html"),
		[]byte(`<a href="mailto:real.person@leaked.net">/apps/downloads/orders/9988776655443</a>`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not .html: a raw capture directory also holds notes and json the tool must skip.
	if err := os.WriteFile(filepath.Join(rawDir, "notes.txt"), []byte("9988776655443"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run(rawDir, mapPath, outDir); err != nil {
		t.Fatalf("run: %v", err)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "orders_1000000000001.html" {
		t.Fatalf("output = %v, want just the scrubbed orders_1000000000001.html", names)
	}
	got, err := os.ReadFile(filepath.Join(outDir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "9988776655443") || strings.Contains(string(got), "real.person@leaked.net") {
		t.Errorf("content was not scrubbed: %s", got)
	}
}

// A missing scrub map has to stop the run rather than write the raw captures through
// unchanged: the map is git-excluded, so an absent one is the ordinary state of a
// fresh clone.
func TestRunRefusesWithoutAScrubMap(t *testing.T) {
	if err := run(t.TempDir(), filepath.Join(t.TempDir(), "absent.json"), t.TempDir()); err == nil {
		t.Error("run without a scrub map reported success")
	}
}
