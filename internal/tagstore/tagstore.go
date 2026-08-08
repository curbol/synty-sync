// Package tagstore is the committed tag store (synty-sync.tags.toml): a palette of
// user-defined tags (each a label plus a color), per-asset assignments, and link
// groups, all keyed by an asset's content fingerprint. It lives beside the project
// manifest and travels with the consuming project in source control, carrying no
// account identity.
//
// A link group is an undirected set of fingerprints that "belong together" (a UI
// frame and its background fill, say), so the browse layer can surface companions
// alongside a match. Groups merge transitively: linking {A,B} then {B,C} yields
// {A,B,C}.
//
// The store round-trips faithfully: it has no knowledge of any asset index and
// never prunes assignments or links to a "currently-scanned" set, so they survive a
// resync, a disabled pack, a narrowed browse root, or a move to another machine. A
// tag's id is its label text (identity); "key:value" labels are ordinary ids by
// convention, not enforced.
package tagstore

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileName is the tag-store filename, a sibling of the project manifest.
const FileName = "synty-sync.tags.toml"

// TagDef is one palette entry: a tag's label (id) and its display color (#rrggbb).
type TagDef struct {
	ID    string `toml:"id"`
	Color string `toml:"color"`
}

// Assignment is the set of tag ids applied to one content fingerprint.
type Assignment struct {
	Fingerprint string   `toml:"fingerprint"`
	Tags        []string `toml:"tags"`
}

// Group is one link group: a set of content fingerprints that travel together.
type Group struct {
	Fingerprints []string `toml:"fingerprints"`
}

// fileTOML is the on-disk shape; sections serialize as [[tag]], then [[assignment]],
// then [[group]].
type fileTOML struct {
	Tags        []TagDef     `toml:"tag"`
	Assignments []Assignment `toml:"assignment"`
	Groups      []Group      `toml:"group"`
}

// Store is an in-memory tag store. The palette (colors), assignments, and link
// groups are the source of truth; Load/Save convert to and from the TOML
// representation.
type Store struct {
	colors map[string]string          // tag id -> color
	assign map[string]map[string]bool // fingerprint -> set of tag ids
	// groups maps a fingerprint to the member set it belongs to. Every member of a
	// group points at the same set instance (which includes the member itself), so a
	// merge is a repoint and membership/lookup is O(1).
	groups map[string]map[string]bool
}

// New returns an empty store.
func New() *Store {
	return &Store{
		colors: map[string]string{},
		assign: map[string]map[string]bool{},
		groups: map[string]map[string]bool{},
	}
}

var colorRe = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// normalizeColor lower-cases and validates a #rrggbb color.
func normalizeColor(c string) (string, error) {
	c = strings.ToLower(strings.TrimSpace(c))
	if !colorRe.MatchString(c) {
		return "", fmt.Errorf("invalid color %q: want #rrggbb", c)
	}
	return c, nil
}

// DefaultColor derives a stable, evenly-spread color from a label, so a new tag
// gets an arbitrary-but-consistent color until the user picks one.
func DefaultColor(label string) string {
	h := fnv.New32a()
	h.Write([]byte(label))
	return hslHex(float64(h.Sum32()%360), 0.62, 0.55)
}

// Define upserts a tag definition with an explicit color.
func (s *Store) Define(id, color string) error {
	if id == "" {
		return fmt.Errorf("empty tag id")
	}
	c, err := normalizeColor(color)
	if err != nil {
		return err
	}
	s.colors[id] = c
	return nil
}

// ensure guarantees id has a palette entry, giving a new tag its default color.
func (s *Store) ensure(id string) {
	if _, ok := s.colors[id]; !ok {
		s.colors[id] = DefaultColor(id)
	}
}

// Has reports whether a tag is defined.
func (s *Store) Has(id string) bool { _, ok := s.colors[id]; return ok }

// Color returns a tag's color and whether it is defined.
func (s *Store) Color(id string) (string, bool) { c, ok := s.colors[id]; return c, ok }

// Assign applies a tag to a fingerprint, defining the tag (default color) if new.
func (s *Store) Assign(fp, id string) {
	if fp == "" || id == "" {
		return
	}
	s.ensure(id)
	if s.assign[fp] == nil {
		s.assign[fp] = map[string]bool{}
	}
	s.assign[fp][id] = true
}

// Unassign removes a tag from a fingerprint. The tag stays in the palette.
func (s *Store) Unassign(fp, id string) {
	set := s.assign[fp]
	if set == nil {
		return
	}
	delete(set, id)
	if len(set) == 0 {
		delete(s.assign, fp)
	}
}

// Rename changes a tag's id everywhere. Renaming onto an existing id merges: the
// two tags' assignments collapse (deduped), the surviving def keeps the target's
// color, and the old def is dropped — the store never holds two defs with one id.
func (s *Store) Rename(old, neu string) error {
	if neu == "" {
		return fmt.Errorf("empty tag id")
	}
	if old == neu {
		return nil
	}
	oldColor, ok := s.colors[old]
	if !ok {
		return nil
	}
	if _, exists := s.colors[neu]; !exists {
		s.colors[neu] = oldColor
	}
	delete(s.colors, old)
	for _, set := range s.assign {
		if set[old] {
			delete(set, old)
			set[neu] = true
		}
	}
	return nil
}

