# Synty Sync Design

`synty-sync`: a Go CLI that mirrors the assets owned on the Synty store into a local
library cache, detecting and downloading only what changed since the last run. It is the
Unity-Hub-equivalent that direct Synty-store purchases don't get. This tool covers
acquisition and library management; promoting specific assets into a given game/project,
format conversion (FBX→glTF, sprites), and a local-edit/patch model are out of scope here
and belong in the consuming project.

## Goals

- One command pulls every owned pack to its latest version with no manual clicking.
- Detect updates without downloading: the store exposes each file's version inline, so a
  run is cheap when nothing changed.
- A committed lockfile is the authoritative record of what is owned and at what version,
  with monthly diffs that read like a changelog.
- The cache is local and expendable: a reconstructable mirror of current versions.
  Durability of the assets you actually use lives in the game repo, not here (see below).
- Resilient to the things that actually break: an expired session, changed page markup,
  and partial downloads.

## Non-goals

- Promotion of assets into the game repo (sub-project 2).
- FBX→glTF or sprite conversion (sub-project 3).
- The patch / 3-way-merge model for local edits (sub-project 4).
- Scripting the store login itself. The session is handed in (see Session handoff);
  automating `account.syntystore.com` (Shopify email-OTP) buys fragility for no gain at
  monthly cadence.
- Any mutation of the game repo's `assets/`.

## The model in one paragraph

The Synty store is the registry; owned packs are vendored dependencies. The registry is
mutable (a pack update replaces the Sky Pilot download, and the prior version is gone from
origin), but that only matters for assets you actually use, and those are captured durably
in the game repo at promotion time (sub-project 2), where git history is their backup and
their patch merge base. So the library cache itself is local and expendable: a
reconstructable mirror of *current* versions, no backup, no version archive. The lockfile
(in git) records what is owned and at what version. `sync` reconciles the two: enumerate the
live store, diff against the lockfile, download the delta into the cache (overwriting the
prior version per pack/variant), rewrite the lockfile.

## What the store exposes (validated)

The download portal is the Sky Pilot Shopify app, server-rendered (no headless browser
needed). Two page types and one download endpoint, all gated by the storefront session
cookie:

- **Library list:** `GET /apps/downloads/orders/{customerId}?line_items_page={n}`. The path
  segment is the *customer* id, not an order id. Titled `Your Library`, 15 packs per page,
  paginated by `line_items_page`. Each pack is an `<a class='sky-pilot-list-item'>` linking
  to `/apps/downloads/customers/{customerId}/orders/{orderId}/order_items/{orderItemId}`.
- **Item page:** lists each downloadable file as a row whose label carries the version
  inline, e.g. `POLYGON_Dungeon Godot_4_5_1 | v1_0_1 (40.3 MB)`,
  `ANIMATION_Base_Locomotion SourceFiles | v3 (91.9 MB)`,
  `ANIMATION_Base_Locomotion Unity_2021_1 | v1_1_3 (98.6 MB)`. Each row links to
  `/apps/downloads/downloads/{fileId}?email={email}&order_id={orderId}&order_item_id={orderItemId}`.
  A pack's preview icon appears as a versionless `*.png` row.
- **Download:** the file link 302-redirects to a short-lived signed CloudFront URL
  (`response-content-disposition=attachment`, real filename, range-capable). Detecting
  updates needs only the item pages (~one GET per pack); bytes are fetched only for the delta.

A label parses as `name=<pack> variant=<engine/format token> version=<vN...> size=<MB>`.
Versionless rows (icons) are skipped. The `variant` token is the filter key
(`Godot_4_5_1`, `Godot_4_6_2`, `SourceFiles`, `Unity_2021_1`, ...).

## CLI surface

```
synty-sync select   # pick which packs to mirror (opens a local web page)
synty-sync status   # enumerate + diff, print what would change. No downloads.
synty-sync sync     # status, then download the delta, verify, rewrite the lockfile.
synty-sync list     # print the current lockfile as a readable table.
synty-sync browse   # local web UI to search and preview the mirrored library.
synty-sync update   # self-replace the running binary from the latest GitHub release.
synty-sync version  # print the installed version.
```

