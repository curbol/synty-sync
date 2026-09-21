// Package syncer orchestrates a sync: enumerate the library, filter variants,
// dedup files by fileId, diff against the lockfile and cache, download the delta,
// and write the new lockfile. status is the same flow stopped before downloads.
package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	Adopted      // matched a file already on disk, folded in without a download
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
func classify(av model.FileEntry, prior lockfile.File, hasPrior bool, cacheOK func(lockfile.File) bool) Class {
	switch {
	case !hasPrior:
		return New
	case !prior.Tracked:
		return DownloadNow
	case prior.Version != av.Version:
		return Changed
	case prior.CachePath == "" || !cacheOK(prior):
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

// Failure is one selected file the run could not resolve. Gone marks the single
// cause no later run can clear — the store no longer serves the file — so it is
// reported without moving the exit status.
type Failure struct {
	PackSlug string
	Key      string
	FileID   int
	Err      string
	Gone     bool
}

// Report summarizes a run.
type Report struct {
	Diffs      []FileDiff
	Downloaded []FileDiff
	Adopted    []FileDiff // matched files already on disk, no download
	Failures   []Failure
	Removed    []string // lockfile packs the library no longer lists
	// PacksInScope is how many packs the run rebuilt from live pages. NewLockfile
	// carries every pack the record holds, including those carried forward untouched,
	// so it is the wrong number to report as what a run acted on.
	PacksInScope int
	Swept        int // abandoned download temps removed
	SweptBytes   int64
	Warnings     []string
	NewLockfile  lockfile.Lockfile
}

// ActionableFailures counts the failures a later run, a fresh session, or a fix on
// this side could clear, and is what the exit status is built from. Gone is excluded:
// the store no longer serves the file, so no re-run clears it, and counting it would
// make every future sync exit non-zero over the same file forever.
func (r Report) ActionableFailures() int {
	n := 0
	for _, f := range r.Failures {
		if !f.Gone {
			n++
		}
	}
	return n
}

// ErrEmptyLibrary guards the committed record against a well-formed but wrong
// enumeration. The lockfile travels with someone's project, and a read that returns
// nothing is far more often markup that moved than a library someone emptied.
var ErrEmptyLibrary = errors.New("the library listed no packs while the lockfile holds entries; " +
	"refusing to treat that as the truth")

// abandonedTempAge is how old an in-flight download temp must be before a run treats
// it as abandoned. It has to clear any transfer a concurrent run could still be
// writing, and the files here take a long time.
const abandonedTempAge = 24 * time.Hour

// Options configures a run.
type Options struct {
	LibraryRoot string
	Filter      func(model.Variant) bool
	OnlyGlob    string // optional pack-slug glob; empty = all
	DryRun      bool   // status: classify only, no downloads, no save
	FullVerify  bool   // sha-verify cache (sync) vs presence-only (status)
	Concurrency int
	Now         string        // timestamp for generatedAt/downloadedAt (default: now, UTC)
	Attempts    int           // download attempts (default 3)
	Backoff     time.Duration // base backoff between attempts (default 500ms)
	// PackSelected is the manifest allowlist and is required: selection is opt-in, so
	// a run states which packs it may touch rather than defaulting to all of them.
	PackSelected func(slug string) bool
	Progress     func(string) // optional per-step progress sink; nil = silent
}

// progressSink returns the run's progress function, or a no-op when the caller did
// not supply one, so every path can call it unconditionally.
func (o Options) progressSink() func(string) {
	if o.Progress == nil {
		return func(string) {}
	}
	return o.Progress
}

type resolved struct {
	cachePath string
	sha       string
	size      int64
	version   string
	variant   string
	now       bool
}

// verdicts is what a run decided about each fileId it read, keyed by the only
// identity a file has across its owning packs. They travel together because they are
// one answer in four parts: every fileId a run touched has to reach the owners it did
// not fetch through exactly one of them, and a verdict with no channel leaves one
// fileId tracked under one owner and untracked under another in a committed file. A
// fifth channel is then one field and its handler rather than a parameter threaded
// through every call site, three of which are tests that would still compile with the
// new one left nil.
type verdicts struct {
	// live is what this run's pages say each fileId is, decided once for every owner.
	// Every entry the run writes takes its identity from here rather than from the row
	// in front of it: the store labels a bundled file per order item, so two in-scope
	// owners reading their own rows commit two versions, two variants or two advertised
	// sizes for one fileId, at one sha.
	live map[int]live
	// resolved is what the run has bytes for: a download, an adoption, or a prior copy
	// it checked and kept.
	resolved map[int]resolved
	// unresolved is what it went looking for and did not find. The identity it is
	// dropped at comes from live, so every owner reports the loss the same way.
	unresolved map[int]struct{}
	// deselected is what it read and declined, against why it declined it. Nothing
	// failed, so no other channel says so, and leaving these records alone is what let
	// one fileId end up tracked under one owner and untracked under the pack the run
	// rebuilt.
	deselected map[int]declined
}

func newVerdicts() verdicts {
	return verdicts{
		live:       map[int]live{},
		resolved:   map[int]resolved{},
		unresolved: map[int]struct{}{},
		deselected: map[int]declined{},
	}
}

// declined is why a run read a file and did not select it. The lockfile outcome is the
// same either way, so this exists for the sentence declinedRecords has to write: one
// cause is the store's doing and the other is the reader's own filter, and they call
// for different things to be done about the bytes left behind.
type declined int

const (
	declinedByFilter declined = iota
	declinedArchived
)

func (d declined) describe(version string) string {
	if d == declinedArchived {
		return fmt.Sprintf("archived by the store (%s)", version)
	}
	return "no longer matched by variant_includes"
}

// live is what this run's pages say a fileId is. Every field travels to the owning
// packs the run did not fetch: the version because a carried entry otherwise names one
// version against another version's sha, the token and variant because together they
// are the entry's key, so an owner that keeps the old ones ends up filing the new bytes
// under the old engine's name, and the advertised size because it describes the store's
// listing rather than the bytes and so refreshes whatever the verdict was.
type live struct {
	fileToken      string
	variant        string
	version        string
	advertisedSize int64
}

// liveOf is one page row read as the identity of the fileId it names.
func liveOf(f model.FileEntry) live {
	return live{
		fileToken:      f.FileToken,
		variant:        string(f.Variant),
		version:        f.Version,
		advertisedSize: f.AdvertisedSize,
	}
}

// readRows turns every in-scope pack's rows into one answer per fileId: the identity
// the run will record it under, which owning packs selected it, and the order to work
// through them in. Building a verdicts anywhere else is how a channel ends up nil for a
// fileId the run did read, so this is the only constructor that fills live.
func readRows(packFiles []packWithFiles, filter func(model.Variant) bool) (verdicts, map[int][]selection, []int) {
	vd := newVerdicts()
	selectedByID := map[int][]selection{}
	var selOrder []int
	for _, pf := range packFiles {
		for _, f := range pf.files {
			// The first row read stands in until a selected one arrives, so a fileId no
			// owner selected still has an identity to be dropped at. A selected row then
			// takes over and keeps it: that row is the one the download is keyed on, so
			// anything else would name the bytes after a row the run never fetched.
			if _, known := vd.live[f.FileID]; !known {
				vd.live[f.FileID] = liveOf(f)
			}
			// The filter is asked first so its answer is the one reported: a reader who
			// has stopped wanting a variant does not need to hear that the store also
			// archived it, and the thing they can act on is their own manifest.
			if !filter(f.Variant) {
				vd.deselected[f.FileID] = declinedByFilter
				continue
			}
			if f.Archived {
				vd.deselected[f.FileID] = declinedArchived
				continue
			}
			if _, seen := selectedByID[f.FileID]; !seen {
				selOrder = append(selOrder, f.FileID)
				vd.live[f.FileID] = liveOf(f)
			}
			selectedByID[f.FileID] = append(selectedByID[f.FileID], selection{pf.pack, f})
		}
	}
	// Selected anywhere wins. Archived is a per-row label and the filter reads a
	// variant, so one pack can decline a fileId that another pack offers under a
	// variant this run does take.
	for id := range selectedByID {
		delete(vd.deselected, id)
	}
	return vd, selectedByID, selOrder
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
	if opts.Now == "" {
		// Now stamps generatedAt and every downloadedAt in a committed file, so an
		// unset one would write empty timestamps rather than fail visibly.
		opts.Now = time.Now().UTC().Format(time.RFC3339)
	}
	opts.Progress = opts.progressSink()
	progress := opts.Progress

	report := Report{NewLockfile: lockfile.Lockfile{GeneratedAt: opts.Now, Packs: map[string]lockfile.Pack{}}}

	// Housekeeping first, so an interrupted earlier run does not keep its bytes for
	// the life of the library. A dry run touches nothing.
	if !opts.DryRun {
		report.Swept, report.SweptBytes = cache.SweepTemps(opts.LibraryRoot, time.Now().Add(-abandonedTempAge))
		if report.Swept > 0 {
			progress(fmt.Sprintf("swept %d abandoned download temp(s)", report.Swept))
		}
	}

	progress("enumerating library…")
	packs, err := c.Enumerate(ctx)
	if err != nil {
		return Report{}, err
	}
	if len(packs) == 0 && len(lf.Packs) > 0 {
		return Report{}, ErrEmptyLibrary
	}
	// Computed before the manifest and --only narrow the list, so a pack that is
	// merely disabled is not mistaken for one that left the library.
	owned := make(map[string]bool, len(packs))
	for _, p := range packs {
		owned[p.Slug] = true
	}
	for slug := range lf.Packs {
		if !owned[slug] {
			report.Removed = append(report.Removed, slug)
		}
	}
	sort.Strings(report.Removed)

	packs = filterPacks(packs, opts.OnlyGlob, opts.PackSelected)

	progress(fmt.Sprintf("%d packs selected; reading item pages…", len(packs)))
	packFiles, unreadable, err := fetchAll(ctx, c, packs, opts.Concurrency)
	if err != nil {
		return Report{}, err
	}
	report.PacksInScope = len(packFiles)

	priorByID := indexByFileID(lf)
	cacheOK := cacheChecker(opts)

	vd, selectedByID, selOrder := readRows(packFiles, opts.Filter)

	if opts.Attempts <= 0 {
		opts.Attempts = 3
	}
	if opts.Backoff <= 0 {
		opts.Backoff = 500 * time.Millisecond
	}

	adoptedByID, adoptWarnings := adoptAll(opts, adoptCandidates(selOrder, selectedByID, priorByID))

	var pruneWarnings []string
	for _, id := range selOrder {
		rep := selectedByID[id][0].file
		fd := FileDiff{PackSlug: rep.PackSlug, Key: rep.Key(), FileID: id}

		if r, ok := adoptedByID[id]; ok {
			fd.Class = Adopted
			report.Diffs = append(report.Diffs, fd)
			report.Adopted = append(report.Adopted, fd)
			r.version, r.variant = rep.Version, string(rep.Variant)
			vd.resolved[id] = r
			continue
		}

		prior, hasPrior := priorByID[id]
		fd.Class = classify(rep, prior, hasPrior, cacheOK)
		report.Diffs = append(report.Diffs, fd)

		switch {
		case fd.Class == Unchanged:
			vd.resolved[id] = resolved{
				cachePath: prior.CachePath, sha: prior.SHA256, size: prior.SizeBytes,
				version: rep.Version, variant: string(rep.Variant),
			}
		case opts.DryRun:
			// classify only; nothing resolved
		default:
			progress(fmt.Sprintf("download %s", rep.Key()))
			r, err := downloadWithRetry(ctx, c, opts, rep)
			if err != nil {
				// An interrupt is not a per-file verdict: every file left would be
				// recorded as failed for a reason that has nothing to do with it.
				if ctx.Err() != nil {
					return Report{}, ctx.Err()
				}
				// One bad file costs that file. Aborting here would also throw away the
				// lockfile, leaving everything this run did download unrecorded.
				report.Failures = append(report.Failures, Failure{
					PackSlug: rep.PackSlug, Key: rep.Key(), FileID: id,
					Err: err.Error(), Gone: goneFromTheStore(err),
				})
				// A failed update must not erase the copy the last run verified.
				// Rebuilding the entry from scratch drops its path and sha while the bytes
				// stay on disk, orphaning them with nothing recording it, and leaves an
				// out-of-scope owner of the same fileId carrying a record this one lost.
				// Only Changed qualifies: every other class reaches here with no good
				// prior copy to hold on to, and every owning pack has to say so.
				if fd.Class == Changed && prior.CachePath != "" && cacheOK(prior) {
					// The prior variant travels with the prior version for the same reason:
					// these are the bytes the last run verified, so the entry has to name
					// them as what they are, not as what the page now advertises.
					vd.resolved[id] = resolved{
						cachePath: prior.CachePath, sha: prior.SHA256,
						size: prior.SizeBytes, version: prior.Version, variant: prior.Variant,
					}
				} else {
					vd.unresolved[id] = struct{}{}
				}
				continue
			}
			if fd.Class == Changed && prior.CachePath != "" && prior.CachePath != r.cachePath {
				// Best-effort, but not silent: a prune that fails leaves the prior
				// version in the cache with nothing recording it.
				if err := cache.Remove(opts.LibraryRoot, prior.CachePath); err != nil {
					pruneWarnings = append(pruneWarnings, fmt.Sprintf("could not remove the prior %s: %v", prior.CachePath, err))
				}
			}
			r.version, r.variant = rep.Version, string(rep.Variant)
			vd.resolved[id] = r
			report.Downloaded = append(report.Downloaded, fd)
		}
	}

	buildLockfile(&report, packFiles, opts, vd, lf)
	report.Warnings = append(warnings(packFiles, opts.Filter), orphanedRecords(lf, report.NewLockfile)...)
	report.Warnings = append(report.Warnings, declinedRecords(vd, lf, report.NewLockfile)...)
	report.Warnings = append(report.Warnings, unreadable...)
	report.Warnings = append(report.Warnings, append(adoptWarnings, pruneWarnings...)...)

	if !opts.DryRun {
		if err := lockfile.Save(lockPath, report.NewLockfile); err != nil {
			return Report{}, err
		}
	}
	return report, nil
}

type packWithFiles struct {
	pack    model.Pack
	files   []model.FileEntry
	unknown []string // rows whose variant this build does not recognize
}

// fetchAll reads every pack's item page, returning one entry per pack it could read
// and a warning for each pack it read but could not make sense of. A pack is dropped
// rather than represented by an empty entry: the caller rebuilds a lockfile record
// from what it is handed here, so a slot that owns no files erases everything that
// pack had.
func fetchAll(ctx context.Context, c *portal.Client, packs []model.Pack, concurrency int) ([]packWithFiles, []string, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	// One failed pack ends the run, so stop the queue behind it. Every pack is
	// launched at once and only the semaphore staggers them, so without this a
	// library's worth of item pages is still fetched, with retries, for a run that
	// has already decided to abort.
	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A pointer per slot rather than a value: a pack that never ran, or one dropped
	// below, leaves a nil that cannot be mistaken for a pack owning no files.
	out := make([]*packWithFiles, len(packs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var dropped []string
	for i, p := range packs {
		wg.Add(1)
		go func(i int, p model.Pack) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if fetchCtx.Err() != nil {
				return
			}
			files, unknown, err := c.ItemFiles(fetchCtx, p)
			if err == nil && len(files) == 0 && len(unknown) == 0 {
				// Nothing on the page read as a file at all: not a pack with nothing in
				// it (an owned pack always ships at least one downloadable file) but
				// markup we failed to read, and rebuilding from it erases every entry
				// the pack holds.
				err = fmt.Errorf("no files parsed (markup may have changed)")
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("item page for %s: %w", p.Slug, err)
				}
				mu.Unlock()
				cancel()
				return
			}
			if len(files) == 0 {
				// The page parsed; every file on it is for an engine this build does not
				// know. That is a future Synty variant, not breakage, and the parser
				// deliberately skips such rows rather than failing, so the run must not
				// fail either. Dropping the pack leaves its prior record to be carried
				// forward whole, which is the only outcome that loses nothing.
				mu.Lock()
				dropped = append(dropped, fmt.Sprintf(
					"%q lists only files whose variant this build does not recognize (%s); its lockfile record is carried forward unchanged",
					p.DisplayName, strings.Join(unknown, ", ")))
				mu.Unlock()
				return
			}
			out[i] = &packWithFiles{pack: p, files: files, unknown: unknown}
		}(i, p)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	// A pack skipped on the way out leaves a nil behind. Only an interrupted run gets
	// here with packs left unread, and it must say so rather than return a short list
	// the caller would read as the whole library.
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	read := make([]packWithFiles, 0, len(out))
	for _, pf := range out {
		if pf != nil {
			read = append(read, *pf)
		}
	}
	sort.Strings(dropped)
	return read, dropped, nil
}

// download fetches one file and checks the delivered bytes before letting them take a
// cache path. The client has already refused a document Content-Type; this catches the
// response that claims to be an archive and is not.
func download(ctx context.Context, c *portal.Client, opts Options, f model.FileEntry) (resolved, error) {
	body, filename, err := c.Resolve(ctx, f)
	if err != nil {
		return resolved{}, err
	}
	defer body.Close()

	// A multi-gigabyte transfer otherwise reports nothing between "download" and
	// "done", which is indistinguishable from a hang, so progress is counted off the
	// body as it streams rather than announced up front.
	sink := opts.progressSink()
	counted := &progressReader{r: body, total: f.AdvertisedSize, report: func(read, total int64) {
		sink(fmt.Sprintf("  %s: %s", f.Key(), progressLine(read, total)))
	}}
	pending, err := cache.Store(opts.LibraryRoot, f.FileToken, filename, counted)
	if err != nil {
		return resolved{}, err
	}
	if err := looksLikePackage(pending.TempPath()); err != nil {
		pending.Discard()
		return resolved{}, fmt.Errorf("%s: %w", f.Key(), err)
	}
	if err := pending.Commit(); err != nil {
		pending.Discard()
		return resolved{}, err
	}
	return resolved{cachePath: pending.RelPath, sha: pending.SHA256, size: pending.Size, now: true}, nil
}

// ErrNotAPackageBody rejects a body that reads as prose. Only text is refused: an
// archive format this tool has not seen must not be turned away, but no archive begins
// with a document, and a zero-byte body is not a pack either.
var ErrNotAPackageBody = errors.New("the body is a document, not package bytes")

// sniffLen is how much of a file http.DetectContentType needs.
const sniffLen = 512

func sniffPackage(head []byte) error {
	if mt := http.DetectContentType(head); strings.HasPrefix(mt, "text/") {
		return fmt.Errorf("%w (%d bytes sniffing as %s)", ErrNotAPackageBody, len(head), mt)
	}
	return nil
}

func looksLikePackage(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, sniffLen)
	n, err := f.Read(head)
	if err != nil && err != io.EOF {
		return err
	}
	return sniffPackage(head[:n])
}

// selection is one owning pack's view of a selected file. A fileId can have several.
type selection struct {
	pack model.Pack
	file model.FileEntry
}

// adoptCandidates returns the representative file for every selected fileId with no
// tracked prior — the precondition both adoption paths share. A file the lockfile
// already tracks is excluded because the cache matches on name alone: adopting one
// would move unverified content over a verified copy and repoint the lockfile at it
// without ever consulting classify. Stating it once is the point; the two paths held
// hand-copied versions of it, and they had already drifted apart once.
func adoptCandidates(order []int, byID map[int][]selection, prior map[int]lockfile.File) []model.FileEntry {
	var out []model.FileEntry
	for _, id := range order {
		if p, has := prior[id]; has && p.Tracked {
			continue // classify handles tracked files (Unchanged / Changed / CacheMissing)
		}
		out = append(out, byID[id][0].file)
	}
	return out
}

// adoptAll folds pre-existing files into the layout and takes any already in it,
// returning what it resolved by fileId and what it refused. Nothing here is worth
// ending a run over: adoption saves a download the run can always fall back to.
func adoptAll(opts Options, cands []model.FileEntry) (map[int]resolved, []string) {
	adopted := map[int]resolved{}
	// A file the flat-file pass moved and then refused is sitting in the layout, where
	// the scan below finds it again. Without this it would be refused a second time and
	// the same reason printed twice for one file.
	refused := map[int]bool{}
	var warnings []string
	wanted := make([]cache.Wanted, 0, len(cands))
	for _, f := range cands {
		wanted = append(wanted, cache.Wanted{FileID: f.FileID, FileToken: f.FileToken, Variant: string(f.Variant), Version: f.Version})
	}

	// Flat files at the library root are moved into the layout, which a dry run must
	// not do. Migrate is best-effort and returns whatever it moved alongside any
	// error, so a failure costs the files it could not fold in rather than the run.
	if !opts.DryRun {
		migrated, err := cache.Migrate(opts.LibraryRoot, wanted)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not fold pre-existing flat files into the layout: %v", err))
		}
		for _, m := range migrated {
			r, err := adopt(opts.LibraryRoot, m.RelPath)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("not adopting %s: %v", m.RelPath, err))
				refused[m.FileID] = true
				continue
			}
			adopted[m.FileID] = r
		}
	}

	// Files already in the <fileToken>/ layout that no lockfile records, so a missing
	// or degraded lockfile does not force a full re-download. Read-only, so it runs
	// for status too.
	progress := opts.progressSink()
	for i, f := range cands {
		if _, done := adopted[f.FileID]; done {
			continue
		}
		if refused[f.FileID] {
			continue
		}
		rel, ok := cache.Locate(opts.LibraryRoot, wanted[i])
		if !ok {
			continue
		}
		if opts.DryRun {
			// status must not read a multi-gigabyte library back just to say what it
			// would do, so the checks alone stand in for the hash here.
			if err := adoptable(opts.LibraryRoot, rel); err != nil {
				warnings = append(warnings, fmt.Sprintf("not adopting %s: %v", rel, err))
				continue
			}
			adopted[f.FileID] = resolved{cachePath: rel}
			continue
		}
		progress(fmt.Sprintf("adopt %s", f.Key()))
		r, err := adopt(opts.LibraryRoot, rel)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("not adopting %s: %v", rel, err))
			continue
		}
		adopted[f.FileID] = r
	}
	return adopted, warnings
}