// Delete removes a tag from the palette and from every assignment.
func (s *Store) Delete(id string) {
	delete(s.colors, id)
	for fp, set := range s.assign {
		if set[id] {
			delete(set, id)
			if len(set) == 0 {
				delete(s.assign, fp)
			}
		}
	}
}

// TagsFor returns the sorted tag ids applied to a fingerprint.
func (s *Store) TagsFor(fp string) []string { return sortedKeys(s.assign[fp]) }

// Link groups the given fingerprints so they travel together, absorbing any groups
// they already belong to into one (so linking {A,B} then {B,C} yields {A,B,C}).
// Fewer than two distinct non-empty fingerprints is a no-op: a link needs at least
// two members.
func (s *Store) Link(fps []string) {
	union := map[string]bool{}
	for _, fp := range fps {
		if fp == "" {
			continue
		}
		union[fp] = true
		for m := range s.groups[fp] {
			union[m] = true
		}
	}
	if len(union) < 2 {
		return
	}
	for m := range union {
		s.groups[m] = union
	}
}

// Unlink removes the given fingerprints from their group, dissolving a group that
// would drop below two members.
func (s *Store) Unlink(fps []string) {
	for _, fp := range fps {
		set := s.groups[fp]
		if set == nil {
			continue
		}
		delete(set, fp)
		delete(s.groups, fp)
		if len(set) < 2 {
			for m := range set {
				delete(s.groups, m)
			}
		}
	}
}

// Related returns the other fingerprints grouped with fp, sorted; nil when fp is in
// no group.
func (s *Store) Related(fp string) []string {
	set := s.groups[fp]
	if set == nil {
		return nil
	}
	out := make([]string, 0, len(set)-1)
	for m := range set {
		if m != fp {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// Groups returns the link groups, each a sorted member list, ordered by first
// member, for stable persistence and tests.
func (s *Store) Groups() [][]string {
	var out [][]string
	for fp, set := range s.groups {
		if len(set) < 2 {
			continue
		}
		min := fp
		for m := range set {
			if m < min {
				min = m
			}
		}
		if fp != min { // emit each group once, from its lowest member
			continue
		}
		out = append(out, sortedKeys(set))
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// Counts returns the number of fingerprints each tag is applied to.
func (s *Store) Counts() map[string]int {
	m := map[string]int{}
	for _, set := range s.assign {
		for id := range set {
			m[id]++
		}
	}
	return m
}

// Tags returns the palette sorted by id.
func (s *Store) Tags() []TagDef {
	out := make([]TagDef, 0, len(s.colors))
	for id, c := range s.colors {
		out = append(out, TagDef{ID: id, Color: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Load reads the tag store at path, returning an empty store if it does not exist.
// Assignments are preserved verbatim; a tag referenced by an assignment but missing
// a definition (a hand-edited file) is given a default color so the palette stays
// complete.
func Load(path string) (*Store, error) {
	s := New()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return s, nil
	}
	var f fileTOML
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return nil, err
	}
	for _, t := range f.Tags {
		if t.ID == "" {
			continue
		}
		if c, err := normalizeColor(t.Color); err == nil {
			s.colors[t.ID] = c
		} else {
			s.colors[t.ID] = DefaultColor(t.ID)
		}
	}
	for _, a := range f.Assignments {
		if a.Fingerprint == "" {
			continue
		}
		for _, id := range a.Tags {
			s.Assign(a.Fingerprint, id)
		}
	}
	for _, g := range f.Groups {
		s.Link(g.Fingerprints)
	}
	return s, nil
}

// Save writes the store at path atomically, with tags sorted by id, assignments
// sorted by fingerprint, groups sorted by first member, and every member list
// sorted, for minimal diffs.
func Save(path string, s *Store) error {
	f := fileTOML{Tags: s.Tags()}
	fps := make([]string, 0, len(s.assign))
	for fp, set := range s.assign {
		if len(set) > 0 {
			fps = append(fps, fp)
		}
	}
	sort.Strings(fps)
	for _, fp := range fps {
		f.Assignments = append(f.Assignments, Assignment{Fingerprint: fp, Tags: sortedKeys(s.assign[fp])})
	}
	for _, g := range s.Groups() {
		f.Groups = append(f.Groups, Group{Fingerprints: g})
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".synty-sync-tags-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := toml.NewEncoder(tmp).Encode(f); err != nil {
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

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hslHex converts an HSL color (h in degrees, s and l in [0,1]) to #rrggbb.
func hslHex(h, s, l float64) string {
	c := (1 - abs(2*l-1)) * s
	x := c * (1 - abs(mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return fmt.Sprintf("#%02x%02x%02x", to255(r+m), to255(g+m), to255(b+m))
}

func to255(v float64) int { return int(v*255 + 0.5) }
func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func mod(a, b float64) float64 {
	m := a - b*float64(int(a/b))
	if m < 0 {
		m += b
	}
	return m
}
