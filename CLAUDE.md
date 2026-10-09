# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`synty-sync` is a Go CLI that mirrors a Synty store "Your Library" into a local cache,
downloading only what changed since the last run. It is a download manager for direct
Synty-store purchases. See `README.md` (user-facing) and `docs/design.md` (the
authoritative design doc: page shapes, lockfile schema, failure model). Read
`docs/design.md` before changing enumeration, parsing, the lockfile, or the cache layout.

Searching and previewing the mirrored library is a separate tool,
[quarry](https://github.com/curbol/quarry). This repo acquires files; quarry reads them.

## Build & test

```bash
go build -o synty-sync .        # requires Go 1.26+; no cgo (pure-Go sqlite)
go test ./...                   # full suite
go test ./internal/syncer/ -run TestClassify -v   # one package / one test
go vet ./...
gofmt -l .                      # list unformatted files
```

There is no Makefile or task runner; use the `go` toolchain directly. The default test
suite is fully offline (no network, no real session): portal tests run against
`net/http/httptest` servers and the committed `testdata/portal/*.html` fixtures.

## Architecture

A subcommand CLI. `main.go` `run()` parses flags and dispatches six subcommands:
`select`, `status`, `sync`, `list`, `update`, `version`. `select`/`status`/`sync`
resolve config → session cookie → `portal.Client`; `list` needs only the lockfile;
`update` and `version` need neither. `status` is `sync` with `DryRun` (classify only, no
downloads, no lockfile write) — both go through `syncer.Run`.

Layered `internal/` packages, each with a package doc comment stating its contract:

- `config` — resolves settings by precedence: built-in defaults → `config.toml` → env
  (`SYNTY_CUSTOMER_ID`, `SYNTY_LIBRARY`) → flags, all inside `config.Load(dir,
  config.Flags{...})`. Config/state dir is XDG-resolved (`ResolveDir`); a dir the user
  named (`--config`, `$SYNTY_CONFIG_DIR`) must exist and be a directory, since an absent
  one reads as no config at all. Library cache dir defaults to `$XDG_DATA_HOME/synty-sync`,
  else `~/.local/share/synty-sync`, and is an error rather than a relative path when
  neither resolves. No machine-specific path is baked in.
- `session` — builds the syntystore.com `Cookie` header from a Gecko browser cookie DB
  (Firefox or Zen, the zero-paste default; profile bases are per-platform), a
  `cookies.txt`, or a pasted-curl file. `Resolve` returns `Resolved{Header, Path}`;
  only `Path` is printable, and `main` names it on stderr and in the expired-session hint.
  Forwards every syntystore.com cookie rather than guessing the session one. Rows are
  grouped per `originAttributes` jar: the default jar decides every name it holds,
  container jars fill only the names it lacks, and private-browsing rows are never read,
  so a container signed into another account cannot outrank the main session. The DB is
  copied with its `-wal` sidecar, and profiles are ranked on the newest of the DB and its
  sidecars, since a running browser moves the main file only on a checkpoint.
- `portal` — the Sky Pilot Shopify portal client. `client.go` does HTTP (retry with
  backoff on 5xx/transient; fail-fast on 4xx), the `Enumerate` / `ItemFiles` / `Resolve`
  calls, and the response-level download guard; `parse.go` parses library-list and item
  pages with goquery. Retry counts, the per-attempt deadline, the page-size bound and the
  response-header timeout live on the client's `Limits` field, not in package vars; the
  header bound is included because a download cannot take a whole-request deadline and the
  stall guard only starts once the headers arrive, so nothing else covers that phase.
  Parsing is deliberately strict: a non-empty page yielding zero files is a loud error, not
  a silent skip, except when every row's variant is unrecognized (see Key invariants).
  `ErrExpiredSession` distinguishes an expired session from an empty library on the
  enumeration walk and from changed markup on an item page (a logout shell carries none of
  the parser's selectors, so `ItemFiles` checks the sentinel before blaming the markup),
  `ErrNotAPackage` a download that answered with a document, and `ErrStalled` a body that
  stopped arriving. A document download asks `CheckSession` (the sentinel on the first
  library page) and carries `ErrExpiredSession` too when the session is logged out.
  Downloads carry no whole-request deadline (a pack is gigabytes); the bound is
  `StallTimeout`, on silence, reset by every byte. `Client.Cookie` is a `Credential`,
  which formats and marshals as `[redacted]`, so printing a `Client` cannot leak the
  session.
- `model` — shared domain types (`Pack`, `FileEntry`, `Variant`) and the identity rules:
  the pack `Slug` (from display name, since file-label tokens aren't stable within a pack)
  and the per-file `Key()` = `fileToken|variant`.
- `syncer` — orchestrates a run: sweep abandoned temps → enumerate → fetch item pages
  (bounded concurrency) → filter variants → **dedup files by `fileId`** → `classify` each
  against the prior lockfile + cache → download the delta (retry, resolving a fresh signed
  URL each attempt) → build and save the new lockfile. `classify` is pure and unit-tested;
  the `Class` enum (New/Changed/DownloadNow/CacheMissing/Adopted/Unchanged) is the core
  diff logic; `Classes()` lists every class, and the summary tally iterates it. It also
  owns the semantic download guards (the body sniff and the zip end-of-central-directory
  check, on downloads and adoption alike) and the failure model: `Report.Failures` plus
  `Report.ActionableFailures()`, which drives the exit status. A prior lockfile holding an
  entry with `fileId` 0 is refused (`checkFileIDs`) before anything is touched.
- `cache` — the local mirror, keyed by **file identity, not owning pack**, so a file
  bundled across packs is stored once. Two-phase writes: `Store` hashes into a
  `.synty-dl-*` temp and returns a `*Pending`, which the caller `Commit`s or `Discard`s.
  Every operation (`Store`, `Commit`, `Discard`, `Verify`, `VerifyDeep`, `Hash`, `Head`,
  `Tail`, `Remove`, `Migrate`, `Locate`, `SweepTemps`) acts through an `os.Root` opened on
  the library, so a symlinked segment cannot carry it out of the tree; a lockfile path is
  first spelled through `Canonical` (slash space; refuses `\`, `:`, `..` escapes and
  Windows device names, identically on every platform). Recorded and derived paths are
  compared with `SamePath` / `SameFile`, never as strings. `Migrate` folds pre-existing
  flat files into the layout, matching on a name key that drops the extension:
  `normalizeName` for a name off the disk, `normalizeKey` for the synthetic wanted-file
  key, which has no extension to drop. `Migrate` and `Locate` try every name that matches
  one wanted file in `inPreferenceOrder` against the caller's `accept` check, and the first
  accepted copy wins; exactly one is folded in and the rest (refused ones included) stay
  flat.
- `lockfile` — `synty-sync.lock.json` (beside the manifest, committed with the consuming
  project): the authoritative record of owned packs, versions, checksums, and which files
  are downloaded. `advertisedSize` (the portal's rounded label, refreshed every run) is
  kept apart from `sizeBytes` (what landed on disk, written only when a run resolves the
  file). Stable formatting for minimal diffs. Written through `atomicfile`.
- `manifest` — `synty-sync.toml`, the committed project manifest (discovered by walking up
  from cwd, lives with the consuming project, carries no account identity): the engine
  `variant_includes` filter (no default — the user must set it) plus the pack-selection
  allowlist. New packs land **disabled** (opt-in), so buying a pack never silently downloads
  it. `sync`/`status` act only on enabled packs. Two `[[pack]]` entries for one slug are
  refused on load. Written through `atomicfile`.
- `web` — serves the local pack-selection page for `select` (checkbox grid, returns the
  chosen set); `newHandler` builds both handlers. Takes a bound listener, not an address.
  `/save` rewrites a committed file, so it requires the per-invocation token the rendered
  form carries, and both handlers refuse a request whose peer is not loopback or whose
  `Host` is not how a browser here addresses them. A page takes exactly one save (a later
  one gets 409), and a save accepted as the run is interrupted still wins.
  `main.listenLocal` refuses a non-loopback `--addr` to match.
- `selfupdate` — the `update` subcommand: fetches a GitHub release, downloads the
  current-platform binary, and atomically replaces the running executable. The repo is
  private, so it resolves a token from `GITHUB_TOKEN` / `GH_TOKEN` / the `gh` CLI.
- `atomicfile` — the one way a committed file is replaced: temp in the same directory,
  `Sync`, then rename, keeping the mode the file already had; each write first sweeps its
  own pattern's temps older than an hour beside the destination. Used by `lockfile` and
  `manifest`, whose records name bytes that are already on disk.
- `releaseyml` — parses the platform matrix out of `.github/workflows/release.yml`, the
  one place the published asset labels are decided. No production caller: it exists so
  the installer's guard and the updater's guard, which sit in packages that cannot share
  a test helper, read that list through one parser instead of two copies.
- `fixtures` + `cmd/scrubfixtures` — regenerate PII-free `testdata/` from git-excluded raw
  captures via an ordered replacement map.

### Key invariants (don't break these)

- **Files dedupe by `fileId`.** A file bundled under several packs downloads once and every
  owning pack's lockfile entry shares the same `cachePath`. Preserve this in `syncer` and
  `cache`. The `verdicts` struct is the whole of it, and every entry a run writes goes
  through one of its channels: `live` for what the store currently calls the file (token,
  variant, version, advertised size, read once for every owner), `resolved` for bytes the
  run has, `unresolved` for a file it went looking for and did not find, and `deselected`
  for one it read and declined, against why. A verdict with no channel leaves one `fileId`
  tracked under one owner and untracked under another, and the declined case has no failure
  behind it, so nothing else reports it. Identity taken off the row in front of a rebuild
  rather than out of `live` is the same bug in a second shape: the store labels a bundled
  file per order item. `readRows` is the only constructor that fills `live`, so a hand-built
  `verdicts` cannot leave it nil. Pruning a `Changed` file's prior copy (`removeSuperseded`)
  never deletes a path another `fileId` records (`claimedPaths`), and compares paths
  canonically and by file identity, so a respelled lockfile path cannot delete the bytes
  the run just fetched.
- **Selection is opt-in, and never silently wiped.** Newly-owned packs are disabled by
  default in `manifest`. An enumeration that returns no packs while a committed file
  holds some is refused rather than written — `syncer.ErrEmptyLibrary` for the lockfile,
  the same check in `main.selectPacks` for the manifest. `selectPacks` also refuses a
  library that owns none of the manifest's enabled packs (a session for the wrong account).
- **An expired session never replaces a record with its own view.** `portal.Enumerate`
  returns `ErrExpiredSession` via the logged-in sentinel, checked on **every** zero-anchor
  page and not just the first, and `ItemFiles` returns it for a session that expires part
  way through the item-page fetches; both abort the run before the lockfile is written. An
  enumeration that comes back empty while the lockfile holds packs is refused
  (`syncer.ErrEmptyLibrary`). A session that expires during the download pass, like an
  interrupt, stops the pass and saves the lockfile with only what was verified plus every
  unreached file carried forward as its prior record (never marked unresolved), then
  returns the error; a dry run writes nothing.
- **Nothing unverified reaches a real cache path.** `cache.Store` does not rename; the
  syncer `Commit`s only after the body checks pass. A download that answers with a
  document is refused twice — by Content-Type in `portal`, by a body sniff in `syncer` —
  because otherwise a login page is hashed and recorded as the pack's content. Downloads
  and adoption both add a zip end-of-central-directory check (`ErrTruncatedArchive`,
  permanent), because a copy that stopped part way still begins with an archive's magic.
  Adoption is the one path into the lockfile that skips `classify`, and a cache written
  before these guards existed can hold error pages under the right names.
- **A failed download fails its file, not the run.** The lockfile is still written; only
  failures a later run could clear move the exit status (`Report.ActionableFailures`). A
  file that already had a verified copy keeps its record when the *update* fails, at the
  version those bytes actually are — otherwise the cache holds them with nothing recording
  them, and an out-of-scope owner of the same `fileId` diverges from an in-scope one.
- **Strict parsing, with one deliberate exception.** A non-empty page that parses to zero
  files is an error; each tracked file must yield a `fileId` and a version. The exception is
  a page whose every row carries a variant keyword this build does not recognize: a future
  Synty engine, not broken markup. `ParseItemPage` returns those labels in `unknown` rather
  than failing, and `syncer.fetchAll` drops the pack so its prior record is carried forward
  whole and names the label in a warning. Failing there would take the whole mirror down over
  one new engine; rebuilding the pack from the empty list would erase everything it holds.
- **No PII in the repo.** The customer id, emails, cookies, and session captures
  (`config.toml`, `*.curl`, `cookies.txt`) live in the config dir outside this repo. The
  project manifest and lockfile (`synty-sync.toml`, `synty-sync.lock.json`) belong with
  the consuming project and are gitignored here
  defensively. The `internal/fixtures` guard test
  fails the build if real PII leaks into committed `testdata/`. Never commit these or
  hard-code a customer id / paths.

## Editing testdata

Don't hand-edit `testdata/portal/*.html`. They are generated by
`go run ./cmd/scrubfixtures` from git-excluded raw captures (`.longrun/`) through a scrub
map. Regenerate rather than patch, and keep the fixture guard test (`internal/fixtures`)
green.
