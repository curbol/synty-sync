package browse

import (
	"path"
	"strings"

	"github.com/curbol/synty-sync/internal/assetindex"
)

// Root-motion pairing collapses each animation that ships in two variants — one with
// world travel baked into the root (an "_RM" file) and one that animates in place —
// into a single card. The in-place variant is the visible card; the browse lightbox's
// root-motion toggle loads the RM sibling to show the travel. The token is a Synty/
// Unreal convention: a trailing "_RM" (Quaternius/explosive GLBs) or a "_RM_" infix
// before a suffix like the gender (Synty FBX, "..._180L_RM_Masc").

// stripRootMotionToken removes the "_RM" token from a file base name, reporting
// whether it was present. It matches the token only bounded by "_" or the end, so
// substrings like "arm"/"Storm" are left alone.
func stripRootMotionToken(base string) (canonical string, isRM bool) {
	if strings.HasSuffix(base, "_RM") {
		return base[:len(base)-3], true
	}
	if i := strings.Index(base, "_RM_"); i >= 0 {
		return base[:i] + base[i+3:], true
	}
	return base, false
}

// assetFileBase is the extension-less base name of the file an asset lives in (the
// archive entry, unity pathname, or loose path), where the "_RM" token appears.
func assetFileBase(a assetindex.Asset) string {
	var name string
	switch a.Source.Kind {
	case assetindex.SourceZip:
		name = a.Source.Entry
	case assetindex.SourceUnityPackage:
		name = a.Source.Pathname
	default:
		name = a.Source.FilePath
	}
	name = path.Base(name)
	return strings.TrimSuffix(name, path.Ext(name))
}

// buildRootMotionPairs maps each in-place animation asset to its root-motion sibling
// (sibling: assetID -> RM assetID) and marks the RM assets that a non-RM sibling
// covers for suppression from the grid (suppressed: RM assetID -> true). Assets are
// grouped by (vendor, pack, canonical file base); a group with both variants pairs
// only when its visible side includes an animation, so an unrelated "_RM" file never
// hijacks a card. For each in-place asset the RM with the same clip is preferred, then
// a whole-file RM (the lightbox plays the in-place asset's clip name from it).
func buildRootMotionPairs(assets []assetindex.Asset) (sibling map[string]string, suppressed map[string]bool) {
	type group struct{ nonRM, rm []int }
	groups := map[string]*group{}
	for i := range assets {
		canon, isRM := stripRootMotionToken(assetFileBase(assets[i]))
		key := assets[i].Vendor + "\x00" + assets[i].Pack + "\x00" + canon
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		if isRM {
			g.rm = append(g.rm, i)
		} else {
			g.nonRM = append(g.nonRM, i)
		}
	}

	sibling = map[string]string{}
	suppressed = map[string]bool{}
	for _, g := range groups {
		if len(g.rm) == 0 || len(g.nonRM) == 0 {
			continue
		}
		hasAnim := false
		for _, i := range g.nonRM {
			if assets[i].Category == assetindex.CategoryAnimation {
				hasAnim = true
				break
			}
		}
		if !hasAnim {
			continue
		}
		for _, ni := range g.nonRM {
			if rmID := pickRM(assets, g.rm, assets[ni].Source.Clip); rmID != "" {
				sibling[assets[ni].ID] = rmID
			}
		}
		for _, ri := range g.rm {
			suppressed[assets[ri].ID] = true
		}
	}
	return sibling, suppressed
}

// pickRM chooses the RM asset for an in-place clip: the RM with the same clip (a
// per-clip RM file), else a whole-file RM (clip ""), else any.
func pickRM(assets []assetindex.Asset, rm []int, clip string) string {
	for _, ri := range rm {
		if assets[ri].Source.Clip == clip {
			return assets[ri].ID
		}
	}
	for _, ri := range rm {
		if assets[ri].Source.Clip == "" {
			return assets[ri].ID
		}
	}
	if len(rm) > 0 {
		return assets[rm[0]].ID
	}
	return ""
}