Flags: `--manifest <path>` (project manifest; default: nearest `synty-sync.toml` walking up
from cwd), `--config <dir>` (user config dir), `--cookies <curl|file>` (override session
source), `--only <pack-glob>`, `--dry-run` (alias of `status` semantics on `sync`),
`--concurrency <n>`, `--library <path>`.

## Run flow

```
1. Load session            -> Cookie header for syntystore.com
2. Enumerate library       -> walk line_items_page until empty; collect pack item URLs
3. Read each item page     -> [{pack, variant, version, sizeBytes, fileId, orderId, orderItemId}]
4. Apply variant filter    -> keep variants matching variant_includes (no default; set in the manifest)
5. Diff vs lockfile        -> new | changed (version differs) | unchanged
6. Download delta          -> 302 -> CloudFront -> temp file -> sha256 -> atomic rename
7. Rewrite lockfile        -> current version/sha per (pack, variant); print summary
```

Steps 2-3 run at small concurrency with polite backoff; this is the only per-run store load
when nothing changed.

## Session handoff

Primary: read cookies directly from Firefox. The store cookies live in
`~/.mozilla/firefox/<profile>/cookies.sqlite` in plaintext; copy the (possibly locked) DB to
a temp path, query rows for `host LIKE '%syntystore.com'`, and rebuild the Cookie header. The
monthly run becomes just `synty sync`, auto-refreshed whenever the user has browsed the store.

Fallback: `--cookies <file>` accepts a pasted `curl` command or a `cookies.txt`, for
portability or when the browser path doesn't apply.

`customerId` is stable but account-identifying, so it is never committed: it comes from
`SYNTY_CUSTOMER_ID` env, `--customer`, or a gitignored `config.toml` in the user config dir.
The gating cookie is the storefront session (`_shopify_essential` and
siblings); the tool sends all `syntystore.com` cookies it finds rather than guessing the
exact one.

## Lockfile

Committed beside the project manifest as `synty-sync.lock.json`. JSON with sorted keys and
stable formatting so a sync produces a minimal, readable diff. Keyed by a stable **pack slug**
derived from the library-list display name, because the file-label token is *not* stable
within a pack (one pack's files can read `POLYGON_Pirate`, `POLYGON_Pirate_Pack`, and
`POLYGON_Pirates_Pack`). Each pack holds a per-**file** map, not per-variant, because a
single pack can carry two files of the same variant (its own `Godot_4_5_1` plus a bundled
`GENERIC_Particle_FX Godot_4_5_1`); the file key is `fileToken|variant`. Files are deduped by
`fileId`, so a file bundled under several packs is stored once and every owning pack's entry
shares the same `cachePath`. The account-identifying `customerId` is **not** stored here (it
is account PII; it lives in env / a gitignored local config). Schema:

```json
{
  "generatedAt": "2026-06-16T22:00:00Z",
  "packs": {
    "polygon-pirate-pack": {
      "displayName": "POLYGON - Pirate Pack",
      "orderId": 95580704, "orderItemId": 166480940,
      "files": {
        "POLYGON_Pirate|Godot_4_5_1": {
          "fileToken": "POLYGON_Pirate", "variant": "Godot_4_5_1", "version": "v1_0_1",
          "fileId": 2282645, "sizeBytes": 41700000, "sha256": "…",
          "cachePath": "POLYGON_Pirate/POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip",
          "downloadedAt": "2026-06-16T…"
        },
        "GENERIC_Particle_FX|Godot_4_5_1": {
          "fileToken": "GENERIC_Particle_FX", "variant": "Godot_4_5_1", "version": "v1_0_0",
          "fileId": 2344711, "sizeBytes": 2731401, "sha256": "…",
          "cachePath": "GENERIC_Particle_FX/GENERIC_Particle_FX_Godot_4_5_1_v1_0_0.zip",
          "downloadedAt": "2026-06-16T…"
        },
        "POLYGON_Pirate|Unity_2022_3": {
          "fileToken": "POLYGON_Pirate", "variant": "Unity_2022_3", "version": "v1_6_1",
          "fileId": 1164794, "sizeBytes": 141000000, "tracked": false
        }
      }
    }
  }
}
```

