// Package config resolves tool settings from layered sources: committed
// config.toml defaults, a gitignored config.local.toml, then environment
// overrides. The account-identifying customer id is never read from the committed
// file; it comes from env or the local file only.
package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/curbol/hexed-haven/tools/synty/internal/model"
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

func defaults() Config {
	return Config{
		LibraryPath:     "~/code/synty-assets",
		VariantIncludes: []string{"Godot_*", "SourceFiles"},
		Concurrency:     4,
		SessionSource:   "firefox",
	}
}

// Load merges defaults, the committed config.toml in dir, the gitignored
// config.local.toml in dir, then environment overrides (SYNTY_CUSTOMER_ID,
// SYNTY_LIBRARY). Missing files are skipped.
func Load(dir string) (Config, error) {
	c := defaults()
	for _, name := range []string{"config.toml", "config.local.toml"} {
		var fc fileConfig
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
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
