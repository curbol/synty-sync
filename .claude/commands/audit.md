# Audit

Deep code quality review of the synty-sync codebase. Reviews everything by default. If the user
provides a scope (e.g., "audit the sync diff logic", "audit portal parsing"), narrow to those
areas.

## Step 1: Determine scope

- **No arguments:** Review the entire codebase. Gather all `.go` files under `internal/`, `cmd/`,
  and the repo root (`main.go`, `main_test.go`).
- **With scope:** Interpret the user's natural language to identify which packages and files to
  review. When in doubt, include more rather than less.

Do not review generated or captured data: `testdata/` fixtures are not hand-edited (they are
produced by `cmd/scrubfixtures`), so review the scrub logic, not the output.

## Step 2: Run baseline checks

Run these to establish a baseline. Proceed with the audit either way — sub-agent findings may
explain failures or reveal them as pre-existing. Report any failure as Tier 1, ahead of new audit
findings.

```bash
go build ./...
go test ./...
go test -race ./internal/syncer/   # syncer fans out with WaitGroup + Mutex
go vet ./...
gofmt -l .                         # any output = unformatted files
```

The default suite is fully offline (httptest + committed `testdata/portal/*.html`), so it must
pass without network or a real session. A network-touching integration test is gated behind a
flag/env and is out of scope; don't try to run it.

## Step 3: Dispatch review sub-agents

Use `feature-dev:code-reviewer` sub-agents to review the scoped files. Split by area so agents run
in parallel:

- **Portal / network** — `internal/portal/` (`client.go`, `parse.go`), `internal/retry/`. HTTP,
  retry/backoff, the failure model, strict parsing, `ErrExpiredSession`.
- **Sync core** — `internal/syncer/`, `internal/model/`, `internal/lockfile/`, `internal/manifest/`.
  `classify`, the `Class` enum, fileId dedup, opt-in selection, concurrency, lockfile/manifest
  stability.
- **Storage & identity** — `internal/cache/`, `internal/session/`, `internal/config/`,
  `internal/selfupdate/`, `internal/fixtures/`, `cmd/scrubfixtures/`. Atomic writes, XDG path
  resolution, cookie building, PII, self-update, fixture scrubbing.
- **CLI & selection page** — `main.go`, `internal/web/`. Flag parsing, subcommand dispatch,
  config → session → client wiring, and the local pack-selection page `select` serves.

For each sub-agent, provide:
- The full list of files in its area (not a diff).
- The scope description from the user (if any), so it understands what to focus on.
- The review criteria below that apply to its area, plus the core invariants (which apply
  everywhere).

Tell sub-agents to read entire files, not just scan for patterns. Understanding context is required
to find real issues. Each package's doc comment states its contract; hold the code to it.

### Priority tiers for sub-agents

Sub-agents must categorize every finding into a tier. Tier 3 findings should only be reported if
they are clearly valuable; when in doubt, leave them out.

**Tier 1 (must fix):** Bugs, invariant violations, correctness errors, data races, unatomic writes,
swallowed errors that hide failures, PII or machine paths in code.

**Tier 2 (should fix):** Significant duplication, meaningful refactors, missing test coverage for
important behavior, API design problems, resource leaks, lockfile-diff churn.

**Tier 3 (consider):** Naming issues, minor API surface cleanup, test reorganization, idiom nits.

### Review criteria

**Core invariants (hard rules — violations are Tier 1)**

These are the contracts the whole tool rests on. See `CLAUDE.md` and `docs/design.md`.

- **Files dedupe by `fileId`.** A file bundled under several packs downloads once, and every owning
  pack's lockfile entry shares the same `cachePath`. Any path that could download the same fileId
  twice, or let two owning packs record diverging `cachePath`, is a violation.
- **An expired session must never overwrite the lockfile.** `portal.Enumerate` returns
  `ErrExpiredSession` (the page-1 logged-in sentinel distinguishes an empty library from an expired
  session). Trace the path: any code that writes the lockfile after an enumeration error, or that
  loses the `ErrExpiredSession` distinction (e.g. `%v` instead of `%w`, or a swallowed error), is a
  violation.
- **Selection is opt-in.** Newly-owned packs are added to the manifest **disabled**. `sync`/`status`
  act only on enabled packs. Any code that enables or downloads a pack the user never opted into is
  a violation.
- **Strict parsing.** A non-empty page that parses to zero files is a loud error, not a silent skip;
  each tracked row must yield a `fileId` and a version. A swallowed parse gap, a `continue` on a
  missing field, or a page that silently yields nothing is a violation.
- **No PII in the repo.** A hard-coded customer id, email, cookie, token, or absolute machine path
  is a violation. Account identity and per-user state live in the config dir, never in code or
  committed files. The `internal/fixtures` guard test must stay green.

