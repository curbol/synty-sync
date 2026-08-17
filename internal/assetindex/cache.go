package assetindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// indexVersion is bumped whenever the scan logic changes (what's indexed, how it's
// classified), so a cached index from older logic is rebuilt rather than reused.
const indexVersion = 13

// SkippedFile records a library file the scan could not read. A damaged archive
// costs its own contents, not the rest of the library, so the failure is carried
// here for the caller to report instead of aborting the build.
type SkippedFile struct {
	RelPath string `json:"relPath"`
	Reason  string `json:"reason"`
}

// Index is the in-memory, on-disk-cacheable catalog of a library. Content requests
// resolve through byID (never by reconstructing a path from client input), and the
// unpacked-archive cache under CacheDir is keyed by each archive's fingerprint so a
// changed archive re-extracts.
type Index struct {
	Version      int               `json:"version"`
	Root         string            `json:"root"`
	Assets       []Asset           `json:"assets"`
	ArchivePrint map[string]string `json:"archivePrint"` // abs archive path -> stat fingerprint
	LoosePrint   map[string]string `json:"loosePrint"`   // abs loose path -> stat fingerprint
	Skipped      []SkippedFile     `json:"skipped,omitempty"`

	cacheDir string
	byID     map[string]*Asset

	extractMu   sync.Mutex
	extractOnce map[string]*sync.Once
	extractErr  map[string]error
}

// fingerprint identifies a file by path, size, and mtime, so a re-download or edit
// invalidates any cached enumeration or extraction of it.
func fingerprint(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	key := path + "\x00" + strconv.FormatInt(fi.Size(), 10) + "\x00" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12]), nil
}

// Build scans a library from scratch into a fresh index.
func Build(root, cacheDir string) (*Index, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	entries, err := walkLibrary(absRoot)
	if err != nil {
		return nil, err
	}
	ix := &Index{Version: indexVersion, Root: absRoot, cacheDir: cacheDir, ArchivePrint: map[string]string{}, LoosePrint: map[string]string{}}
	var assets []Asset
	for _, e := range entries {
		if e.kind == SourceLoose {
			if fp, err := fingerprint(e.path); err == nil {
				ix.LoosePrint[e.path] = fp
			}
			assets = append(assets, looseAssets(e)...)
			continue
		}
		a, skip := archiveAssets(e)
		if skip != nil {
			ix.Skipped = append(ix.Skipped, *skip)
			continue
		}
		if fp, err := fingerprint(e.path); err == nil {
			ix.ArchivePrint[e.path] = fp
		}
		assets = append(assets, a...)
	}
	ix.setAssets(dedup(assets))
	return ix, nil
}

// Refresh re-walks the library, reusing the cached enumeration of every archive
// and the cached fingerprint of every loose file whose stat fingerprint is
// unchanged, re-deriving only changed or new files. This avoids re-decompressing
// every unitypackage and re-reading every loose file's bytes on each run.
func (ix *Index) Refresh() error {
	entries, err := walkLibrary(ix.Root)
	if err != nil {
		return err
	}
	oldByArchive := map[string][]Asset{}
	oldByLoose := map[string][]Asset{}
	for _, a := range ix.Assets {
		if a.Source.Kind == SourceLoose {
			oldByLoose[a.Source.FilePath] = append(oldByLoose[a.Source.FilePath], a)
		} else {
			oldByArchive[a.Source.ArchivePath] = append(oldByArchive[a.Source.ArchivePath], a)
		}
	}
	newPrint := map[string]string{}
	newLoose := map[string]string{}
	var assets []Asset
	var skipped []SkippedFile
	for _, e := range entries {
		if e.kind == SourceLoose {
			if fp, err := fingerprint(e.path); err == nil {
				newLoose[e.path] = fp
				if old, ok := oldByLoose[e.path]; ok && ix.LoosePrint[e.path] == fp {
					assets = append(assets, old...)
					continue
				}
			}
			assets = append(assets, looseAssets(e)...)
			continue
		}
		fp, err := fingerprint(e.path)
		if err != nil {
			skipped = append(skipped, SkippedFile{RelPath: e.rel, Reason: err.Error()})
			continue
		}
		newPrint[e.path] = fp
		if old, ok := oldByArchive[e.path]; ok && ix.ArchivePrint[e.path] == fp {
			assets = append(assets, old...)
			continue
		}
		a, skip := archiveAssets(e)
		if skip != nil {
			delete(newPrint, e.path)
			skipped = append(skipped, *skip)
			continue
		}
		assets = append(assets, a...)
	}
	ix.ArchivePrint = newPrint
	ix.LoosePrint = newLoose
	ix.Skipped = skipped
	ix.setAssets(dedup(assets))
	return nil
}

