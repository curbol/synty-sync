# synty-sync

A small Go CLI that mirrors your Synty store "Your Library" into a local cache,
downloading only what changed since the last run. It is the download manager that
direct Synty-store purchases don't get. Design: `docs/design.md`.

## Build

```bash
go build -o synty-sync .
```

Requires Go 1.26+. No cgo (the Firefox cookie reader uses pure-Go sqlite).

## One-time setup

The customer id is account-identifying, so it is never committed. Provide it via env
or a gitignored local config:

```bash
export SYNTY_CUSTOMER_ID=<your-customer-id>
```

or `config.local.toml`:

```toml
customer_id  = "<your-customer-id>"
library_path = "/home/you/code/synty-assets"   # where the cache lives (outside any repo)
```

Your customer id is the number in your library URL:
`https://syntystore.com/apps/downloads/orders/<customer-id>`.

## Session

The store gates downloads behind your logged-in browser session. By default the tool
reads cookies straight from Firefox (`session_source = "firefox"`), so a logged-in
browser means `synty-sync sync` needs no pasting. Override the source with `--cookies`:

```bash
# Firefox (default): just run it
synty-sync status

# Or paste a session: in DevTools > Network, right-click the library request >
# Copy > Copy as cURL, save to a file, then:
synty-sync status --cookies session.curl

# Or a Netscape cookies.txt export:
synty-sync status --cookies cookies.txt
```

A captured session expires; the Firefox source refreshes itself whenever you browse
the store, which is why it's the default for the monthly run.

## Commands

```bash
synty-sync status   # what a sync would change (no downloads, no writes)
synty-sync sync     # download the delta and rewrite the lockfile
synty-sync list     # print the current lockfile
```

Useful flags: `--only <pack-slug-glob>`, `--library <dir>`, `--concurrency <n>`,
`--config <dir>` (where `config.toml` and the lockfile live; default `.`).

## What it does

- Enumerates every owned pack, reading each file's version inline, so it detects
  updates without downloading.
- Downloads only new/changed/missing files into `<library>/<fileToken>/<filename>.zip`,
  deduping files bundled across packs (e.g. `GENERIC_Particle_FX`) so they download once.
- Records everything in `synty-library.lock.json` (committed): owned packs, versions,
  checksums, and which files are downloaded under the current filter. Its git history is
  your changelog.
- Warns for any owned pack that has no file matching the variant filter.

## Variant filter

By default it downloads `Godot_*` and `SourceFiles` variants. Packs that ship only
Unity/Unreal builds (the Sidekick character packs) or only `SourceSprites` (the HUDs)
produce a "no downloadable variant" warning. To pull those, add to
`config.local.toml`:

```toml
variant_includes = ["Godot_*", "SourceFiles", "SourceSprites", "Unity_2022_3"]
```

## Cache

The library cache is local and expendable. Current versions are always re-downloadable
from the store, and a `sync` re-downloads any cached file that is missing or fails its
checksum, so deleting the cache and re-syncing rebuilds it. Durability of the assets you
actually ship lives in the game repo at promotion time (a later sub-project), not here.

## Notes

- Generated/test data: `cmd/scrubfixtures` regenerates the PII-free `testdata/` from
  the git-excluded raw captures; a guard test fails the build on any leaked PII.
- The tool is polite: bounded-concurrency page fetches and sequential downloads.
