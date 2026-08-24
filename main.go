// Command synty-sync mirrors the Synty store's "Your Library" into a local cache,
// downloading only what changed. See README.md.
//
//	synty-sync status   # what would change (no downloads)
//	synty-sync sync     # download the delta and update the lockfile
//	synty-sync list     # print the current lockfile
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"time"

	"github.com/curbol/synty-sync/internal/config"
	"github.com/curbol/synty-sync/internal/lockfile"
	"github.com/curbol/synty-sync/internal/manifest"
	"github.com/curbol/synty-sync/internal/portal"
	"github.com/curbol/synty-sync/internal/selfupdate"
	"github.com/curbol/synty-sync/internal/session"
	"github.com/curbol/synty-sync/internal/syncer"
	"github.com/curbol/synty-sync/internal/web"
)

// version is the release version, set at build time via
// -ldflags "-X main.version=<v>". It is "dev" for local builds.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "synty-sync:", err)
		os.Exit(1)
	}
}

// stdout is where the tool's actual output goes (the run summary, the lockfile
// listing). It is a package variable so tests can capture and assert it; progress
// and diagnostics stay on stderr.
var stdout io.Writer = os.Stdout

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}
	cmd, rest := args[0], args[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	// Silence the flag package's own dump: help prints usage() below, and a bad flag
	// comes back as an error that main reports once.
	fs.SetOutput(io.Discard)
	cfgDir := fs.String("config", "", "user config dir holding config.toml (default: $XDG_CONFIG_HOME/synty-sync or ~/.config/synty-sync)")
	manifestFlag := fs.String("manifest", "", "project manifest path (default: nearest synty-sync.toml walking up from cwd)")
	cookies := fs.String("cookies", "", "cookie source: a cookies.txt or pasted-curl file (overrides config; default Firefox)")
	library := fs.String("library", "", "library cache directory (overrides config / SYNTY_LIBRARY)")
	only := fs.String("only", "", "limit to packs whose slug matches this glob")
	concurrency := fs.Int("concurrency", 0, "max concurrent item-page fetches (overrides config)")
	customer := fs.String("customer", "", "Synty customer id (overrides SYNTY_CUSTOMER_ID and config)")
	dryRun := fs.Bool("dry-run", false, "on sync, classify and report only (no downloads or writes)")
	addr := fs.String("addr", selectAddr, "on select, the address to serve the page at (host:port)")
	switch cmd {
	case "status", "sync", "list", "select", "update", "version", "-h", "--help", "help", "--version", "-v":
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage()
		return nil
	}
	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		printVersion()
		return nil
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage()
			return nil
		}
		return err
	}

	// flag.Parse stops at the first non-flag argument, so an unchecked positional
	// silently swallows every flag after it — `sync <pack> --dry-run` would download
	// the delta and rewrite the lockfile.
	if cmd == "update" {
		if fs.NArg() > 1 {
			return fmt.Errorf("update takes at most one version argument, got %d", fs.NArg())
		}
		return selfupdate.Run(version, fs.Arg(0))
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s takes no positional arguments (got %q); to limit packs use --only %s", cmd, fs.Arg(0), fs.Arg(0))
	}

	authDir := config.ResolveDir(*cfgDir)
	cfg, err := config.Load(authDir)
	if err != nil {
		return err
	}
	if *library != "" {
		cfg.LibraryPath = *library
	}
	if *concurrency > 0 {
		cfg.Concurrency = *concurrency
	}
	if *customer != "" {
		cfg.CustomerID = *customer
	}

	manifestPath, err := resolveManifestPath(*manifestFlag, cmd)
	if err != nil {
		return err
	}
	lockPath := manifest.LockPath(manifestPath)

	if cmd == "list" {
		return list(stdout, lockPath)
	}

	if cfg.CustomerID == "" {
		return fmt.Errorf("no customer id: pass --customer, set SYNTY_CUSTOMER_ID, or put customer_id in config.toml")
	}
	src := sessionSource(cfg, *cookies)
	cookie, err := resolveCookie(cfg, *cookies)
	if err != nil {
		return err
	}
	client := newPortalClient(cfg.CustomerID, cookie)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cmd == "select" {
		return explainSession(selectPacks(ctx, client, manifestPath, *addr), src)
	}
	return explainSession(runSyncOrStatus(ctx, client, cfg, manifestPath, lockPath, *only, isDryRun(cmd, *dryRun)), src)
}

// explainSession turns the bare expired-session sentinel into something actionable.
// Only main knows which cookie source was actually resolved, so the hint belongs
// here; the wrap keeps errors.Is working for callers that check the sentinel.
func explainSession(err error, src string) error {
	if !errors.Is(err, portal.ErrExpiredSession) {
		return err
	}
	return fmt.Errorf("%w\n  session source: %s\n  log in at https://syntystore.com in that browser (or re-export your cookie file) and run again", err, src)
}

