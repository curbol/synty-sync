package model

import "testing"

// Slug is the key the lockfile and the manifest allowlist are both stored under, so
// a change to the collapsing rules silently renames every pack in a committed file.
// Real Synty names mix ASCII hyphens with en dashes and pad them with spaces.
func TestSlug(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"POLYGON - Pirate Pack", "polygon-pirate-pack"},
		{"Fantasy Knights – Sidekick Modular Characters", "fantasy-knights-sidekick-modular-characters"},
		{"Elven Warriors - Sidekick Modular Characters", "elven-warriors-sidekick-modular-characters"},
		{"INTERFACE - Dark Fantasy HUD", "interface-dark-fantasy-hud"},
		{"POLYGON — Nature Biomes: Alpine Mountain", "polygon-nature-biomes-alpine-mountain"},
		{"  leading and trailing  ", "leading-and-trailing"},
		{"--already--hyphenated--", "already-hyphenated"},
		{"Numbers 2024 Stay", "numbers-2024-stay"},
	} {
		if got := Slug(tc.name); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The en-dash and hyphen forms of one name have to agree, since the store is not
// consistent about which it uses and a rename would strand the pack's whole record.
func TestSlugFoldsDashStyles(t *testing.T) {
	hyphen := Slug("Goblin Fighters - Sidekick Modular Characters")
	enDash := Slug("Goblin Fighters – Sidekick Modular Characters")
	emDash := Slug("Goblin Fighters — Sidekick Modular Characters")
	if hyphen != enDash || hyphen != emDash {
		t.Errorf("dash styles disagree: %q / %q / %q", hyphen, enDash, emDash)
	}
}

// A display name with no ASCII alphanumerics leaves nothing to key on. Two such
// packs would collapse onto one lockfile entry, so the empty result is worth
// pinning: it is the shape a caller would have to notice.
func TestSlugOfANameWithNothingToKeyOn(t *testing.T) {
	if got := Slug("—— ——"); got != "" {
		t.Errorf("Slug = %q, want the empty string", got)
	}
}

// Key is the per-file lockfile key. It has to separate a pack's own file from a
// bundled one of the same variant, and must not fold two variants of one token.
func TestKey(t *testing.T) {
	own := FileEntry{FileToken: "POLYGON_Pirate", Variant: "Godot_4_5_1"}
	bundled := FileEntry{FileToken: "GENERIC_Particle_FX", Variant: "Godot_4_5_1"}
	otherVariant := FileEntry{FileToken: "POLYGON_Pirate", Variant: "Unity_2022_3"}

	if own.Key() != "POLYGON_Pirate|Godot_4_5_1" {
		t.Errorf("Key = %q", own.Key())
	}
	if own.Key() == bundled.Key() {
		t.Error("a pack's own file and a bundled one of the same variant share a key")
	}
	if own.Key() == otherVariant.Key() {
		t.Error("two variants of one token share a key")
	}
}
