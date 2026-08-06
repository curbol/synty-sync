# Browse: split the image bucket, add a data category, multi-select filters

Date: 2026-08-05

## Problem

Three friction points in the browse UI's filtering, all observed against the live
index (`~/.cache/synty-sync/browse-index.json`, 141k assets):

1. **`image` is one undifferentiated bucket of 20,918 items** — UI sprites and
   material texture maps mixed together, with no way to filter to just UI or just
   textures. Measured composition: ~13.5k UI (all four top packs are `INTERFACE_*`
   HUD/Menu packs), ~7.2k texture maps (96% under a `/textures/` tree), ~200 misc.

2. **`.asset` (10,847 Unity serialized files) is classified as `doc`**, so "doc"
   is 99% engine data and the ~76 real documents are drowned. The same species of
   serialized engine data also fills `other` (`res` 4,823, `playable` 405,
   `terrainlayer` 108, `mesh` 91, `preset`, `lighting`, `sk`), with no home of its own.

3. **The type/vendor/variant filters are single-select**, so "model AND animation"
   can't be viewed together. Facet dropdowns are also sorted by descending count,
   which makes a specific option hard to find.

## Changes

### 1. Category taxonomy — `internal/assetindex`

Add three categories: `ui`, `texture`, `data`.

`Classify(ext)` stays a pure, deterministic extension→category map (unchanged
contract, still unit-tested on its own). Two independent refinement steps run in
`newAsset`, each separately tested:

**Image refinement.** When `Classify` returns `CategoryImage`, `refineImage(relPath)`
decides `ui` / `texture` / `image` from the lowercased path (which ends in the
filename). Precedence UI → texture → image:

- **ui** — path has a UI token at a `[/_]` boundary: `ui`, `hud`, `gui`,
  `interface`, `menu(s)`, `icon(s)`, `sprite(s)`, `branding`, `widget`, `cursor`,
  `minimap`. This is folder/token based, not pack-name based; it covers 100% of the
  `INTERFACE_*` packs via their `Source_Sprites/`, `Icons_*/`, `Branding/` segments,
  so no coupling to Synty pack naming is introduced.
- **texture** — path has a texture folder segment (`/textures?/`, `/decals?/`,
  `/emissive/`, `/normals?/`) OR the filename carries a map suffix at a boundary:
  `albedo`, `basecolor`, `diffuse`, `normal(s)`, `metallic(smoothness)`,
  `roughness`, `specular`, `emissive`, `emission`, `occlusion`, `ao`, `height`,
  `orm`, `gloss`, `opacity`, `mask`, `texture`.
- **image** — the remainder (color palettes, loose `fx_*` sprites, ~200 items).

Measured buckets with these rules: **ui 13,492 · texture 7,214 · image 212**.
Thumbnails are unchanged: renderable `png`/`jpg` UI and texture files keep
`ThumbImage`, so you still see the icon/atlas.

**Data reclassification.** Move engine-serialized formats to `data`:

- out of the `doc` arm: `asset`, `meta`
- out of the `other` default (add an explicit `data` arm): `res`, `playable`,
  `terrainlayer`, `preset`, `lighting`, `mesh`, `sk`

`doc` returns to real documents (`pdf`, `txt`, `md`, `rtf`, `url`, `json`, `xml`,
`yaml`, `yml`, `csv`). `other` becomes true unknowns. The long-tail junk
(`unwrap_cache`, `depren`, `ma`) stays in `other`; reclassifying it is out of scope.
`.tres` stays in the material arm (Godot material resources).

**Cache invalidation.** Bump `indexVersion` 3 → 4 (`cache.go:15`). `LoadOrBuild`
already rebuilds when the cached version differs (`cache.go:158`), so the first
browse after this ships reclassifies the whole library automatically — no manual
`--reindex` needed.

The UI-token, texture-folder, and map-suffix token lists compile to boundary-anchored
package-level regexes. `refineImage` is pure and unit-tested with representative real
paths: an `INTERFACE_*` sprite (→ui), a `POLYGON_*` `/Textures/` atlas (→texture), a
`*_Normal` map (→texture), and a bare `fx_circle` (→image).

### 2. Multi-select filters — `internal/browse`

**Backend (`handleAssets`, `server.go:159`).** Each of type/vendor/variant reads all
repeated query values into a set and matches by membership:

- `type=model&type=animation` → keep assets whose category is in the set.
- Same for `vendor` and `variant`.
- **variant**: presence of the param (even with an empty value) filters; an empty
  value in the set is the loose/unknown bucket. This replaces the `hasVariant`
  special-case with the same set logic (`""` is just another member).
- No param for a facet = no filter on that facet.

**Frontend (`app.js`, `index.html`, `style.css`).** Replace the three native
`<select>`s with one reusable checkbox-dropdown component:

- Markup per filter: a `.ms` wrapper with a `.ms-btn` button and a hidden `.ms-pop`
  popover; the popover's checkboxes are built from the server facets (value + count).
- **none checked = all** (emit no param for that facet). Button summary: 0 →
  "all types", 1 → the value, N → "N selected".
- `query()` appends every checked value (`p.append('type', v)`); the variant popover
  includes a `(loose / unknown)` checkbox whose value is `""`.
- Change on any popover triggers `reset()` (same as today's `change` handlers).
- Popover closes on outside-click / Escape.
- `ICONS` map (`app.js:249`) gains `ui`, `texture`, `data` glyphs so cards and the
  lightbox render a category icon for the new kinds.

### 3. Facet sort — `sortedFacet` (`server.go:329`)

Sort facet values alphabetically (case-insensitive) instead of by descending count.
Counts still show in the checkbox labels; the "all …" affordance stays pinned at the
top (it's the button summary / client-side, not a facet value).

## Files touched

- `internal/assetindex/asset.go` — `CategoryUI`, `CategoryTexture`, `CategoryData`
  consts; `newAsset` calls `refineImage` when base category is image.
- `internal/assetindex/classify.go` — `data` arm; drop `asset`/`meta` from `doc`;
  `refineImage` + the three compiled token regexes.
- `internal/assetindex/classify_test.go` — `refineImage` cases; `data` cases.
- `internal/assetindex/cache.go` — `indexVersion` 3 → 4.
- `internal/browse/server.go` — multi-value set membership for type/vendor/variant;
  `sortedFacet` alphabetical.
- `internal/browse/server_test.go` — multi-value filter + alphabetical-sort tests.
- `internal/browse/assets/app.js` — checkbox-dropdown component; `query()` append;
  new `ICONS` entries.
- `internal/browse/assets/index.html` — replace the three `<select>`s with `.ms`
  component mounts.
- `internal/browse/assets/style.css` — dropdown button + popover styling.

## Out of scope

- Reclassifying the full `other` long tail beyond the named `data` formats.
- Multi-select for the `sort` control (it is inherently single-choice).
- Persisting filter selections across reloads.

## Testing

- `go test ./internal/assetindex/` — `refineImage` maps representative real paths to
  ui/texture/image; `Classify` maps `asset`/`meta`/`res`/`playable`/… to `data`.
- `go test ./internal/browse/` — `handleAssets` with repeated `type`/`vendor`/
  `variant` params returns the union; empty `variant=` selects the loose bucket;
  `sortedFacet` returns values in alphabetical order.
- Manual: `go run . browse`, confirm the type dropdown lists `ui`/`texture`/`data`
  with counts near 13.5k / 7.2k / 16k, checking `model` + `animation` shows both, and
  options are alphabetical.
