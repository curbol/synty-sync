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

A subcommand CLI. `main.go` `run()` parses flags, resolves config → session cookie →
`portal.Client`, then dispatches. Four subcommands: `select`, `status`, `sync`, `list`.
`status` is `sync` with `DryRun` (classify only, no downloads, no lockfile write) — both
go through `syncer.Run`.

Layered `internal/` packages, each with a package doc comment stating its contract:

- `config` — resolves settings by precedence: built-in defaults → `config.toml` → env
  (`SYNTY_CUSTOMER_ID`, `SYNTY_LIBRARY`) → flags. Config/state dir is XDG-resolved
  (`ResolveDir`); library cache dir defaults to `$XDG_DATA_HOME/synty-sync`. No
  machine-specific path is baked in. There is **no default engine variant** — the user
  must set `variant_includes`.
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
- `lockfile` — `synty-library.lock.json`: the authoritative record of owned packs,
  versions, checksums, and which files are downloaded. Stable formatting for minimal diffs.
- `manifest` — `packs.toml`, the pack-selection allowlist. New packs land **disabled**
  (opt-in), so buying a pack never silently downloads it. `sync`/`status` act only on
  enabled packs.
- `web` — serves the local pack-selection page for `select` (checkbox grid, returns the
  chosen set).
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
- **No PII in the repo.** The customer id, emails, cookies, and per-user state
  (`config.toml`, `packs.toml`, the lockfile, `*.curl`, `cookies.txt`) are gitignored and
  live in the config dir outside this repo. The `internal/fixtures` guard test fails the
  build if real PII leaks into committed `testdata/`. Never commit these or hard-code a
  customer id / paths.

## Editing testdata

Don't hand-edit `testdata/portal/*.html`. They are generated by
`go run ./cmd/scrubfixtures` from git-excluded raw captures (`.longrun/`) through a scrub
map. Regenerate rather than patch, and keep the fixture guard test (`internal/fixtures`)
green.
