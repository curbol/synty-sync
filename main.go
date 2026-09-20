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
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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

// cliFlags holds every flag the CLI accepts. A subcommand only ever sees the ones
// registerFlags binds for it; the rest keep their zero values and are never read.
type cliFlags struct {
	cfgDir       string
	manifestFlag string
	cookies      string
	library      string
	only         string
	customer     string
	concurrency  int
	dryRun       bool
	addr         string
}

// takesOnly reports whether cmd accepts --only. It is a function rather than a line
// inside registerFlags because the stray-positional error suggests the flag, and a
// subcommand told to use one it does not bind sends the user into a parse error.
func takesOnly(cmd string) bool { return cmd == "status" || cmd == "sync" }

// registerFlags binds the flags that mean something for cmd, so one that does not
// becomes a parse error rather than being accepted and ignored. A shared set let
// `select --dry-run` serve the page and rewrite the committed manifest, which is the
// opposite of what the flag says.
func registerFlags(fs *flag.FlagSet, cmd string) *cliFlags {
	var f cliFlags
	needsManifest := cmd != "update"
	// list reads the lockfile beside the manifest and never opens the user config, so
	// it does not take the flag that points at one.
	needsConfigDir := needsManifest && cmd != "list"
	needsSession := cmd == "select" || cmd == "status" || cmd == "sync"
	syncs := takesOnly(cmd)

	if needsManifest {
		fs.StringVar(&f.manifestFlag, "manifest", "", "project manifest path (default: nearest synty-sync.toml walking up from cwd)")
	}
	if needsConfigDir {
		fs.StringVar(&f.cfgDir, "config", "", "user config dir holding config.toml (default: $XDG_CONFIG_HOME/synty-sync or ~/.config/synty-sync)")
	}
	if needsSession {
		fs.StringVar(&f.cookies, "cookies", "", "cookie source: a cookies.txt or pasted-curl file (overrides config; default Firefox)")
		fs.StringVar(&f.customer, "customer", "", "Synty customer id (overrides SYNTY_CUSTOMER_ID and config)")
	}
	if syncs {
		fs.StringVar(&f.library, "library", "", "library cache directory (overrides config / SYNTY_LIBRARY)")
		fs.StringVar(&f.only, "only", "", "limit to packs whose slug matches this glob")
		fs.IntVar(&f.concurrency, "concurrency", 0, "max concurrent item-page fetches (overrides config)")
	}
	if cmd == "sync" {
		fs.BoolVar(&f.dryRun, "dry-run", false, "classify and report only (no downloads or writes)")
	}
	if cmd == "select" {
		fs.StringVar(&f.addr, "addr", selectAddr, "the address to serve the page at (host:port)")
	}
	return &f
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "status", "sync", "list", "select", "update", "version", "-h", "--help", "help", "--version", "-v":
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	// Silence the flag package's own dump: help prints usage() below, and a bad flag
	// comes back as an error that main reports once.
	fs.SetOutput(io.Discard)
	f := registerFlags(fs, cmd)
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
	// Cancellation is set up before the first subcommand that reaches the network, so
	// update honors Ctrl-C the way sync and select do.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cmd == "update" {
		if fs.NArg() > 1 {
			return fmt.Errorf("update takes at most one version argument, got %d", fs.NArg())
		}
		return selfupdate.Run(ctx, version, fs.Arg(0))
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("%s takes no positional arguments (got %q)", cmd, fs.Arg(0))
		if takesOnly(cmd) {
			return fmt.Errorf("%w; to limit packs use --only %s", err, fs.Arg(0))
		}
		return err
	}

	manifestPath, err := resolveManifestPath(f.manifestFlag, cmd)
	if err != nil {
		return err
	}
	lockPath := manifest.LockPath(manifestPath)

	// Before the user config is even looked at: list reads one JSON file beside the
	// manifest and nothing else, so a stray key in config.toml, which Load rejects
	// outright, has no business stopping it.
	if cmd == "list" {
		return list(stdout, lockPath)
	}

	// The listener is bound before the user config and the browser cookie DB are
	// touched, so a bad --addr is reported as a bad --addr. Resolving the session
	// first answers "0.0.0.0:8787" with whatever is wrong with the user's browser
	// profile, which is not what they typed.
	var ln net.Listener
	if cmd == "select" {
		bind := f.addr
		if bind == "" {
			bind = selectAddr
		}
		var err error
		if ln, err = listenLocal(bind); err != nil {
			return err
		}
		defer func() { _ = ln.Close() }()
	}

	authDir := config.ResolveDir(f.cfgDir)
	cfg, err := config.Load(authDir)
	if err != nil {
		return err
	}
	cfg = applyFlags(cfg, f.library, f.customer, f.concurrency)

	if cfg.CustomerID == "" {
		return fmt.Errorf("no customer id: pass --customer, set SYNTY_CUSTOMER_ID, or put customer_id in config.toml")
	}
	src := sessionSource(cfg, f.cookies)
	cookie, err := resolveCookie(cfg, f.cookies)
	if err != nil {
		return err
	}
	client := newPortalClient(cfg.CustomerID, cookie)

	if cmd == "select" {
		return explainSession(selectPacks(ctx, client, manifestPath, ln), src)
	}
	return explainSession(runSyncOrStatus(ctx, client, cfg, manifestPath, lockPath, f.only, isDryRun(cmd, f.dryRun)), src)
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