`tracked: false` (absent `sha256`/`cachePath`) marks a file that is owned and version-tracked
but not downloaded under the current filter. Git history of this file is the changelog.

## Cache

Local working mirror outside the repo, default `$XDG_DATA_HOME/synty-sync`
(`~/.local/share/synty-sync`). Current version only, and keyed by **file identity** (not
owning pack), so a file bundled under several packs is stored once:

```
<library>/<fileToken>/<original-filename>.zip
```

The original filename comes from the final signed-URL path basename (it matches Synty's
`<fileToken>_<variant>_<version>.zip` convention). Files are deduped by `fileId`: the bundled
`GENERIC_Particle_FX` lands once under `GENERIC_Particle_FX/` and every owning pack's lockfile
entry points at it. On update the tool writes the new version and removes the prior file for
that file identity, so the cache always reflects the lockfile's current state. No backup and
no version archive: current versions are re-downloadable from Synty, and the assets you depend
on are made durable in the game repo at promotion (sub-project 2), not here. What makes the
cache reconstructable in practice: `sync` reads the cache when diffing, so a tracked file that
is missing or fails its sha check on disk re-downloads instead of being reported unchanged.
The existing flat zips in the library are migrated into this layout on first run
(matched by a normalized filename key, since the real names render the variant unlike the
item-page token, e.g. `Source_Sprites` vs `SourceSprites`, and carry `(N)` collision suffixes).

## Download integrity

Each download streams to a temp file in the destination directory, hashes sha256 while
writing, then atomically renames into place and records `sha256` plus the actual byte count
in the lockfile. The portal's label size is rounded (e.g. "2.6 MB" for 2,731,401 bytes), so
it is a display ballpark only, not an exact integrity figure; integrity rests on the sha and
the atomic write. The cache filename comes from the final signed-CloudFront URL path
basename (the signed URL sets `Content-Disposition` to a bare "attachment"). A failed or
interrupted download leaves only the temp file, so the next run re-fetches.

## Failure handling

- **Expired session vs terminator:** the terminator is a page with zero order_item anchors;
  enumeration walks until one (a short page is not the terminator). The logged-in sentinel
  (the real `.sky-pilot-search-input` element, the "Search My Products" box) is checked only
  on **page 1's** zero-anchor case to tell an empty library (sentinel present) from an
  expired session (absent → exit with "refresh your session", do not overwrite the
  lockfile). The page past the last is a bare overflow shell with neither the heading nor a
  real search element nor anchors (confirmed live), so no per-page element can mark the
  terminator; zero anchors does.
- **Changed markup:** parsing is defensive and asserts invariants (each tracked row yields a
  `fileId` and a version). A parse that yields zero files for a non-empty page is a loud
  error, not a silent skip.
- **Partial / corrupt downloads:** temp-file + atomic-rename + size/sha verification, as above.
- **Politeness:** capped concurrency, small inter-request delay, honor obvious rate limits.

## Configuration

Two scopes. The **user config** (`~/.config/synty-sync/config.toml`, not committed to any
project) holds account identity and machine defaults: `customer_id`, `session_source`,
`library_path`, `concurrency`. It resolves via `--config` › `$SYNTY_CONFIG_DIR` ›
`$XDG_CONFIG_HOME/synty-sync` › `~/.config/synty-sync`, and the customer id may instead come
from `SYNTY_CUSTOMER_ID` / `--customer`. The **project manifest** (`synty-sync.toml`,
committed in the consuming repo, discovered by walking up from cwd or via `--manifest`) holds
the project-scoped settings: `variant_includes` and the `[[pack]]` allowlist. The manifest
schema has no account field, so no account PII can be committed through it. Machine paths also
via env (`SYNTY_LIBRARY`).

