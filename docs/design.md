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
synty-sync select        # pick which packs to mirror (opens a local web page)
synty-sync status        # enumerate + diff, print what would change. No downloads.
synty-sync sync          # status, then download the delta, verify, rewrite the lockfile.
                         # Exits non-zero if a file it was asked for could not be fetched.
synty-sync list          # print the current lockfile as a readable table.
synty-sync update [ver]  # self-replace the running binary from the latest GitHub
                         # release, or the version named.
synty-sync version       # print the installed version.
```

Flags: `--manifest <path>` (project manifest; default: nearest `synty-sync.toml` walking up
from cwd), `--config <dir>` (user config dir; not a `list` flag, since `list` reads no
user config), `--cookies <curl|file>` (override session source), `--only <pack-glob>`,
`--dry-run` (alias of `status` semantics on `sync`), `--concurrency <n>`, `--library <path>`, `--addr <host:port>` (the `select` page's address,
default `localhost:8787`, and loopback only — see The selection page). No subcommand but
`update` takes a positional argument, and `update` takes at most one (the version to
install), so a stray one is an error rather than silently swallowing the flags after it.

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
records everything that succeeded. An interrupt, or a session found logged out during step 7,
ends the pass instead of failing each remaining file; step 8 still runs (see Failure
handling).

## Session handoff

Primary: read cookies directly from Firefox. The store cookies live in
`~/.mozilla/firefox/<profile>/cookies.sqlite` in plaintext; copy the (possibly locked) DB to
a temp path, query rows for `syntystore.com` and its subdomains, and rebuild the Cookie
header. The monthly run becomes just `synty sync`, auto-refreshed whenever the user has
browsed the store. Profile bases are per platform: the native, snap and flatpak layouts on
Linux, `~/Library/Application Support` on macOS, and on Windows `%APPDATA%` (read from the
variable, since Folder Redirection moves it off the profile) plus the Microsoft Store
Firefox's package container under `%LOCALAPPDATA%\Packages`, matched by a glob because the
container is named for an undocumented publisher hash. The run prints which database it
read (`session: read from <path>`) and names it again in the expired-session hint, since
which profile a search settled on is otherwise invisible.

Rows are grouped by `originAttributes`, which gives every cookie jar its own value.
Multi-Account Containers keeps a container's cookies in a separate jar, so one profile can
hold two sessions for two accounts, and ranking every row together lets a container left
signed into the other account supply the header, or half of it. The default jar decides
every name it holds; the other jars, most recently used first, fill only the names it
lacks, which keeps a sign-in made only inside a container (or under first-party isolation,
where every cookie carries attributes) working. Private-browsing rows are never read: they
are meant to die with the window.

The `-wal` sidecar is copied with it (the `-shm` index is not: SQLite rebuilds it from the
`-wal`), and when several profiles are in the running they are
ranked on the newest of the database and its sidecars: a WAL-mode database — which is every
browser that is actually open — takes its writes in the sidecar and moves the main file only
on a checkpoint, so the main file's mtime reads a live profile as older than it is. That is
how a leftover from a browser that moved between layouts (a deb install replaced by a snap)
wins the tie, and the run then reads real but months-old cookies and reports a session the
user just refreshed as expired.

Fallback: `--cookies <file>` accepts a pasted `curl` command or a `cookies.txt`, for
portability or when the browser path doesn't apply. A pasted curl is split the way the shell
it was copied for would split it, since DevTools writes it in whichever quoting the value
needs: POSIX single quotes, ANSI-C `$'…'`, double quotes with `\"`, or the Windows cmd
`^"…^"` form. The cookie is taken from `-H`/`--header 'Cookie: …'` or from `-b`/`--cookie`
(a `-b` value with no `=` is a cookie-jar filename, not a cookie, and is skipped).

`customerId` is stable but account-identifying, so it is never committed: it comes from
`SYNTY_CUSTOMER_ID` env, `--customer`, or a gitignored `config.toml` in the user config dir.
The gating cookie is the storefront session (`_shopify_essential` and
siblings); the tool sends all `syntystore.com` cookies it finds rather than guessing the
exact one.

## Lockfile

