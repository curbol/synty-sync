package browse

import (
	"net/url"
	"sort"
	"testing"

	"github.com/curbol/synty-sync/internal/assetindex"
)

func TestSearchQueryMatch(t *testing.T) {
	anim := assetindex.Asset{
		Name:     "HumanF@TurnLeft_Loop01.fbx",
		Pack:     "KevDev Anims",
		RelPath:  "kevdev/anims.zip::HumanF@TurnLeft_Loop01.fbx",
		Vendor:   "kevdev",
		Category: assetindex.CategoryAnimation,
		Ext:      "fbx",
		Variant:  "SourceFiles",
	}
	dash := assetindex.Asset{
		Name:     "Explosive@Dash-Left.fbx",
		Pack:     "RPG Combat",
		RelPath:  "explosive/rpg.zip::Explosive@Dash-Left.fbx",
		Vendor:   "explosive",
		Category: assetindex.CategoryAnimation,
		Ext:      "fbx",
	}

	cases := []struct {
		query string
		asset assetindex.Asset
		want  bool
	}{
		{"", anim, true},
		{"   ", anim, true},
		{"turn", anim, true},
		{"TURN", anim, true},
		{"walk", anim, false},

		{"turn loop", anim, true},
		{"turn jump", anim, false},
		{"turnleft loop01", anim, true},

		{"loop OR jump", anim, true},
		{"jump OR walk", anim, false},
		{"jump | loop", anim, true},

		{"turn -idle", anim, true},
		{"turn -loop", anim, false},

		{`"TurnLeft_Loop01"`, anim, true},
		{`"Loop01_TurnLeft"`, anim, false},

		{"vendor:kevdev", anim, true},
		{"vendor:synty", anim, false},
		{"type:animation", anim, true},
		{"type:model", anim, false},
		{"ext:fbx", anim, true},
		{"ext:glb", anim, false},
		{"variant:sourcefiles", anim, true},

		{"anims", anim, true},
		{"zip", anim, true},

		{"(jump OR turn) loop", anim, true},
		{"(jump OR walk) loop", anim, false},

		{"dash-left", dash, true},
		{"dash -left", dash, false},
		{`pack:"RPG Combat"`, dash, true},
		{"-vendor:synty", dash, true},
		{"-vendor:explosive", dash, false},
	}

	for _, c := range cases {
		got := parseQuery(c.query).match(c.asset)
		if got != c.want {
			t.Errorf("parseQuery(%q).match(%q) = %v, want %v", c.query, c.asset.Name, got, c.want)
		}
	}
}

// TestSearchQueryEndpoint drives the query language through /api/assets against
// the fixture library (Heart.fbx/png/prefab, Rock.fbx, Sword.glb).
func TestSearchQueryEndpoint(t *testing.T) {
	srv := testServer(t)
	names := func(q string) []string {
		r := getAssets(t, srv, "q="+url.QueryEscape(q))
		var out []string
		for _, it := range r.Items {
			out = append(out, it.Name)
		}
		sort.Strings(out)
		return out
	}
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	cases := []struct {
		query string
		want  []string
	}{
		{"heart", []string{"Heart.fbx", "Heart.png", "Heart.prefab"}},
		{"heart -png", []string{"Heart.fbx", "Heart.prefab"}},
		{"vendor:explosive", []string{"Sword.glb"}},
		{"ext:glb", []string{"Sword.glb"}},
		{"rock OR sword", []string{"Rock.fbx", "Sword.glb"}},
		{"(rock OR sword) vendor:synty", []string{"Rock.fbx"}},
	}
	for _, c := range cases {
		if got := names(c.query); !eq(got, c.want) {
			t.Errorf("q=%q returned %v, want %v", c.query, got, c.want)
		}
	}
}

func TestSearchQueryEmptyIsNil(t *testing.T) {
	if parseQuery("").match(assetindex.Asset{Name: "anything"}) != true {
		t.Error("empty query must match everything")
	}
}

// An empty conjunction must contribute nothing, not truth. Search is debounced, so
// every OR query passes through its half-typed form on the way to being complete;
// if that matches everything, the grid floods with the whole library mid-keystroke.
func TestMalformedQueriesDoNotMatchEverything(t *testing.T) {
	unrelated := assetindex.Asset{Name: "barrel", Pack: "polygon-dungeon", Category: "props"}
	// A real term OR'd with an empty branch must not widen to everything.
	for _, q := range []string{"sword OR ", "sword |", "sword OR", "(sword OR )", "sword OR ()"} {
		if parseQuery(q).match(unrelated) {
			t.Errorf("query %q matched an unrelated asset", q)
		}
	}
	// The terms that are present still have to work.
	sword := assetindex.Asset{Name: "sword", Pack: "polygon-fantasy", Category: "models"}
	for _, q := range []string{"sword OR ", "sword |", "sword OR axe"} {
		if !parseQuery(q).match(sword) {
			t.Errorf("query %q dropped the term it did have", q)
		}
	}
	// A query carrying no terms at all is no filter, whether it is blank or the user
	// has typed only operators so far.
	for _, q := range []string{"", "   ", "OR", "|", ")", "()"} {
		if !parseQuery(q).match(unrelated) {
			t.Errorf("term-free query %q should be treated as no filter", q)
		}
	}
}