**Correctness**

- `classify` (`internal/syncer/syncer.go`) is pure and returns the `Class` that drives
  download-vs-skip. A wrong branch means a missed or redundant download. Verify each case
  (`Unchanged`/`New`/`Changed`/`DownloadNow`/`CacheMissing`/`Adopted`) against its condition. Tier 1.
- Off-by-one in enumeration paging (the walk to the zero-anchor terminator), inverted conditions,
  unreachable branches. Tier 1.
- Edge cases: empty library, single-page library, a pack with zero selected files after variant
  filtering, a file present in the lockfile but absent on disk. Tier 1 if it crashes or corrupts
  state; Tier 3 if it's theoretical and no caller hits it.

**Download & write integrity**

- Every write of bytes to disk must be atomic: stream to a temp file in the destination dir, hash
  sha256 while writing, then `os.Rename` into place, recording sha + actual byte count. This applies
  to `cache.Store`, the lockfile write, and the `selfupdate` binary replace. A write that truncates
  the target first, skips the hash/size record, or renames across filesystems is Tier 1.
- The cache filename derives from the signed-CloudFront URL path basename. Confirm it can't produce
  a path-traversal (`..`, absolute) escape from the library root, and that path building uses
  `filepath.Join`, not string concatenation. Tier 1/2.

**Failure model & errors**

- Retry with exponential backoff on 5xx / transient; **fail fast on 4xx**. Retrying a 4xx, or
  failing-fast on a 5xx, is a bug. Tier 1.
- Each download retry must resolve a **fresh** signed URL (signed URLs expire); reusing a stale URL
  across attempts defeats the retry. Tier 1.
- Errors that callers inspect must be wrapped with `%w` (notably `ErrExpiredSession`); use
  `errors.Is`/`errors.As`, not string matching. Tier 1/2.
- Swallowed errors (`_ =` on a write, close, or download that matters; an error logged and dropped
  where the run should abort). Tier 1 if it hides a failed download or a corrupt write.
- `context.Context` threaded through HTTP and downloads; cancellation honored, not ignored. Tier 2.

**Concurrency**

- `syncer.fetchAll` fans out bounded by `Concurrency` using `WaitGroup` + `Mutex`. Look for data
  races: unguarded appends to shared slices/maps, a shared result written without the mutex, a
  loop variable captured by a goroutine. Anything `go test -race` would trip is Tier 1.
- The bound must actually bound (correct worker count / semaphore), and a failure in one goroutine
  must not deadlock the rest or drop the error. Tier 1/2.

**Lockfile & manifest stability**

- The lockfile must marshal deterministically for minimal diffs (`MarshalIndent` relies on sorted
  JSON map keys; any slice written must be sorted by a stable key). A timestamp, random ordering,
  or map-iteration-into-slice-without-sort that churns the file is Tier 2 — Tier 1 if it corrupts
  identity or the downloaded-set.
- The manifest must round-trip without dropping packs or flipping `enabled` flags, and preserve the
  opt-in default on write. Tier 1/2.

**Paths & portability**

- All config/state/library paths resolve via XDG with the documented precedence
  (`--config` › `$SYNTY_CONFIG_DIR` › `$XDG_CONFIG_HOME` › `~/.config`), with no baked-in machine
  path or hard-coded `HOME`. Tier 1 (also a PII concern).

**Session & auth handling**

- Cookies are forwarded, not guessed; the session cookie value, `customer_id`, and any GitHub token
  must never be logged or embedded in an error message. A secret reaching stdout/stderr is Tier 1.
- `selfupdate`: token resolution order (`GITHUB_TOKEN` › `GH_TOKEN` › `gh` CLI), atomic replace of
  the running binary, and validation of the downloaded asset before swapping it in. Tier 1/2.

**Duplication and extraction**

- Repeated blocks (5+ lines, similar structure) across files that should be a shared helper. Tier 2.
- Ignore trivial similarities (both call `filepath.Join`, both have an `if err != nil`). Only flag
  duplication where extracting it meaningfully reduces the bug surface or eases change.

**Refactoring opportunities**

- Code that grew incrementally and would benefit from restructuring now that its shape is clear: a
  function doing three things that should be three; a type that accumulated responsibilities; data
  flow that got indirect when a simpler path exists. Tier 2.
- Every refactor suggestion must point to a concrete, defensible improvement — it **removes**
  something (an indirection, a duplicated pattern, a coordination point, a way for two things to get
  out of sync), **enables** something (makes X unit-testable, unblocks a use case, lets a change
  land in one place instead of N), or **generalizes meaningfully** (one shape replaces N
  near-duplicate variants). "Same behavior, different shape" does not qualify, even if more elegant.
  If you can't name what concretely improved, leave it out.

