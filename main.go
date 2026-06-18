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

	"github.com/curbol/synty-sync/internal/config"
	"github.com/curbol/synty-sync/internal/lockfile"
	"github.com/curbol/synty-sync/internal/manifest"
	"github.com/curbol/synty-sync/internal/portal"
	"github.com/curbol/synty-sync/internal/session"
	"github.com/curbol/synty-sync/internal/syncer"
	"github.com/curbol/synty-sync/internal/web"
)

const lockfileName = "synty-library.lock.json"

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
	cfgDir := fs.String("config", "", "config/state dir (default: $XDG_CONFIG_HOME/synty-sync or ~/.config/synty-sync)")
	cookies := fs.String("cookies", "", "cookie source: a cookies.txt or pasted-curl file (overrides config; default Firefox)")
	library := fs.String("library", "", "library cache directory (overrides config / SYNTY_LIBRARY)")
	only := fs.String("only", "", "limit to packs whose slug matches this glob")
	concurrency := fs.Int("concurrency", 0, "max concurrent item-page fetches (overrides config)")
	customer := fs.String("customer", "", "Synty customer id (overrides SYNTY_CUSTOMER_ID and config)")
	dryRun := fs.Bool("dry-run", false, "on sync, classify and report only (no downloads or writes)")
	switch cmd {
	case "status", "sync", "list", "select", "-h", "--help", "help":
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

	dir := config.ResolveDir(*cfgDir)
	cfg, err := config.Load(dir)
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
	lockPath := filepath.Join(dir, lockfileName)

	if cmd == "list" {
		return list(lockPath)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
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
	manifestPath := filepath.Join(dir, manifestName)

	if cmd == "select" {
		return selectPacks(ctx, client, manifestPath)
	}

	man, err := manifest.Load(manifestPath)
	if err != nil {
		return err
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
		Filter:       cfg.Filter(),
		OnlyGlob:     *only,
		DryRun:       dry,
		FullVerify:   !dry,
		Concurrency:  cfg.Concurrency,
		Now:          time.Now().UTC().Format(time.RFC3339),
		PackSelected: func(slug string) bool { return enabled[slug] },
	}
	rep, err := syncer.Run(ctx, client, lf, lockPath, opts)
	if err != nil {
		return err
	}
	printReport(dry, cfg, rep)
	return nil
}

const manifestName = "packs.toml"

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
	return nil
}

func resolveCookie(cfg config.Config, override string) (string, error) {
	src := cfg.SessionSource
	if override != "" {
		src = override
	}
	if src == "" || src == "firefox" {
		return session.FromFirefox()
	}
	return session.FromFile(src)
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

flags:
  -config <dir>       config/state dir (default: $XDG_CONFIG_HOME/synty-sync or ~/.config/synty-sync)
  -customer <id>      Synty customer id (overrides SYNTY_CUSTOMER_ID / config)
  -cookies <file>     cookies.txt or pasted-curl file (default: Firefox cookies)
  -library <dir>      cache directory (overrides config / SYNTY_LIBRARY)
  -only <glob>        limit to packs whose slug matches the glob
  -concurrency <n>    max concurrent item-page fetches

State (config.toml, packs.toml, lockfile) lives in the config dir, outside the tool.
The customer id comes from --customer, SYNTY_CUSTOMER_ID, or config.toml there.
`)
}
