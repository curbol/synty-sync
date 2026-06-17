// Package syncer orchestrates a sync: enumerate the library, filter variants,
// dedup files by fileId, diff against the lockfile and cache, download the delta,
// and write the new lockfile. status is the same flow stopped before downloads.
package syncer

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/curbol/hexed-haven/tools/synty/internal/cache"
	"github.com/curbol/hexed-haven/tools/synty/internal/lockfile"
	"github.com/curbol/hexed-haven/tools/synty/internal/model"
	"github.com/curbol/hexed-haven/tools/synty/internal/portal"
)

// Class is the diff outcome for one selected file.
type Class int

const (
	Unchanged Class = iota
	New
	Changed
	DownloadNow  // owned, was filtered out before, now selected
	CacheMissing // tracked + version matches, but absent/corrupt on disk
)

func (c Class) NeedsDownload() bool { return c != Unchanged }

func (c Class) String() string {
	switch c {
	case New:
		return "new"
	case Changed:
		return "changed"
	case DownloadNow:
		return "download-now"
	case CacheMissing:
		return "cache-missing"
	default:
		return "unchanged"
	}
}

// classify decides the outcome for a file given its prior lockfile record (looked
// up by fileId) and a cache check. Pure and unit-tested.
func classify(av model.FileEntry, prior lockfile.File, hasPrior bool, cacheOK func(relPath, sha string) bool) Class {
	switch {
	case !hasPrior:
		return New
	case !prior.Tracked:
		return DownloadNow
	case prior.Version != av.Version:
		return Changed
	case prior.CachePath == "" || !cacheOK(prior.CachePath, prior.SHA256):
		return CacheMissing
	default:
		return Unchanged
	}
}

// FileDiff reports one selected file's classification.
type FileDiff struct {
	PackSlug string
	Key      string
	FileID   int
	Class    Class
}

// Report summarizes a run.
type Report struct {
	Diffs       []FileDiff
	Downloaded  []FileDiff
	Warnings    []string
	NewLockfile lockfile.Lockfile
}

// Options configures a run.
type Options struct {
	LibraryRoot string
	Filter      func(model.Variant) bool
	OnlyGlob    string // optional pack-slug glob; empty = all
	DryRun      bool   // status: classify only, no downloads, no save
	FullVerify  bool   // sha-verify cache (sync) vs presence-only (status)
	Concurrency int
	Now         string // timestamp for generatedAt/downloadedAt
}

type resolved struct {
	cachePath string
	sha       string
	size      int64
	now       bool
}

// Run executes a sync (or status when DryRun) and returns a Report. The lockfile
// is saved only on a non-dry run.
func Run(ctx context.Context, c *portal.Client, lf lockfile.Lockfile, lockPath string, opts Options) (Report, error) {
	packs, err := c.Enumerate(ctx)
	if err != nil {
		return Report{}, err
	}
	packs = filterPacks(packs, opts.OnlyGlob)

	packFiles, err := fetchAll(ctx, c, packs, opts.Concurrency)
	if err != nil {
		return Report{}, err
	}

	priorByID := indexByFileID(lf)
	cacheOK := cacheChecker(opts)

	// Group selected files by fileId for dedup.
	type sel struct {
		pack model.Pack
		file model.FileEntry
	}
	selectedByID := map[int][]sel{}
	var selOrder []int
	for _, pf := range packFiles {
		for _, f := range pf.files {
			if !opts.Filter(f.Variant) || f.Archived {
				continue
			}
			if _, seen := selectedByID[f.FileID]; !seen {
				selOrder = append(selOrder, f.FileID)
			}
			selectedByID[f.FileID] = append(selectedByID[f.FileID], sel{pf.pack, f})
		}
	}

	report := Report{NewLockfile: lockfile.Lockfile{GeneratedAt: opts.Now, Packs: map[string]lockfile.Pack{}}}
	resolvedByID := map[int]resolved{}

	for _, id := range selOrder {
		group := selectedByID[id]
		rep := group[0].file
		prior, hasPrior := priorByID[id]
		class := classify(rep, prior, hasPrior, cacheOK)

		fd := FileDiff{PackSlug: rep.PackSlug, Key: rep.Key(), FileID: id, Class: class}
		report.Diffs = append(report.Diffs, fd)

		switch {
		case class == Unchanged:
			resolvedByID[id] = resolved{cachePath: prior.CachePath, sha: prior.SHA256, size: prior.SizeBytes}
		case opts.DryRun:
			// classify only; nothing resolved
		default:
			r, err := download(ctx, c, opts.LibraryRoot, rep)
			if err != nil {
				return Report{}, fmt.Errorf("download %s: %w", rep.Key(), err)
			}
			if class == Changed && prior.CachePath != "" && prior.CachePath != r.cachePath {
				_ = cache.Remove(opts.LibraryRoot, prior.CachePath)
			}
			resolvedByID[id] = r
			report.Downloaded = append(report.Downloaded, fd)
		}
	}

	buildLockfile(&report, packFiles, opts, resolvedByID, lf)
	report.Warnings = warnings(packFiles, opts.Filter)

	if !opts.DryRun {
		if err := lockfile.Save(lockPath, report.NewLockfile); err != nil {
			return Report{}, err
		}
	}
	return report, nil
}