// adopt takes a file already on disk as a pack's content: it checks the bytes and
// hashes them. Adoption is the one path into the lockfile that never consults
// classify, so both steps live here rather than at each call site, where a check
// added to one and missed on the other would let a rejected body through the gap.
// Every failure is the caller's to report and skip: adoption saves a download it
// could always fall back to, so nothing here is worth ending a run over.
func adopt(libraryRoot, relPath string) (resolved, error) {
	if err := adoptable(libraryRoot, relPath); err != nil {
		return resolved{}, err
	}
	sha, size, err := cache.Hash(libraryRoot, relPath)
	if err != nil {
		return resolved{}, err
	}
	return resolved{cachePath: relPath, sha: sha, size: size, now: true}, nil
}

// ErrTruncatedArchive rejects a zip whose end-of-central-directory record is missing,
// which is what a copy interrupted part way looks like.
var ErrTruncatedArchive = errors.New("the zip is missing its end-of-central-directory record, so it is not the whole file")

// eocdSig ends every zip. A trailing comment may follow it, and the comment length
// field is 16 bits, so the record starts within the last 64KiB plus its own 22 bytes.
var eocdSig = []byte("PK\x05\x06")

const eocdSearchLen = (1 << 16) + 22

// zipMagic opens every zip that holds anything: the first entry's local file header.
var zipMagic = []byte("PK\x03\x04")

