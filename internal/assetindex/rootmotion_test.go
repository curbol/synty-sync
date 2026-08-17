package assetindex

import "testing"

func TestRootMotionVariant(t *testing.T) {
	cases := []struct {
		base      string
		canonical string
		isRM      bool
	}{
		{"UAL1_RM", "UAL1", true},   // Quaternius trailing (glb)
		{"1Hand_RM", "1Hand", true}, // explosive trailing (glb)
		{"A_MOD_BL_Turn_Standing_180L_RM_Masc", "A_MOD_BL_Turn_Standing_180L_Masc", true},     // Synty Sidekick infix
		{"HumanF@Crouch01_Walk_ForwardRight [RM]", "HumanF@Crouch01_Walk_ForwardRight", true}, // kevdev bracket
		{"HumanM@Roll01 [RM]", "HumanM@Roll01", true},                                         // kevdev bracket, no infix
		{"A_Dodge_L_RootMotion_Sword", "A_Dodge_L_Sword", true},                               // Synty Polygon RootMotion infix
		{"A_Jump_Idle_RootMotion_Femn", "A_Jump_Idle_Femn", true},                             // Synty RootMotion infix
		{"Idle_RootMotion", "Idle", true},                                                     // RootMotion suffix
		{"A_MOD_BL_Turn_Standing_180L_Masc", "A_MOD_BL_Turn_Standing_180L_Masc", false},       // the in-place sibling
		{"A_Dodge_L_Sword", "A_Dodge_L_Sword", false},                                         // the in-place sibling
		{"Warm_Idle", "Warm_Idle", false},                                                     // "arm"/"rm" substrings are not the token
		{"Storm", "Storm", false},
		{"A_Jump_Running_RootMotionVertical_Femn", "A_Jump_Running_RootMotionVertical_Femn", false}, // deliberately unmatched: ambiguous in-place target
	}
	for _, c := range cases {
		canon, isRM := RootMotionVariant(c.base)
		if canon != c.canonical || isRM != c.isRM {
			t.Errorf("RootMotionVariant(%q) = (%q,%v), want (%q,%v)", c.base, canon, isRM, c.canonical, c.isRM)
		}
	}
}
