// Command synty mirrors the Synty store's "Your Library" into a local cache,
// downloading only what changed. See README.md.
//
//	synty status   # what would change (no downloads)
//	synty sync     # download the delta and update the lockfile
//	synty list     # print the current lockfile
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/curbol/hexed-haven/tools/synty/internal/config"
	"github.com/curbol/hexed-haven/tools/synty/internal/lockfile"
	"github.com/curbol/hexed-haven/tools/synty/internal/portal"
	"github.com/curbol/hexed-haven/tools/synty/internal/session"
	"github.com/curbol/hexed-haven/tools/synty/internal/syncer"
)

const lockfileName = "synty-library.lock.json"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "synty:", err)
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
	cfgDir := fs.String("config", ".", "directory holding config.toml and the lockfile")
	cookies := fs.String("cookies", "", "cookie source: a cookies.txt or pasted-curl file (overrides config; default Firefox)")
	library := fs.String("library", "", "library cache directory (overrides config / SYNTY_LIBRARY)")
	only := fs.String("only", "", "limit to packs whose slug matches this glob")
	concurrency := fs.Int("concurrency", 0, "max concurrent item-page fetches (overrides config)")
	dryRun := fs.Bool("dry-run", false, "on sync, classify and report only (no downloads or writes)")
	switch cmd {
	case "status", "sync", "list", "-h", "--help", "help":
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

	cfg, err := config.Load(*cfgDir)
	if err != nil {
		return err
	}
	if *library != "" {
		cfg.LibraryPath = *library
	}
	if *concurrency > 0 {
		cfg.Concurrency = *concurrency
	}
	lockPath := filepath.Join(*cfgDir, lockfileName)

	if cmd == "list" {
		return list(lockPath)
	}

	if cfg.CustomerID == "" {
		return fmt.Errorf("no customer id: set SYNTY_CUSTOMER_ID or customer_id in config.local.toml")
	}
	cookie, err := resolveCookie(cfg, *cookies)
	if err != nil {
		return err
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		return err
	}
	client := &portal.Client{
		HTTP:       &http.Client{Timeout: 0},
		BaseURL:    "https://syntystore.com",
		CustomerID: cfg.CustomerID,
		Cookie:     cookie,
	}
	dry := cmd == "status" || *dryRun
	opts := syncer.Options{
		LibraryRoot: cfg.LibraryPath,
		Filter:      cfg.Filter(),
		OnlyGlob:    *only,
		DryRun:      dry,
		FullVerify:  !dry,
		Concurrency: cfg.Concurrency,
		Now:         time.Now().UTC().Format(time.RFC3339),
	}
	rep, err := syncer.Run(context.Background(), client, lf, lockPath, opts)
	if err != nil {
		return err
	}
	printReport(dry, cfg, rep)
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
	fmt.Fprint(os.Stderr, `synty - mirror your Synty store library into a local cache

usage:
  synty status [flags]   show what a sync would change (no downloads)
  synty sync   [flags]   download the delta and update the lockfile
  synty list   [flags]   print the current lockfile

flags:
  -config <dir>       directory with config.toml and the lockfile (default ".")
  -cookies <file>     cookies.txt or pasted-curl file (default: Firefox cookies)
  -library <dir>      cache directory (overrides config / SYNTY_LIBRARY)
  -only <glob>        limit to packs whose slug matches the glob
  -concurrency <n>    max concurrent item-page fetches

The customer id comes from SYNTY_CUSTOMER_ID or config.local.toml (never committed).
`)
}
