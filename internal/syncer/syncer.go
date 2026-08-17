// Package syncer orchestrates a sync: enumerate the library, filter variants,
// dedup files by fileId, diff against the lockfile and cache, download the delta,
// and write the new lockfile. status is the same flow stopped before downloads.
package syncer

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/curbol/synty-sync/internal/cache"
	"github.com/curbol/synty-sync/internal/lockfile"
	"github.com/curbol/synty-sync/internal/model"
	"github.com/curbol/synty-sync/internal/portal"
	"github.com/curbol/synty-sync/internal/retry"
)

// Class is the diff outcome for one selected file.
type Class int

const (
	Unchanged Class = iota
	New
	Changed
	DownloadNow  // owned, was filtered out before, now selected
	CacheMissing // tracked + version matches, but absent/corrupt on disk
	Adopted      // matched a pre-existing flat zip and folded into the layout
)

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
	case Adopted:
		return "adopted"
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
	Adopted     []FileDiff // matched pre-existing flat zips, no download
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
	Now         string        // timestamp for generatedAt/downloadedAt
	Attempts    int           // download attempts (default 3)
	Backoff     time.Duration // base backoff between attempts (default 500ms)
	// PackSelected is the manifest allowlist and is required: selection is opt-in, so
	// a run states which packs it may touch rather than defaulting to all of them.
	PackSelected func(slug string) bool
	Progress     func(string) // optional per-step progress sink; nil = silent
}

type resolved struct {
	cachePath string
	sha       string
	size      int64
	version   string
	now       bool
}

