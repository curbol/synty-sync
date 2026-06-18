// Package manifest is the committed pack-selection allowlist (packs.toml). Every
// owned pack appears with an enabled flag; sync only pulls enabled packs. New
// packs are added disabled (opt-in), so buying a pack never silently downloads it.
package manifest

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/curbol/synty-sync/internal/model"
)

// Entry is one pack's selection state.
type Entry struct {
	Slug    string `toml:"slug"`
	Name    string `toml:"name"`
	Enabled bool   `toml:"enabled"`
}

// Manifest is the full allowlist, ordered by slug for stable diffs.
type Manifest struct {
	Packs []Entry `toml:"pack"`
}

// Load reads packs.toml, returning an empty manifest if it does not exist.
func Load(path string) (Manifest, error) {
	var m Manifest
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return m, nil
	}
	if _, err := toml.DecodeFile(path, &m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Save writes packs.toml atomically, packs sorted by slug.
func Save(path string, m Manifest) error {
	sort.Slice(m.Packs, func(i, j int) bool { return m.Packs[i].Slug < m.Packs[j].Slug })
	tmp, err := os.CreateTemp(filepath.Dir(path), ".synty-packs-*")
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
