package browse

import (
	"testing"

	"github.com/curbol/synty-sync/internal/assetindex"
)

func TestStripRootMotionToken(t *testing.T) {
	cases := []struct {
		base      string
		canonical string
		isRM      bool
	}{
		{"A_MOD_BL_Turn_Standing_180L_RM_Masc", "A_MOD_BL_Turn_Standing_180L_Masc", true}, // Synty infix
		{"UAL1_RM", "UAL1", true},   // Quaternius trailing
		{"1Hand_RM", "1Hand", true}, // explosive trailing
		{"A_MOD_BL_Turn_Standing_180L_Masc", "A_MOD_BL_Turn_Standing_180L_Masc", false},
		{"Warm_Idle", "Warm_Idle", false}, // "arm"/"rm" substrings are not the token
		{"Storm", "Storm", false},
	}
	for _, c := range cases {
		canon, isRM := stripRootMotionToken(c.base)
		if canon != c.canonical || isRM != c.isRM {
			t.Errorf("stripRootMotionToken(%q) = (%q,%v), want (%q,%v)", c.base, canon, isRM, c.canonical, c.isRM)
		}
	}
}

func TestBuildRootMotionPairs(t *testing.T) {
	animZip := func(id, entry, clip, ext string) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: ext, Vendor: "synty", Pack: "Loco", Category: assetindex.CategoryAnimation,
			Source: assetindex.Source{Kind: assetindex.SourceZip, ArchivePath: "a.zip", Entry: entry, Clip: clip}}
	}
	loose := func(id, fp, clip, ext string, cat assetindex.Category) assetindex.Asset {
		return assetindex.Asset{ID: id, Ext: ext, Vendor: "quaternius", Pack: "UAL", Category: cat,
			Source: assetindex.Source{Kind: assetindex.SourceLoose, FilePath: fp, Clip: clip}}
	}
	assets := []assetindex.Asset{
		animZip("n1", "SF/Turn_Masc.fbx", "", "fbx"),                              // Synty non-RM
		animZip("r1", "SF/Turn_RM_Masc.fbx", "", "fbx"),                           // its RM sibling
		loose("g1", "/lib/UAL1.glb", "Walk", "glb", assetindex.CategoryAnimation), // GLB non-RM clips
		loose("g2", "/lib/UAL1.glb", "Run", "glb", assetindex.CategoryAnimation),
		loose("grf", "/lib/UAL1_RM.fbx", "", "fbx", assetindex.CategoryModel),      // wrong-ext RM, listed first (as Unity/ sorts before Unreal-Godot/)
		loose("gr", "/lib/UAL1_RM.glb", "", "glb", assetindex.CategoryModel),       // the glb RM sibling
		loose("solo", "/lib/Idle.glb", "Sit", "glb", assetindex.CategoryAnimation), // no RM sibling
	}

	sibling, suppressed := buildRootMotionPairs(assets)

	if sibling["n1"] != "r1" {
		t.Errorf("Synty pair: n1 -> %q, want r1", sibling["n1"])
	}
	if sibling["g1"] != "gr" || sibling["g2"] != "gr" {
		t.Errorf("GLB clips must pair the glb RM (same ext), not the fbx RM: g1=%q g2=%q, want gr", sibling["g1"], sibling["g2"])
	}
	if _, ok := sibling["solo"]; ok {
		t.Error("an animation with no RM sibling must not be paired")
	}
	if !suppressed["r1"] || !suppressed["gr"] || !suppressed["grf"] {
		t.Errorf("all RM siblings must be suppressed: r1=%v gr=%v grf=%v", suppressed["r1"], suppressed["gr"], suppressed["grf"])
	}
	if suppressed["n1"] || suppressed["g1"] {
		t.Error("non-RM cards must never be suppressed")
	}
}
