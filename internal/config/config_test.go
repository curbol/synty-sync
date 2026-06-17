package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/curbol/hexed-haven/tools/synty/internal/model"
)

func TestDefaultsWhenNoFiles(t *testing.T) {
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Concurrency != 4 || c.SessionSource != "firefox" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if len(c.VariantIncludes) != 2 {
		t.Errorf("variant includes: %v", c.VariantIncludes)
	}
}

func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
variant_includes = ["Godot_*"]
concurrency = 2
library_path = "/from/committed"
`), 0o644)
	os.WriteFile(filepath.Join(dir, "config.local.toml"), []byte(`
customer_id = "1234567890123"
library_path = "/from/local"
`), 0o644)

	// committed < local < env
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Concurrency != 2 {
		t.Errorf("concurrency = %d, want 2 (committed)", c.Concurrency)
	}
	if c.CustomerID != "1234567890123" {
		t.Errorf("customer id = %q, want from local", c.CustomerID)
	}
	if c.LibraryPath != "/from/local" {
		t.Errorf("library path = %q, want /from/local (local over committed)", c.LibraryPath)
	}

	t.Setenv("SYNTY_LIBRARY", "/from/env")
	t.Setenv("SYNTY_CUSTOMER_ID", "9999999999999")
	c, _ = Load(dir)
	if c.LibraryPath != "/from/env" {
		t.Errorf("library path = %q, want /from/env (env wins)", c.LibraryPath)
	}
	if c.CustomerID != "9999999999999" {
		t.Errorf("customer id = %q, want env", c.CustomerID)
	}
}

func TestFilter(t *testing.T) {
	c := Config{VariantIncludes: []string{"Godot_*", "SourceFiles"}}
	f := c.Filter()
	cases := map[model.Variant]bool{
		"Godot_4_5_1":   true,
		"Godot_4_6_2":   true,
		"SourceFiles":   true,
		"SourceSprites": false,
		"Unity_2022_3":  false,
		"Unreal_5_3":    false,
	}
	for v, want := range cases {
		if f(v) != want {
			t.Errorf("filter(%s) = %v, want %v", v, f(v), want)
		}
	}
}
