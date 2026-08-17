package main

import (
	"path/filepath"
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
	// list reads only the lockfile beside the manifest (none here -> empty), no
	// network/session needed. An explicit --manifest is honored without the file existing.
	tmp := t.TempDir()
	if err := run([]string{"list", "-config", tmp, "-manifest", filepath.Join(tmp, "synty-sync.toml")}); err != nil {
		t.Errorf("list: %v", err)
	}
}

func TestIsDryRun(t *testing.T) {
	cases := []struct {
		cmd  string
		flag bool
		want bool
	}{
		{"status", false, true}, // status is always dry, even without --dry-run
		{"status", true, true},
		{"sync", false, false}, // sync writes unless --dry-run
		{"sync", true, true},
	}
	for _, c := range cases {
		if got := isDryRun(c.cmd, c.flag); got != c.want {
			t.Errorf("isDryRun(%q, %v) = %v, want %v", c.cmd, c.flag, got, c.want)
		}
	}
}

func TestRunStatusNoManifest(t *testing.T) {
	// With no --manifest and nothing discoverable up from cwd, read commands error before
	// any network/session. t.Chdir isolates cwd to an empty tree.
	t.Chdir(t.TempDir())
	err := run([]string{"status", "-config", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no synty-sync.toml") {
		t.Errorf("status without a manifest: got %v, want no-manifest error", err)
	}
}
