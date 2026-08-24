# Synty Sync Design

`synty-sync`: a Go CLI that mirrors the assets owned on the Synty store into a local
library cache, detecting and downloading only what changed since the last run. It is the
Unity-Hub-equivalent that direct Synty-store purchases don't get. This tool covers
acquisition and library management; promoting specific assets into a given game/project,
format conversion (FBX→glTF, sprites), and a local-edit/patch model are out of scope here
and belong in the consuming project. Searching and previewing what has been mirrored is
also out of scope: that is [quarry](https://github.com/curbol/quarry), a separate tool that
reads the library this one writes.

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
                    # Exits non-zero if a file it was asked for could not be fetched.
synty-sync list     # print the current lockfile as a readable table.
synty-sync update   # self-replace the running binary from the latest GitHub release.
synty-sync version  # print the installed version.
```

Flags: `--manifest <path>` (project manifest; default: nearest `synty-sync.toml` walking up
from cwd), `--config <dir>` (user config dir), `--cookies <curl|file>` (override session
source), `--only <pack-glob>`, `--dry-run` (alias of `status` semantics on `sync`),
`--concurrency <n>`, `--library <path>`, `--addr <host:port>` (the `select` page's address,
default 8787). Subcommands take no positional arguments, so a stray one is an error rather
than silently swallowing the flags after it.

## Run flow

```
1. Load session            -> Cookie header for syntystore.com
2. Sweep abandoned temps   -> reclaim what an interrupted earlier run left behind
3. Enumerate library       -> walk line_items_page until empty; collect pack item URLs
4. Read each item page     -> [{pack, variant, version, sizeBytes, fileId, orderId, orderItemId}]
5. Apply variant filter    -> keep variants matching variant_includes (no default; set in the manifest)
6. Diff vs lockfile        -> new | changed (version differs) | unchanged
7. Download delta          -> 302 -> CloudFront -> temp file -> checks -> sha256 -> commit
8. Rewrite lockfile        -> current version/sha per file; print summary and failures
```

Steps 3-4 run at small concurrency with polite backoff; this is the only per-run store load
when nothing changed. Step 7 fails per file: the run continues, and the lockfile still
records everything that succeeded.

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
shares the same `cachePath`. A `sync` rebuilds only the packs it acted on; packs it did not
fetch (disabled in the manifest, or outside `--only`) keep their prior records rather than
being dropped, so the file stays a complete record of what is owned — and a bundled file
shared with a re-downloaded pack is repointed in lockstep. The account-identifying
`customerId` is **not** stored here (it is account PII; it lives in env / a gitignored local
config). Schema:

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
          "fileId": 2282645, "advertisedSize": 41700000, "sizeBytes": 41712983, "sha256": "…",
          "cachePath": "POLYGON_Pirate/POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip",
          "downloadedAt": "2026-06-16T…"
        },
        "GENERIC_Particle_FX|Godot_4_5_1": {
          "fileToken": "GENERIC_Particle_FX", "variant": "Godot_4_5_1", "version": "v1_0_0",
          "fileId": 2344711, "advertisedSize": 2700000, "sizeBytes": 2731401, "sha256": "…",
          "cachePath": "GENERIC_Particle_FX/GENERIC_Particle_FX_Godot_4_5_1_v1_0_0.zip",
          "downloadedAt": "2026-06-16T…"
        },
        "POLYGON_Pirate|Unity_2022_3": {
          "fileToken": "POLYGON_Pirate", "variant": "Unity_2022_3", "version": "v1_6_1",
          "fileId": 1164794, "advertisedSize": 141000000, "tracked": false
        }
      }
    }
  }
}
```

`tracked: false` (absent `sha256`/`cachePath`) marks a file that is owned and version-tracked
but not downloaded under the current filter. Git history of this file is the changelog.

The two size fields are deliberately separate. `advertisedSize` is the portal's rounded label
figure and refreshes on every run; `sizeBytes` is what actually landed on disk and is written
only when a run resolves the file. One field holding both leaves no way to tell a display
ballpark from the count the integrity check compares against.

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
cache reconstructable in practice: both commands read the cache when diffing, so a tracked
file that is missing, truncated, or (on `sync`, which re-hashes) corrupt re-downloads instead
of being reported unchanged.
The existing flat zips in the library are migrated into this layout on first run
(matched by a normalized filename key, since the real names render the variant unlike the
item-page token, e.g. `Source_Sprites` vs `SourceSprites`, and carry `(N)` collision suffixes).
Migration never replaces a copy already in the layout: the match is on name alone, and the
adopted file's hash is what gets recorded, so overwriting would let a stale flat zip be
adopted as verified content. Cache paths read back from the lockfile are confined to the
library root before use, since that file is committed and travels with the project.

## Download integrity

Nothing unverified ever holds a real cache path. `cache.Store` streams the body into a
`.synty-dl-*` temp file beside its eventual destination, hashing as it goes, and stops there:
the caller inspects the bytes and then `Commit`s or `Discard`s them. Renaming inside `Store`
would leave a window where an interrupt strands a rejected body exactly where the next run's
adopt scan would take it for genuine.

Three checks stand between a response and the lockfile:

1. The client refuses a document `Content-Type` before streaming anything. An expired session
   and a CDN refusal both answer the download href with a login page or an XML error, often at
   200, which the status check alone waves through.
2. The syncer sniffs the delivered bytes for the response that claims to be an archive and is
   not. Only text is refused: an archive format this tool has not seen must not be turned
   away, but no archive begins with prose, and a zero-byte body is not a pack.
3. `sha256` and the exact byte count are recorded from the committed file, and later runs
   compare against them.

Both rejections are permanent: no number of retries turns a login page into a pack.

The cache filename comes from the final signed-CloudFront URL path basename (the signed URL
sets `Content-Disposition` to a bare "attachment"). The portal's label size is rounded (e.g.
"2.6 MB" for 2,731,401 bytes), so it is a display ballpark only; it drives the progress line
and nothing else.