// wholeArchive reports whether a file that opens as a zip carries the trailer only a
// complete one has. The head sniff cannot see this: a copy that stopped part way still
// begins with an archive's magic, and adopting it records its own short bytes as the
// file's truth, after which every Verify compares those bytes against themselves and
// finds them intact forever.
//
// Keyed on the leading bytes rather than the extension. The name comes from a signed
// URL, a Content-Disposition, or a file someone placed by hand, so an archive can
// arrive with no .zip on it at all, and the cache deliberately matches a wanted file
// under any extension or none, which is exactly the set an extension check would
// leave unexamined. A container this cannot read (.unitypackage) has no decidable
// answer without decompressing and is passed through.
func wholeArchive(libraryRoot, relPath string, head []byte) error {
	if !bytes.HasPrefix(head, zipMagic) {
		return nil
	}
	tail, err := cache.Tail(libraryRoot, relPath, eocdSearchLen)
	if err != nil {
		return err
	}
	if !bytes.Contains(tail, eocdSig) {
		return fmt.Errorf("%w: %s", ErrTruncatedArchive, relPath)
	}
	return nil
}

// adoptable reports whether a file already on disk can be taken as a pack's content.
// A cache written before these guards existed can hold error pages under exactly the
// right names, so the bytes are checked rather than trusted.
func adoptable(libraryRoot, relPath string) error {
	head, err := cache.Head(libraryRoot, relPath, sniffLen)
	if err != nil {
		return err
	}
	if err := sniffPackage(head); err != nil {
		return err
	}
	return wholeArchive(libraryRoot, relPath, head)
}