Committed beside the project manifest as `synty-sync.lock.json`. JSON with sorted keys and
stable formatting so a sync produces a minimal, readable diff; HTML escaping is off, so a
display name carrying `&` or `<` reads as itself rather than as `&`. It and the manifest
are both replaced through `internal/atomicfile`: a temp in the same directory, flushed, then
renamed. The flush is the half that is easy to leave out and impossible to notice: renaming
is atomic against a reader but says nothing about durability, so a crash can otherwise leave
a full-length file of zeros where the record of every cached byte used to be. A kill between
the create and the rename leaves the temp behind in the directory the user commits, so each
write first sweeps its own pattern's temps there that are more than an hour old. Every entry
must carry a `fileId`: every lookup a run makes goes through it, so an entry without one (a
hand edit or a merge) is refused, named, before anything is touched. Keyed by a stable **pack slug**
derived from the library-list display name, because the file-label token is *not* stable
within a pack (one pack's files can read `POLYGON_Pirate`, `POLYGON_Pirate_Pack`, and
`POLYGON_Pirates_Pack`). The slug carries the whole of a pack's identity in both committed
files, so enumeration refuses a library in which one is empty or two packs share one:
either shape writes a record that cannot be read back, and a generated discriminator would
have to rename the pack that held the name first, dropping its enabled flag. Each pack holds a per-**file** map, not per-variant, because a
single pack can carry two files of the same variant (its own `Godot_4_5_1` plus a bundled
`GENERIC_Particle_FX Godot_4_5_1`); the file key is `fileToken|variant`. Files are deduped by
`fileId`, so a file bundled under several packs is stored once and every owning pack's entry
shares the same `cachePath`, `version`, `sha256`, `advertisedSize` and `downloadedAt`,
including the owners a run did not fetch. A `sync` rebuilds only the packs it acted on; packs it did not
fetch (disabled in the manifest, or outside `--only`) keep their prior records rather than
being dropped, so the file stays a complete record of what is owned — and a bundled file
shared with a re-downloaded pack is repointed in lockstep. Every verdict travels to those
owners, not just a successful download: a file the run went looking for and did not find, and
a file it read and declined (filtered out, or archived by the store), both land untracked
under every owner. The declined case is the one with no failure behind it, so nothing else
would report it, and leaving it to the packs in scope alone puts one `fileId` in the file
tracked under one owner and untracked under another. The identity every owner records it at
travels the same way, read once from the pages rather than off the row in front of each
rebuild: the store labels a bundled file per order item, so two owners reading their own rows
commit two versions, two variants or two advertised sizes for one `fileId` at one sha. The account-identifying
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
          "fileId": 2282645, "tracked": true, "advertisedSize": 41700000, "sizeBytes": 41712983, "sha256": "…",
          "cachePath": "POLYGON_Pirate/POLYGON_Pirate_Godot_4_5_1_v1_0_1.zip",
          "downloadedAt": "2026-06-16T…"
        },
        "GENERIC_Particle_FX|Godot_4_5_1": {
          "fileToken": "GENERIC_Particle_FX", "variant": "Godot_4_5_1", "version": "v1_0_0",
          "fileId": 2344711, "tracked": true, "advertisedSize": 2700000, "sizeBytes": 2731401, "sha256": "…",
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
that file identity, so a tracked entry and the bytes it names stay in step. The prior path
comes out of a committed, hand-editable file, so it is compared with the new one canonically
(`./TOK/f.zip` is `TOK/f.zip`) and then by the filesystem (on a case-insensitive one
`tok/f.zip` is the same file), never as a string, or the prune deletes the bytes the run just
fetched; and a path any other `fileId` records is left alone with a warning, since a
hand-merged lockfile can name one path twice. A `Changed` file's new version may already be
on disk (the library is user-scoped and the lockfile project-scoped, so another project can
have fetched it); a copy under the new version's name is adopted on the same terms as an
untracked file, except at the prior record's own path, and the prior copy is then pruned the
same way. Removing a file also removes the `<fileToken>/` it leaves empty, and a store that
fails or is discarded does the same. A file that
stops being tracked leaves its bytes behind instead of deleting them, and is reported rather
than pruned (see Failure handling): the cache is the expensive half to rebuild, and a run
that declined a file this time is not evidence the reader wants it gone. No backup and
no version archive: current versions are re-downloadable from Synty, and the assets you depend
on are made durable in the game repo at promotion (sub-project 2), not here. What makes the
cache reconstructable in practice: both commands read the cache when diffing, so a tracked
file that is missing, truncated, or (on `sync`, which re-hashes) corrupt re-downloads instead
of being reported unchanged.
The existing flat files in the library are migrated into this layout on first run
(matched by a normalized filename key, since the real names render the variant unlike the
item-page token, e.g. `Source_Sprites` vs `SourceSprites`, and carry `(N)` collision suffixes).
A name off the disk drops its extension, so a Unity pack's `.unitypackage` folds in the same
way a `.zip` does; the synthetic `<token>_<variant>_<version>` key does not, since it has no
extension and stripping one would truncate it at the first version rendered with a dot. When
several names normalize onto one wanted file, they are tried in preference order (the one
that needed the least normalizing first) against the same byte checks adoption runs, and the
first that passes is folded in; the rest, refused ones included, are left flat rather than
stacked up in the layout with nothing recording them. The checks go into the choice rather
than running on what it returns, or a truncated canonical copy masks an intact `(1)` beside
it and the file re-downloads in full. Migration never replaces a copy
already in the layout: the match is on name alone, and the adopted file's hash is what gets
recorded, so overwriting would let a stale flat file be adopted as verified content.

Cache paths read back from the lockfile are confined to the library root before use, since
that file is committed and travels with the project. A path is first put in one canonical
spelling in slash space, which refuses a backslash, a colon, an escape through `..` and a
Windows device name identically on every platform: `Z:..\..\x` cleans to itself on Windows
and would confine on the machine that wrote it and escape on the one that read it. Every read,
write, rename and delete then goes through an `os.Root` opened on the library, so a symlinked
segment cannot carry it out of the tree; a `<fileToken>/` symlinked elsewhere fails its file
once with the reason rather than storing bytes no later check can find. The library root itself
may be a symlink, and the temp sweep descends it. Names the store supplies are held to the same
rules (no separators, no colon, no device name, no temp prefix), so a name stored on one
machine is one the other can resolve.

## Download integrity

Nothing unverified ever holds a real cache path. `cache.Store` streams the body into a
`.synty-dl-*` temp file beside its eventual destination, hashing as it goes, and stops there:
the caller inspects the bytes and then `Commit`s or `Discard`s them. Renaming inside `Store`
would leave a window where an interrupt strands a rejected body exactly where the next run's
adopt scan would take it for genuine.

Four checks stand between a response and the lockfile:

1. The client refuses a document `Content-Type` before streaming anything. An expired session
   and a CDN refusal both answer the download href with a login page or an XML error, often at
   200, which the status check alone waves through.
2. The syncer sniffs the delivered bytes for the response that claims to be an archive and is
   not. Only text is refused: an archive format this tool has not seen must not be turned
   away, but no archive begins with prose, and a zero-byte body is not a pack.
3. A file whose leading bytes say it is a zip must carry its end-of-central-directory record.
   A copy that stopped part way still begins with an archive's magic, and recording one takes
   its own short bytes as the file's truth, after which every verify compares them against
   themselves. The transport already fails a body that ends short of its length, so one that
   arrives whole without the trailer is what the server holds.
4. `sha256` and the exact byte count are recorded from the committed file, and later runs
   compare against them.

Every rejection is permanent: no number of retries turns a login page into a pack, and
refetching a short archive transfers the same short bytes. A document cannot say on its own
whether it is a CDN refusal or a session that expired during the download pass, so on either
document rejection the client asks the first library page for the logged-in sentinel, and a
session found logged out is reported as `ErrExpiredSession` (see Failure handling) rather than
as one more failed file. The same sniff and trailer check run on adoption, which is the one
path into the lockfile that never consults `classify`, and a cache written before these guards
existed can hold error pages under exactly the right names. The trailer check is keyed on the
bytes rather than the extension: the filename comes from a signed URL or from whoever placed
the file, and the adopt scan matches a wanted file under any extension or none. A container
it cannot read (`.unitypackage`) passes through.

The cache filename comes from the final signed-CloudFront URL path basename (the signed URL
sets `Content-Disposition` to a bare "attachment"). The portal's label size is rounded (e.g.
"2.6 MB" for 2,731,401 bytes), so it is a display ballpark only; it drives the progress line
and nothing else.