// storeBaseURL is the store every subcommand talks to. It is a package variable for
// the same reason stdout is: run's config -> session -> client wiring is otherwise
// unreachable from a test without going to the real store, so the one place that
// decides which id and which cookie reach the client would ship untested.
var storeBaseURL = "https://syntystore.com"

// newPortalClient builds the store client. The transport policy (no whole-request
// timeout because asset downloads are large, but a response-header timeout so a
// stalled connection fails instead of hanging forever) belongs to portal, which is
// the layer that knows a download cannot take a deadline, so a nil client here gets
// it rather than this one reproducing it.
func newPortalClient(customerID, cookie string) *portal.Client {
	return portal.New(nil, storeBaseURL, customerID, cookie)
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
	if n := rep.ActionableFailures(); n > 0 {
		return fmt.Errorf("%d of %d selected files could not be downloaded; the cache and lockfile hold everything that did", n, len(rep.Diffs))
	}
	return nil
}

// isDryRun reports whether a run should classify only, with no downloads and no
// lockfile write: status is always dry, and sync honors --dry-run.
func isDryRun(cmd string, dryRun bool) bool {
	return cmd == "status" || dryRun
}

// applyFlags layers the command-line overrides on last, after config.Load has
// merged the built-in defaults, config.toml, and the environment. The library path
// is expanded here as well as in Load: the shell leaves a quoted --library
// "~/assets" alone, which would otherwise put the mirror in a directory named "~".
func applyFlags(cfg config.Config, library, customer string, concurrency int) config.Config {
	if library != "" {
		cfg.LibraryPath = config.ExpandHome(library)
	}
	if concurrency > 0 {
		cfg.Concurrency = concurrency
	}
	if customer != "" {
		cfg.CustomerID = customer
	}
	return cfg
}

