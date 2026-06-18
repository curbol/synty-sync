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

All per-user state — your config, pack selection, and lockfile — lives in a config
directory *outside* this repo, resolved as:
`--config <dir>` › `$SYNTY_CONFIG_DIR` › `$XDG_CONFIG_HOME/synty-sync` › `~/.config/synty-sync`.

Provide your customer id via the `--customer` flag, `SYNTY_CUSTOMER_ID`, or a
`config.toml` in that directory (precedence in that order):

```bash
synty-sync select --customer <your-customer-id>
# or
export SYNTY_CUSTOMER_ID=<your-customer-id>
# or, persistently:
mkdir -p ~/.config/synty-sync && cp config.example.toml ~/.config/synty-sync/config.toml
$EDITOR ~/.config/synty-sync/config.toml   # set customer_id, library_path, etc.
```

The downloaded packs are cached at `$XDG_DATA_HOME/synty-sync`
(`~/.local/share/synty-sync`) by default; override with `library_path`,
`SYNTY_LIBRARY`, or `--library`.

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
synty-sync select   # pick which packs to mirror (opens a local web page)
synty-sync status   # what a sync would change (no downloads, no writes)
synty-sync sync     # download the delta and rewrite the lockfile
synty-sync list     # print the current lockfile
```

Useful flags: `--only <pack-slug-glob>`, `--library <dir>`, `--concurrency <n>`,
`--customer <id>`, `--config <dir>` (the config/state dir; default `~/.config/synty-sync`).

## Selecting packs

Selection is opt-in: a pack is only mirrored once you enable it. `synty-sync select`
enumerates your library and opens a local web page listing every owned pack with its
thumbnail and a checkbox; tick the ones you want, hit Save, and it writes `packs.toml`:

```toml
[[pack]]
slug = "polygon-pirate-pack"
name = "POLYGON - Pirate Pack"
enabled = true
```

`packs.toml` lives in your config dir (not this repo) — a small, diffable allowlist you can
keep under your own version control if you like. Newly-bought packs appear disabled on the
next `select`, so buying a pack never silently downloads it. `sync` and `status` only act on
enabled packs; with nothing enabled they do nothing and remind you to run `select`. You can
also hand-edit `packs.toml` instead of using the web page.

## What it does

- Enumerates every owned pack, reading each file's version inline, so it detects
  updates without downloading.
- Downloads only new/changed/missing files into `<library>/<fileToken>/<filename>.zip`,
  deduping files bundled across packs (e.g. `GENERIC_Particle_FX`) so they download once.
- Records everything in `synty-library.lock.json` in your config dir: owned packs,
  versions, checksums, and which files are downloaded under the current filter. Version it
  yourself if you want a changelog; it also regenerates from a sync.
- Warns for any owned pack that has no file matching the variant filter.

## Variant filter

By default it downloads `Godot_*` and `SourceFiles` variants. Packs that ship only
Unity/Unreal builds (the Sidekick character packs) or only `SourceSprites` (the HUDs)
produce a "no downloadable variant" warning. To pull those, set `variant_includes` in your
`config.toml`:

```toml
variant_includes = ["Godot_*", "SourceFiles", "SourceSprites", "Unity_2022_3"]
```

## Cache

The library cache is local and expendable. Current versions are always re-downloadable
from the store, and a `sync` re-downloads any cached file that is missing or fails its
checksum, so deleting the cache and re-syncing rebuilds it. Durability of the assets you
actually ship belongs in the consuming project (you promote the ones you use into it), not
in this cache.

## Notes

- Generated/test data: `cmd/scrubfixtures` regenerates the PII-free `testdata/` from
  the git-excluded raw captures; a guard test fails the build on any leaked PII.
- The tool is polite: bounded-concurrency page fetches and sequential downloads.