A run that dies mid-transfer leaves its temp behind. Every run sweeps temps older than a day
before enumerating — old enough that a concurrent run's in-flight transfer survives — and the
adopt scan skips the prefix outright, since a partial can carry enough of a name to normalize
onto a wanted file. The prefix is a reserved namespace on the way in as well: a filename the
store supplies cannot wear it, or the file would commit to a real path that the next day's
sweep deletes and no scan can take back.

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
  library and, through `select`, drop the user's enabled flags. The walk also fails if a
  page adds no packs it has not already seen, so a paginator that clamps an out-of-range
  page cannot loop forever; it is an error rather than a stop, since the packs gathered so
  far may be a truncated library and returning them is the outcome the sentinel exists to
  prevent. An item page that turns out to be a logout shell is reported the same way rather
  than as changed markup.
- **Interrupted, or logged out mid-download:** a Ctrl-C, or a download that finds the session
  logged out, ends the download pass, since every file after it would fail for the same
  reason. A real run still saves the lockfile: what the pass verified is recorded, and every
  file it never reached keeps its prior record unchanged (a tracked copy is carried as itself,
  unexamined), rather than being marked as looked for and not found. Nothing is lost and
  nothing is replaced by an expired session's view; the run prints its summary and then
  exits with the error. A dry run writes nothing either way.
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
  on it forever helps nobody. A file that already had a verified copy is the exception to
  "left untracked": when an *update* fails, the entry keeps pointing at the copy on disk, at
  the version that copy actually is. Rebuilding it from the live page would drop the path and
  sha while the bytes stayed in the cache with nothing recording them, and would leave an
  out-of-scope pack bundling the same `fileId` carrying a record its owner no longer had.
