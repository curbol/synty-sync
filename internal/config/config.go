// Package config resolves tool settings. The config/state directory follows XDG
// with fallbacks; built-in defaults live in code; an optional config.toml in that
// directory overrides them; environment variables and flags override that. No
// machine-specific path is baked into the tool.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the resolved user-scoped tool configuration: account identity, session
// source, and machine defaults. Project-scoped settings (engine variants, the pack
// selection) live in the manifest, not here.
type Config struct {
	CustomerID    string
	LibraryPath   string
	Concurrency   int
	SessionSource string // "firefox" or a path to a cookies.txt / curl file
}

type fileConfig struct {
	CustomerID    string `toml:"customer_id"`
	LibraryPath   string `toml:"library_path"`
	Concurrency   int    `toml:"concurrency"`
	SessionSource string `toml:"session_source"`
}

// ResolveDir picks the user config directory (where config.toml lives): an explicit
// flag, else $SYNTY_CONFIG_DIR, else $XDG_CONFIG_HOME/synty-sync, else
// ~/.config/synty-sync. The project manifest and lockfile live with the project, not
// here.
//
// A directory the user named has to exist and be a directory; one this function fell
// back to need not exist. Load reads an absent config.toml as no config at all, which is
// the ordinary first run, and a misspelled --config is indistinguishable from it: the
// run drops library_path and mirrors gigabytes into the default directory. Only here is
// it known which of the two a path is.
func ResolveDir(flag string) (string, error) {
	if flag != "" {
		return named(ExpandHome(flag), "--config")
	}
	// No shell expands an environment value or a quoted flag, so a "~" written in
	// either arrives literally and would resolve to a directory of that name — the
	// config file is then never found, and the run reports the setting it contains
	// as missing.
	if v := os.Getenv("SYNTY_CONFIG_DIR"); v != "" {
		return named(ExpandHome(v), "$SYNTY_CONFIG_DIR")
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "synty-sync"), nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "synty-sync"), nil
	}
	return "synty-sync", nil
}

// named checks a config directory the user chose.
func named(dir, source string) (string, error) {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s names %s, which does not exist; a config directory that is not there "+
			"reads as no config at all, so every setting in it would be silently ignored", source, dir)
	}
	if err != nil {
		return "", fmt.Errorf("%s names %s: %w", source, dir, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s names %s, which is not a directory: it takes the directory holding "+
			"config.toml, not the file itself", source, dir)
	}
	return dir, nil
}

// defaultLibraryPath is the cache location when nothing overrides it:
// $XDG_DATA_HOME/synty-sync, else ~/.local/share/synty-sync. App data, not
// ~/.cache, so an OS cache-cleaner won't wipe a multi-GB library.
//
// It refuses rather than falling back to a relative path: writing a multi-gigabyte
// mirror into whatever directory the user happened to run from is worse than an error
// naming the ways to say where it goes.
func defaultLibraryPath() (string, error) {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "synty-sync"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to put the library under (%w): set XDG_DATA_HOME "+
			"or SYNTY_LIBRARY, put library_path in config.toml, or pass --library", err)
	}
	return filepath.Join(home, ".local", "share", "synty-sync"), nil
}

func defaults() Config {
	return Config{
		Concurrency:   4,
		SessionSource: "firefox",
	}
}

// Flags are the command-line overrides, the highest-precedence layer. A zero field is
// one the user did not pass. They are applied here rather than by the caller so every
// layer gets the same treatment: the library default is resolved only when no layer
// supplied one, and a quoted --library "~/assets" is expanded like the others.
type Flags struct {
	LibraryPath string
	CustomerID  string
	Concurrency int
}

// Load merges built-in defaults, an optional config.toml in dir, environment overrides
// (SYNTY_CUSTOMER_ID, SYNTY_LIBRARY), then flags. A missing config.toml is fine.
func Load(dir string, f Flags) (Config, error) {
	c := defaults()
	// Asked of dir itself rather than read off the errno of opening a file through it:
	// Windows answers a path that runs through a regular file with ERROR_PATH_NOT_FOUND,
	// which errors.Is reads as fs.ErrNotExist, so the absent-file case below would take
	// it there.
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		return Config{}, fmt.Errorf("%s is not a directory: the config dir holds config.toml", dir)
	}
	p := filepath.Join(dir, "config.toml")
	// Only a path with nothing at it is "no config file". Anything else is surfaced,
	// since skipping it leaves the user reading an error that names a setting their
	// file already holds.
	var fc fileConfig
	md, err := toml.DecodeFile(p, &fc)
	switch {
	case err != nil && absentConfig(p, err):
	case err != nil:
		return Config{}, err
	default:
		// A key that decodes to nothing is a typo, and silently dropping it leaves the
		// user reading an error that names the setting their file already contains.
		if un := md.Undecoded(); len(un) > 0 {
			keys := make([]string, 0, len(un))
			for _, k := range un {
				keys = append(keys, k.String())
			}
			return Config{}, fmt.Errorf("%s: unknown key(s): %s", p, strings.Join(keys, ", "))
		}
		// overlay cannot tell a zero someone wrote from a key nobody wrote, so the
		// metadata is asked: an absent key is the ordinary case, a written zero is not a
		// number of simultaneous fetches.
		if md.IsDefined("concurrency") && fc.Concurrency < 1 {
			return Config{}, fmt.Errorf("%s: concurrency = %d is not a number of simultaneous fetches; "+
				"set 1 or more, or remove the key to use the default", p, fc.Concurrency)
		}
		overlay(&c, fc)
	}
	if v := os.Getenv("SYNTY_CUSTOMER_ID"); v != "" {
		c.CustomerID = v
	}
	if v := os.Getenv("SYNTY_LIBRARY"); v != "" {
		c.LibraryPath = v
	}
	if f.LibraryPath != "" {
		c.LibraryPath = f.LibraryPath
	}
	if f.CustomerID != "" {
		c.CustomerID = f.CustomerID
	}
	if f.Concurrency > 0 {
		c.Concurrency = f.Concurrency
	}
	// Last, and only when no layer supplied one: the default needs a home directory, and
	// a run that names its own library has no use for one.
	if c.LibraryPath == "" {
		lib, err := defaultLibraryPath()
		if err != nil {
			return Config{}, err
		}
		c.LibraryPath = lib
	}
	c.LibraryPath = ExpandHome(c.LibraryPath)
	c.SessionSource = ExpandHome(c.SessionSource)
	return c, nil
}

func overlay(c *Config, fc fileConfig) {
	if fc.CustomerID != "" {
		c.CustomerID = fc.CustomerID
	}
	if fc.LibraryPath != "" {
		c.LibraryPath = fc.LibraryPath
	}
	if fc.Concurrency > 0 {
		c.Concurrency = fc.Concurrency
	}
	if fc.SessionSource != "" {
		c.SessionSource = fc.SessionSource
	}
}

// ExpandHome resolves a leading ~ to the user's home directory. It is exported
// because --manifest and --cookies are resolved outside this package and need the same
// treatment as the paths inside it.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// absentConfig reports whether the decode failed because nothing is at the path at
// all. A dangling symlink — a dotfiles tree that has not been checked out — opens with
// the same ENOENT as an absent file, so Lstat is what separates them. Skipping one in
// silence leaves the run reporting the very setting the file holds as missing.
func absentConfig(path string, err error) bool {
	if !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	_, lerr := os.Lstat(path)
	return errors.Is(lerr, fs.ErrNotExist)
}
