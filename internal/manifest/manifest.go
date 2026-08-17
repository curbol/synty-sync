// Package manifest is the committed project manifest (synty-sync.toml): the engine
// variant filter plus the pack-selection allowlist. It is discovered by walking up
// from the working directory and lives with the consuming project, not the tool. Every
// owned pack appears with an enabled flag; sync only pulls enabled packs, and new packs
// are added disabled (opt-in), so buying a pack never silently downloads it. The
// manifest carries no account identity: auth stays in the user config.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/curbol/synty-sync/internal/model"
)

// FileName is the discovered manifest filename.
const FileName = "synty-sync.toml"

// Entry is one pack's selection state.
type Entry struct {
	Slug    string `toml:"slug"`
	Name    string `toml:"name"`
	Enabled bool   `toml:"enabled"`
}

// Manifest is the project manifest: the engine variant include globs and the pack
// allowlist (ordered by slug for stable diffs). VariantIncludes is declared first so it
// serializes above the [[pack]] tables.
type Manifest struct {
	VariantIncludes []string `toml:"variant_includes"`
	Packs           []Entry  `toml:"pack"`
}

// Discover walks up from startDir looking for a manifest named FileName, returning its
// path and true on the first hit, or ("", false) at the filesystem root.
func Discover(startDir string) (string, bool) {
	dir := startDir
	for {
		p := filepath.Join(dir, FileName)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// LockPath returns the lockfile path paired with a manifest: the manifest path with its
// .toml suffix replaced by .lock.json (synty-sync.toml -> synty-sync.lock.json).
func LockPath(manifestPath string) string {
	return strings.TrimSuffix(manifestPath, ".toml") + ".lock.json"
}

// Validate reports a manifest the user has to fix by hand. A malformed
// variant_includes glob is the case that matters: filepath.Match reports it as "no
// match", so a typo looks exactly like a library with nothing for your engine.
func (m Manifest) Validate() error {
	for _, pat := range m.VariantIncludes {
		if _, err := filepath.Match(pat, ""); err != nil {
			return fmt.Errorf("bad variant_includes pattern %q: %w", pat, err)
		}
	}
	return nil
}

// Filter returns a predicate selecting variants whose token matches any of the
// manifest's include globs.
func (m Manifest) Filter() func(model.Variant) bool {
	includes := m.VariantIncludes
	return func(v model.Variant) bool {
		for _, pat := range includes {
			if ok, _ := filepath.Match(pat, string(v)); ok {
				return true
			}
		}
		return false
	}
}

// Load reads the manifest at path, returning an empty manifest if it does not exist.
func Load(path string) (Manifest, error) {
	var m Manifest
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return m, nil
	}
	md, err := toml.DecodeFile(path, &m)
	if err != nil {
		return Manifest{}, err
	}
	// Save re-encodes from the struct, so a key that decodes to nothing here is also
	// deleted from the user's file on the next write.
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, 0, len(un))
		for _, k := range un {
			keys = append(keys, k.String())
		}
		return Manifest{}, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	return m, nil
}

// Save writes the manifest at path atomically, packs sorted by slug.
func Save(path string, m Manifest) error {
	// Sort a copy: the value receiver shares the caller's backing array, so sorting
	// in place would reorder their slice behind their back.
	m.Packs = append([]Entry(nil), m.Packs...)
	sort.Slice(m.Packs, func(i, j int) bool { return m.Packs[i].Slug < m.Packs[j].Slug })
	tmp, err := os.CreateTemp(filepath.Dir(path), ".synty-sync-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := toml.NewEncoder(tmp).Encode(m); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// Reconcile rebuilds the manifest against the currently-owned packs: existing
// enabled flags are preserved, newly-owned packs are added disabled (opt-in), and
// packs no longer owned drop out. Names refresh from the store.
func (m *Manifest) Reconcile(packs []model.Pack) {
	prev := map[string]bool{}
	for _, e := range m.Packs {
		prev[e.Slug] = e.Enabled
	}
	out := make([]Entry, 0, len(packs))
	for _, p := range packs {
		out = append(out, Entry{Slug: p.Slug, Name: p.DisplayName, Enabled: prev[p.Slug]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	m.Packs = out
}

// EnabledSet returns the set of enabled pack slugs.
func (m Manifest) EnabledSet() map[string]bool {
	set := map[string]bool{}
	for _, e := range m.Packs {
		if e.Enabled {
			set[e.Slug] = true
		}
	}
	return set
}

// SetEnabled applies a slug->enabled selection to the manifest entries.
func (m *Manifest) SetEnabled(enabled map[string]bool) {
	for i := range m.Packs {
		m.Packs[i].Enabled = enabled[m.Packs[i].Slug]
	}
}
