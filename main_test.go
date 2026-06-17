package main

import (
	"strings"
	"testing"
)

func TestRunNoSubcommand(t *testing.T) {
	if err := run(nil); err == nil {
		t.Error("want error for no subcommand")
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	err := run([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("got %v, want unknown-subcommand error", err)
	}
}

func TestRunHelp(t *testing.T) {
	for _, a := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		if err := run(a); err != nil {
			t.Errorf("%v: %v", a, err)
		}
	}
}

func TestRunListEmpty(t *testing.T) {
	// list reads only the lockfile (none here -> empty), no network/session needed.
	if err := run([]string{"list", "-config", t.TempDir()}); err != nil {
		t.Errorf("list: %v", err)
	}
}