type packWithFiles struct {
	pack  model.Pack
	files []model.FileEntry
}

func fetchAll(ctx context.Context, c *portal.Client, packs []model.Pack, concurrency int) ([]packWithFiles, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	out := make([]packWithFiles, len(packs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i, p := range packs {
		wg.Add(1)
		go func(i int, p model.Pack) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			files, err := c.ItemFiles(ctx, p)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("item page for %s: %w", p.Slug, err)
				}
				mu.Unlock()
				return
			}
			out[i] = packWithFiles{pack: p, files: files}
		}(i, p)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func download(ctx context.Context, c *portal.Client, libraryRoot string, f model.FileEntry) (resolved, error) {
	body, filename, err := c.Resolve(ctx, f)
	if err != nil {
		return resolved{}, err
	}
	defer body.Close()
	rel, sha, size, err := cache.Store(libraryRoot, f.FileToken, filename, body)
	if err != nil {
		return resolved{}, err
	}
	return resolved{cachePath: rel, sha: sha, size: size, now: true}, nil
}

func buildLockfile(report *Report, packFiles []packWithFiles, opts Options, resolvedByID map[int]resolved, prev lockfile.Lockfile) {
	prevByID := indexByFileID(prev)
	for _, pf := range packFiles {
		lp := lockfile.Pack{
			DisplayName: pf.pack.DisplayName,
			OrderID:     pf.pack.OrderID,
			OrderItemID: pf.pack.OrderItemID,
			Files:       map[string]lockfile.File{},
		}
		for _, f := range pf.files {
			selected := opts.Filter(f.Variant) && !f.Archived
			entry := lockfile.File{
				FileToken: f.FileToken,
				Variant:   string(f.Variant),
				Version:   f.Version,
				FileID:    f.FileID,
				SizeBytes: f.SizeBytes, // approximate label size; replaced below if downloaded
			}
			if selected {
				if r, ok := resolvedByID[f.FileID]; ok && r.cachePath != "" {
					entry.Tracked = true
					entry.CachePath = r.cachePath
					entry.SHA256 = r.sha
					entry.SizeBytes = r.size
					if r.now {
						entry.DownloadedAt = opts.Now
					} else if p, ok := prevByID[f.FileID]; ok {
						entry.DownloadedAt = p.DownloadedAt
					}
				}
				// DryRun selected-but-not-resolved stays Tracked=false here; status
				// does not mutate the committed lockfile, so this report copy is
				// informational only.
			}
			lp.Files[f.Key()] = entry
		}
		report.NewLockfile.Packs[pf.pack.Slug] = lp
	}
}

func warnings(packFiles []packWithFiles, filter func(model.Variant) bool) []string {
	var w []string
	for _, pf := range packFiles {
		any := false
		for _, f := range pf.files {
			if filter(f.Variant) && !f.Archived {
				any = true
				break
			}
		}
		if !any {
			w = append(w, fmt.Sprintf("no downloadable variant for %q (owned, but nothing matches the filter)", pf.pack.DisplayName))
		}
	}
	sort.Strings(w)
	return w
}

func indexByFileID(lf lockfile.Lockfile) map[int]lockfile.File {
	m := map[int]lockfile.File{}
	for _, p := range lf.Packs {
		for _, f := range p.Files {
			// Prefer a tracked entry (with cachePath) if duplicated across packs.
			if existing, ok := m[f.FileID]; !ok || (!existing.Tracked && f.Tracked) {
				m[f.FileID] = f
			}
		}
	}
	return m
}

func cacheChecker(opts Options) func(relPath, sha string) bool {
	if opts.FullVerify {
		return func(relPath, sha string) bool { return cache.Verify(opts.LibraryRoot, relPath, sha) }
	}
	return func(relPath, _ string) bool { return cache.Exists(opts.LibraryRoot, relPath) }
}

func filterPacks(packs []model.Pack, glob string) []model.Pack {
	if glob == "" {
		return packs
	}
	var out []model.Pack
	for _, p := range packs {
		if ok, _ := filepath.Match(glob, p.Slug); ok {
			out = append(out, p)
		}
	}
	return out
}
