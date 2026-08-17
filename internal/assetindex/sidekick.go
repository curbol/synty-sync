package assetindex

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path"
	"strings"
)

// parseSidekick reads a Synty Sidekick character definition (.sk): a top-level
// "Name:" naming the character and a top-level "Parts:" block of "- Name:" entries
// naming the body-part meshes it assembles. Only the Parts block is collected — a
// later top-level key (ColorSet, BlendShapes) ends it, so a "Name" nested under one
// of those never leaks in. The format is YAML-shaped but shallow, so a line scanner
// suffices and avoids a YAML dependency.
func parseSidekick(data []byte) (name string, parts []string) {
	inParts := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		if c := line[0]; c == ' ' || c == '\t' || c == '-' {
			if inParts {
				if t := strings.TrimSpace(line); strings.HasPrefix(t, "- Name:") {
					parts = append(parts, strings.TrimSpace(t[len("- Name:"):]))
				}
			}
			continue
		}
		// A top-level key ends any open Parts block, then may open a new one.
		inParts = false
		switch {
		case name == "" && strings.HasPrefix(line, "Name:"):
			name = strings.TrimSpace(line[len("Name:"):])
		case strings.TrimSpace(line) == "Parts:":
			inParts = true
		}
	}
	return name, parts
}

// applySidekick upgrades the .sk entries of a Synty Sidekick modular-character package
// into assembled-character assets. Each .sk lists the body parts of one character; those
// parts are separate FBX meshes in the same package that share one skeleton. The upgraded
// asset takes the character's name, renders as ThumbSidekick, and carries the parts' ids
// so the frontend can load and merge them. Its own id and fingerprint stay tied to the
// .sk guid, so tags survive. Packages with no .sk entry are returned untouched (the
// common case, so the extra decompress pass only runs for Sidekick packs).
func applySidekick(archivePath string, entries map[string]*unityEntry, order []string, assets []Asset) ([]Asset, error) {
	skGuids := map[string]bool{}
	fbxByBase := map[string]string{}
	for _, g := range order {
		p := entries[g].pathname
		switch strings.ToLower(strings.TrimPrefix(path.Ext(p), ".")) {
		case "sk":
			skGuids[g] = true
		case "fbx":
			fbxByBase[strings.TrimSuffix(path.Base(p), path.Ext(p))] = g
		}
	}
	if len(skGuids) == 0 {
		return assets, nil
	}
	skBytes, err := readUnityAssetBytes(archivePath, skGuids)
	if err != nil {
		return nil, err
	}
	byGuid := make(map[string]int, len(assets))
	for i := range assets {
		if assets[i].Source.Kind == SourceUnityPackage {
			byGuid[assets[i].Source.Guid] = i
		}
	}
	// Only a character that actually assembled supersedes its byproducts; the trees
	// rooted at those characters bound the suppression below.
	var assembledTrees []string
	for g := range skGuids {
		data, ok := skBytes[g]
		if !ok {
			continue
		}
		name, partNames := parseSidekick(data)
		var partIDs []string
		for _, pn := range partNames {
			if pg, ok := fbxByBase[pn]; ok {
				partIDs = append(partIDs, id(Source{Kind: SourceUnityPackage, ArchivePath: archivePath, Guid: pg}))
			}
		}
		i, ok := byGuid[g]
		if !ok || len(partIDs) == 0 {
			continue
		}
		if name != "" {
			assets[i].Name = name
		}
		assets[i].Category = CategoryModel
		assets[i].Thumb = ThumbSidekick
		assets[i].Source.Parts = partIDs
		assembledTrees = append(assembledTrees, path.Dir(assets[i].Source.Pathname)+"/")
	}
	kept := assets[:0]
	for _, a := range assets {
		if !sidekickByproduct(a, assembledTrees) {
			kept = append(kept, a)
		}
	}
	return kept, nil
}

// sidekickByproduct reports a per-character byproduct that its assembled .sk
// character supersedes: the magenta prefab, its material, and the combined-mesh /
// avatar .asset data, which sit in the character's own tree (directly beside the
// .sk or under its Materials/ and Meshes/ subdirs). The reusable part meshes live
// under Resources/ and the character's textures are a kept extension, so both stay
// browseable. A character that failed to assemble contributes no tree, so its
// byproducts survive — they are the only representation it has left.
func sidekickByproduct(a Asset, assembledTrees []string) bool {
	if a.Thumb == ThumbSidekick {
		return false
	}
	switch a.Ext {
	case "prefab", "mat", "asset":
	default:
		return false
	}
	for _, tree := range assembledTrees {
		if strings.HasPrefix(a.Source.Pathname, tree) {
			return true
		}
	}
	return false
}

// readUnityAssetBytes streams a .unitypackage once and returns the full `asset` payload
// of each requested GUID. Used for the few small .sk files a Sidekick package holds,
// whose full bytes the enumeration pass (which buffers only a head) doesn't retain.
func readUnityAssetBytes(archivePath string, want map[string]bool) (map[string][]byte, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	out := make(map[string][]byte, len(want))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		guid, member, ok := splitUnityName(hdr.Name)
		if !ok || member != "asset" || !want[guid] {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[guid] = b
	}
}
