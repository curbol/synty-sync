package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearSyntyEnv isolates a test from the maintainer's own environment. Load reads
// these, so anyone who actually uses the tool would otherwise see these tests fail
// on their own machine.
func clearSyntyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SYNTY_LIBRARY", "SYNTY_CUSTOMER_ID"} {
		t.Setenv(k, "")
	}
}

func TestDefaultsWhenNoConfig(t *testing.T) {
	clearSyntyEnv(t)
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Concurrency != 4 || c.SessionSource != "firefox" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	// Library default is XDG-derived, never a baked-in personal path.
	if c.LibraryPath == "" || strings.Contains(c.LibraryPath, "code/synty-assets") {
		t.Errorf("library path default = %q, want an XDG-derived path", c.LibraryPath)
	}
	if !strings.HasSuffix(c.LibraryPath, "synty-sync") {
		t.Errorf("library path = %q, want it to end in synty-sync", c.LibraryPath)
	}
}

func TestConfigFileThenEnv(t *testing.T) {
	clearSyntyEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
concurrency = 2
customer_id = "1234567890123"
library_path = "/from/file"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Concurrency != 2 || c.CustomerID != "1234567890123" || c.LibraryPath != "/from/file" {
		t.Errorf("config.toml not applied: %+v", c)
	}

	t.Setenv("SYNTY_LIBRARY", "/from/env")
	t.Setenv("SYNTY_CUSTOMER_ID", "9999999999999")
	c, _ = Load(dir)
	if c.LibraryPath != "/from/env" || c.CustomerID != "9999999999999" {
		t.Errorf("env should override config.toml: %+v", c)
	}

	// And the expansion runs after the environment, not inside the config-file overlay.
	// No shell expands a value exported as SYNTY_LIBRARY="~/assets" (a quoted export, a
	// systemd unit, direnv), so moving ExpandHome up alongside the other per-field logic
	// keeps every assertion above green while the mirror lands in a directory literally
	// named "~" that the user will never find.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SYNTY_LIBRARY", "~/assets")
	c, _ = Load(dir)
	if want := filepath.Join(home, "assets"); c.LibraryPath != want {
		t.Errorf("LibraryPath = %q, want %q; a tilde from the environment was not expanded", c.LibraryPath, want)
	}
}

// A key that decodes to nothing is a typo. Dropping it silently leaves the user
// reading "no customer id: ... or put customer_id in config.toml" while looking at a
// config.toml that appears to contain exactly that.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("customer-id = \"1234567890123\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("a misspelled key was accepted and dropped")
	}
	if !strings.Contains(err.Error(), "customer-id") {
		t.Errorf("err = %v, want it to name the key that went unread", err)
	}
}