A run that dies mid-transfer leaves its temp behind. Every run sweeps temps older than a day
before enumerating — old enough that a concurrent run's in-flight transfer survives — and the
adopt scan skips the prefix outright, since a partial can carry enough of a name to normalize
onto a wanted file.

## Failure handling

- **Expired session vs terminator:** the terminator is a page with zero order_item anchors;
  enumeration walks until one (a short page is not the terminator). The logged-in sentinel
  (the real `.sky-pilot-search-input` element, the "Search My Products" box) is checked on
  **every** zero-anchor page to tell a legitimate end-of-walk (sentinel present: an empty
  library on page 1, the overflow page beyond the last otherwise) from an expired session
  (absent → exit with "refresh your session", do not overwrite the lockfile). The page past
  the last drops the "Your Library" heading but keeps the search box, so the sentinel is a
  reliable per-page marker while the heading is not; a session that expires mid-walk is
  therefore caught rather than read as the terminator, which would silently truncate the
  library and, through `select`, drop the user's enabled flags. The walk also stops if a
  page adds no packs it has not already seen, so a paginator that clamps an out-of-range
  page cannot loop forever.
- **Empty library against a populated lockfile:** refused outright. A read that returns
  nothing is far more often markup that moved than a library someone emptied, and the
  lockfile is committed to someone's project.
- **Changed markup:** parsing is defensive and asserts invariants (each tracked row yields a
  `fileId` and a version). A parse that yields zero files for a non-empty page is a loud
  error, not a silent skip — enforced at both layers: the item parser fails when a page has
  rows but none carry a version label (the file-heading class moving would otherwise make
  every row look like a versionless icon row), and the syncer refuses a pack that reaches it
  with no files at all, since rebuilding one from an empty list erases every entry it holds.
- **A failed download costs its file, not the run.** It is reported, left untracked so the
  next run retries it, and the lockfile is still written — aborting would throw away the
  record of everything the run *did* download. Only failures a later run could clear move the
  exit status; a 404 means the store no longer serves the file, and failing every future sync
  on it forever helps nobody.
- **Partial / corrupt downloads:** two-phase write, body checks, and sha + exact byte count,
  as above. `status` compares the recorded byte count (cheap, and enough to see a truncation);
  `sync` also re-hashes, which is the only check that sees a mid-file corruption.
- **De-owned packs:** a pack the library no longer lists is reported and its lockfile record
  kept. One enumeration is not enough to erase a committed record.
- **Politeness:** capped concurrency, honor obvious rate limits (429 and 408 back off and
  retry), and abandon the queue once a pack fails rather than fetching a whole library's
  item pages for a run that is going to abort.

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