// Run executes a sync (or status when DryRun) and returns a Report. The lockfile
// is saved only on a non-dry run.
func Run(ctx context.Context, c *portal.Client, lf lockfile.Lockfile, lockPath string, opts Options) (Report, error) {
	if opts.PackSelected == nil {
		return Report{}, fmt.Errorf("syncer: PackSelected is required (selection is opt-in)")
	}
	if opts.Filter == nil {
		return Report{}, fmt.Errorf("syncer: Filter is required (variant_includes has no default)")
	}
	if opts.OnlyGlob != "" {
		if _, err := filepath.Match(opts.OnlyGlob, ""); err != nil {
			return Report{}, fmt.Errorf("bad --only pattern %q: %w", opts.OnlyGlob, err)
		}
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}

	progress("enumerating library…")
	packs, err := c.Enumerate(ctx)
	if err != nil {
		return Report{}, err
	}
	packs = filterPacks(packs, opts.OnlyGlob, opts.PackSelected)

	progress(fmt.Sprintf("%d packs selected; reading item pages…", len(packs)))
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

	if opts.Attempts <= 0 {
		opts.Attempts = 3
	}
	if opts.Backoff <= 0 {
		opts.Backoff = 500 * time.Millisecond
	}

	report := Report{NewLockfile: lockfile.Lockfile{GeneratedAt: opts.Now, Packs: map[string]lockfile.Pack{}}}
	resolvedByID := map[int]resolved{}

	// Adopt pre-existing flat zips into the layout so they are not re-downloaded.
	// Files the lockfile already tracks are excluded: cache.Migrate matches on name
	// alone, so adopting one would move unverified content over a verified copy and
	// repoint the lockfile at it without ever consulting classify.
	adoptedByID := map[int]resolved{}
	if !opts.DryRun {
		var wanted []cache.Wanted
		for _, id := range selOrder {
			if prior, hasPrior := priorByID[id]; hasPrior && prior.Tracked {
				continue
			}
			f := selectedByID[id][0].file
			wanted = append(wanted, cache.Wanted{FileID: f.FileID, FileToken: f.FileToken, Variant: string(f.Variant), Version: f.Version})
		}
		migrated, err := cache.Migrate(opts.LibraryRoot, wanted)
		if err != nil {
			return Report{}, fmt.Errorf("migrate flat zips: %w", err)
		}
		for _, m := range migrated {
			sha, size, err := cache.Hash(opts.LibraryRoot, m.RelPath)
			if err != nil {
				return Report{}, fmt.Errorf("hash migrated %s: %w", m.From, err)
			}
			adoptedByID[m.FileID] = resolved{cachePath: m.RelPath, sha: sha, size: size, now: true}
		}
	}

	// Adopt files already in the <fileToken>/ layout that no lockfile records, so a
	// missing or degraded lockfile does not force a full re-download. Read-only (no move),
	// so it runs for status too; the hash is skipped there to keep status cheap.
	for _, id := range selOrder {
		if _, done := adoptedByID[id]; done {
			continue
		}
		if prior, hasPrior := priorByID[id]; hasPrior && prior.Tracked {
			continue // classify handles tracked files (Unchanged / Changed / CacheMissing)
		}
		f := selectedByID[id][0].file
		rel, ok := cache.Locate(opts.LibraryRoot, cache.Wanted{FileID: f.FileID, FileToken: f.FileToken, Variant: string(f.Variant), Version: f.Version})
		if !ok {
			continue
		}
		if opts.DryRun {
			adoptedByID[id] = resolved{cachePath: rel}
			continue
		}
		progress(fmt.Sprintf("adopt %s", f.Key()))
		sha, size, err := cache.Hash(opts.LibraryRoot, rel)
		if err != nil {
			return Report{}, fmt.Errorf("hash adopted %s: %w", rel, err)
		}
		adoptedByID[id] = resolved{cachePath: rel, sha: sha, size: size, now: true}
	}

	var pruneWarnings []string
	for _, id := range selOrder {
		rep := selectedByID[id][0].file
		fd := FileDiff{PackSlug: rep.PackSlug, Key: rep.Key(), FileID: id}

		if r, ok := adoptedByID[id]; ok {
			fd.Class = Adopted
			report.Diffs = append(report.Diffs, fd)
			report.Adopted = append(report.Adopted, fd)
			r.version = rep.Version
			resolvedByID[id] = r
			continue
		}

		prior, hasPrior := priorByID[id]
		fd.Class = classify(rep, prior, hasPrior, cacheOK)
		report.Diffs = append(report.Diffs, fd)

		switch {
		case fd.Class == Unchanged:
			resolvedByID[id] = resolved{cachePath: prior.CachePath, sha: prior.SHA256, size: prior.SizeBytes, version: rep.Version}
		case opts.DryRun:
			// classify only; nothing resolved
		default:
			progress(fmt.Sprintf("download %s", rep.Key()))
			r, err := downloadWithRetry(ctx, c, opts.LibraryRoot, rep, opts.Attempts, opts.Backoff)
			if err != nil {
				return Report{}, fmt.Errorf("download %s: %w", rep.Key(), err)
			}
			if fd.Class == Changed && prior.CachePath != "" && prior.CachePath != r.cachePath {
				// Best-effort, but not silent: a prune that fails leaves the prior
				// version in the cache with nothing recording it.
				if err := cache.Remove(opts.LibraryRoot, prior.CachePath); err != nil {
					pruneWarnings = append(pruneWarnings, fmt.Sprintf("could not remove the prior %s: %v", prior.CachePath, err))
				}
			}
			r.version = rep.Version
			resolvedByID[id] = r
			report.Downloaded = append(report.Downloaded, fd)
		}
	}

	buildLockfile(&report, packFiles, opts, resolvedByID, lf)
	report.Warnings = append(warnings(packFiles, opts.Filter), pruneWarnings...)

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
			if err == nil && len(files) == 0 {
				// Rebuilding a pack from an empty list erases every entry it holds, and
				// an owned pack always ships at least one downloadable file, so this is
				// markup we failed to read rather than a pack with nothing in it.
				err = fmt.Errorf("no files parsed (markup may have changed)")
			}
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

// downloadWithRetry retries a download with bounded exponential backoff, resolving
// a fresh signed URL on each attempt (the CloudFront signature expires). A
// permanently-failing 4xx aborts immediately instead of burning every attempt.
func downloadWithRetry(ctx context.Context, c *portal.Client, libraryRoot string, f model.FileEntry, attempts int, base time.Duration) (resolved, error) {
	var r resolved
	err := retry.Do(ctx, attempts, base, func() error {
		var err error
		r, err = download(ctx, c, libraryRoot, f)
		if err != nil && permanentDownloadFailure(err) {
			return retry.Stop(err)
		}
		return err
	})
	return r, err
}

// permanentDownloadFailure reports a download error a retry cannot fix: a 4xx,
// except 403 — an expired CloudFront signature that a fresh Resolve re-signs — and
// 429, a rate limit that backing off clears.
func permanentDownloadFailure(err error) bool {
	code, ok := portal.StatusOf(err)
	if !ok || code < 400 || code >= 500 {
		return false
	}
	return code != http.StatusForbidden && code != http.StatusTooManyRequests
}

func buildLockfile(report *Report, packFiles []packWithFiles, opts Options, resolvedByID map[int]resolved, prev lockfile.Lockfile) {
	prevByID := indexByFileID(prev)
	// A run acts only on the packs it fetched: those filtered out (disabled in the
	// manifest, or outside --only) are never re-fetched, so carry their prior records
	// forward rather than dropping them from the committed lockfile. The in-scope packs
	// are rebuilt from live data below, overwriting these. A carried file whose fileId
	// was (re)downloaded this run is repointed to the new cache path, so a bundled file
	// shared with an in-scope pack never diverges across its owning packs.
	inScope := make(map[string]bool, len(packFiles))
	for _, pf := range packFiles {
		inScope[pf.pack.Slug] = true
	}
	for slug, p := range prev.Packs {
		if inScope[slug] {
			continue
		}
		carried := lockfile.Pack{DisplayName: p.DisplayName, OrderID: p.OrderID, OrderItemID: p.OrderItemID, Files: map[string]lockfile.File{}}
		for key, f := range p.Files {
			// The question is whether this fileId was re-resolved on this run, not
			// whether its path moved: a re-fetch to the same filename still changes the
			// bytes, and the version has to travel with them or the carried entry ends
			// up naming one version against another version's sha.
			if r, ok := resolvedByID[f.FileID]; ok && r.cachePath != "" && f.Tracked {
				if r.version != "" {
					f.Version = r.version
				}
				f.CachePath = r.cachePath
				f.SHA256 = r.sha
				f.SizeBytes = r.size
				if r.now {
					f.DownloadedAt = opts.Now
				}
			}
			carried.Files[key] = f
		}
		report.NewLockfile.Packs[slug] = carried
	}
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

func filterPacks(packs []model.Pack, glob string, selected func(string) bool) []model.Pack {
	var out []model.Pack
	for _, p := range packs {
		if !selected(p.Slug) {
			continue
		}
		if glob != "" {
			if ok, _ := filepath.Match(glob, p.Slug); !ok {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}
