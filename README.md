# synty-sync

A small Go CLI that mirrors your Synty store "Your Library" into a local cache,
downloading only what changed since the last run. It is the download manager that
direct Synty-store purchases don't get. Design: `docs/design.md`.

## Install

Grab the latest release binary into `~/.local/bin` (private repo, so it uses your
`gh` login or `GITHUB_TOKEN`):

```bash
gh api repos/curbol/synty-sync/contents/install.sh --jq .content | base64 -d | bash
```

Then update in place anytime:

```bash
synty-sync update           # latest release
synty-sync update 0.2.0     # a specific version
synty-sync version          # what's installed
```

`update` self-replaces the running binary; it needs a release build (a dev build
tells you to use the installer). Releases are cut by pushing a `v*` tag — a GitHub
Actions workflow builds the cross-platform binaries and publishes them:

```bash
git tag v0.2.0 && git push origin v0.2.0
```

## Build from source

```bash
go build -o synty-sync .
```

Requires Go 1.26+. No cgo (the browser cookie reader uses pure-Go sqlite). To stamp
a version into a local build: `go build -ldflags "-X main.version=0.2.0" -o synty-sync .`

## One-time setup

Two kinds of state, kept apart:

- **User config** (account identity, session, machine defaults) lives *outside* any
  project, resolved as `--config <dir>` › `$SYNTY_CONFIG_DIR` › `$XDG_CONFIG_HOME/synty-sync`
  › `~/.config/synty-sync`. Provide your customer id via `--customer`, `SYNTY_CUSTOMER_ID`,
  or a `config.toml` there (precedence in that order):

  ```bash
  mkdir -p ~/.config/synty-sync && cp config.example.toml ~/.config/synty-sync/config.toml
  $EDITOR ~/.config/synty-sync/config.toml   # set customer_id, library_path, session_source
  ```

- **Project manifest** (`synty-sync.toml`: engine variants + pack selection) lives *in* the
  project that consumes the assets, committed to its repo. synty-sync discovers it by walking
  up from the working directory, or point at it with `--manifest`. Its lockfile
  (`synty-sync.lock.json`) is written beside it. It carries no account identity. See
  `synty-sync.example.toml`.

The downloaded packs are cached at `$XDG_DATA_HOME/synty-sync`
(`~/.local/share/synty-sync`) by default; override with `library_path`,
`SYNTY_LIBRARY`, or `--library`.

Your customer id is the number in your library URL:
`https://syntystore.com/apps/downloads/orders/<customer-id>`.

## Session

The store gates downloads behind your logged-in browser session. The tool reads cookies
straight from a Gecko browser — `session_source = "firefox"` (default) or `"zen"` (a
Firefox fork, same cookie store) — so a logged-in browser means `synty-sync sync` needs
no pasting. Override the source with `--cookies`:

```bash
# Read from the browser you set in config: just run it
synty-sync status

# Or pick a browser per-run:
synty-sync status --cookies zen

# Or paste a session: in DevTools > Network, right-click the library request >
# Copy > Copy as cURL, save to a file, then:
synty-sync status --cookies session.curl

# Or a Netscape cookies.txt export:
synty-sync status --cookies cookies.txt
```

Reading from the browser auto-refreshes the session whenever you browse the store, which
is why it's the no-maintenance default. (A pasted curl/cookies.txt expires and must be
re-grabbed.)

## Commands

```bash
synty-sync select   # pick which packs to mirror (opens a local web page)
synty-sync status   # what a sync would change (no downloads, no writes)
synty-sync sync     # download the delta and rewrite the lockfile
synty-sync list     # print the current lockfile
synty-sync browse   # search & preview the local library in a web UI
```

Useful flags: `--manifest <path>` (project manifest; default: nearest `synty-sync.toml`
walking up from cwd), `--only <pack-slug-glob>`, `--library <dir>`, `--concurrency <n>`,
`--customer <id>`, `--config <dir>` (user config dir; default `~/.config/synty-sync`).

## Browsing the library

`synty-sync browse` indexes the local library and opens a web page to search it by
name, filter by type / vendor / engine variant, see thumbnails, click to enlarge
(images, plus live 3D for GLB and FBX), and copy an asset's path. It reads inside
`.zip` and `.unitypackage` archives, so individual models, sprites, and materials are
searchable without unpacking anything.

The same file shipped across engine variants or bundled in several packs collapses
into one card (a `×N` badge); the enlarged view lists every copy's path with a
copy-all. Toggle "group dupes" off to see each occurrence separately.

### Tagging

Tag assets to organize the library your way. Hover a card and hit the tag button to
assign existing tags or type a new one (created on the spot with a random color); the
enlarged view has the same controls plus a color swatch on each tag. Tagged assets
show a thin colored sliver per tag along the card's bottom edge, and the header **tags**
filter narrows the grid to the tags you pick, with an **ANY / ALL** toggle (match any
selected tag, or only assets carrying all of them) and a **manage** mode to rename,
recolor, or delete tags. A tag is just a label; `key:value` labels (e.g. `biome:forest`)
are a convention, not enforced.

Assets can also be **linked** into a set that belongs together (a UI frame and its
background fill, say). The enlarged view lists a linked asset's companions as *parts of
this set* (click to open one), and the tags filter's **linked** toggle folds each
match's companions into the grid, so a `verdict:` query keeps the frame and its fill
together instead of dropping the untagged one. Links are made through the API (below) or
a project seeder, not the tag buttons.

