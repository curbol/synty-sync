# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`synty-sync` is a Go CLI that mirrors a Synty store "Your Library" into a local cache,
downloading only what changed since the last run. It is a download manager for direct
Synty-store purchases. See `README.md` (user-facing) and `docs/design.md` (the
authoritative design doc: page shapes, lockfile schema, failure model). Read
`docs/design.md` before changing enumeration, parsing, the lockfile, or the cache layout.

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

A subcommand CLI. `main.go` `run()` parses flags and dispatches seven subcommands:
`select`, `status`, `sync`, `list`, `browse`, `update`, `version`. `select`/`status`/`sync`
resolve config → session cookie → `portal.Client`; `list` needs only the lockfile; `browse`
needs only config (a read-only server over the local library); `update` and `version` need
neither. `status` is `sync` with `DryRun` (classify only, no downloads, no lockfile write) —
both go through `syncer.Run`.

Layered `internal/` packages, each with a package doc comment stating its contract:

- `config` — resolves settings by precedence: built-in defaults → `config.toml` → env
  (`SYNTY_CUSTOMER_ID`, `SYNTY_LIBRARY`) → flags. Config/state dir is XDG-resolved
  (`ResolveDir`); library cache dir defaults to `$XDG_DATA_HOME/synty-sync`. No
  machine-specific path is baked in.
- `session` — builds the syntystore.com `Cookie` header from a Gecko browser cookie DB
  (Firefox or Zen, the zero-paste default), a `cookies.txt`, or a pasted-curl file.
  Forwards every syntystore.com cookie rather than guessing the session one.
- `portal` — the Sky Pilot Shopify portal client. `client.go` does HTTP (retry with
  backoff on 5xx/transient; fail-fast on 4xx) and the `Enumerate` / `ItemFiles` /
  `Resolve` calls; `parse.go` parses library-list and item pages with goquery. Parsing is
  deliberately strict: a non-empty page yielding zero files is a loud error, not a silent
  skip. `ErrExpiredSession` distinguishes an expired session from an empty library.
- `model` — shared domain types (`Pack`, `FileEntry`, `Variant`) and the identity rules:
  the pack `Slug` (from display name, since file-label tokens aren't stable within a pack)
  and the per-file `Key()` = `fileToken|variant`.
- `syncer` — orchestrates a run: enumerate → fetch item pages (bounded concurrency) →
  filter variants → **dedup files by `fileId`** → `classify` each against the prior
  lockfile + cache → download the delta (retry, resolving a fresh signed URL each attempt)
  → build and save the new lockfile. `classify` is pure and unit-tested; the `Class` enum
  (New/Changed/DownloadNow/CacheMissing/Adopted/Unchanged) is the core diff logic.
- `cache` — the local mirror, keyed by **file identity, not owning pack**, so a file
  bundled across packs is stored once. Atomic temp-file + sha256 write (`Store`);
  `Verify`/`Exists`/`Hash`/`Remove`; `Migrate` folds pre-existing flat zips into the layout.
- `lockfile` — `synty-sync.lock.json` (beside the manifest, committed with the consuming
  project): the authoritative record of owned packs, versions, checksums, and which files
  are downloaded. Stable formatting for minimal diffs.
- `manifest` — `synty-sync.toml`, the committed project manifest (discovered by walking up
  from cwd, lives with the consuming project, carries no account identity): the engine
  `variant_includes` filter (no default — the user must set it) plus the pack-selection
  allowlist. New packs land **disabled** (opt-in), so buying a pack never silently downloads
  it. `sync`/`status` act only on enabled packs.
- `web` — serves the local pack-selection page for `select` (checkbox grid, returns the
  chosen set).
- `assetindex` — scans the local library into a searchable index of individual assets,
  seeing inside `.zip` and `.unitypackage` archives as well as loose files, and splitting a
  multi-animation `.glb` (a Quaternius-style animation library) into one virtual per-clip
  asset (`Source.Clip`) that shares the file's bytes; serves each asset's bytes and thumbnail
  on demand. HTTP-free — the `browse` server queries it.
- `browse` — serves the `browse` web UI to search and preview the library, querying an
  `assetindex.Index` and streaming asset bytes and thumbnails (three.js 3D previews,
  copy-path). Read-only over the local library except for asset tagging and linking, its
  one write surface (see `tagstore`); no session. It discovers the project manifest
  (best-effort, never required) only to locate the tag store beside it. `includeRelated=1`
  folds each tag match's linked companions into results; `/api/link` and `/api/related`
  write and resolve links.
- `tagstore` — `synty-sync.tags.toml`, a committed project file beside the manifest: a
  palette of user tags (label + color), per-asset **assignments keyed by content
  fingerprint**, and **link groups** (undirected sets of fingerprints that travel
  together, merged transitively) — all keyed on content (see the invariant below). Pure
  model + atomic, sorted TOML IO like `manifest`; the `browse` server loads it, mutates it
  under a mutex, and re-saves on each change. Rename onto an existing tag merges; links are
  result-expansion only, orthogonal to tags (a link never changes a fingerprint's tags);
  the store never prunes assignments or groups to a scanned set, so both survive a resync /
  another machine.
- `selfupdate` — the `update` subcommand: fetches a GitHub release, downloads the
  current-platform binary, and atomically replaces the running executable. The repo is
  private, so it resolves a token from `GITHUB_TOKEN` / `GH_TOKEN` / the `gh` CLI.
- `fixtures` + `cmd/scrubfixtures` — regenerate PII-free `testdata/` from git-excluded raw
  captures via an ordered replacement map.

### Key invariants (don't break these)

- **Files dedupe by `fileId`.** A file bundled under several packs downloads once and every
  owning pack's lockfile entry shares the same `cachePath`. Preserve this in `syncer` and
  `cache`.
- **Selection is opt-in.** Newly-owned packs are disabled by default in `manifest`.
- **An expired session must never overwrite the lockfile.** `portal.Enumerate` returns
  `ErrExpiredSession` (via the page-1 logged-in sentinel) so a bad session aborts cleanly.
- **Strict parsing.** A non-empty page that parses to zero files is an error; each tracked
  file must yield a `fileId` and a version.
- **Tags and links key on content, not the browse `id`.** `Asset.Fingerprint`
  (`crc32:<hex>:<size>` for zip/loose, `uguid:<guid>` for unitypackage, `<file-fp>#<clip>` for
  a split GLB clip) is the tag and link
  identity; it is portable and stable across updates, unlike `Asset.ID` (a machine-absolute,
  version-bearing locator hash used only to serve bytes). Bump `assetindex.indexVersion` when
  the fingerprint scheme or any indexed field changes so stale caches rebuild.
- **No PII in the repo.** The customer id, emails, cookies, and session captures
  (`config.toml`, `*.curl`, `cookies.txt`) live in the config dir outside this repo. The
  project manifest, lockfile, and tag store (`synty-sync.toml`, `synty-sync.lock.json`,
  `synty-sync.tags.toml`) belong with the consuming project and are gitignored here
  defensively. The `internal/fixtures` guard test
  fails the build if real PII leaks into committed `testdata/`. Never commit these or
  hard-code a customer id / paths.

## Editing testdata

Don't hand-edit `testdata/portal/*.html`. They are generated by
`go run ./cmd/scrubfixtures` from git-excluded raw captures (`.longrun/`) through a scrub
map. Regenerate rather than patch, and keep the fixture guard test (`internal/fixtures`)
green.