// resolveManifestPath locates the project manifest. An explicit --manifest is honored
// verbatim (existence is not pre-checked, so `list` can derive a lockfile path beside a
// not-yet-created manifest). Otherwise it is discovered by walking up from the working
// directory; when nothing is found, `select` defaults to synty-sync.toml in the working
// directory (it is about to create one), and the read commands error.
func resolveManifestPath(flag, cmd string) (string, error) {
	if flag != "" {
		// The shell leaves a quoted --manifest "~/game/synty-sync.toml" alone, and
		// manifest.Load reports a path that does not exist as an empty manifest rather
		// than an error, so an unexpanded tilde surfaces as "no variant_includes"
		// against a file that has them.
		return config.ExpandHome(flag), nil
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

// listenLocal binds the selection page's listener, refusing an address that is not on
// this machine's loopback. The page lists every pack the account owns and its form
// rewrites the committed manifest, so it is built for one browser here. A wildcard or
// LAN bind does not widen that, it breaks it: the handlers answer only a request from
// this machine, so the browser such a bind was meant to reach is refused while the page
// sits open on a port anyone can knock on.
func listenLocal(bind string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return nil, fmt.Errorf("bad --addr %q: %w (want host:port, e.g. %s)", bind, err, selectAddr)
	}
	if !loopbackHost(host) {
		return nil, fmt.Errorf("--addr %q is not a loopback address: the selection page shows your whole library "+
			"and its form rewrites the manifest, so it is served to this machine only. "+
			"To reach it from elsewhere, forward the port (ssh -L 8787:localhost:8787 …)", bind)
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", bind, err)
	}
	return ln, nil
}

// loopbackHost reports whether the host half of a bind address names this machine. An
// empty host is the wildcard form (":8787"), which binds every interface.
func loopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sortedKeys is a map's keys in a fixed order, so what the terminal prints does not
// depend on Go's map iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// selectAddr is where `select` serves its page when --addr is not given.
const selectAddr = "localhost:8787"

// selectPacks serves the selection page on ln and writes the result to the manifest.
// It takes a bound listener for the same reason web.Serve one layer down does: the
// caller decides where the page lives, and a test gets an ephemeral port instead of
// a fixed one that another process may hold. The listener is closed here whichever
// way this returns; web.Serve closing it earlier, the moment the user saves, is not
// something this level has to know.
func selectPacks(ctx context.Context, client *portal.Client, manifestPath string, ln net.Listener) error {
	defer func() { _ = ln.Close() }()
	packs, err := client.Enumerate(ctx)
	if err != nil {
		return err
	}
	man, err := manifest.Load(manifestPath)
	if err != nil {
		return err
	}
	// Enumerate returns no packs both for an empty library and for a page whose anchors
	// stopped parsing, and Reconcile rebuilds the pack list from what it is handed. The
	// syncer refuses the same shape rather than rewriting its committed file; so does
	// this one, or a markup change deletes the user's whole pack list.
	if len(packs) == 0 && len(man.Packs) > 0 {
		return fmt.Errorf("the library listed no packs while %s holds %d; refusing to rewrite it", manifestPath, len(man.Packs))
	}
	// Reconcile rebuilds the pack list from this one enumeration, so anything the walk
	// did not return drops out and takes its enabled flag with it. The zero-pack case
	// above is refused, but a partial read is not detectable from here — the library
	// parser can only see a page that yielded nothing at all — so the entries that
	// leave are named on the way out rather than vanishing behind a count.
	dropped := map[string]string{}
	for _, e := range man.Packs {
		dropped[e.Slug] = e.Name
	}
	man.Reconcile(packs)
	for _, e := range man.Packs {
		delete(dropped, e.Slug)
	}
	// Compared against what the page actually offered, not against what was enabled
	// before: a pack that has left the library is dropped by Reconcile, so measuring
	// against the prior set would refuse an honest empty submission naming a pack the
	// user was never shown.
	offered := man.EnabledSet()
	chosen, err := web.Serve(ctx, ln, packs, offered)
	if err != nil {
		return err
	}
	// Turning off every pack is a real choice, but it is also what an empty or
	// drive-by submission looks like, and it costs the user their whole selection in
	// a committed file. Make them state it.
	if len(chosen) == 0 && len(offered) > 0 {
		return fmt.Errorf("the selection came back empty while %d packs were enabled; %s left unchanged (deselect them in the manifest if that is what you meant)", len(offered), manifestPath)
	}
	man.SetEnabled(chosen)
	if err := manifest.Save(manifestPath, man); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "saved %s: %d of %d packs enabled. Run `synty-sync sync` to download.\n",
		manifestPath, len(chosen), len(packs))
	for _, slug := range sortedKeys(dropped) {
		fmt.Fprintf(stdout, "  no longer in your library: %s (%s); dropped from %s\n",
			slug, dropped[slug], manifestPath)
	}
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
		// The flag is applied after config.Load has expanded its own paths, so a
		// quoted --cookies "~/cookies.txt" needs the same treatment here.
		src = config.ExpandHome(override)
	}
	return session.Resolve(src)
}

func printReport(w io.Writer, dry bool, cfg config.Config, rep syncer.Report) {
	counts := map[syncer.Class]int{}
	for _, d := range rep.Diffs {
		counts[d.Class]++
	}
	fmt.Fprintf(w, "library: %s\n", cfg.LibraryPath)
	fmt.Fprintf(w, "packs: %d of %d in the lockfile  files selected: %d\n",
		rep.PacksInScope, len(rep.NewLockfile.Packs), len(rep.Diffs))
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
	if len(lf.Packs) == 0 {
		// Load treats a missing file as an empty lockfile, so without this the whole
		// output of `list` before the first sync is the legend under the empty table.
		fmt.Fprintf(w, "no packs recorded yet: %s does not exist.\nRun `synty-sync select` to choose packs, then `synty-sync sync`.\n", lockPath)
		return nil
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

flags (a subcommand accepts only the ones listed for it):
  select status sync list
    -manifest <path>    project manifest (default: nearest synty-sync.toml walking up from cwd)
  select status sync
    -config <dir>       user config dir with config.toml (default: $XDG_CONFIG_HOME/synty-sync or ~/.config/synty-sync)
    -customer <id>      Synty customer id (overrides SYNTY_CUSTOMER_ID / config)
    -cookies <src>      "firefox" | "zen" | a cookies.txt / pasted-curl file (default: firefox)
  status sync
    -library <dir>      cache directory (overrides config / SYNTY_LIBRARY)
    -only <glob>        limit to packs whose slug matches the glob
    -concurrency <n>    max concurrent item-page fetches
  sync
    -dry-run            report only (no downloads or lockfile write)
  select
    -addr <host:port>   the address to serve the page at (default: localhost:8787)

Auth is user-scoped: config.toml (customer id, session, cache default) lives in the
config dir. The project manifest (synty-sync.toml: variant_includes + the pack
allowlist) and its lockfile (synty-sync.lock.json beside it) are committed with the
consuming project. The customer id comes from --customer, SYNTY_CUSTOMER_ID, or config.toml.

To search and preview what you have mirrored, see quarry:
https://github.com/curbol/quarry
`)
}
