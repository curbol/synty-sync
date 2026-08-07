package assetindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// indexVersion is bumped whenever the scan logic changes (what's indexed, how it's
// classified), so a cached index from older logic is rebuilt rather than reused.
const indexVersion = 6

// Index is the in-memory, on-disk-cacheable catalog of a library. Content requests
// resolve through byID (never by reconstructing a path from client input), and the
// unpacked-archive cache under CacheDir is keyed by each archive's fingerprint so a
// changed archive re-extracts.
type Index struct {
	Version      int               `json:"version"`
	Root         string            `json:"root"`
	Assets       []Asset           `json:"assets"`
	ArchivePrint map[string]string `json:"archivePrint"` // abs archive path -> fingerprint

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
	ix := &Index{Version: indexVersion, Root: absRoot, cacheDir: cacheDir, ArchivePrint: map[string]string{}}
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
		if fp, err := fingerprint(e.path); err == nil {
			ix.ArchivePrint[e.path] = fp
		}
		assets = append(assets, a...)
	}
	ix.setAssets(dedup(assets))
	return ix, nil
}

// Refresh re-walks the library, reusing the cached enumeration of every archive
// whose fingerprint is unchanged and re-enumerating only changed or new archives;
// loose files are always re-derived (cheap). This avoids re-decompressing every
// unitypackage on each run.
func (ix *Index) Refresh() error {
	entries, err := walkLibrary(ix.Root)
	if err != nil {
		return err
	}
	oldByArchive := map[string][]Asset{}
	for _, a := range ix.Assets {
		if a.Source.Kind != SourceLoose {
			oldByArchive[a.Source.ArchivePath] = append(oldByArchive[a.Source.ArchivePath], a)
		}
	}
	newPrint := map[string]string{}
	var assets []Asset
	for _, e := range entries {
		if e.kind == SourceLoose {
			assets = append(assets, looseAsset(e))
			continue
		}
		fp, err := fingerprint(e.path)
		if err != nil {
			return err
		}
		newPrint[e.path] = fp
		if old, ok := oldByArchive[e.path]; ok && ix.ArchivePrint[e.path] == fp {
			assets = append(assets, old...)
			continue
		}
		a, err := enumerateArchive(e)
		if err != nil {
			return err
		}
		assets = append(assets, a...)
	}
	ix.ArchivePrint = newPrint
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
	ix.setAssets(ix.Assets)
	return &ix, nil
}

// Save writes the index JSON, creating parent dirs.
func (ix *Index) Save(cachePath string) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	return os.WriteFile(cachePath, b, 0o644)
}

// LoadOrBuild returns a usable index: a fresh build when reindex is set or no valid
// cache exists for this root, otherwise the cached index refreshed against the
// current tree. The result is written back to cachePath.
func LoadOrBuild(root, cacheDir, cachePath string, reindex bool) (*Index, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if !reindex {
		if ix, err := Load(cachePath, cacheDir); err == nil && ix.Root == absRoot && ix.Version == indexVersion {
			if err := ix.Refresh(); err != nil {
				return nil, err
			}
			_ = ix.Save(cachePath)
			return ix, nil
		}
	}
	ix, err := Build(absRoot, cacheDir)
	if err != nil {
		return nil, err
	}
	_ = ix.Save(cachePath)
	return ix, nil
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