func sessionSource(cfg config.Config, override string) string {
	if override != "" {
		return override
	}
	if cfg.SessionSource != "" {
		return cfg.SessionSource
	}
	return "firefox"
}

// newPortalClient builds the store client. There is no whole-request timeout (asset
// downloads are large), but a response-header timeout so a stalled connection fails
// instead of hanging forever. The transport is cloned from the default so proxy
// settings from the environment still apply.
func newPortalClient(customerID, cookie string) *portal.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	return portal.New(&http.Client{Transport: tr}, "https://syntystore.com", customerID, cookie)
}

// runSyncOrStatus loads the manifest and lockfile, runs the diff (downloading unless
// dry), and prints the summary. Taking the client as a parameter keeps the sync flow
// testable against a stub store, separately from flag parsing and dispatch.
func runSyncOrStatus(ctx context.Context, client *portal.Client, cfg config.Config, manifestPath, lockPath, onlyGlob string, dry bool) error {
	man, err := manifest.Load(manifestPath)
	if err != nil {
		return err
	}
	if len(man.VariantIncludes) == 0 {
		return fmt.Errorf("no variant_includes in %s: add your engine's variants, e.g.\n  variant_includes = [\"Godot_*\"]   (also Unity_*, Unreal_*, SourceFiles, SourceSprites)", manifestPath)
	}
	if err := man.Validate(); err != nil {
		return fmt.Errorf("%s: %w", manifestPath, err)
	}
	enabled := man.EnabledSet()
	if len(enabled) == 0 {
		fmt.Fprintln(os.Stderr, "note: no packs enabled; run `synty-sync select` to choose (nothing will download).")
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		return err
	}
	opts := syncer.Options{
		LibraryRoot:  cfg.LibraryPath,
		Filter:       man.Filter(),
		OnlyGlob:     onlyGlob,
		DryRun:       dry,
		FullVerify:   !dry,
		Concurrency:  cfg.Concurrency,
		Now:          time.Now().UTC().Format(time.RFC3339),
		PackSelected: func(slug string) bool { return enabled[slug] },
		Progress:     func(m string) { fmt.Fprintln(os.Stderr, m) },
	}
	rep, err := syncer.Run(ctx, client, lf, lockPath, opts)
	if err != nil {
		return err
	}
	printReport(stdout, dry, cfg, rep)
	// The files the run could not fetch are the point of the command, so they move the
	// exit status. A file the store no longer serves does not: no re-run clears it, and
	// it would fail every future sync forever.
	if n := actionableFailures(rep); n > 0 {
		return fmt.Errorf("%d of %d selected files could not be downloaded; the cache and lockfile hold everything that did", n, len(rep.Diffs))
	}
	return nil
}

func actionableFailures(rep syncer.Report) int {
	n := 0
	for _, f := range rep.Failures {
		if !f.Gone {
			n++
		}
	}
	return n
}

// isDryRun reports whether a run should classify only, with no downloads and no
// lockfile write: status is always dry, and sync honors --dry-run.
func isDryRun(cmd string, dryRun bool) bool {
	return cmd == "status" || dryRun
}

// resolveManifestPath locates the project manifest. An explicit --manifest is honored
// verbatim (existence is not pre-checked, so `list` can derive a lockfile path beside a
// not-yet-created manifest). Otherwise it is discovered by walking up from the working
// directory; when nothing is found, `select` defaults to synty-sync.toml in the working
// directory (it is about to create one), and the read commands error.
func resolveManifestPath(flag, cmd string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if p, ok := manifest.Discover(wd); ok {
		return p, nil
	}
	if cmd == "select" {
		return filepath.Join(wd, manifest.FileName), nil
	}
	return "", fmt.Errorf("no %s found (searched up from %s); run `synty-sync select` or pass --manifest <path>", manifest.FileName, wd)
}

// selectAddr is where `select` serves its page when --addr is not given.
const selectAddr = "localhost:8787"

func selectPacks(ctx context.Context, client *portal.Client, manifestPath, addr string) error {
	if addr == "" {
		addr = selectAddr
	}
	packs, err := client.Enumerate(ctx)
	if err != nil {
		return err
	}
	man, err := manifest.Load(manifestPath)
	if err != nil {
		return err
	}
	prev := man.EnabledSet()
	man.Reconcile(packs)
	chosen, err := web.Serve(ctx, addr, packs, man.EnabledSet())
	if err != nil {
		return err
	}
	// Turning off every pack is a real choice, but it is also what an empty or
	// drive-by submission looks like, and it costs the user their whole selection in
	// a committed file. Make them state it.
	if len(chosen) == 0 && len(prev) > 0 {
		return fmt.Errorf("the selection came back empty while %d packs were enabled; %s left unchanged (deselect them in the manifest if that is what you meant)", len(prev), manifestPath)
	}
	man.SetEnabled(chosen)
	if err := manifest.Save(manifestPath, man); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "saved %s: %d of %d packs enabled. Run `synty-sync sync` to download.\n",
		manifestPath, len(chosen), len(packs))
	if len(man.VariantIncludes) == 0 {
		fmt.Fprintf(stdout, "note: %s has no variant_includes yet — add your engine's variants, e.g.\n  variant_includes = [\"Godot_*\", \"SourceFiles\"]\nbefore `synty-sync sync`.\n", manifestPath)
	}
	return nil
}