- **A stalled transfer.** A download carries no whole-request deadline, because a pack
  legitimately runs to gigabytes. The bound is on silence instead: the response headers
  satisfy `ResponseHeaderTimeout` and a server can then stop sending, which would block the
  read forever — the attempt never returns, so the retry whose whole purpose is re-signing an
  expired CloudFront URL never runs. Every byte that arrives resets the window.
- **Partial / corrupt downloads:** two-phase write, body checks, and sha + exact byte count,
  as above. `status` compares the recorded byte count (cheap, and enough to see a truncation);
  `sync` also re-hashes, which is the only check that sees a mid-file corruption.
- **De-owned packs:** a pack the library no longer lists is reported and its lockfile record
  kept. One enumeration is not enough to erase a committed record. The summary names the
  first ten and counts the rest: a session for another account lists a library disjoint from
  the lockfile, and a line per recorded pack buries everything printed around it.
- **A file that stops being tracked is named on the way out.** A pack the store still lists
  keeps its entry, so a file the run declines never reaches `orphanedRecords`, but the entry
  is rebuilt untracked and takes its cache path and sha with it while the bytes stay on disk,
  where nothing points at them and no later run can take them back. Both causes are reported
  the same way and said once, on the run that drops the record: the store archiving the file,
  and a `variant_includes` that no longer matches it. Only the sentence differs, because one
  is the store's doing and the other is the reader's own manifest.
- **Politeness:** capped concurrency, honor obvious rate limits (429 and 408 back off and
  retry), and abandon the queue once a pack fails rather than fetching a whole library's
  item pages for a run that is going to abort.

## The selection page

`select` serves the checkbox grid on loopback and takes back the whole pack selection,
which it writes to a committed file, so the endpoint that receives it is treated as one
worth protecting. A cross-origin form POST is a CORS "simple request" — no preflight, and
nothing in the browser stops it — so being POST-only is not enough on its own: any page open
in another tab while `select` is running could otherwise submit a set of slugs (they are
derived from public display names, so they are guessable) and both discard the real
selection and enable packs nobody chose. The rendered form carries a per-invocation token
that `/save` requires, read from the body so a link cannot stand in for the page. Both
handlers also refuse a request whose peer is not loopback, and then one whose `Host` is not a
way a browser on this machine addresses them, since a page that points its own name at a
loopback address is otherwise same-origin with the selection page and free to read the whole
pack list, which is a purchase history. The peer comes first because the `Host` is the
client's to claim: on a wildcard bind, checking only the `Host` inverts, admitting a remote
client that says `127.0.0.1` while refusing the browser on the machine the bind was aimed at,
which can only send that machine's real address. `--addr` therefore takes a loopback address
or nothing: a wider bind cannot widen the page's reach, only leave the port open. A page
bound to port 80 is addressed with a portless `Host`, and that is accepted.

