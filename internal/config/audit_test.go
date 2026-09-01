package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The library is a multi-gigabyte mirror, so it lives in the data dir rather than a
// cache dir: an OS cache cleaner that wiped it would cost a full re-download of
// everything. Nothing but this pins the choice — a switch to os.UserCacheDir would
// still land in a directory named synty-sync and satisfy a suffix check.
func TestDefaultLibraryLivesInDataNotCache(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.LibraryPath != filepath.Join(data, "synty-sync") {
		t.Errorf("library = %q, want it under XDG_DATA_HOME %q", c.LibraryPath, data)
	}
	for _, seg := range strings.Split(filepath.ToSlash(c.LibraryPath), "/") {
		if seg == ".cache" || seg == "Caches" {
			t.Errorf("the library landed in a cache directory: %q", c.LibraryPath)
		}
	}
}

// A "~" in an environment value or a quoted flag is never expanded by a shell, so it
// arrives literally. Without the same expansion the config file gets, the tool looks
// for config.toml in a directory named "~" and then reports the customer id the user
// is looking at as missing.
func TestResolveDirExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := ResolveDir("~/synty"); got != filepath.Join(home, "synty") {
		t.Errorf("ResolveDir(flag) = %q, want it under %q", got, home)
	}
	t.Setenv("SYNTY_CONFIG_DIR", "~/from-env")
	if got := ResolveDir(""); got != filepath.Join(home, "from-env") {
		t.Errorf("ResolveDir(env) = %q, want it under %q", got, home)
	}
}

// A session_source pointing at a file gets the same treatment, since it is a path
// like any other and session.Resolve hands it straight to os.ReadFile.
func TestLoadExpandsHomeInSessionSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("session_source = \"~/synty.curl\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionSource != filepath.Join(home, "synty.curl") {
		t.Errorf("session_source = %q, want it under %q", c.SessionSource, home)
	}
}

// Only a file that is not there means "no config file". A dangling symlink from a
// dotfiles tree that is not checked out used to be skipped in silence, and the run
// then reported the very setting the file holds as missing.
func TestLoadReportsAConfigItCannotRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "nowhere", "config.toml"), filepath.Join(dir, "config.toml")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a config.toml that could not be read was silently skipped")
	}
}
