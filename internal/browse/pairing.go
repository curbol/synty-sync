package browse

import (
	"path"
	"strings"

	"github.com/curbol/synty-sync/internal/assetindex"
)

// Root-motion pairing collapses each animation that ships in two variants — one with
// world travel baked into the root (a root-motion file) and one that animates in place —
// into a single card. The in-place variant is the visible card; the browse lightbox's
// root-motion toggle loads the RM sibling to show the travel. Which file base names are
// root-motion variants is decided by assetindex.RootMotionVariant, the shared recognizer.

// assetFileBase is the extension-less base name of the file an asset lives in (the
// archive entry, unity pathname, or loose path), where the root-motion token appears.
func assetFileBase(s assetindex.Source) string {
	var name string
	switch s.Kind {
	case assetindex.SourceZip:
		name = s.Entry
	case assetindex.SourceUnityPackage:
		name = s.Pathname
	default:
		name = s.FilePath
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
		canon, isRM := assetindex.RootMotionVariant(assetFileBase(assets[i].Source))
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
			if rmID := pickRM(assets, g.rm, assets[ni]); rmID != "" {
				sibling[assets[ni].ID] = rmID
			}
		}
		for _, ri := range g.rm {
			suppressed[assets[ri].ID] = true
		}
	}
	return sibling, suppressed
}

// pickRM chooses the RM sibling for an in-place asset. It prefers a sibling with the
// same file extension (a glb clip's travel is the glb RM, not the fbx RM of the same
// library shipped in the same pack — loading the wrong container fails), then the same
// clip (a per-clip RM over a whole-file one).
func pickRM(assets []assetindex.Asset, rm []int, nonRM assetindex.Asset) string {
	best, bestScore := "", -1
	for _, ri := range rm {
		r := assets[ri]
		score := 0
		if r.Ext == nonRM.Ext {
			score += 2
		}
		if r.Source.Clip == nonRM.Source.Clip {
			score++
		}
		if score > bestScore {
			best, bestScore = r.ID, score
		}
	}
	return best
}
