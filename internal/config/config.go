// Package config resolves tool settings. The config/state directory follows XDG
// with fallbacks; built-in defaults live in code; an optional config.toml in that
// directory overrides them; environment variables and flags override that. No
// machine-specific path is baked into the tool.
package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/curbol/synty-sync/internal/model"
)

// Config is the resolved tool configuration.
type Config struct {
	CustomerID      string
	LibraryPath     string
	VariantIncludes []string
	Concurrency     int
	SessionSource   string // "firefox" or a path to a cookies.txt / curl file
}

type fileConfig struct {
	CustomerID      string   `toml:"customer_id"`
	LibraryPath     string   `toml:"library_path"`
	VariantIncludes []string `toml:"variant_includes"`
	Concurrency     int      `toml:"concurrency"`
	SessionSource   string   `toml:"session_source"`
}

// ResolveDir picks the config/state directory (where config.toml, packs.toml, and
// the lockfile live): an explicit flag, else $SYNTY_CONFIG_DIR, else
// $XDG_CONFIG_HOME/synty-sync, else ~/.config/synty-sync.
func ResolveDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("SYNTY_CONFIG_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "synty-sync")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "synty-sync")
	}
	return "synty-sync"
}

// defaultLibraryPath is the cache location when nothing overrides it:
// $XDG_DATA_HOME/synty-sync, else ~/.local/share/synty-sync. App data, not
// ~/.cache, so an OS cache-cleaner won't wipe a multi-GB library.
func defaultLibraryPath() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "synty-sync")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "synty-sync")
	}
	return "synty-library"
}

func defaults() Config {
	return Config{
		LibraryPath: defaultLibraryPath(),
		// No engine default: the right variant depends on the user's engine, so it
		// must be configured (Godot_*, Unity_*, Unreal_*, SourceFiles, SourceSprites).
		VariantIncludes: nil,
		Concurrency:     4,
		SessionSource:   "firefox",
	}
}

// Load merges built-in defaults, an optional config.toml in dir, then environment
// overrides (SYNTY_CUSTOMER_ID, SYNTY_LIBRARY). A missing config.toml is fine.
func Load(dir string) (Config, error) {
	c := defaults()
	p := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(p); err == nil {
		var fc fileConfig
		if _, err := toml.DecodeFile(p, &fc); err != nil {
			return Config{}, err
		}
		overlay(&c, fc)
	}
	if v := os.Getenv("SYNTY_CUSTOMER_ID"); v != "" {
		c.CustomerID = v
	}
	if v := os.Getenv("SYNTY_LIBRARY"); v != "" {
		c.LibraryPath = v
	}
	c.LibraryPath = expandHome(c.LibraryPath)
	return c, nil
}

func overlay(c *Config, fc fileConfig) {
	if fc.CustomerID != "" {
		c.CustomerID = fc.CustomerID
	}
	if fc.LibraryPath != "" {
		c.LibraryPath = fc.LibraryPath
	}
	if len(fc.VariantIncludes) > 0 {
		c.VariantIncludes = fc.VariantIncludes
	}
	if fc.Concurrency > 0 {
		c.Concurrency = fc.Concurrency
	}
	if fc.SessionSource != "" {
		c.SessionSource = fc.SessionSource
	}
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Filter returns a predicate selecting variants whose token matches any include
// glob. Archived variants are always excluded (callers check FileEntry.Archived).
func (c Config) Filter() func(model.Variant) bool {
	includes := c.VariantIncludes
	return func(v model.Variant) bool {
		for _, pat := range includes {
			if ok, _ := filepath.Match(pat, string(v)); ok {
				return true
			}
		}
		return false
	}
}
