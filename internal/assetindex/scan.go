package assetindex

import (
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// versionSuffix matches a trailing Synty version token like "_v3" or "_v1_1_3".
var versionSuffix = regexp.MustCompile(`_v[0-9][0-9_]*$`)

// deriveVariant extracts the engine/format token from a Synty archive filename,
// whose base name is prefixed by its pack dir and suffixed by a version token:
// "<packDir>_<variant>_v<ver>.<ext>" → "<variant>". Returns "" when the
// convention doesn't hold (e.g. kevdev "Human Basic Motions.zip"), leaving the
// asset in the unknown-variant facet bucket.
func deriveVariant(packDir, filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	prefix := packDir + "_"
	if !strings.HasPrefix(base, prefix) {
		return ""
	}
	return versionSuffix.ReplaceAllString(base[len(prefix):], "")
}

// isSidecar reports extensions that are engine bookkeeping, not browseable assets
// (Unity .meta, Godot .import), so they never clutter the index.
func isSidecar(ext string) bool {
	return ext == "meta" || ext == "import"
}

// skipEntry reports archive entries that aren't browseable assets: dot-files
// (.editorconfig, .gitignore, …) and engine sidecars, matching the loose-file walk.
func skipEntry(name string) bool {
	base := path.Base(name)
	if strings.HasPrefix(base, ".") {
		return true
	}
	return isSidecar(strings.ToLower(strings.TrimPrefix(path.Ext(base), ".")))
}

// libEntry is one file discovered by walking the library: either an archive (to
// be enumerated into many assets) or a loose file (one asset). Sidecars, dot-files
// and dot-dirs are already filtered out.
type libEntry struct {
	kind    SourceKind // SourceZip / SourceUnityPackage / SourceLoose
	path    string     // absolute
	rel     string     // root-relative, slash form
	vendor  string
	pack    string
	name    string
	variant string // archives only
	size    int64  // loose only
}

// walkLibrary enumerates the browseable files under absRoot without opening any
// archive, so callers can decide per archive whether to re-enumerate or reuse a
// cached result. Dot-dirs (Synty working dirs) and engine sidecars are skipped.
func walkLibrary(absRoot string) ([]libEntry, error) {
	var entries []libEntry
	err := filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != absRoot && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		if isSidecar(ext) {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, absRoot+string(filepath.Separator)))
		vendor, pack := vendorPack(rel)
		e := libEntry{path: p, rel: rel, vendor: vendor, pack: pack, name: name}
		switch ext {
		case "zip":
			e.kind, e.variant = SourceZip, deriveVariant(pack, name)
		case "unitypackage":
			e.kind, e.variant = SourceUnityPackage, deriveVariant(pack, name)
		default:
			info, err := d.Info()
			if err != nil {
				return err
			}
			e.kind, e.size = SourceLoose, info.Size()
		}
		entries = append(entries, e)
		return nil
	})
	return entries, err
}

// enumerateArchive opens one archive entry and returns its assets.
func enumerateArchive(e libEntry) ([]Asset, error) {
	switch e.kind {
	case SourceZip:
		return zipAssets(e.path, e.rel, e.vendor, e.pack, e.variant)
	case SourceUnityPackage:
		return unityAssets(e.path, e.rel, e.vendor, e.pack, e.variant)
	}
	return nil, nil
}

// looseAsset builds the single asset for a loose file entry, reading the file to
// compute its content fingerprint. Build/Refresh reuse a cached loose asset via
// LoosePrint so this read only happens for new or changed files.
func looseAsset(e libEntry) Asset {
	return newAsset(Source{Kind: SourceLoose, FilePath: e.path}, e.name, e.rel, e.vendor, e.pack, "", e.size, looseFingerprint(e.path))
}

// Scan walks the library root and returns every browseable asset: loose files and
// the entries inside .zip / .unitypackage archives, de-duplicated (see dedup).
func Scan(root string) ([]Asset, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	entries, err := walkLibrary(absRoot)
	if err != nil {
		return nil, err
	}
	var assets []Asset
	for _, e := range entries {
		if e.kind == SourceLoose {
			assets = append(assets, looseAsset(e))
			continue
		}
		a, err := enumerateArchive(e)
		if err != nil {
			return nil, err
		}
		assets = append(assets, a...)
	}
	return dedup(assets), nil
}

// vendorPack derives the vendor (first path segment) and pack (second segment) of
// a root-relative path. A file directly under a vendor has no pack.
func vendorPack(rel string) (vendor, pack string) {
	segs := strings.Split(rel, "/")
	vendor = segs[0]
	if len(segs) >= 3 {
		pack = segs[1]
	}
	return vendor, pack
}

// dedup drops an archive entry when a loose file in the same pack matches it at the
// same pack-relative subpath (normalizing a leading "src/") and size. Keying on the
// full subpath, never the bare basename, is required: two genuinely-distinct files
// can share a basename+size across packs or subtrees. Only archive-vs-loose is
// de-duplicated; two archive variants of the same asset are kept (distinct
// deliverables, distinguished by the variant facet).
func dedup(assets []Asset) []Asset {
	looseKeys := make(map[string]struct{})
	for i := range assets {
		if assets[i].Source.Kind == SourceLoose {
			looseKeys[looseDedupKey(assets[i])] = struct{}{}
		}
	}
	out := assets[:0]
	for _, a := range assets {
		if a.Source.Kind != SourceLoose {
			if _, ok := looseKeys[archiveDedupKey(a)]; ok {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

func normSubpath(s string) string { return strings.TrimPrefix(s, "src/") }

func dedupKey(vendor, pack, subpath string, size int64) string {
	return vendor + "\x00" + pack + "\x00" + normSubpath(subpath) + "\x00" + strconv.FormatInt(size, 10)
}

func looseDedupKey(a Asset) string {
	within := strings.TrimPrefix(a.RelPath, a.Vendor+"/"+a.Pack+"/")
	return dedupKey(a.Vendor, a.Pack, within, a.Size)
}

func archiveDedupKey(a Asset) string {
	entry := a.Source.Entry
	if a.Source.Kind == SourceUnityPackage {
		entry = a.Source.Pathname
	}
	return dedupKey(a.Vendor, a.Pack, entry, a.Size)
}