**Test quality (review tests as a whole, not individually)**

Tests use the standard library `testing` package with table-driven cases and httptest servers — no
testify. Step back and look at each area's suite as a unit:
- Significant production behavior with no test at all — especially the core invariants (dedup,
  opt-in, expired-session-aborts, strict-parsing-errors) and `classify` branches. Tier 2.
- Clusters of overlapping tests that could consolidate into fewer, clearer table cases. Tier 2.
- Tests asserting implementation details (internal call order, private field values) instead of
  observable behavior; they break on refactor for no value. Tier 2.
- Duplicated test-helper/setup code that could be shared. Tier 2.
- Weak assertions (checking a count but not the values, asserting the wrong field) — Tier 2 only if
  the weakness could mask a real bug.
- Do NOT suggest tests for trivial edge cases, every possible nil input, or splitting working tests
  for "purity." Fewer, stronger tests beat many fragile ones.

**Go idioms**

- Missing `defer` cleanup: an unclosed `resp.Body`, file handle, `zip.ReadCloser`, or `*sql.Rows` is
  a leak. Tier 1/2.
- `%w` vs `%v` where a caller uses `errors.Is`/`As`. Tier 2.
- Ignored errors on operations that matter. Tier 2.
- Unnecessary exported surface, needless interfaces, inconsistent pointer/value receivers, naked
  returns in long functions. Tier 3.
- `go vet` / `gofmt` cleanliness. Tier 3 — but a real `go vet` finding (e.g. a copied lock, a lost
  struct copy, a bad Printf verb) is Tier 1.

**Cross-package consistency (sub-agents flag; synthesis happens in Step 4)**

- When reviewing a package, note any exported API that looks easy to misuse (parameter ordering,
  unclear units, implicit preconditions) and any call into another package that assumes something
  about what it returns. These are inputs for the cross-cutting analysis in Step 4.

## Step 4: Cross-cutting invariant analysis

After sub-agents report, trace each core invariant end-to-end across package boundaries — something
no single sub-agent could do:

1. **fileId dedup:** read the dedup point in `syncer`, the cache's identity-keyed layout in `cache`,
   and the lockfile entries in `lockfile`. Confirm a file owned by multiple packs downloads once and
   every owning pack's entry shares one `cachePath`.
2. **Expired session → no lockfile write:** trace `portal.Enumerate` returning `ErrExpiredSession`
   through `syncer.Run` and `main.go`. Confirm no path writes the lockfile once enumeration failed,
   and the error keeps its `errors.Is` identity all the way up.
3. **Opt-in selection:** trace a newly-owned pack from `portal` enumeration through `manifest`
   (added disabled) and confirm `sync`/`status`/`select` never act on it until enabled.
4. **Atomic-write discipline:** compare every site that writes bytes to disk (`cache.Store`, lockfile
   save, `selfupdate`) against the temp-file + hash + rename contract. Any divergence is a finding.
5. **No PII / no machine paths:** scan config, session, manifest, and lockfile schemas plus any
   default paths for a hard-coded customer id, email, cookie, token, or absolute path. Confirm the
   `internal/fixtures` guard would catch a leak into `testdata/`.

## Step 5: Verify and expand findings

For every finding from sub-agents or cross-cutting analysis:

1. **Verify it yourself.** Read the file and line number. Confirm the issue is real. Drop anything
   speculative, cosmetic, or not actually a problem.
2. **Search for the same pattern across the codebase.** If a sub-agent found an issue in one file,
   grep/glob for the same pattern elsewhere. Report all occurrences, not just the one the sub-agent
   happened to look at.
3. **Drop non-actionable observations.** If a finding amounts to "noting this but it's fine," remove
   it. Every item in the final report must be worth fixing.
4. **Deduplicate.** If multiple sub-agents flagged the same underlying issue from different angles,
   merge into one finding.

## Step 6: Report

Organize findings by tier, then by category within each tier.

**Format for each finding:**
- File path and line number(s)
- What's wrong (concrete, not vague)
- Suggested fix (specific enough to act on)
- If a pattern repeats across files, group all occurrences under one finding

**For test quality findings:** present as a cohesive assessment of each test area, not a list of
individual test files. "The syncer tests cover classify and dedup well but nothing exercises the
expired-session abort path" is better than "syncer_test.go line 42: missing test for X."

**For refactor findings:** include a brief sketch of the target structure, not just "this should be
refactored." Show what the code would look like after, or at minimum name the functions/types that
would result.

No nits. No cosmetic notes. No "just flagging this." Only actionable items.
