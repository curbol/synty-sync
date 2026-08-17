package main

import (
	"os"
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

func TestResolveTagsPath(t *testing.T) {
	// An explicit --tags wins outright.
	if got := resolveTagsPath("/custom/tags.toml", "/some/synty-sync.toml"); got != "/custom/tags.toml" {
		t.Errorf("explicit --tags = %q", got)
	}
	// Otherwise it derives from --manifest.
	if got := resolveTagsPath("", filepath.Join("proj", "synty-sync.toml")); got != filepath.Join("proj", "synty-sync.tags.toml") {
		t.Errorf("from --manifest = %q", got)
	}
	// With neither, and no manifest discoverable up from cwd, it is empty (disabled).
	t.Chdir(t.TempDir())
	if got := resolveTagsPath("", ""); got != "" {
		t.Errorf("no manifest neighborhood should disable tagging, got %q", got)
	}
	// A manifest up the tree is discovered and its sibling returned.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "synty-sync.toml"), []byte("variant_includes = []\n"), 0o644)
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o755)
	t.Chdir(sub)
	if got, want := resolveTagsPath("", ""), filepath.Join(dir, "synty-sync.tags.toml"); got != want {
		t.Errorf("discovered tags path = %q, want %q", got, want)
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
