// Command synty-sync mirrors the Synty store's "Your Library" into a local cache,
// downloading only what changed. See README.md.
//
//	synty-sync status   # what would change (no downloads)
//	synty-sync sync     # download the delta and update the lockfile
//	synty-sync list     # print the current lockfile
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"time"

	"github.com/curbol/synty-sync/internal/assetindex"
	"github.com/curbol/synty-sync/internal/browse"
	"github.com/curbol/synty-sync/internal/config"
	"github.com/curbol/synty-sync/internal/lockfile"
	"github.com/curbol/synty-sync/internal/manifest"
	"github.com/curbol/synty-sync/internal/portal"
	"github.com/curbol/synty-sync/internal/session"
	"github.com/curbol/synty-sync/internal/syncer"
	"github.com/curbol/synty-sync/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "synty-sync:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}
	cmd, rest := args[0], args[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgDir := fs.String("config", "", "user config dir holding config.toml (default: $XDG_CONFIG_HOME/synty-sync or ~/.config/synty-sync)")
	manifestFlag := fs.String("manifest", "", "project manifest path (default: nearest synty-sync.toml walking up from cwd)")
	cookies := fs.String("cookies", "", "cookie source: a cookies.txt or pasted-curl file (overrides config; default Firefox)")
	library := fs.String("library", "", "library cache directory (overrides config / SYNTY_LIBRARY)")
	only := fs.String("only", "", "limit to packs whose slug matches this glob")
	concurrency := fs.Int("concurrency", 0, "max concurrent item-page fetches (overrides config)")
	customer := fs.String("customer", "", "Synty customer id (overrides SYNTY_CUSTOMER_ID and config)")
	dryRun := fs.Bool("dry-run", false, "on sync, classify and report only (no downloads or writes)")
	root := fs.String("root", "", "browse: asset scan root (overrides browse_root / SYNTY_BROWSE_ROOT; default: library dir)")
	addr := fs.String("addr", "localhost:8788", "browse: server address (host:port)")
	reindex := fs.Bool("reindex", false, "browse: rebuild the asset index from scratch")
	browseCache := fs.String("cache", "", "browse: cache dir for the index and unpacked archives (default: $XDG_CACHE_HOME/synty-sync)")
	switch cmd {
	case "status", "sync", "list", "select", "browse", "-h", "--help", "help":
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage()
		return nil
	}
	if err := fs.Parse(rest); err != nil {
		return err
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

	// browse is a standalone read-only server over the local library: it needs no
	// customer id, cookie, manifest, or lockfile, so it branches before manifest
	// resolution (which errors when no synty-sync.toml is discoverable).
	if cmd == "browse" {
		return browseAssets(cfg, *root, *addr, *browseCache, *reindex)
	}

	manifestPath, err := resolveManifestPath(*manifestFlag, cmd)
	if err != nil {
		return err
	}
	lockPath := manifest.LockPath(manifestPath)

	if cmd == "list" {
		return list(lockPath)
	}

	if cfg.CustomerID == "" {
		return fmt.Errorf("no customer id: pass --customer, set SYNTY_CUSTOMER_ID, or put customer_id in config.toml")
	}
	cookie, err := resolveCookie(cfg, *cookies)
	if err != nil {
		return err
	}
	// No whole-request timeout (asset downloads are large), but a response-header
	// timeout so a stalled connection fails instead of hanging forever.
	client := &portal.Client{
		HTTP: &http.Client{
			Transport: &http.Transport{ResponseHeaderTimeout: 60 * time.Second},
		},
		BaseURL:    "https://syntystore.com",
		CustomerID: cfg.CustomerID,
		Cookie:     cookie,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if cmd == "select" {
		return selectPacks(ctx, client, manifestPath)
	}

	man, err := manifest.Load(manifestPath)
	if err != nil {
		return err
	}
	if len(man.VariantIncludes) == 0 {
		return fmt.Errorf("no variant_includes in %s: add your engine's variants, e.g.\n  variant_includes = [\"Godot_*\"]   (also Unity_*, Unreal_*, SourceFiles, SourceSprites)", manifestPath)
	}
	enabled := man.EnabledSet()
	if len(enabled) == 0 {
		fmt.Fprintln(os.Stderr, "note: no packs enabled; run `synty-sync select` to choose (nothing will download).")
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		return err
	}
	dry := cmd == "status" || *dryRun
	opts := syncer.Options{
		LibraryRoot:  cfg.LibraryPath,
		Filter:       man.Filter(),
		OnlyGlob:     *only,
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
	printReport(dry, cfg, rep)
	return nil
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

func selectPacks(ctx context.Context, client *portal.Client, manifestPath string) error {
	packs, err := client.Enumerate(ctx)
	if err != nil {
		return err
	}
	man, err := manifest.Load(manifestPath)
	if err != nil {
		return err
	}
	man.Reconcile(packs)
	chosen, err := web.Serve(ctx, "localhost:8787", packs, man.EnabledSet())
	if err != nil {
		return err
	}
	man.SetEnabled(chosen)
	if err := manifest.Save(manifestPath, man); err != nil {
		return err
	}
	fmt.Printf("saved %s: %d of %d packs enabled. Run `synty-sync sync` to download.\n",
		manifestPath, len(chosen), len(packs))
	if len(man.VariantIncludes) == 0 {
		fmt.Printf("note: %s has no variant_includes yet — add your engine's variants, e.g.\n  variant_includes = [\"Godot_*\", \"SourceFiles\"]\nbefore `synty-sync sync`.\n", manifestPath)
	}
	return nil
}

// browseAssets indexes the asset library and serves the browse UI. The scan root
// resolves as --root > browse_root/SYNTY_BROWSE_ROOT > the library dir, so browsing
// a broader tree (e.g. all vendors) just needs browse_root set or --root passed.
func browseAssets(cfg config.Config, root, addr, cacheFlag string, reindex bool) error {
	if root == "" {
		root = cfg.BrowseRoot
	}
	if root == "" {
		root = cfg.LibraryPath
	}
	cacheDir := config.ResolveCacheDir(cacheFlag)
	cachePath := filepath.Join(cacheDir, "browse-index.json")

	fmt.Fprintf(os.Stderr, "indexing %s …\n", root)
	ix, err := assetindex.LoadOrBuild(root, cacheDir, cachePath, reindex)
	if err != nil {
		return fmt.Errorf("build asset index: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return browse.Serve(ctx, addr, ix)
}

func resolveCookie(cfg config.Config, override string) (string, error) {
	src := cfg.SessionSource
	if override != "" {
		src = override
	}
	return session.Resolve(src)
}

func printReport(dry bool, cfg config.Config, rep syncer.Report) {
	counts := map[syncer.Class]int{}
	for _, d := range rep.Diffs {
		counts[d.Class]++
	}
	fmt.Printf("library: %s\n", cfg.LibraryPath)
	fmt.Printf("packs: %d  files selected: %d\n", len(rep.NewLockfile.Packs), len(rep.Diffs))
	fmt.Printf("  new=%d changed=%d download-now=%d cache-missing=%d adopted=%d unchanged=%d\n",
		counts[syncer.New], counts[syncer.Changed], counts[syncer.DownloadNow],
		counts[syncer.CacheMissing], counts[syncer.Adopted], counts[syncer.Unchanged])
	if dry {
		pending := counts[syncer.New] + counts[syncer.Changed] + counts[syncer.DownloadNow] + counts[syncer.CacheMissing]
		fmt.Printf("would download: %d files\n", pending)
	} else {
		fmt.Printf("downloaded: %d files  adopted: %d existing\n", len(rep.Downloaded), len(rep.Adopted))
	}
	for _, w := range rep.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}

func list(lockPath string) error {
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
		fmt.Printf("%s  (%s)\n", s, p.DisplayName)
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
			fmt.Printf("  %s %s  %s\n", mark, k, f.Version)
		}
	}
	fmt.Printf("(* = downloaded into the cache)\n")
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `synty-sync - mirror your Synty store library into a local cache

usage:
  synty-sync select [flags]   pick which packs to mirror (opens a local web page)
  synty-sync status [flags]   show what a sync would change (no downloads)
  synty-sync sync   [flags]   download the delta and update the lockfile
  synty-sync list   [flags]   print the current lockfile
  synty-sync browse [flags]   search & preview the local library in a web UI

flags:
  -manifest <path>    project manifest (default: nearest synty-sync.toml walking up from cwd)
  -config <dir>       user config dir with config.toml (default: $XDG_CONFIG_HOME/synty-sync or ~/.config/synty-sync)
  -customer <id>      Synty customer id (overrides SYNTY_CUSTOMER_ID / config)
  -cookies <src>      "firefox" | "zen" | a cookies.txt / pasted-curl file (default: firefox)
  -library <dir>      cache directory (overrides config / SYNTY_LIBRARY)
  -only <glob>        limit to packs whose slug matches the glob
  -concurrency <n>    max concurrent item-page fetches

browse flags:
  -root <dir>         asset scan root (overrides browse_root / SYNTY_BROWSE_ROOT; default: library dir)
  -addr <host:port>   server address (default: localhost:8788)
  -reindex            rebuild the asset index from scratch
  -cache <dir>        index / unpacked-archive cache dir (default: $XDG_CACHE_HOME/synty-sync)

Auth is user-scoped: config.toml (customer id, session, cache default) lives in the
config dir. The project manifest (synty-sync.toml: variant_includes + the pack
allowlist) and its lockfile (synty-sync.lock.json beside it) are committed with the
consuming project. The customer id comes from --customer, SYNTY_CUSTOMER_ID, or config.toml.
`)
}