// Load reads a cached index from disk and rebuilds its id lookup.
func Load(cachePath, cacheDir string) (*Index, error) {
	b, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}
	var ix Index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, err
	}
	ix.cacheDir = cacheDir
	if ix.ArchivePrint == nil {
		ix.ArchivePrint = map[string]string{}
	}
	if ix.LoosePrint == nil {
		ix.LoosePrint = map[string]string{}
	}
	ix.setAssets(ix.Assets)
	return &ix, nil
}

// Save writes the index JSON, creating parent dirs. The write goes to a temp file
// in the destination dir and is renamed into place: an interrupted in-place write
// would leave a truncated cache, and rebuilding one costs a full library scan.
func (ix *Index) Save(cachePath string) error {
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".browse-index-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// LoadOrBuild returns a usable index: a fresh build when reindex is set or no valid
// cache exists for this root, otherwise the cached index refreshed against the
// current tree. The result is written back to cachePath.
// warn reports a non-fatal condition; nil discards.
func LoadOrBuild(root, cacheDir, cachePath string, reindex bool, warn func(string)) (*Index, error) {
	if warn == nil {
		warn = func(string) {}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if !reindex {
		if ix, err := Load(cachePath, cacheDir); err == nil && ix.Root == absRoot && ix.Version == indexVersion {
			if err := ix.Refresh(); err != nil {
				return nil, err
			}
			saveCache(ix, cachePath, warn)
			return ix, nil
		}
	}
	ix, err := Build(absRoot, cacheDir)
	if err != nil {
		return nil, err
	}
	saveCache(ix, cachePath, warn)
	return ix, nil
}

// saveCache persists the index. The cache is expendable and the index in hand is
// usable without it, so a write failure is not fatal — but it is not swallowed
// either: silently failing here re-pays a whole library scan on every run.
func saveCache(ix *Index, cachePath string, warn func(string)) {
	if err := ix.Save(cachePath); err != nil {
		warn(fmt.Sprintf("could not write the index cache (%v); the library will be rescanned next run", err))
	}
}

func (ix *Index) setAssets(assets []Asset) {
	ix.Assets = assets
	ix.byID = make(map[string]*Asset, len(assets))
	for i := range ix.Assets {
		ix.byID[ix.Assets[i].ID] = &ix.Assets[i]
	}
}

// Lookup resolves an asset id to its asset. This is the only path from a client id
// to a locator, so an unknown id simply misses.
func (ix *Index) Lookup(id string) (Asset, bool) {
	a, ok := ix.byID[id]
	if !ok {
		return Asset{}, false
	}
	return *a, true
}

// Vendors, Variants, Categories return the distinct facet values with counts, for
// the filter UI.
func (ix *Index) facet(get func(Asset) string) map[string]int {
	m := map[string]int{}
	for _, a := range ix.Assets {
		m[get(a)]++
	}
	return m
}

func (ix *Index) Vendors() map[string]int { return ix.facet(func(a Asset) string { return a.Vendor }) }
func (ix *Index) Variants() map[string]int {
	return ix.facet(func(a Asset) string { return a.Variant })
}
func (ix *Index) Categories() map[string]int {
	return ix.facet(func(a Asset) string { return string(a.Category) })
}