Tags and links live in `synty-sync.tags.toml` beside the project manifest, committed to
the consuming project so they travel with it and diff cleanly. They key on each asset's
**content**, so a tag follows the file across packs and survives a `sync` — an unchanged
file keeps its tags, and a file Synty re-exports starts fresh. Tagging needs a project
manifest to sit next to (discovered by walking up from the working directory, or
`--tags <path>` / `--manifest <path>`); with no manifest found, browse still runs and
the tag controls are simply hidden.

Animated models play in the viewer with a scrub bar and clip selector. A mesh-less
animation clip (Synty `ANIMATION_*` packs, kevdev, etc.) plays on a character whose
rig it matches, auto-picked from the same vendor's library assets. Use "change" to
swap the body, or "pin" one as the default for its rig. Cross-rig cases that need
true retargeting (e.g. an A-pose rig onto a T-pose body) are out of scope for the
preview — bake those offline.

```bash
synty-sync browse --root ~/code/raw-assets   # scan a whole asset tree
synty-sync browse                            # defaults to the library dir
```

The scan root resolves as `--root` > `browse_root` (config.toml) / `SYNTY_BROWSE_ROOT`
> the library dir, so set `browse_root` once to browse everything. Browse flags:
`--addr <host:port>` (default `localhost:8788`), `--reindex` (rebuild the index),
`--cache <dir>` (index / unpacked-archive cache; default `~/.cache/synty-sync`). The
index is cached and refreshed incrementally, so only the first run pays the full scan.
Textureless Synty source FBX render as neutral clay; animation-only FBX (no mesh) show
an icon.

The page is backed by a small read-only JSON API you can script against. `GET /api/assets`
takes `q` (a Google-style query: space is AND, `OR` / `|` alternate, `-` excludes, `"…"` is
an exact phrase, `( )` groups, and `field:value` scopes a term to one field — `name`, `pack`,
`vendor`, `type`, `variant`, `ext`, `guid`, `path` — so `turn loop vendor:kevdev -idle` works;
a bare term matches name, pack, and path), repeatable `type` / `vendor` / `variant` / `guid` filters, repeatable `tag` with
`tagmode=and` (match all selected tags) or `tagmode=or` (any; the default), `group=0` to keep
duplicates separate, `sort=path`, and `offset` / `limit`; each asset carries its `source`
locator (`kind`, `archivePath`, `entry`, `guid`, `pathname`, `hasPreview`, `clip`), pixel
`width` / `height` for images, and every duplicate copy. A multi-animation `.glb` (an
animation library like Quaternius, one file holding ~120 clips) is split into one card per
clip: each shares the file's bytes and names its animation in `source.clip`, so the clips are
individually searchable, taggable, and previewable. `?guid=` resolves a scene or
prefab's asset references (bare GUIDs) straight back to the owning asset, so composition
extraction is one request rather than tar-scraping the archive. Each asset also carries
its content `fingerprints`, current `tags`, and any linked `related` fingerprints;
`includeRelated=1` folds each tag match's linked companions into the results. `GET
/api/content?id=` streams an asset's bytes and `GET /api/thumb?id=` its Unity preview.
When tagging is enabled, `GET /api/tags` returns the palette, `POST/PATCH/DELETE
/api/tags` create/rename/recolor/delete a tag, `POST /api/assign` toggles a tag across an
asset's fingerprint set, `POST /api/link` links or unlinks a set of fingerprints as one
travel-together group, and `GET /api/related?fingerprint=` returns the cards linked to a
fingerprint set.

## Selecting packs

Selection is opt-in: a pack is only mirrored once you enable it. `synty-sync select`
enumerates your library and opens a local web page listing every owned pack with its
thumbnail and a checkbox; tick the ones you want, hit Save, and it writes the `[[pack]]`
entries into `synty-sync.toml`:

```toml
[[pack]]
slug = "polygon-pirate-pack"
name = "POLYGON - Pirate Pack"
enabled = true
```

`synty-sync.toml` is committed in the consuming project — a small, diffable manifest of the
packs that project draws from. Newly-bought packs appear disabled on the next `select`, so
buying a pack never silently downloads it. `sync` and `status` only act on enabled packs;
with nothing enabled they do nothing and remind you to run `select`. You can also hand-edit
`synty-sync.toml` instead of using the web page.

## What it does

- Enumerates every owned pack, reading each file's version inline, so it detects
  updates without downloading.
- Downloads only new/changed/missing files into `<library>/<fileToken>/<filename>.zip`,
  deduping files bundled across packs (e.g. `GENERIC_Particle_FX`) so they download once.
- Records everything in `synty-sync.lock.json` beside the manifest: owned packs,
  versions, checksums, and which files are downloaded under the current filter. Commit it
  for a changelog; it also regenerates from a sync.
- Warns for any owned pack that has no file matching the variant filter.

## Variant filter

The tool is engine-agnostic and has **no default engine** — you set the variants for
your engine in the project manifest `synty-sync.toml` (`sync`/`status` tell you if it's
unset). Synty packs ship
`Godot_*`, `Unity_*`, `Unreal_*`, plus engine-agnostic `SourceFiles` (raw source) and
`SourceSprites` (HUD/interface images):

```toml
variant_includes = ["Godot_*", "SourceFiles"]            # Godot
# variant_includes = ["Unity_*", "SourceFiles"]          # Unity
# variant_includes = ["Unreal_*", "SourceFiles"]         # Unreal
# add "SourceSprites" for HUD packs
```

A pack with no file matching your variants produces a "no downloadable variant" warning
(e.g. Sidekick character packs ship Unity-only; HUDs ship `SourceSprites`-only).

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