## Tag store

`synty-sync.tags.toml`, a third project-scoped file committed beside the manifest and
lockfile (discovered the same way; `--tags` / `--manifest` override), records user tags and
links for the `browse` UI. It holds a palette of `[[tag]]` definitions (`id` = the label text,
which is its identity; `color` = `#rrggbb`), `[[assignment]]` rows mapping a content
**fingerprint** to its tag ids, and `[[group]]` rows recording link groups, all sorted for
minimal diffs:

```toml
[[tag]]
  id = "hero"
  color = "#e11d48"
[[assignment]]
  fingerprint = "crc32:1a2b3c4d:41700000"
  tags = ["biome:forest", "hero"]
[[group]]
  fingerprints = ["crc32:2c54c32c:8635", "uguid:98960c3a158d24c4a933f0d99fb26946"]
```

Assignments and groups key on an asset's content, not its location or the browse `id` (which
embeds a machine-absolute path and a version-bearing archive name, so it is neither portable nor
stable across updates). The fingerprint is `crc32:<hex>:<size>` for zip entries and loose files
(the CRC is free from the zip central directory) and `uguid:<guid>` for unitypackage entries
(Unity's stable per-asset GUID). Byte-identical copies therefore share one fingerprint, so a tag
set once applies to every copy and survives a `sync` for unchanged files.

A multi-animation `.glb` (a Quaternius-style animation library, one file holding ~120 clips on a
shared rig) is split at scan time into one virtual asset per embedded clip: `assetindex` reads
only the GLB's JSON chunk for the animation names, then emits a per-clip asset whose `Source.Clip`
names the animation and whose bytes are the whole file (`/api/content` serves the file; the
preview plays `Source.Clip`). Each clip fingerprints as `<file-fingerprint>#<clipName>`, so clips
tag independently and stably. A root-motion (`_RM`) sibling is left whole (its clips would
duplicate the base file's); pairing the two as a root-motion toggle is a later concern.

`GET /api/assets` filters by tags with a repeatable `tag` param combined by `tagmode`: `and`
matches cards carrying all selected tags, `or` (the default) any. A card is matched on the
**union** of tags over its fingerprints, so a grouped card can satisfy an `and` query even when no
single copy carries every tag. (The browse UI's ANY/ALL toggle is this `tagmode`.)

A `[[group]]` is an **undirected** set of fingerprints that belong together (a UI frame and its
background fill, say). Groups merge transitively: linking `{A,B}` then `{B,C}` yields `{A,B,C}`.
They are a result-expansion concern only, orthogonal to tags: a group never changes what tags a
fingerprint carries. `GET /api/assets?includeRelated=1` takes each tag match's linked companions
and folds them into the result (relaxing only the tag filter, so other facets and the text search
still apply); each card's `related` field lists its companions' fingerprints; `GET
/api/related?fingerprint=` resolves a fingerprint set's companions to whole cards (for the
lightbox "parts of this set" strip); `POST /api/link {fingerprints, on}` links or unlinks.

The store never prunes to a currently-scanned set: assignments and groups for fingerprints
outside the current view are preserved, so tags and links survive a disabled pack, a narrowed
`--root`, or another machine. `browse` is otherwise read-only; this is its one write surface,
guarded by a mutex and written atomically. Tagging is disabled (the UI hides it) when no manifest
neighborhood is found, so `browse` still needs no manifest.

## Testing

- The two real portal pages already captured are checked in as parser fixtures (with the
  email/customer-id scrubbed): one library-list page, one item page. Unit tests assert the
  parser extracts the expected packs, variants, versions, sizes, and file ids.
- Diff logic is unit-tested against synthetic lockfile + enumeration pairs (new / changed /
  unchanged / variant-filtered).
- A network-touching integration test is gated behind a flag/env and is not part of the
  default suite.

## Open questions

- Session longevity: monthly cookie refresh is assumed; revisit only if a longer-lived
  auth path appears.
- Whether to also fetch and cache pack preview icons (currently skipped as versionless).
