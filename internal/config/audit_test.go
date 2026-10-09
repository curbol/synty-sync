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
//
// Both branches, because most distributions leave XDG_DATA_HOME unset and so most
// readers get the second one. Setting it and checking only that left the fallback free
// to move to ~/.cache with the whole suite green.
func TestDefaultLibraryLivesInDataNotCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		// xdg is what XDG_DATA_HOME is set to, or "" to unset it and take the fallback.
		xdg bool
	}{
		{"XDG_DATA_HOME is set", true},
		{"XDG_DATA_HOME is unset, as it is on most machines", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			want := filepath.Join(home, ".local", "share", "synty-sync")
			if tc.xdg {
				data := t.TempDir()
				t.Setenv("XDG_DATA_HOME", data)
				want = filepath.Join(data, "synty-sync")
			} else {
				t.Setenv("XDG_DATA_HOME", "")
			}

			c, err := Load(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if c.LibraryPath != want {
				t.Errorf("library = %q, want %q", c.LibraryPath, want)
			}
			for _, seg := range strings.Split(filepath.ToSlash(c.LibraryPath), "/") {
				if seg == ".cache" || seg == "Caches" {
					t.Errorf("the library landed in a cache directory: %q", c.LibraryPath)
				}
			}
		})
	}
}

// A "~" in an environment value or a quoted flag is never expanded by a shell, so it
// arrives literally. Without the same expansion the config file gets, the tool looks
// for config.toml in a directory named "~" and then reports the customer id the user
// is looking at as missing.
func TestResolveDirExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	mkdir(t, filepath.Join(home, "synty"))
	mkdir(t, filepath.Join(home, "from-env"))

	if got := resolveDir(t, "~/synty"); got != filepath.Join(home, "synty") {
		t.Errorf("ResolveDir(flag) = %q, want it under %q", got, home)
	}
	t.Setenv("SYNTY_CONFIG_DIR", "~/from-env")
	if got := resolveDir(t, ""); got != filepath.Join(home, "from-env") {
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

// The documented precedence is --config › $SYNTY_CONFIG_DIR › $XDG_CONFIG_HOME ›
// ~/.config. TestResolveDir walks the rungs one at a time with everything above each
// one cleared, so it says which rung is *reachable* but nothing about which wins: an
// implementation that consulted XDG_CONFIG_HOME before SYNTY_CONFIG_DIR, or the
// environment before the flag, passes it unchanged. Order is the whole contract of
// this function: getting it wrong points the tool at a config dir the user is not
// editing, and it then reports the settings their file already holds as missing.
func TestResolveDirPrecedenceWithEveryRungSetAtOnce(t *testing.T) {
	flagDir := mkdir(t, filepath.Join(t.TempDir(), "from-flag"))
	envDir := mkdir(t, filepath.Join(t.TempDir(), "from-synty-env"))
	t.Setenv("SYNTY_CONFIG_DIR", envDir)
	t.Setenv("XDG_CONFIG_HOME", "/from/xdg")

	// All three set: the flag wins.
	if got := resolveDir(t, flagDir); got != flagDir {
		t.Errorf("with every rung set, ResolveDir = %q, want the flag to win", got)
	}
	// Flag gone: SYNTY_CONFIG_DIR beats XDG.
	if got := resolveDir(t, ""); got != envDir {
		t.Errorf("ResolveDir = %q, want SYNTY_CONFIG_DIR to beat XDG_CONFIG_HOME", got)
	}
	// SYNTY_CONFIG_DIR gone: XDG beats the home fallback.
	t.Setenv("SYNTY_CONFIG_DIR", "")
	want := filepath.Join("/from/xdg", "synty-sync")
	if got := resolveDir(t, ""); got != want {
		t.Errorf("ResolveDir = %q, want %q", got, want)
	}
	// And only with both cleared does the home fallback apply.
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := resolveDir(t, ""); !strings.HasSuffix(got, filepath.Join(".config", "synty-sync")) {
		t.Errorf("ResolveDir = %q, want the ~/.config fallback", got)
	}
}

// resolveDir is ResolveDir where the directory is expected to resolve. The two named
// rungs refuse a directory that is not there, so a test about precedence supplies ones
// that exist; the fallbacks are not checked and need none.
func resolveDir(t *testing.T, flag string) string {
	t.Helper()
	dir, err := ResolveDir(flag)
	if err != nil {
		t.Fatalf("ResolveDir(%q): %v", flag, err)
	}
	return dir
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A config directory the user named is checked; one this package fell back to is not.
// An absent config.toml is the ordinary first run, so Load reads it as "no config", and
// a misspelled --config is indistinguishable from that: library_path is dropped and the
// run mirrors gigabytes into the default directory, said only by the "library:" line
// the summary prints once the downloads are done. Naming the file rather than the
// directory is the same mistake, and on Windows it arrives as ERROR_PATH_NOT_FOUND,
// which errors.Is reads as fs.ErrNotExist.
func TestANamedConfigDirMustExistAndBeADirectory(t *testing.T) {
	t.Setenv("SYNTY_CONFIG_DIR", "")
	missing := filepath.Join(t.TempDir(), "synty-snyc")
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte("library_path = \"/mnt/big\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{missing, file} {
		if _, err := ResolveDir(bad); err == nil {
			t.Errorf("--config %s was accepted", bad)
		} else if !strings.Contains(err.Error(), bad) {
			t.Errorf("error %q does not name %s", err, bad)
		}
	}
	t.Setenv("SYNTY_CONFIG_DIR", missing)
	if _, err := ResolveDir(""); err == nil {
		t.Error("a $SYNTY_CONFIG_DIR naming a directory that does not exist was accepted")
	}

	// The fallbacks need not exist: refusing there would fail every first run until a
	// directory was made by hand for a file that is optional.
	t.Setenv("SYNTY_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "nothing-here"))
	if _, err := ResolveDir(""); err != nil {
		t.Errorf("the XDG fallback refused a directory that does not exist yet: %v", err)
	}
	// A fallback that exists but is a file is still not a directory to read from.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clash := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "synty-sync")
	if err := os.WriteFile(clash, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(clash); err == nil {
		t.Error("Load read a regular file as a config directory with nothing in it")
	}
}