func printVersion() {
	fmt.Fprintf(stdout, "synty-sync %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func resolveCookie(cfg config.Config, override string) (string, error) {
	src := cfg.SessionSource
	if override != "" {
		src = override
	}
	return session.Resolve(src)
}

func printReport(w io.Writer, dry bool, cfg config.Config, rep syncer.Report) {
	counts := map[syncer.Class]int{}
	for _, d := range rep.Diffs {
		counts[d.Class]++
	}
	fmt.Fprintf(w, "library: %s\n", cfg.LibraryPath)
	fmt.Fprintf(w, "packs: %d  files selected: %d\n", len(rep.NewLockfile.Packs), len(rep.Diffs))
	fmt.Fprintf(w, "  new=%d changed=%d download-now=%d cache-missing=%d adopted=%d unchanged=%d\n",
		counts[syncer.New], counts[syncer.Changed], counts[syncer.DownloadNow],
		counts[syncer.CacheMissing], counts[syncer.Adopted], counts[syncer.Unchanged])
	if dry {
		pending := counts[syncer.New] + counts[syncer.Changed] + counts[syncer.DownloadNow] + counts[syncer.CacheMissing]
		fmt.Fprintf(w, "would download: %d files\n", pending)
	} else {
		fmt.Fprintf(w, "downloaded: %d files  adopted: %d existing  failed: %d\n",
			len(rep.Downloaded), len(rep.Adopted), len(rep.Failures))
	}
	if rep.Swept > 0 {
		fmt.Fprintf(w, "swept %d abandoned download temp(s), %d bytes reclaimed\n", rep.Swept, rep.SweptBytes)
	}
	for _, f := range rep.Failures {
		what := "failed"
		if f.Gone {
			what = "gone from the store"
		}
		fmt.Fprintf(w, "  %s: %s %s: %s\n", what, f.PackSlug, f.Key, f.Err)
	}
	for _, slug := range rep.Removed {
		fmt.Fprintf(w, "  no longer in your library: %s (its lockfile record is kept)\n", slug)
	}
	for _, warning := range rep.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", warning)
	}
}

func list(w io.Writer, lockPath string) error {
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		return err
	}
	slugs := make([]string, 0, len(lf.Packs))
	for s := range lf.Packs {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	for _, s := range slugs {
		p := lf.Packs[s]
		fmt.Fprintf(w, "%s  (%s)\n", s, p.DisplayName)
		keys := make([]string, 0, len(p.Files))
		for k := range p.Files {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f := p.Files[k]
			mark := " "
			if f.Tracked {
				mark = "*"
			}
			fmt.Fprintf(w, "  %s %s  %s\n", mark, k, f.Version)
		}
	}
	fmt.Fprintf(w, "(* = downloaded into the cache)\n")
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `synty-sync - mirror your Synty store library into a local cache

usage:
  synty-sync select [flags]   pick which packs to mirror (opens a local web page)
  synty-sync status [flags]   show what a sync would change (no downloads)
  synty-sync sync   [flags]   download the delta and update the lockfile
  synty-sync list   [flags]   print the current lockfile
  synty-sync update [ver]     update to the latest release (or a specific version)
  synty-sync version          print the version

flags:
  -manifest <path>    project manifest (default: nearest synty-sync.toml walking up from cwd)
  -config <dir>       user config dir with config.toml (default: $XDG_CONFIG_HOME/synty-sync or ~/.config/synty-sync)
  -customer <id>      Synty customer id (overrides SYNTY_CUSTOMER_ID / config)
  -cookies <src>      "firefox" | "zen" | a cookies.txt / pasted-curl file (default: firefox)
  -library <dir>      cache directory (overrides config / SYNTY_LIBRARY)
  -only <glob>        limit to packs whose slug matches the glob
  -concurrency <n>    max concurrent item-page fetches
  -dry-run            on sync, report only (no downloads or lockfile write)
  -addr <host:port>   on select, the address to serve the page at (default: localhost:8787)

Auth is user-scoped: config.toml (customer id, session, cache default) lives in the
config dir. The project manifest (synty-sync.toml: variant_includes + the pack
allowlist) and its lockfile (synty-sync.lock.json beside it) are committed with the
consuming project. The customer id comes from --customer, SYNTY_CUSTOMER_ID, or config.toml.

To search and preview what you have mirrored, see quarry:
https://github.com/curbol/quarry
`)
}
