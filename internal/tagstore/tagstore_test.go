package tagstore

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.tags.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tags()) != 0 || len(s.Counts()) != 0 {
		t.Errorf("missing file should load empty, got %d tags", len(s.Tags()))
	}
}

func TestDefineAssignRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := New()
	if err := s.Define("hero", "#E11D48"); err != nil { // upper-case normalizes
		t.Fatal(err)
	}
	s.Assign("crc32:abc:10", "hero")
	s.Assign("crc32:abc:10", "wip")
	s.Assign("uguid:xyz", "hero")
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := got.Color("hero"); c != "#e11d48" {
		t.Errorf("hero color = %q, want normalized #e11d48", c)
	}
	if !reflect.DeepEqual(got.TagsFor("crc32:abc:10"), []string{"hero", "wip"}) {
		t.Errorf("tags for crc32:abc:10 = %v", got.TagsFor("crc32:abc:10"))
	}
	if got.Counts()["hero"] != 2 {
		t.Errorf("hero count = %d, want 2", got.Counts()["hero"])
	}
}

// The store never prunes to a scanned set: an assignment for any fingerprint
// survives a save+load, which is the "tags survive resync / travel across
// machines" guarantee.
func TestAssignmentsPreservedRegardlessOfIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := New()
	s.Assign("crc32:notinanyindex:999", "keep")
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.TagsFor("crc32:notinanyindex:999"), []string{"keep"}) {
		t.Errorf("assignment for an unknown fingerprint was dropped: %v", got.TagsFor("crc32:notinanyindex:999"))
	}
}

func TestRenameRewritesAssignments(t *testing.T) {
	s := New()
	s.Define("wip", "#123456")
	s.Assign("fp1", "wip")
	s.Assign("fp2", "wip")
	if err := s.Rename("wip", "in-progress"); err != nil {
		t.Fatal(err)
	}
	if s.Has("wip") {
		t.Error("old id still present after rename")
	}
	if !reflect.DeepEqual(s.TagsFor("fp1"), []string{"in-progress"}) || !reflect.DeepEqual(s.TagsFor("fp2"), []string{"in-progress"}) {
		t.Errorf("rename did not rewrite assignments: fp1=%v fp2=%v", s.TagsFor("fp1"), s.TagsFor("fp2"))
	}
	if c, _ := s.Color("in-progress"); c != "#123456" {
		t.Errorf("renamed tag lost its color: %q", c)
	}
}

func TestRenameOntoExistingMerges(t *testing.T) {
	s := New()
	s.Define("a", "#aaaaaa")
	s.Define("b", "#bbbbbb")
	s.Assign("fp1", "a")
	s.Assign("fp1", "b") // fp1 has both
	s.Assign("fp2", "a") // fp2 has only a

	if err := s.Rename("a", "b"); err != nil {
		t.Fatal(err)
	}
	if s.Has("a") {
		t.Error("merged-away id still present")
	}
	// fp1 collapses a+b to a single b; fp2's a becomes b.
	if !reflect.DeepEqual(s.TagsFor("fp1"), []string{"b"}) {
		t.Errorf("fp1 after merge = %v, want [b]", s.TagsFor("fp1"))
	}
	if !reflect.DeepEqual(s.TagsFor("fp2"), []string{"b"}) {
		t.Errorf("fp2 after merge = %v, want [b]", s.TagsFor("fp2"))
	}
	if c, _ := s.Color("b"); c != "#bbbbbb" {
		t.Errorf("merge should keep target color, got %q", c)
	}
	if s.Counts()["b"] != 2 {
		t.Errorf("b count = %d, want 2", s.Counts()["b"])
	}
}

func TestDeletePurgesAssignments(t *testing.T) {
	s := New()
	s.Assign("fp1", "gone")
	s.Assign("fp1", "stay")
	s.Assign("fp2", "gone")
	s.Delete("gone")
	if s.Has("gone") {
		t.Error("deleted tag still in palette")
	}
	if !reflect.DeepEqual(s.TagsFor("fp1"), []string{"stay"}) {
		t.Errorf("fp1 = %v, want [stay]", s.TagsFor("fp1"))
	}
	if len(s.TagsFor("fp2")) != 0 {
		t.Errorf("fp2 should have no tags after delete, got %v", s.TagsFor("fp2"))
	}
}

func TestUnassignKeepsPaletteEntry(t *testing.T) {
	s := New()
	s.Assign("fp1", "solo")
	s.Unassign("fp1", "solo")
	if !s.Has("solo") {
		t.Error("unassign should keep the tag in the palette")
	}
	if len(s.TagsFor("fp1")) != 0 {
		t.Error("unassign left the assignment")
	}
}

func TestSaveIsSortedAndDeterministic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s := New()
	s.Define("zebra", "#000000")
	s.Define("alpha", "#ffffff")
	s.Assign("fp-b", "zebra")
	s.Assign("fp-a", "zebra")
	s.Assign("fp-a", "alpha")
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(path)
	text := string(b1)

	// Tags sorted by id: alpha before zebra.
	if strings.Index(text, `id = "alpha"`) > strings.Index(text, `id = "zebra"`) {
		t.Errorf("tags not sorted by id:\n%s", text)
	}
	// Assignments sorted by fingerprint: fp-a before fp-b.
	if strings.Index(text, "fp-a") > strings.Index(text, "fp-b") {
		t.Errorf("assignments not sorted by fingerprint:\n%s", text)
	}
	// Each assignment's tags sorted: alpha before zebra within fp-a.
	if strings.Index(text, `"alpha"`) > strings.Index(text, `"zebra", "alpha"`)+1 && strings.Contains(text, `"zebra", "alpha"`) {
		t.Errorf("assignment tags not sorted:\n%s", text)
	}
	// Deterministic: a second save of an equivalently-built store is byte-identical.
	path2 := filepath.Join(dir, "second-"+FileName)
	if err := Save(path2, s); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(path2)
	if string(b2) != text {
		t.Errorf("save not deterministic:\n--- first ---\n%s\n--- second ---\n%s", text, string(b2))
	}
}

func TestDefaultColorDeterministicAndValid(t *testing.T) {
	a := DefaultColor("biome:forest")
	b := DefaultColor("biome:forest")
	if a != b {
		t.Errorf("DefaultColor not deterministic: %q vs %q", a, b)
	}
	if !colorRe.MatchString(a) {
		t.Errorf("DefaultColor %q is not #rrggbb", a)
	}
	if DefaultColor("hero") == DefaultColor("villain") {
		t.Error("distinct labels should generally get distinct default colors")
	}
}

func TestDefineRejectsBadColor(t *testing.T) {
	s := New()
	if err := s.Define("t", "red"); err == nil {
		t.Error("expected error for non-hex color")
	}
	if err := s.Define("t", "#12345"); err == nil {
		t.Error("expected error for short hex")
	}
	if err := s.Define("", "#123456"); err == nil {
		t.Error("expected error for empty id")
	}
}
