package assetindex

import (
	"fmt"
	"io"
	"io/fs"
	"os"
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

// archiveAssets enumerates one archive, turning a read failure into a skip note
// instead of an error. A truncated or otherwise damaged file in a large library
// must not make the whole index unbuildable — browse treats a build failure as
// fatal, so one bad file would take the entire browser down with it.
func archiveAssets(e libEntry) ([]Asset, *SkippedFile) {
	a, err := enumerateArchive(e)
	if err != nil {
		return nil, &SkippedFile{RelPath: e.rel, Reason: err.Error()}
	}
	return a, nil
}

// looseAsset builds the single asset for a loose file entry, reading the file to
// compute its content fingerprint and, for an image, its pixel dimensions.
// Build/Refresh reuse a cached loose asset via LoosePrint so these reads only
// happen for new or changed files.
func looseAsset(e libEntry) Asset {
	a := newAsset(Source{Kind: SourceLoose, FilePath: e.path}, e.name, e.rel, e.vendor, e.pack, "", e.size, looseFingerprint(e.path))
	if isDimExt(a.Ext) {
		if f, err := os.Open(e.path); err == nil {
			a.Width, a.Height = imageDims(readHead(f), a.Ext)
			f.Close()
		}
	}
	return a
}

// isRootMotionVariant reports a root-motion sibling of an animation library (e.g.
// "UAL1_RM.glb" beside "UAL1.glb"). It is left whole so its clips don't duplicate the
// base file's; pairing the two as a root-motion toggle is a later concern.
func isRootMotionVariant(name string) bool {
	_, isRM := RootMotionVariant(strings.TrimSuffix(name, filepath.Ext(name)))
	return isRM
}

// clipAsset builds one virtual asset for a single embedded animation of a multi-clip
// model file. All clips of a file share its bytes (Source.FilePath); Source.Clip
// names which animation the preview plays. The content fingerprint combines the
// file's fingerprint with the clip name so each clip tags independently and stably.
func clipAsset(e libEntry, clip, fileFP string) Asset {
	s := Source{Kind: SourceLoose, FilePath: e.path, Clip: clip}
	fp := ""
	if fileFP != "" {
		fp = fileFP + "#" + clip
	}
	return Asset{
		ID:          id(s),
		Name:        clip,
		RelPath:     e.rel + "::" + clip,
		CopyPath:    e.path + "::" + clip,
		Category:    CategoryAnimation,
		Ext:         strings.ToLower(strings.TrimPrefix(filepath.Ext(e.name), ".")),
		Vendor:      e.vendor,
		Pack:        e.pack,
		Size:        e.size,
		Thumb:       ThumbGLB,
		Fingerprint: fp,
		Source:      s,
	}
}

// looseAssets builds the asset(s) for a loose file. A multi-animation .glb (a
// Quaternius-style animation library) is split into one virtual asset per embedded
// clip, all sharing the file's bytes; its root-motion (_RM) sibling is left whole.
// Everything else (including single-animation and unreadable GLBs) is one asset.
func looseAssets(e libEntry) []Asset {
	if strings.EqualFold(filepath.Ext(e.name), ".glb") && !isRootMotionVariant(e.name) {
		if names, err := glbAnimationNames(e.path); err == nil && len(names) >= 2 {
			names = uniqueClipNames(names)
			fp := looseFingerprint(e.path)
			out := make([]Asset, 0, len(names))
			for _, n := range names {
				out = append(out, clipAsset(e, n, fp))
			}
			return out
		}
	}
	return []Asset{looseAsset(e)}
}

// uniqueClipNames makes a GLB's animation names usable as identity. glTF names are
// optional and need not be unique, but the clip name is what distinguishes one
// virtual asset's id and fingerprint from another's, so duplicates would collide on
// both: two cards for the same clip, tagging either one tagging both.
func uniqueClipNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]int, len(names))
	for i, n := range names {
		if n == "" {
			n = fmt.Sprintf("clip %d", i+1)
		}
		if c, dup := seen[n]; dup {
			base := n
			for {
				c++
				n = fmt.Sprintf("%s (%d)", base, c)
				if _, taken := seen[n]; !taken {
					break
				}
			}
			seen[base] = c
		}
		seen[n] = 1
		out = append(out, n)
	}
	return out
}

// readHead reads up to dimsHeadBytes from r, enough to recover an image's
// dimensions without pulling a whole file into memory.
func readHead(r io.Reader) []byte {
	head, _ := io.ReadAll(io.LimitReader(r, dimsHeadBytes))
	return head
}

// Scan walks the library root and returns every browseable asset: loose files and
// the entries inside .zip / .unitypackage archives, de-duplicated (see dedup).
// Unreadable archives are skipped; Build reports why in Index.Skipped.
func Scan(root string) ([]Asset, error) {
	ix, err := Build(root, "")
	if err != nil {
		return nil, err
	}
	return ix.Assets, nil
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
