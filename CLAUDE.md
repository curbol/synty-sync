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
  (`SYNTY_CUSTOMER_ID`, `SYNTY_LIBRARY`) → flags. Config/state dir is XDG-resolved
  (`ResolveDir`); library cache dir defaults to `$XDG_DATA_HOME/synty-sync`. No
  machine-specific path is baked in.
- `session` — builds the syntystore.com `Cookie` header from a Gecko browser cookie DB
  (Firefox or Zen, the zero-paste default; profile bases are per-platform), a
  `cookies.txt`, or a pasted-curl file.
  Forwards every syntystore.com cookie rather than guessing the session one.
- `portal` — the Sky Pilot Shopify portal client. `client.go` does HTTP (retry with
  backoff on 5xx/transient; fail-fast on 4xx), the `Enumerate` / `ItemFiles` / `Resolve`
  calls, and the response-level download guard; `parse.go` parses library-list and item
  pages with goquery. Retry counts, the per-attempt deadline and the page-size bound live
  on the client's `Limits` field, not in package vars. Parsing is deliberately strict: a
  non-empty page yielding zero files is a loud error, not a silent skip.
  `ErrExpiredSession` distinguishes an expired session from an empty library,
  `ErrNotAPackage` a download that answered with a document, and `ErrStalled` a body that
  stopped arriving. Downloads carry no whole-request deadline (a pack is gigabytes); the
  bound is `StallTimeout`, on silence, reset by every byte.
- `model` — shared domain types (`Pack`, `FileEntry`, `Variant`) and the identity rules:
  the pack `Slug` (from display name, since file-label tokens aren't stable within a pack)
  and the per-file `Key()` = `fileToken|variant`.
- `syncer` — orchestrates a run: sweep abandoned temps → enumerate → fetch item pages
  (bounded concurrency) → filter variants → **dedup files by `fileId`** → `classify` each
  against the prior lockfile + cache → download the delta (retry, resolving a fresh signed
  URL each attempt) → build and save the new lockfile. `classify` is pure and unit-tested;
  the `Class` enum (New/Changed/DownloadNow/CacheMissing/Adopted/Unchanged) is the core
  diff logic. It also owns the semantic download guards (the body sniff) and the failure
  model: `Report.Failures` plus `Report.ActionableFailures()`, which drives the exit status.
- `cache` — the local mirror, keyed by **file identity, not owning pack**, so a file
  bundled across packs is stored once. Two-phase writes: `Store` hashes into a
  `.synty-dl-*` temp and returns a `*Pending`, which the caller `Commit`s or `Discard`s.
  `Verify` (exact recorded size) / `VerifyDeep` (sha) / `Hash` / `Head` / `Tail` /
  `Remove` / `SweepTemps` — every one confines its `relPath` through `resolve`;
  `Migrate` folds pre-existing flat files into the layout, matching on a name key that
  drops the extension.
- `lockfile` — `synty-sync.lock.json` (beside the manifest, committed with the consuming
  project): the authoritative record of owned packs, versions, checksums, and which files
  are downloaded. `advertisedSize` (the portal's rounded label, refreshed every run) is
  kept apart from `sizeBytes` (what landed on disk, written only when a run resolves the
  file). Stable formatting for minimal diffs.
- `manifest` — `synty-sync.toml`, the committed project manifest (discovered by walking up
  from cwd, lives with the consuming project, carries no account identity): the engine
  `variant_includes` filter (no default — the user must set it) plus the pack-selection
  allowlist. New packs land **disabled** (opt-in), so buying a pack never silently downloads
  it. `sync`/`status` act only on enabled packs.
- `web` — serves the local pack-selection page for `select` (checkbox grid, returns the
  chosen set). Takes a bound listener, not an address. `/save` rewrites a committed file,
  so it requires the per-invocation token the rendered form carries, and both handlers
  refuse a `Host` that is not how a browser here addresses them.
- `selfupdate` — the `update` subcommand: fetches a GitHub release, downloads the
  current-platform binary, and atomically replaces the running executable. The repo is
  private, so it resolves a token from `GITHUB_TOKEN` / `GH_TOKEN` / the `gh` CLI.
- `fixtures` + `cmd/scrubfixtures` — regenerate PII-free `testdata/` from git-excluded raw
  captures via an ordered replacement map.

### Key invariants (don't break these)

- **Files dedupe by `fileId`.** A file bundled under several packs downloads once and every
  owning pack's lockfile entry shares the same `cachePath`. Preserve this in `syncer` and
  `cache`.
- **Selection is opt-in, and never silently wiped.** Newly-owned packs are disabled by
  default in `manifest`. An enumeration that returns no packs while a committed file
  holds some is refused rather than written — `syncer.ErrEmptyLibrary` for the lockfile,
  the same check in `main.selectPacks` for the manifest.
- **An expired session must never overwrite the lockfile.** `portal.Enumerate` returns
  `ErrExpiredSession` (via the page-1 logged-in sentinel) so a bad session aborts cleanly,
  and an enumeration that comes back empty while the lockfile holds packs is refused
  (`syncer.ErrEmptyLibrary`).
- **Nothing unverified reaches a real cache path.** `cache.Store` does not rename; the
  syncer `Commit`s only after the body checks pass. A download that answers with a
  document is refused twice — by Content-Type in `portal`, by a body sniff in `syncer` —
  because otherwise a login page is hashed and recorded as the pack's content. Adoption
  runs the same sniff plus a zip end-of-central-directory check: it is the one path into
  the lockfile that skips `classify`, a cache written before these guards existed can
  hold error pages under the right names, and a copy that stopped part way still begins
  with an archive's magic.
- **A failed download fails its file, not the run.** The lockfile is still written; only
  failures a later run could clear move the exit status (`Report.ActionableFailures`). A
  file that already had a verified copy keeps its record when the *update* fails, at the
  version those bytes actually are — otherwise the cache holds them with nothing recording
  them, and an out-of-scope owner of the same `fileId` diverges from an in-scope one.
- **Strict parsing.** A non-empty page that parses to zero files is an error; each tracked
  file must yield a `fileId` and a version.
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