A page takes exactly one save. `select` reads one selection and stops, so a second
submission (a double click, another tab) is answered 409 rather than told it was taken while
nobody reads it. A save accepted as the run is interrupted has already told the browser it
was taken, so it wins over the interrupt and is written. A page that fails to render answers
500 and says why on the terminal, rather than serving a blank 200.

Before the page goes up, `select` refuses two libraries it cannot tell from a bad read: one
that lists no packs while the manifest holds some, and one that owns none of the packs the
manifest enables, which is what a session signed into another account enumerates. Either
would rewrite the allowlist from a library that is not this project's.

## Configuration

Two scopes. The **user config** (`~/.config/synty-sync/config.toml`, not committed to any
project) holds account identity and machine defaults: `customer_id`, `session_source`,
`library_path`, `concurrency`. It resolves via `--config` › `$SYNTY_CONFIG_DIR` ›
`$XDG_CONFIG_HOME/synty-sync` › `~/.config/synty-sync`, and the customer id may instead come
from `SYNTY_CUSTOMER_ID` / `--customer`. A config dir named by `--config` or
`$SYNTY_CONFIG_DIR` must exist and be a directory: an absent `config.toml` is the ordinary
first run, so a misspelled dir would otherwise read as no config and mirror gigabytes into
the default library. The library defaults to `$XDG_DATA_HOME/synty-sync`, else
`~/.local/share/synty-sync`; with neither available the run stops and names the ways to set
it rather than writing into the working directory. A `concurrency` below 1, in the file or
on the flag, is refused. The **project manifest** (`synty-sync.toml`,
committed in the consuming repo, discovered by walking up from cwd or via `--manifest`) holds
the project-scoped settings: `variant_includes` and the `[[pack]]` allowlist. Two entries for
one slug are refused, since the readers disagree over which wins. The manifest
schema has no account field, so no account PII can be committed through it. Machine paths also
via env (`SYNTY_LIBRARY`).

## Testing

The suite is offline and hermetic: no network, no session, no customer id. `go test ./...`
is the gate CI and the release workflow run, under `-race` on Linux and plainly on Windows and
macOS, where the executable signature, the installer's checks, the browser profile bases and
the rename that replaces a running binary take their other branches. CI also cross-compiles
every platform `release.yml` builds and refuses a tracked compiled binary (proving with probe
builds that its pattern still matches one). `.gitattributes` pins LF in the working tree,
since the tests read workflows and scripts byte for byte, and leaves `testdata/` untouched.

- Real portal pages are checked in as parser fixtures, scrubbed of the email and customer id
  by `go run ./cmd/scrubfixtures` (never hand-edited). `internal/fixtures` fails the build if
  either leaks back in, walking every `testdata/` directory in the repo (not dot- or
  underscore-prefixed ones, nor a nested checkout) and refusing a committed SQLite database
  or WAL, which is what a copied `cookies.sqlite` fixture would be. Unit tests assert the parser extracts the expected packs, variants,
  versions, sizes, and file ids, and the pagination walk runs against the real captures.
- Diff logic is unit-tested against synthetic lockfile + enumeration pairs (new / changed /
  unchanged / variant-filtered). Whole runs go through `httptest` stores that can withhold a
  file, serve a login page where a pack belongs, or stop advancing their paginator.
- Each package keeps its guard tests in `audit_test.go`, one per invariant, each carrying a
  comment naming the failure it prevents. A guard test that fails is a regression, not a test
  to update. `install_test.go` guards `install.sh` and the workflows, including that every
  action is pinned to a commit SHA with its exact version in a comment.

## Open questions

- Session longevity: monthly cookie refresh is assumed; revisit only if a longer-lived
  auth path appears.
- Whether to also fetch and cache pack preview icons (currently skipped as versionless).