// progressStep is how much has to transfer before another line is printed. Small
// enough to show a large file moving, large enough not to flood a log.
const progressStep = 8 << 20

// progressReader reports how much of a body has arrived, on a byte threshold and once
// more at the end so a file smaller than the threshold still reports something.
type progressReader struct {
	r        io.Reader
	total    int64
	read     int64
	reported int64
	report   func(read, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.read-p.reported >= progressStep || (err != nil && p.read != p.reported) {
		p.reported = p.read
		p.report(p.read, p.total)
	}
	return n, err
}

// progressLine renders bytes against the portal's label size. That figure is rounded,
// so it is shown only while it is still plausible; past it, the count stands alone
// rather than claiming 103%.
func progressLine(read, total int64) string {
	if total > 0 && read <= total {
		return fmt.Sprintf("%s / %s (%d%%)", humanBytes(read), humanBytes(total), read*100/total)
	}
	return humanBytes(read)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// downloadWithRetry retries a download with bounded exponential backoff, resolving
// a fresh signed URL on each attempt (the CloudFront signature expires). A
// permanently-failing attempt aborts immediately instead of burning every attempt.
func downloadWithRetry(ctx context.Context, c *portal.Client, opts Options, f model.FileEntry) (resolved, error) {
	var r resolved
	err := retry.Do(ctx, opts.Attempts, opts.Backoff, func() error {
		var err error
		r, err = download(ctx, c, opts, f)
		if err != nil && permanentDownloadFailure(err) {
			return retry.Stop(err)
		}
		return err
	})
	return r, err
}

// permanentDownloadFailure reports a download error a retry cannot fix: a body that
// is not a package however many times it is fetched, or a 4xx, except the three that
// are about timing rather than the request being wrong. 403 is an expired CloudFront
// signature that a fresh Resolve re-signs, 429 a rate limit that backing off clears,
// and 408 the server saying the request did not finish in time. Only 403 is specific
// to this layer; the other two match the page fetcher's policy.
func permanentDownloadFailure(err error) bool {
	if errors.Is(err, portal.ErrNotAPackage) || errors.Is(err, ErrNotAPackageBody) {
		return true
	}
	code, ok := portal.StatusOf(err)
	if !ok || code < 400 || code >= 500 {
		return false
	}
	return code != http.StatusForbidden &&
		code != http.StatusTooManyRequests &&
		code != http.StatusRequestTimeout
}

// goneFromTheStore reports the one failure no future run can clear, so it is worth
// reporting without failing the run: the store no longer serves the file.
func goneFromTheStore(err error) bool {
	code, ok := portal.StatusOf(err)
	return ok && (code == http.StatusNotFound || code == http.StatusGone)
}

// clearTracking marks an entry as not downloaded, at the identity this run's pages give
// the fileId. applyLive travels with it for the same reason it travels with a resolved
// entry: an owner that keeps its own row's labels drops the record against a version the
// run was not looking for, under a key no other owner agrees on.
func clearTracking(f lockfile.File, v live) lockfile.File {
	f.Tracked, f.CachePath, f.SHA256, f.SizeBytes, f.DownloadedAt = false, "", "", 0, ""
	return applyLive(f, v)
}

// applyLive writes this run's identity for a fileId onto an entry, leaving a field the
// run has nothing to say about alone. It is the one place an entry learns what the store
// currently calls a file, so every owner learns the same thing.
func applyLive(f lockfile.File, v live) lockfile.File {
	if v.fileToken != "" {
		f.FileToken = v.fileToken
	}
	if v.variant != "" {
		f.Variant = v.variant
	}
	if v.version != "" {
		f.Version = v.version
	}
	f.AdvertisedSize = v.advertisedSize
	return f
}

// applyResolved writes what the run resolved for a fileId onto an entry. The resolved
// version and variant travel with the bytes: the sha belongs to whichever version was
// actually resolved, so recording the live page's labels against it would name one
// version, under one engine, over another's content. Only a failed update on a renamed
// variant makes these differ from the page, and that is exactly the case where the page
// describes bytes this run did not get.
//
// fallbackDownloadedAt is what to keep when the bytes were not fetched on this run:
// the entry's own stamp for a carried pack, the prior record's for a rebuilt one.
func applyResolved(f lockfile.File, r resolved, now, fallbackDownloadedAt string) lockfile.File {
	if r.version != "" {
		f.Version = r.version
	}
	if r.variant != "" {
		f.Variant = r.variant
	}
	f.Tracked = true
	f.CachePath = r.cachePath
	f.SHA256 = r.sha
	f.SizeBytes = r.size
	switch {
	case r.now:
		f.DownloadedAt = now
	case fallbackDownloadedAt != "":
		f.DownloadedAt = fallbackDownloadedAt
	}
	return f
}

// keyFor is the key an entry belongs under once the run is done with it. The key is
// the token and the variant, so an entry whose either moved has to move key with it, or
// the renamed file stays filed under the old name for every owner the run did not fetch
// while the one it did fetch is rebuilt under the new one. An entry the run left alone
// keeps the key it arrived with, whatever shape that key is in.
func keyFor(f lockfile.File, was lockfile.File, key string) string {
	if f.FileToken == was.FileToken && f.Variant == was.Variant {
		return key
	}
	return model.FileEntry{FileToken: f.FileToken, Variant: model.Variant(f.Variant)}.Key()
}

func buildLockfile(report *Report, packFiles []packWithFiles, opts Options, vd verdicts, prev lockfile.Lockfile) {
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
		// Sorted rather than map order: re-keying below can land two prior keys on one
		// new key, and which entry survives must not depend on the iteration.
		for _, key := range sortedKeys(p.Files) {
			f := p.Files[key]
			was := f
			v, read := vd.live[f.FileID]
			if read {
				// Read a page for this fileId, so the store's current labels are known and
				// apply to every owner, whatever the verdict turned out to be. A fileId no
				// in-scope pack lists keeps what it arrived with: nothing this run saw
				// describes it.
				f = applyLive(f, v)
			}
			// The question is whether this fileId was re-resolved on this run, not
			// whether its path moved: a re-fetch to the same filename still changes the
			// bytes, and the identity has to travel with them or the carried entry ends
			// up naming one version against another version's sha.
			_, unresolved := vd.unresolved[f.FileID]
			_, declined := vd.deselected[f.FileID]
			switch {
			case unresolved:
				// The run went looking for these bytes and did not find them, so the
				// record naming them has to go with them — at the version the run was
				// looking for, or this owner reports the loss against a stale one.
				f = clearTracking(f, v)
			case declined:
				// The run read this file and declined it, so it is not downloaded any
				// more for this owner either. Nothing failed, so no other channel says
				// so, and leaving the record alone is what let one fileId end up
				// tracked here and untracked in the pack the run rebuilt.
				f = clearTracking(f, v)
			default:
				if r, ok := vd.resolved[f.FileID]; ok && r.cachePath != "" {
					// Tracked or not: a fileId this run selected and resolved was selectable
					// for this owner too, and the only way its entry stayed untracked is an
					// earlier run that failed to fetch it. That is the case that has to
					// converge.
					//
					// An owner that never held the file has no stamp of its own to keep, so
					// it takes the one another owner recorded for the same fileId rather
					// than ending the run tracked with no downloadedAt at all.
					stamp := f.DownloadedAt
					if stamp == "" {
						stamp = prevByID[f.FileID].DownloadedAt
					}
					f = applyResolved(f, r, opts.Now, stamp)
				}
			}
			carried.Files[keyFor(f, was, key)] = f
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
			// Every identity field comes through the shared channel rather than off this
			// row, which is the same reason the carried owners read it there: the store
			// labels a bundled file per order item, and the fan-out reads those pages
			// concurrently. Taking each row's own figures leaves two in-scope owners
			// committing two versions, two variants or two advertised sizes for one
			// fileId at one sha. vd.live is populated from exactly these rows, so the
			// lookup cannot miss.
			entry := applyLive(lockfile.File{FileID: f.FileID}, vd.live[f.FileID])
			// The key follows the identity, so two owners of one fileId file it under one
			// key even when their rows disagree about what to call it.
			key := model.FileEntry{FileToken: entry.FileToken, Variant: model.Variant(entry.Variant)}.Key()
			was := entry
			// Keyed on the fileId's outcome for the run, not on this row's own verdict.
			// Selection is decided once across every owner ("selected anywhere wins"),
			// and the bytes are one file in one place, so an owner whose row the store
			// archived still holds the copy the owner beside it just resolved. Reading
			// this row's own Archived label here instead is what let one owning pack
			// record the shared path while another recorded nothing for the same fileId.
			// A fileId no owner selected reaches this untracked at the live identity,
			// which is where clearTracking leaves the carried owners too.
			//
			// DryRun selected-but-not-resolved stays Tracked=false; status does not
			// mutate the committed lockfile, so this report copy is informational only.
			if r, ok := vd.resolved[f.FileID]; ok && r.cachePath != "" {
				entry = applyResolved(entry, r, opts.Now, prevByID[f.FileID].DownloadedAt)
			}
			lp.Files[keyFor(entry, was, key)] = entry
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
		for _, label := range pf.unknown {
			w = append(w, fmt.Sprintf("%q lists a file whose variant this build does not recognize: %q (it will not be mirrored)", pf.pack.DisplayName, label))
		}
	}
	sort.Strings(w)
	return w
}

// sortedKeys returns a pack's file keys in a fixed order, for the two places that
// must not let Go's map iteration decide an outcome: which of two prior entries
// sharing a fileId wins, and which of two entries landing on one key survives.
func sortedKeys(files map[string]lockfile.File) []string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// declinedRecords names every file the prior lockfile tracked that this run read and
// declined. The store still lists it, so the pack keeps its entry and the file never
// reaches orphanedRecords, but the entry is rebuilt untracked, taking its cache path
// and sha with it while the bytes stay on disk. Nothing can take them back either: a
// declined file is never selected, so it is never an adopt candidate. Said once, on the
// run that drops the record, since the run after finds nothing tracked to report.
//
// Both causes are here because they leave the same thing behind, and only the sentence
// differs: the store archiving a file is its doing, a variant_includes that no longer
// matches is the reader's, and the copy is equally unreferenced either way. Reporting
// only the first left someone who narrowed their filter with gigabytes on disk that
// nothing points at and no run would ever mention.
//
// A fileId another owner still tracks is not one of these. Both labels are per-row, so
// a bundled file can be declined under one order item while another still serves it,
// and the new record keeps the path under that owner. Claiming the copy is unreferenced
// there sends someone looking for bytes that nothing lost.
func declinedRecords(vd verdicts, prev, next lockfile.Lockfile) []string {
	prevByID := indexByFileID(prev)
	referenced := map[int]bool{}
	for _, p := range next.Packs {
		for _, f := range p.Files {
			if f.Tracked && f.CachePath != "" {
				referenced[f.FileID] = true
			}
		}
	}
	var w []string
	for id, why := range vd.deselected {
		if referenced[id] {
			continue
		}
		p, ok := prevByID[id]
		if !ok || !p.Tracked || p.CachePath == "" {
			continue
		}
		v := vd.live[id]
		key := model.FileEntry{FileToken: v.fileToken, Variant: model.Variant(v.variant)}.Key()
		w = append(w, fmt.Sprintf(
			"%s is %s; it is no longer tracked and the cached copy at %s is now unreferenced",
			key, why.describe(v.version), p.CachePath))
	}
	sort.Strings(w)
	return w
}

// orphanedRecords names every file the prior lockfile tracked whose fileId the new
// one records nowhere. A pack that leaves the library is reported on its own and
// keeps its record; a single file leaving takes its record with it, and the bytes
// stay in the cache with nothing pointing at them, so say so rather than let the
// entry disappear between two runs. Matching on fileId rather than key keeps a
// renamed variant — same file under a new key — from reading as a loss.
func orphanedRecords(prev, next lockfile.Lockfile) []string {
	kept := map[int]bool{}
	for _, p := range next.Packs {
		for _, f := range p.Files {
			kept[f.FileID] = true
		}
	}
	seen := map[int]bool{}
	var w []string
	for _, p := range prev.Packs {
		for key, f := range p.Files {
			if !f.Tracked || f.CachePath == "" || kept[f.FileID] || seen[f.FileID] {
				continue
			}
			seen[f.FileID] = true
			w = append(w, fmt.Sprintf("%s is no longer listed by any pack you own; its record is gone and the cached copy at %s is now unreferenced", key, f.CachePath))
		}
	}
	sort.Strings(w)
	return w
}

// indexByFileID picks one prior record per fileId. Packs are visited in slug order
// rather than map order: a hand-merged lockfile can hold the same fileId tracked at
// two versions under two packs, and whichever record wins decides between Unchanged
// and a multi-gigabyte refetch. The ambiguity is not resolvable from the data, so the
// goal is that two runs over the same file agree, not that either answer is right.
func indexByFileID(lf lockfile.Lockfile) map[int]lockfile.File {
	m := map[int]lockfile.File{}
	slugs := make([]string, 0, len(lf.Packs))
	for slug := range lf.Packs {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		p := lf.Packs[slug]
		for _, k := range sortedKeys(p.Files) {
			f := p.Files[k]
			// Prefer a tracked entry (with cachePath) if duplicated across packs.
			if existing, ok := m[f.FileID]; !ok || (!existing.Tracked && f.Tracked) {
				m[f.FileID] = f
			}
		}
	}
	return m
}

// cacheChecker builds the cache-side half of classify. The cheap check compares the
// recorded byte count, which is what separates a file that is present from one that
// is intact: an interrupted transfer and a stored error page both leave something at
// the path. Re-hashing is reserved for sync, where the cost of reading the library
// back buys the only check that sees a mid-file corruption.
func cacheChecker(opts Options) func(lockfile.File) bool {
	if opts.FullVerify {
		return func(f lockfile.File) bool {
			return cache.Verify(opts.LibraryRoot, f.CachePath, f.SizeBytes) &&
				cache.VerifyDeep(opts.LibraryRoot, f.CachePath, f.SHA256)
		}
	}
	return func(f lockfile.File) bool { return cache.Verify(opts.LibraryRoot, f.CachePath, f.SizeBytes) }
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
