import * as THREE from 'three';
import { GLTFLoader } from 'three/addons/loaders/GLTFLoader.js';
import { FBXLoader } from 'three/addons/loaders/FBXLoader.js';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import {
  contentURL, loadModel, normalizeClip, boneNames, clipBones, clipsForAsset, loadRMClips,
  hasBakedMotion, coversBones, posedBox, frameBox, isRenderable, captureRootRest, uprightRig,
  prepareClipRig, cloneRig, poseAt, retargetedFor, stripRootMotion, dispose, CharRegistry, CLAY, _posedV,
} from '/static/scene.js';

const PAGE = 200;

// Above this file size a grid thumbnail shows the category icon instead of a 3D
// render: three.js parses a model synchronously on the main thread, so parsing a
// 65 MB FBX just for a 220px still froze the page. The full model still opens in the
// lightbox on demand.
const MAX_THUMB_BYTES = 40 * 1024 * 1024;

const els = {
  q: document.getElementById('q'),
  sort: document.getElementById('sort'),
  group: document.getElementById('group'),
  count: document.getElementById('count'),
  grid: document.getElementById('grid'),
  sentinel: document.getElementById('sentinel'),
  empty: document.getElementById('empty'),
};

const state = { offset: 0, total: 0, loading: false, done: false, facetsLoaded: false, items: [] };

// ---- data ----

function query(extra = {}) {
  const p = new URLSearchParams();
  if (els.q.value.trim()) p.set('q', els.q.value.trim());
  // No boxes checked = no param = no filter; each checked value is appended so the
  // backend unions them. An empty value is the variant "(loose / unknown)" bucket.
  for (const f of FILTERS) for (const v of filters[f.id].getSelected()) p.append(f.id, v);
  for (const t of tagFilter.selected) p.append('tag', t);
  if (tagFilter.selected.size) p.set('tagmode', tagFilter.mode);
  if (tagFilter.selected.size && tagFilter.related) p.set('includeRelated', '1');
  p.set('sort', els.sort.value);
  if (!els.group.checked) p.set('group', '0');
  for (const [k, v] of Object.entries(extra)) p.set(k, v);
  return p;
}

const thumbURL = (id) => '/api/thumb?id=' + encodeURIComponent(id);

// sentinelNear reports whether the load sentinel is within a screenful of the
// viewport bottom — i.e. more should load.
function sentinelNear() {
  const vh = window.innerHeight || document.documentElement.clientHeight;
  return els.sentinel.getBoundingClientRect().top <= vh + 600;
}

async function fetchPage() {
  if (state.loading || state.done) return;
  state.loading = true;
  const p = query({ offset: state.offset, limit: PAGE });
  const res = await fetch('/api/assets?' + p.toString());
  const data = await res.json();
  if (!state.facetsLoaded) { populateFacets(data.facets); state.facetsLoaded = true; }
  state.total = data.total;
  for (const a of data.items) { state.items.push(a); els.grid.appendChild(card(a)); }
  state.offset += data.items.length;
  if (data.items.length === 0 || state.offset >= data.total) state.done = true;
  els.count.textContent = state.total + (state.total === 1 ? ' asset' : ' assets');
  els.empty.hidden = state.total !== 0;
  state.loading = false;
  // A page may not push the sentinel off-screen (big monitor, short page); the
  // IntersectionObserver won't re-fire while it stays visible, so keep filling.
  if (!state.done && sentinelNear()) fetchPage();
}

function reset() {
  state.offset = 0; state.total = 0; state.done = false; state.loading = false;
  state.items = [];
  els.grid.replaceChildren();
  fetchPage();
}

// Each type/vendor/variant filter is a checkbox dropdown: none checked = no filter
// (all), any checked = the union of those values. The empty-string value is a real
// facet bucket ("(loose / unknown)" for variants), so it's just another checkbox.
const multiSelects = [];

class MultiSelect {
  constructor(root, allLabel, isVariant, onChange) {
    this.allLabel = allLabel;
    this.isVariant = isVariant;
    this.onChange = onChange;
    this.selected = new Set();
    this.root = root;
    this.btn = root.querySelector('.ms-btn');
    this.pop = root.querySelector('.ms-pop');
    this.label = document.createElement('span');
    this.label.className = 'ms-btn-label';
    const caret = document.createElement('span');
    caret.className = 'ms-caret';
    caret.textContent = '▾';
    this.btn.append(this.label, caret);
    this.btn.addEventListener('click', (e) => {
      e.stopPropagation();
      const open = this.pop.hidden;
      for (const ms of multiSelects) ms.setOpen(false);
      this.setOpen(open);
    });
    this.pop.addEventListener('click', (e) => e.stopPropagation());
    multiSelects.push(this);
    this.renderButton();
  }

  setOpen(open) { this.pop.hidden = !open; this.root.classList.toggle('open', open); }

  // display renders a facet value, giving the empty bucket a readable label.
  display(value) {
    if (value !== '') return value;
    return this.isVariant ? '(loose / unknown)' : '(none)';
  }

  setOptions(values) {
    this.pop.replaceChildren();
    for (const f of values) {
      const row = document.createElement('label');
      row.className = 'ms-opt';
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = this.selected.has(f.value);
      cb.addEventListener('change', () => {
        if (cb.checked) this.selected.add(f.value); else this.selected.delete(f.value);
        this.renderButton();
        this.onChange();
      });
      const text = document.createElement('span');
      text.className = 'ms-opt-label';
      text.textContent = this.display(f.value);
      const count = document.createElement('span');
      count.className = 'ms-opt-count';
      count.textContent = f.count;
      row.append(cb, text, count);
      this.pop.appendChild(row);
    }
    this.renderButton();
  }

  renderButton() {
    const n = this.selected.size;
    this.btn.classList.toggle('active', n > 0);
    if (n === 0) this.label.textContent = this.allLabel;
    else if (n === 1) this.label.textContent = this.display([...this.selected][0]);
    else this.label.textContent = n + ' selected';
  }

  getSelected() { return [...this.selected]; }
}

const FILTERS = [
  { id: 'type', all: 'all types', key: 'categories', isVariant: false },
  { id: 'vendor', all: 'all vendors', key: 'vendors', isVariant: false },
  { id: 'variant', all: 'all variants', key: 'variants', isVariant: true },
];
const filters = {};
for (const f of FILTERS) filters[f.id] = new MultiSelect(document.getElementById(f.id), f.all, f.isVariant, reset);
document.addEventListener('click', () => { for (const ms of multiSelects) ms.setOpen(false); });

function populateFacets(facets) {
  for (const f of FILTERS) filters[f.id].setOptions(facets[f.key]);
}

// ---- tags ----

const TAG_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20.6 13.4 12 22l-8-8V4h10l6.6 6.6a2 2 0 0 1 0 2.8z"/><circle cx="7.5" cy="7.5" r="1.2"/></svg>';
const MAX_SLIVERS = 6;

const tagState = { enabled: false, colors: new Map(), counts: new Map() };

function tagColor(id) { return tagState.colors.get(id) || '#9aa0aa'; }
function hex6(c) { return /^#[0-9a-fA-F]{6}$/.test(c) ? c : '#9aa0aa'; }

// applyPalette syncs the local palette from any tag API response, so a newly
// created or recolored tag is known before its slivers/chips render.
function applyPalette(p) {
  if (!p) return;
  tagState.enabled = !!p.enabled;
  tagState.colors = new Map((p.tags || []).map((t) => [t.id, t.color]));
  tagState.counts = new Map((p.tags || []).map((t) => [t.id, t.count]));
  document.body.classList.toggle('tags-on', tagState.enabled);
  tagFilter.root.hidden = !tagState.enabled;
  tagFilter.setOptions();
  restyleTags();
}

async function loadPalette() {
  try { applyPalette(await (await fetch('/api/tags')).json()); } catch { /* tagging stays off */ }
}

// apiAssign toggles a tag across a card's whole fingerprint set and returns the
// resulting union of tag ids for that set.
// apiAssign returns the set's resulting union tags, or null on failure so a caller
// leaves the card's displayed tags untouched rather than wiping them.
async function apiAssign(fingerprints, tag, on) {
  const res = await fetch('/api/assign', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ fingerprints, tag, on }),
  });
  if (!res.ok) return null;
  const data = await res.json();
  applyPalette(data.palette);
  return data.tags || [];
}

async function apiTag(method, body) {
  const res = await fetch('/api/tags', {
    method, headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  applyPalette(await res.json());
}

// restyleTags repaints every rendered sliver, chip, and filter dot after a recolor.
function restyleTags() {
  for (const s of document.querySelectorAll('.sliver[data-tag]')) s.style.background = tagColor(s.dataset.tag);
  for (const c of document.querySelectorAll('.tag-chip[data-tag]')) c.style.setProperty('--tc', tagColor(c.dataset.tag));
  for (const d of document.querySelectorAll('.tag-dot[data-tag]')) d.style.background = tagColor(d.dataset.tag);
}

// renderSlivers fills a card's tag strip: one colored segment per tag (no text) plus
// a +N overflow marker, hidden when the card has no tags.
function renderSlivers(bar, a) {
  bar.replaceChildren();
  const tags = a.tags || [];
  bar.hidden = tags.length === 0;
  const title = tags.join(', ');
  for (const t of tags.slice(0, MAX_SLIVERS)) {
    const s = document.createElement('span');
    s.className = 'sliver';
    s.dataset.tag = t;
    s.style.background = tagColor(t);
    s.title = title;
    bar.appendChild(s);
  }
  if (tags.length > MAX_SLIVERS) {
    const more = document.createElement('span');
    more.className = 'sliver-more';
    more.textContent = '+' + (tags.length - MAX_SLIVERS);
    more.title = title;
    bar.appendChild(more);
  }
}

// hasFingerprints reports whether a card can be tagged at all.
function hasFingerprints(a) { return Array.isArray(a.fingerprints) && a.fingerprints.length > 0; }

// ---- tag menu (assign / create-on-the-fly, from a card or the lightbox) ----

let tagMenu = null;
function closeTagMenu() { if (tagMenu) { tagMenu.remove(); tagMenu = null; } }

// openTagMenu shows a checkbox list of existing tags (toggling assigns/unassigns
// across the card's whole fingerprint set) plus a field to create-and-assign. It
// calls onChange after any change so the caller repaints its slivers/chips.
function openTagMenu(anchor, a, onChange) {
  closeTagMenu();
  const menu = document.createElement('div');
  menu.className = 'tag-menu';
  menu.addEventListener('click', (e) => e.stopPropagation());

  const input = document.createElement('input');
  input.className = 'tag-menu-new';
  input.type = 'text';
  input.placeholder = 'Search or add a tag…';

  const list = document.createElement('div');
  list.className = 'tag-menu-list';

  const rebuild = () => {
    list.replaceChildren();
    const have = new Set(a.tags || []);
    const q = input.value.trim().toLowerCase();
    const ids = [...tagState.colors.keys()]
      .filter((id) => !q || id.toLowerCase().includes(q))
      .sort((x, y) => x.localeCompare(y));
    if (ids.length === 0) {
      const hint = document.createElement('div');
      hint.className = 'tag-menu-empty';
      hint.textContent = q
        ? 'Enter to create "' + input.value.trim() + '"'
        : (tagState.colors.size ? 'No matches.' : 'No tags yet. Type to create one.');
      list.appendChild(hint);
    }
    for (const id of ids) {
      const row = document.createElement('label');
      row.className = 'tag-menu-opt';
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = have.has(id);
      cb.addEventListener('change', async () => {
        const t = await apiAssign(a.fingerprints, id, cb.checked);
        if (t === null) { cb.checked = !cb.checked; return; }
        a.tags = t;
        onChange();
      });
      const dot = document.createElement('span');
      dot.className = 'tag-dot';
      dot.style.background = tagColor(id);
      const lbl = document.createElement('span');
      lbl.className = 'tag-menu-label';
      lbl.textContent = id;
      row.append(cb, dot, lbl);
      list.appendChild(row);
    }
  };
  rebuild();

  input.addEventListener('input', rebuild);
  input.addEventListener('keydown', async (e) => {
    if (e.key !== 'Enter') return;
    const name = input.value.trim();
    if (!name) return;
    input.value = '';
    const t = await apiAssign(a.fingerprints, name, true);
    if (t !== null) a.tags = t;
    rebuild();
    onChange();
  });

  menu.append(input, list);
  document.body.appendChild(menu);
  const r = anchor.getBoundingClientRect();
  menu.style.top = Math.min(r.bottom + 4, window.innerHeight - menu.offsetHeight - 8) + 'px';
  menu.style.left = Math.min(r.left, window.innerWidth - menu.offsetWidth - 8) + 'px';
  tagMenu = menu;
  setTimeout(() => input.focus(), 0);
}

// ---- tag filter (header): select tags to filter by, with an AND/OR toggle and an
// inline manage mode to rename / recolor / delete tags library-wide. ----

const tagFilter = {
  root: document.getElementById('tagfilter'),
  selected: new Set(),
  mode: 'or',
  related: false,
  manage: false,
  init() {
    this.btn = this.root.querySelector('.ms-btn');
    this.pop = this.root.querySelector('.ms-pop');
    this.label = document.createElement('span');
    this.label.className = 'ms-btn-label';
    const caret = document.createElement('span');
    caret.className = 'ms-caret';
    caret.textContent = '▾';
    this.btn.append(this.label, caret);
    this.btn.addEventListener('click', (e) => {
      e.stopPropagation();
      const open = this.pop.hidden;
      for (const ms of multiSelects) ms.setOpen(false);
      this.setOpen(open);
    });
    this.pop.addEventListener('click', (e) => e.stopPropagation());
    this.renderButton();
  },
  setOpen(open) {
    this.pop.hidden = !open;
    this.root.classList.toggle('open', open);
    if (open) this.render();
  },
  renderButton() {
    const n = this.selected.size;
    this.btn.classList.toggle('active', n > 0);
    this.label.textContent = n === 0 ? 'tags' : (n === 1 ? [...this.selected][0] : n + ' tags');
  },
  // setOptions runs after any palette change: drop selections for deleted tags and
  // repaint the open popover.
  setOptions() {
    for (const id of [...this.selected]) if (!tagState.colors.has(id)) this.selected.delete(id);
    if (!this.pop.hidden) this.render();
    this.renderButton();
  },
  render() {
    this.pop.replaceChildren();
    const head = document.createElement('div');
    head.className = 'tag-pop-head';
    const modeBtn = document.createElement('button');
    modeBtn.type = 'button';
    modeBtn.className = 'tag-mode';
    const setModeLabel = () => {
      modeBtn.textContent = this.mode === 'and' ? 'ALL' : 'ANY';
      modeBtn.title = 'match ' + (this.mode === 'and' ? 'all selected tags (AND)' : 'any selected tag (OR)');
    };
    setModeLabel();
    modeBtn.addEventListener('click', () => {
      this.mode = this.mode === 'and' ? 'or' : 'and';
      setModeLabel();
      if (this.selected.size) reset();
    });
    const manageBtn = document.createElement('button');
    manageBtn.type = 'button';
    manageBtn.className = 'tag-manage';
    manageBtn.textContent = this.manage ? 'done' : 'manage';
    manageBtn.addEventListener('click', () => { this.manage = !this.manage; this.render(); });
    head.append(modeBtn, manageBtn);
    if (!this.manage) {
      const linked = document.createElement('label');
      linked.className = 'tag-linked';
      linked.title = 'also show assets linked to a match (companions, like a frame’s background)';
      const lcb = document.createElement('input');
      lcb.type = 'checkbox';
      lcb.checked = this.related;
      lcb.addEventListener('change', () => { this.related = lcb.checked; if (this.selected.size) reset(); });
      const lt = document.createElement('span');
      lt.textContent = 'linked';
      linked.append(lcb, lt);
      head.appendChild(linked);
    }
    this.pop.appendChild(head);

    const ids = [...tagState.colors.keys()].sort((x, y) => x.localeCompare(y));
    if (ids.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'tag-pop-empty';
      empty.textContent = 'No tags yet.';
      this.pop.appendChild(empty);
      return;
    }
    for (const id of ids) this.pop.appendChild(this.manage ? this.manageRow(id) : this.filterRow(id));
  },
  filterRow(id) {
    const row = document.createElement('label');
    row.className = 'ms-opt';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = this.selected.has(id);
    cb.addEventListener('change', () => {
      if (cb.checked) this.selected.add(id); else this.selected.delete(id);
      this.renderButton();
      reset();
    });
    const dot = document.createElement('span');
    dot.className = 'tag-dot';
    dot.dataset.tag = id;
    dot.style.background = tagColor(id);
    const text = document.createElement('span');
    text.className = 'ms-opt-label';
    text.textContent = id;
    const count = document.createElement('span');
    count.className = 'ms-opt-count';
    count.textContent = tagState.counts.get(id) || 0;
    row.append(cb, dot, text, count);
    return row;
  },
  manageRow(id) {
    const row = document.createElement('div');
    row.className = 'tag-manage-row';
    const color = document.createElement('input');
    color.type = 'color';
    color.className = 'tag-color';
    color.value = hex6(tagColor(id));
    color.addEventListener('change', () => apiTag('PATCH', { id, color: color.value }));
    const name = document.createElement('input');
    name.type = 'text';
    name.className = 'tag-name';
    name.value = id;
    const commit = async () => {
      const v = name.value.trim();
      if (v && v !== id) { await apiTag('PATCH', { id, newId: v }); reset(); }
    };
    name.addEventListener('keydown', (e) => { if (e.key === 'Enter') { name.blur(); } });
    name.addEventListener('blur', commit);
    const del = document.createElement('button');
    del.type = 'button';
    del.className = 'tag-del';
    del.textContent = '🗑';
    del.title = 'delete tag';
    del.addEventListener('click', async () => {
      if (confirm('Delete tag "' + id + '"? It will be removed from all assets.')) { await apiTag('DELETE', { id }); reset(); }
    });
    row.append(color, name, del);
    return row;
  },
};
tagFilter.init();

document.addEventListener('click', () => { closeTagMenu(); tagFilter.setOpen(false); });

// ---- cards ----

const COPY_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>';

// splitName splits a name into a head (fills line 1, ellipsized) and a tail (the
// distinguishing suffix + extension on line 2), cutting at a separator near the end
// so the tail is a clean chunk. Short names go entirely on line 1.
function splitName(name) {
  if (name.length <= 20) return [name, ''];
  let cut = name.length - 14;
  for (let i = cut; i >= cut - 9 && i > 4; i--) {
    if ('-_@.'.includes(name[i - 1])) { cut = i; break; }
  }
  return [name.slice(0, cut), name.slice(cut)];
}

function card(a) {
  const el = document.createElement('div');
  el.className = 'card';
  el.tabIndex = 0;

  const thumb = document.createElement('div');
  thumb.className = 'thumb';
  thumb.appendChild(thumbContent(a));

  const ext = document.createElement('span');
  ext.className = 'ext-badge';
  ext.textContent = a.ext || a.category;
  const vendor = document.createElement('span');
  vendor.className = 'vendor-badge';
  vendor.textContent = a.vendor;
  thumb.append(ext, vendor);

  if (a.count > 1) {
    const cb = document.createElement('span');
    cb.className = 'count-badge';
    cb.textContent = '×' + a.count;
    cb.title = a.count + ' copies (variants / packs)';
    thumb.appendChild(cb);
  }

  // Copy icon (hover), top-right; ✓ feedback without replacing the icon glyph.
  const copy = document.createElement('button');
  copy.className = 'copy-icon';
  copy.innerHTML = COPY_SVG;
  copy.title = 'copy path';
  copy.addEventListener('click', (e) => {
    e.stopPropagation();
    navigator.clipboard.writeText(a.copyPath).then(() => {
      copy.classList.add('done');
      setTimeout(() => copy.classList.remove('done'), 1000);
    });
  });
  thumb.appendChild(copy);

  // Tag affordances: a colored sliver strip (one per tag) along the bottom edge so
  // it never covers the preview, and a hover add button. Both are CSS-gated on
  // body.tags-on; the add button also hides when the asset has no fingerprint.
  const bar = document.createElement('div');
  bar.className = 'sliver-bar';
  renderSlivers(bar, a);
  thumb.appendChild(bar);
  a._rerender = () => renderSlivers(bar, a);

  const tagBtn = document.createElement('button');
  tagBtn.type = 'button';
  tagBtn.className = 'tag-add';
  tagBtn.innerHTML = TAG_SVG;
  tagBtn.title = 'tags';
  if (!hasFingerprints(a)) tagBtn.classList.add('nofp');
  tagBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    openTagMenu(tagBtn, a, () => renderSlivers(bar, a));
  });
  thumb.appendChild(tagBtn);

  const name = document.createElement('div');
  name.className = 'name';
  name.title = a.name; // full name on hover
  const [head, tail] = splitName(a.name);
  const l1 = document.createElement('span'); l1.className = 'l1'; l1.textContent = head;
  const l2 = document.createElement('span'); l2.className = 'l2'; l2.textContent = tail;
  name.append(l1, l2);

  el.append(thumb, name);
  el.addEventListener('click', () => openLightbox(a));
  el.addEventListener('keydown', (e) => { if (e.key === 'Enter') openLightbox(a); });
  return el;
}

function thumbContent(a) {
  if (a.thumb === 'image') {
    const img = new Image();
    img.loading = 'lazy';
    img.src = contentURL(a.id);
    img.onerror = () => img.replaceWith(iconEl(a.category));
    return img;
  }
  if (a.thumb === 'preview') {
    const img = new Image();
    img.loading = 'lazy';
    img.src = thumbURL(a.id);
    img.onerror = () => img.replaceWith(iconEl(a.category));
    return img;
  }
  if (a.thumb === 'glb' || a.thumb === 'fbx') {
    if (a.size > MAX_THUMB_BYTES) return iconEl(a.category); // too big to parse for a grid still
    const holder = document.createElement('div');
    holder.className = 'thumb-3d';
    holder.appendChild(iconEl(a.category));
    modelThumbs.observe(holder, a);
    return holder;
  }
  if (a.thumb === 'font') {
    const el = document.createElement('div');
    el.className = 'font-thumb';
    el.textContent = 'Ag';
    ensureFont(a).then((fam) => { el.style.fontFamily = `"${fam}", serif`; })
      .catch(() => el.replaceWith(iconEl(a.category)));
    return el;
  }
  return iconEl(a.category);
}

// ---- fonts ----

const loadedFonts = new Set();

// ensureFont registers a font's bytes as a FontFace under a per-asset family and
// resolves that family name, so sample text can be rendered in the real typeface.
function ensureFont(a) {
  const fam = 'f' + a.id;
  if (loadedFonts.has(a.id)) return Promise.resolve(fam);
  return new FontFace(fam, `url(${contentURL(a.id)})`).load()
    .then((face) => { document.fonts.add(face); loadedFonts.add(a.id); return fam; });
}

// fontSample renders a specimen (name, pangram, glyph set, size ramp) in the font.
function fontSample(a) {
  const wrap = document.createElement('div');
  wrap.className = 'font-sample';
  const name = document.createElement('div'); name.className = 'fs-name'; name.textContent = a.name.replace(/\.[^.]+$/, '');
  const pangram = document.createElement('div'); pangram.className = 'fs-pangram'; pangram.textContent = 'The quick brown fox jumps over the lazy dog';
  const glyphs = document.createElement('div'); glyphs.className = 'fs-glyphs';
  glyphs.textContent = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ\nabcdefghijklmnopqrstuvwxyz\n0123456789 !?&@#$%()';
  const ramp = document.createElement('div'); ramp.className = 'fs-ramp';
  for (const px of [30, 22, 16]) {
    const line = document.createElement('div'); line.style.fontSize = px + 'px';
    line.textContent = 'The quick brown fox jumps over the lazy dog';
    ramp.appendChild(line);
  }
  wrap.append(name, pangram, glyphs, ramp);
  ensureFont(a).then((fam) => { wrap.style.fontFamily = `"${fam}"`; })
    .catch(() => { const p = document.createElement('p'); p.className = 'fs-fail'; p.textContent = 'Could not load this font.'; wrap.prepend(p); });
  return wrap;
}

function copyPath(text, btn) {
  navigator.clipboard.writeText(text).then(() => {
    const label = btn.textContent;
    btn.textContent = 'copied ✓';
    btn.classList.add('done');
    setTimeout(() => { btn.textContent = label; btn.classList.remove('done'); }, 1200);
  });
}

// ---- category icons ----

const ICONS = {
  model: '<path d="M12 2 3 7v10l9 5 9-5V7z"/><path d="M3 7l9 5 9-5M12 12v10"/>',
  image: '<rect x="3" y="4" width="18" height="16" rx="2"/><circle cx="8.5" cy="9.5" r="1.8"/><path d="M4 18l5-5 4 3 3-2 4 4"/>',
  ui: '<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18M9 9v11"/>',
  texture: '<rect x="3" y="3" width="18" height="18" rx="2"/><path d="M3 9h18M3 15h18M9 3v18M15 3v18"/>',
  material: '<circle cx="12" cy="12" r="9"/><path d="M4 12a8 8 0 0 1 16 0"/>',
  data: '<ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
  scene: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 9h18"/>',
  animation: '<circle cx="12" cy="12" r="9"/><path d="M10 8l6 4-6 4z"/>',
  audio: '<path d="M4 9v6h4l5 4V5L8 9z"/><path d="M16 8a5 5 0 0 1 0 8"/>',
  script: '<path d="M8 4h9l3 3v13H8z"/><path d="M4 8v12h11"/>',
  doc: '<path d="M6 2h8l4 4v16H6z"/><path d="M14 2v4h4"/>',
  font: '<path d="M5 20 12 4l7 16"/><path d="M8.3 14h7.4"/>',
  other: '<circle cx="12" cy="12" r="9"/><path d="M12 8v4l3 2"/>',
};

function iconEl(category) {
  const wrap = document.createElement('div');
  wrap.className = 'icon-wrap';
  wrap.style.display = 'flex';
  const svg = `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round">${ICONS[category] || ICONS.other}</svg>`;
  wrap.innerHTML = svg;
  return wrap.firstChild;
}

// ---- shared three.js helpers ----

// Synty source FBX reference a shared texture atlas that isn't adjacent to the
// file, so their materials load near-black and the loader chases missing textures.
// A LoadingManager stubs out every sub-resource (only the model URL is served),
// and FBX meshes get a neutral clay material so the geometry is always visible.
// GLB/glTF carry embedded textures, so they keep their real materials.

// ---- lazy 3D thumbnails: one shared renderer, sequential queue, cached ----

class ModelThumbnails {
  constructor(size = 220) {
    this.size = size;
    this.cache = new Map();
    this.rigs = new Map();  // matched character id -> loaded rig, reused to pose clips
    this.files = new Map(); // file path -> parsed+oriented file, reused across its split clips
    this.queue = Promise.resolve();
    this.observer = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (e.isIntersecting) {
          this.observer.unobserve(e.target);
          e.target.classList.add('loading'); // queued or rendering; cleared when the image swaps in
          this.enqueue(e.target, e.target._asset);
        }
      }
    }, { rootMargin: '200px' });
  }
  observe(holder, asset) { holder._asset = asset; this.observer.observe(holder); }
  // pause/resume gate the queue so the lightbox (its own heavy model load) doesn't fight
  // the background thumbnail parsing for the main thread while it's open.
  pause() {
    if (this.gate) return;
    let resume;
    this.gate = new Promise((r) => { resume = r; });
    this._resume = resume;
  }
  resume() {
    if (!this.gate) return;
    this._resume();
    this.gate = null;
    this._resume = null;
  }
  enqueue(holder, asset) {
    // Yield a frame between thumbnails so posing/rendering never starves scroll and
    // clicks — the whole page felt frozen until the queue drained otherwise.
    this.queue = this.queue
      .then(() => this.render(holder, asset))
      .then(() => new Promise((r) => requestAnimationFrame(() => r())))
      .catch(() => {});
  }
  ensureRenderer() {
    if (this.renderer) return;
    this.renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true, preserveDrawingBuffer: true });
    this.renderer.setSize(this.size, this.size);
    this.scene = new THREE.Scene();
    this.camera = new THREE.PerspectiveCamera(45, 1, 0.1, 1000);
    this.scene.add(new THREE.HemisphereLight(0xffffff, 0x33343a, 2.6));
    const dir = new THREE.DirectionalLight(0xffffff, 2.2);
    dir.position.set(4, 6, 5);
    this.scene.add(dir);
  }
  async render(holder, asset) {
    if (this.gate) await this.gate; // hold while the lightbox is open
    let url = this.cache.get(asset.id);
    if (url === undefined) {
      url = await this.build(asset);
      this.cache.set(asset.id, url);
    }
    if (url && holder.isConnected) {
      const img = new Image();
      img.src = url;
      holder.replaceWith(img);
    } else if (holder.isConnected) {
      holder.classList.remove('loading'); // no render (failed/mesh-less-no-rig): drop the spinner
    }
  }
  async build(asset) {
    try {
      this.ensureRenderer();
      // A split multi-clip file (source.clip) is loaded and oriented once, then every
      // clip card poses that shared object — otherwise each of the ~120 clips re-fetched
      // and re-parsed the whole 20–65 MB file, which pinned the main thread.
      const key = asset.source && asset.source.clip && asset.source.filePath;
      return key ? await this.buildShared(asset, key) : await this.buildStandalone(asset);
    } catch (e) {
      return null;
    }
  }

  async buildStandalone(asset) {
    const obj = await loadModel(contentURL(asset.id), asset.ext);
    const rootRest = captureRootRest(obj);
    if (isRenderable(obj)) {
      const cs = clipsForAsset(obj, asset);
      const refBox = prepareClipRig(obj, null); // self-contained plays on its own body; fixes Z-up files (e.g. some explosive dashes), no-op when upright
      if (cs.length) {
        let rootBoneName = null;
        obj.traverse((n) => { if (n.isBone && !rootBoneName && (!n.parent || !n.parent.isBone)) rootBoneName = n.name; });
        poseAt(obj, stripRootMotion(cs[0], rootBoneName, obj.userData.upAxis)); // in place, or a dash/locomotion clip drifts out of the still's frame
      }
      const dataURL = this.snap(obj, refBox);
      dispose(obj);
      return dataURL;
    }
    // Mesh-less animation clip: pose a matching rig at a representative frame so
    // each clip gets a distinguishable still instead of the same bare icon.
    const clips = clipsForAsset(obj, asset);
    dispose(obj);
    return clips.length ? await this.buildPosed(clips[0], asset.vendor, rootRest) : null;
  }

  // buildShared renders one clip of a multi-clip file whose parsed+oriented object is
  // loaded once and reused across all its clips (thumbnails render serially, so the
  // shared object is safe to re-pose per clip). Orientation and the framing box are
  // measured once — they belong to the file's rig, not the clip.
  async buildShared(asset, key) {
    let pending = this.files.get(key);
    if (!pending) {
      pending = this.loadSharedFile(asset);
      this.files.set(key, pending);
      this.evictFiles();
    }
    const ctx = await pending;
    if (!ctx) return null;
    if (ctx.renderable) {
      const cs = clipsForAsset(ctx.obj, asset);
      const mixer = cs.length ? poseAt(ctx.obj, stripRootMotion(cs[0], ctx.rootBoneName, ctx.upAxis)) : null;
      const dataURL = this.snap(ctx.obj, ctx.refBox);
      if (mixer) mixer.stopAllAction();
      return dataURL;
    }
    const cs = clipsForAsset(ctx.obj, asset);
    return cs.length ? await this.buildPosed(cs[0], asset.vendor, ctx.rootRest) : null;
  }

  async loadSharedFile(asset) {
    const obj = await loadModel(contentURL(asset.id), asset.ext);
    if (isRenderable(obj)) {
      const refBox = prepareClipRig(obj, null);
      let rootBoneName = null;
      obj.traverse((n) => { if (n.isBone && !rootBoneName && (!n.parent || !n.parent.isBone)) rootBoneName = n.name; });
      return { renderable: true, obj, refBox, upAxis: obj.userData.upAxis, rootBoneName };
    }
    return { renderable: false, obj, rootRest: captureRootRest(obj) };
  }

  // evictFiles bounds the shared-file cache so scrolling a large library doesn't hold
  // every parsed model in memory. Clips of a file render together, so the oldest file is
  // done by the time it's evicted.
  evictFiles() {
    const CAP = 6;
    while (this.files.size > CAP) {
      const oldest = this.files.keys().next().value;
      const pending = this.files.get(oldest);
      this.files.delete(oldest);
      Promise.resolve(pending).then((ctx) => { if (ctx && ctx.obj) dispose(ctx.obj); }).catch(() => {});
    }
  }

  // snap adds an object to the shared scene, frames it to the given box (the character's
  // constant reference box, so every thumbnail of a character is the same scale) and
  // renders it, then removes it (without disposing — the caller owns the object) and
  // returns a PNG data URL.
  snap(object, box) {
    this.scene.add(object);
    frameBox(box, this.camera, null);
    this.renderer.render(this.scene, this.camera);
    const dataURL = this.renderer.domElement.toDataURL('image/png');
    this.scene.remove(object);
    return dataURL;
  }

  // rigFor returns (and caches) a loaded character whose skeleton can play a clip,
  // using the same registry the lightbox uses; null when no rig matches.
  async rigFor(clip, vendor) {
    const bones = clipBones(clip);
    await CharRegistry.seed();
    let m = CharRegistry.match(bones, vendor);
    if (!m) { await CharRegistry.discoverForVendor(vendor, bones); m = CharRegistry.match(bones, vendor); }
    if (!m) return null;
    if (!this.rigs.has(m.id)) {
      const rig = await loadModel(contentURL(m.id), m.ext)
        .then((r) => (isRenderable(r) ? r : (dispose(r), null)))
        .catch(() => null);
      this.rigs.set(m.id, rig); // pristine template; buildPosed clones it per clip

    }
    return this.rigs.get(m.id);
  }

  async buildPosed(clip, vendor, rootRest) {
    const template = await this.rigFor(clip, vendor);
    if (!template) return null;
    // Pose each clip on a fresh clone of the loaded body — the same pristine-skeleton pipeline
    // the lightbox uses, so the two never diverge. (Reusing one posed skeleton re-measures
    // orientation unreliably and flips every other thumbnail.)
    const rig = cloneRig(template);
    let rootBoneName = null;
    rig.traverse((n) => { if (n.isBone && !rootBoneName && (!n.parent || !n.parent.isBone)) rootBoneName = n.name; });
    const refBox = prepareClipRig(rig, vendor === 'synty' ? null : rootRest); // synty retargeted (own bind); a cross-file body needs the clip's root axis
    const posed = stripRootMotion(await retargetedFor(clip, vendor, rig), rootBoneName, rig.userData.upAxis); // in place, so the still stays framed
    const mixer = poseAt(rig, posed);
    const dataURL = this.snap(rig, refBox);
    mixer.stopAllAction();
    dispose(rig);
    return dataURL;
  }
}
const modelThumbs = new ModelThumbnails();

// ---- lightbox ----

const lb = {
  root: document.getElementById('lightbox'),
  view: document.getElementById('lb-view'),
  name: document.getElementById('lb-name'),
  fields: document.getElementById('lb-fields'),
  tags: document.getElementById('lb-tags'),
  related: document.getElementById('lb-related'),
  character: document.getElementById('lb-character'),
  copies: document.getElementById('lb-copies'),
  prev: document.getElementById('lb-prev'),
  next: document.getElementById('lb-next'),
  index: -1,
};
let activeViewer = null;

// updateLbNav enables/disables the prev/next arrows for the current position in the
// loaded result set. Next stays enabled at the tail while more pages can still load.
function updateLbNav() {
  lb.prev.disabled = lb.index <= 0;
  lb.next.disabled = lb.index < 0 || (lb.index >= state.items.length - 1 && state.done);
}

// navLightbox steps to an adjacent asset in the filtered result set, loading the next
// page first when stepping past the last loaded item.
async function navLightbox(delta) {
  if (lb.root.hidden || lb.index < 0) return;
  let i = lb.index + delta;
  if (i < 0) return;
  if (i >= state.items.length) {
    if (state.done) return;
    await fetchPage();
    if (i >= state.items.length) return;
  }
  openLightbox(state.items[i]);
}

function openLightbox(a) {
  if (activeViewer) { activeViewer.stop(); activeViewer = null; } // tear down when navigating
  modelThumbs.pause(); // give the viewer's model load the main thread, not the thumbnail queue
  lb.index = state.items.indexOf(a);
  updateLbNav();
  lb.name.textContent = a.name;
  // The metadata carries the shared file properties; every location (one or many)
  // lives in the copies list below, so there's a single copy system either way.
  const bitmap = /^(png|jpe?g|gif|webp|bmp)$/i.test(a.ext || '');
  const hasDims = a.width > 0 && a.height > 0;
  const fields = [['Category', a.category], ['Format', a.ext || '—'], ['Size', humanSize(a.size)]];
  if (hasDims) fields.push(['Dimensions', `${a.width} × ${a.height}`]);
  else if (bitmap) fields.push(['Dimensions', '…']);
  lb.fields.innerHTML = fields.map(([k, v]) => `<dt>${k}</dt><dd data-field="${k}">${escapeHTML(v)}</dd>`).join('');
  // The index carries dimensions for images it could measure; probe the bytes only
  // as a fallback (a copy dropped from the index, or a format the scanner skipped).
  if (!hasDims && bitmap) {
    const probe = new Image();
    const dd = () => lb.fields.querySelector('dd[data-field="Dimensions"]');
    probe.onload = () => { const el = dd(); if (el) el.textContent = `${probe.naturalWidth} × ${probe.naturalHeight}`; };
    probe.onerror = () => { const el = dd(); if (el) el.textContent = '—'; };
    probe.src = contentURL(a.id);
  }
  lb.character.replaceChildren(); // the viewer fills this for clip-only animations
  renderLbTags(a);
  renderLbRelated(a);
  renderCopies(a);

  lb.view.replaceChildren();
  if (a.thumb === 'glb' || a.thumb === 'fbx') {
    activeViewer = startViewer(lb.view, a);
  } else if (bitmap) {
    // The expanded view shows the full-resolution image, not the small Unity
    // preview.png a unitypackage entry may also carry; fall back to that preview,
    // then the category icon.
    const img = new Image();
    img.onerror = () => {
      if (a.thumb === 'preview') { img.onerror = () => img.replaceWith(iconEl(a.category)); img.src = thumbURL(a.id); }
      else img.replaceWith(iconEl(a.category));
    };
    img.src = contentURL(a.id);
    lb.view.appendChild(img);
  } else if (a.thumb === 'preview') {
    const img = new Image(); img.src = thumbURL(a.id); lb.view.appendChild(img);
  } else if (a.thumb === 'font') {
    lb.view.appendChild(fontSample(a));
  } else {
    lb.view.appendChild(iconEl(a.category));
  }
  lb.root.hidden = false;
}

// renderLbTags shows the card's tags as colored chips (each recolorable and
// removable) with an add control, all targeting the card's whole fingerprint set.
// Hidden entirely when tagging is disabled.
function renderLbTags(a) {
  lb.tags.replaceChildren();
  lb.tags.hidden = !tagState.enabled;
  if (!tagState.enabled) return;

  const head = document.createElement('div');
  head.className = 'lb-tags-head';
  const heading = document.createElement('span');
  heading.textContent = 'Tags';
  const add = document.createElement('button');
  add.type = 'button';
  add.className = 'lb-tag-add';
  add.textContent = '+ add';
  if (hasFingerprints(a)) {
    add.addEventListener('click', (e) => {
      e.stopPropagation();
      openTagMenu(add, a, () => { renderLbTags(a); a._rerender && a._rerender(); });
    });
  } else {
    add.disabled = true;
    add.title = 'this asset has no content fingerprint, so it cannot be tagged';
  }
  head.append(heading, add);

  const chips = document.createElement('div');
  chips.className = 'tag-chips';
  for (const id of (a.tags || [])) chips.appendChild(lbTagChip(a, id));

  lb.tags.append(head, chips);
}

function lbTagChip(a, id) {
  const chip = document.createElement('span');
  chip.className = 'tag-chip';
  chip.dataset.tag = id;
  chip.style.setProperty('--tc', tagColor(id));

  const color = document.createElement('input');
  color.type = 'color';
  color.className = 'tag-chip-color';
  color.value = hex6(tagColor(id));
  color.title = 'change color';
  color.addEventListener('click', (e) => e.stopPropagation());
  color.addEventListener('change', () => apiTag('PATCH', { id, color: color.value }));

  const label = document.createElement('span');
  label.className = 'tag-chip-label';
  label.textContent = id;

  const x = document.createElement('button');
  x.type = 'button';
  x.className = 'tag-chip-x';
  x.textContent = '×';
  x.title = 'remove tag';
  x.addEventListener('click', async (e) => {
    e.stopPropagation();
    const t = await apiAssign(a.fingerprints, id, false);
    if (t === null) return;
    a.tags = t;
    renderLbTags(a);
    a._rerender && a._rerender();
  });

  chip.append(color, label, x);
  return chip;
}

// renderCopies lists where the file lives — one row for a unique file, or every
// occurrence for a file shipped across variants/packs — each with its own copy
// button, plus a copy-all when there's more than one. One system, one or many.
function renderCopies(a) {
  lb.copies.replaceChildren();
  const copies = (a.copies && a.copies.length)
    ? a.copies
    : [{ variant: a.variant, vendor: a.vendor, pack: a.pack, copyPath: a.copyPath }];
  const many = copies.length > 1;

  const head = document.createElement('div');
  head.className = 'lb-copies-head';
  const heading = document.createElement('span');
  heading.textContent = many ? copies.length + ' copies' : 'Location';
  head.appendChild(heading);
  if (many) {
    const all = document.createElement('button');
    all.className = 'lb-copyall';
    all.textContent = 'copy all';
    all.addEventListener('click', () => copyPath(copies.map((c) => c.copyPath).join('\n'), all));
    head.appendChild(all);
  }
  lb.copies.appendChild(head);

  for (const c of copies) {
    const row = document.createElement('div');
    row.className = 'lb-copyrow';
    const label = document.createElement('span');
    label.className = 'lb-copyrow-label';
    label.textContent = [c.variant || 'loose', c.pack || c.vendor].filter(Boolean).join(' · ');
    const code = document.createElement('code');
    code.textContent = c.copyPath;
    const btn = document.createElement('button');
    btn.className = 'lb-copyicon';
    btn.title = 'copy path';
    btn.innerHTML = COPY_SVG;
    btn.addEventListener('click', () => {
      navigator.clipboard.writeText(c.copyPath).then(() => {
        btn.classList.add('done');
        setTimeout(() => btn.classList.remove('done'), 1000);
      });
    });
    row.append(label, code, btn);
    lb.copies.appendChild(row);
  }
}

// renderLbRelated shows the card's linked companions ("parts of this set") as small
// clickable thumbnails that open that asset. It fetches the related cards on demand
// (they can live anywhere in the library) and stays hidden when there are none or
// tagging is off. It clears synchronously first, so a fetch in flight never flashes a
// previous card's companions.
async function renderLbRelated(a) {
  const box = lb.related;
  box.replaceChildren();
  box.hidden = true;
  if (!tagState.enabled || !hasFingerprints(a)) return;
  let items;
  try {
    const qs = a.fingerprints.map((fp) => 'fingerprint=' + encodeURIComponent(fp)).join('&');
    items = ((await (await fetch('/api/related?' + qs)).json()).items) || [];
  } catch { return; }
  if (!items.length || lb.root.hidden) return;

  const head = document.createElement('div');
  head.className = 'lb-related-head';
  const heading = document.createElement('span');
  heading.textContent = items.length > 1 ? `Parts of this set (${items.length})` : 'Part of this set';
  head.appendChild(heading);

  const strip = document.createElement('div');
  strip.className = 'lb-related-strip';
  for (const it of items) strip.appendChild(relatedThumb(it));

  box.append(head, strip);
  box.hidden = false;
}

function relatedThumb(it) {
  const el = document.createElement('button');
  el.type = 'button';
  el.className = 'lb-related-item';
  el.title = it.name;
  const th = document.createElement('div');
  th.className = 'lb-related-thumb';
  th.appendChild(thumbContent(it));
  const name = document.createElement('span');
  name.className = 'lb-related-name';
  name.textContent = it.name;
  el.append(th, name);
  el.addEventListener('click', () => openLightbox(it));
  return el;
}

function closeLightbox() {
  lb.root.hidden = true;
  modelThumbs.resume(); // let background thumbnails continue
  if (activeViewer) { activeViewer.stop(); activeViewer = null; }
  lb.view.replaceChildren();
  lb.character.replaceChildren();
  lb.related.replaceChildren();
  lb.related.hidden = true;
}

function startViewer(container, asset) {
  const w = container.clientWidth || 600, h = container.clientHeight || 500;
  const renderer = new THREE.WebGLRenderer({ antialias: true });
  renderer.setSize(w, h);
  renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
  renderer.setClearColor(0x14161d, 1);
  renderer.shadowMap.enabled = true;
  renderer.shadowMap.type = THREE.PCFSoftShadowMap;
  container.appendChild(renderer.domElement);
  const scene = new THREE.Scene();
  scene.add(new THREE.HemisphereLight(0xffffff, 0x2a2c33, 3.0));
  const dir = new THREE.DirectionalLight(0xffffff, 2.4); dir.position.set(4, 6, 5); dir.castShadow = true; scene.add(dir); scene.add(dir.target);
  dir.shadow.mapSize.set(2048, 2048); dir.shadow.bias = -0.0005;
  const fill = new THREE.DirectionalLight(0xffffff, 1.0); fill.position.set(-4, 2, -3); scene.add(fill);
  const camera = new THREE.PerspectiveCamera(45, w / h, 0.1, 5000);
  const controls = new OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;
  controls.enablePan = false; // the lock toggle (applyLock, below) sets the turntable constraint

  // A ground grid under the character's feet gives the pose a floor to read against, plus
  // a transparent shadow-catcher plane so the directional light drops a soft shadow, and
  // the light's shadow frustum is sized to the character (Synty rigs are ~100+ units tall).
  let ground = null, shadowPlane = null;
  const placeGround = (object, box) => {
    if (!box || box.isEmpty()) return;
    const size = box.getSize(new THREE.Vector3()), center = box.getCenter(new THREE.Vector3());
    const span = Math.max(size.x, size.y, size.z) || 1;
    // Ground at the lowest bone (feet), not box.min.y — posedBox pads its bounds, which
    // would float the plane ~0.1 below the feet and leave the character hovering.
    let footY = Infinity;
    object.traverse((n) => { if (n.isBone) footY = Math.min(footY, n.getWorldPosition(_posedV).y); });
    if (!isFinite(footY)) footY = box.min.y;
    if (ground) { scene.remove(ground); ground.geometry.dispose(); ground.material.dispose(); }
    ground = new THREE.GridHelper(Math.max(size.x, size.z) * 3 || 1, 16, 0x40444f, 0x2b2e37);
    ground.position.set(center.x, footY, center.z);
    scene.add(ground);
    if (shadowPlane) { scene.remove(shadowPlane); shadowPlane.geometry.dispose(); shadowPlane.material.dispose(); }
    shadowPlane = new THREE.Mesh(new THREE.PlaneGeometry(span * 4, span * 4), new THREE.ShadowMaterial({ opacity: 0.32 }));
    shadowPlane.rotation.x = -Math.PI / 2;
    shadowPlane.position.set(center.x, footY, center.z);
    shadowPlane.receiveShadow = true;
    scene.add(shadowPlane);
    dir.position.set(center.x + span, box.max.y + span * 1.5, center.z + span * 0.6);
    dir.target.position.copy(center); dir.target.updateMatrixWorld();
    const sc = dir.shadow.camera;
    sc.near = span * 0.1; sc.far = span * 6; sc.left = -span; sc.right = span; sc.top = span; sc.bottom = -span;
    sc.updateProjectionMatrix();
  };
  // Aim slightly above the vertical centre so the character sits centred in the viewport
  // (a touch of lift reads more naturally than dead-centre without wasting the top third).
  const eyeLevel = (box) => {
    if (!box || box.isEmpty()) return;
    const dy = (box.min.y + (box.max.y - box.min.y) * 0.55) - controls.target.y;
    controls.target.y += dy;
    camera.position.y += dy;
    controls.update();
  };
  // A small corner gizmo showing the world axes from the current view, so orientation is
  // legible while turntable-spinning (X red, Y green/up, Z blue).
  const gizmoScene = new THREE.Scene();
  gizmoScene.add(new THREE.AxesHelper(1));
  const gizmoCam = new THREE.OrthographicCamera(-1.5, 1.5, 1.5, -1.5, 0.1, 10);
  const viewSize = new THREE.Vector2();

  const clock = new THREE.Clock();
  let raf = 0, obj = null, stopped = false;
  let mixer = null, action = null, clips = [], soloClips = null, soloRootRest = null, clipDur = 0, playing = true, ctrls = null;
  let rawClips = [], playRootName = null, playUpAxis = null, motionOn = false, curClip = 0;
  let playInPlace = [], playMotion = []; // the two clip sets the root-motion toggle swaps between

  // View controls overlaid on the canvas: three view modes (isometric default / flat
  // eye-level / free rotation), and — for a root-motion clip — show the travel or in place.
  const ISO_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2 3 7v10l9 5 9-5V7z"/><path d="M3 7l9 5 9-5M12 12v10"/></svg>';
  const FLAT_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 14h18"/></svg>';
  const FREE_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v5h-5"/></svg>';
  const MOVE_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 9l-3 3 3 3M9 5l3-3 3 3M15 19l-3 3-3-3M19 9l3 3-3 3M2 12h20M12 2v20"/></svg>';
  const DEFAULT_POLAR = Math.PI * 0.36; // elevated 3/4 (isometric-ish) angle
  const toolbar = document.createElement('div'); toolbar.className = 'lb-viewtools';
  const mkBtn = (cls, svg, title, onClick) => {
    const btn = document.createElement('button'); btn.type = 'button'; btn.className = 'lb-viewbtn ' + cls;
    btn.innerHTML = svg; btn.title = title; btn.addEventListener('click', onClick); toolbar.appendChild(btn); return btn;
  };
  const viewBtns = {};
  const setViewMode = (mode) => {
    if (mode === 'iso') { controls.minPolarAngle = controls.maxPolarAngle = DEFAULT_POLAR; }
    else if (mode === 'flat') { controls.minPolarAngle = controls.maxPolarAngle = Math.PI / 2; }
    else { controls.minPolarAngle = 0.0001; controls.maxPolarAngle = Math.PI - 0.0001; }
    for (const m in viewBtns) viewBtns[m].classList.toggle('on', m === mode);
    controls.update();
  };
  viewBtns.iso = mkBtn('', ISO_ICON, 'Isometric view', () => setViewMode('iso'));
  viewBtns.flat = mkBtn('', FLAT_ICON, 'Flat (eye-level) view', () => setViewMode('flat'));
  viewBtns.free = mkBtn('', FREE_ICON, 'Free rotation', () => setViewMode('free'));
  const moveBtn = mkBtn('lb-move', MOVE_ICON, '', () => {
    motionOn = !motionOn;
    moveBtn.classList.toggle('on', motionOn);
    moveBtn.title = motionOn ? 'Showing root motion — click to play in place' : 'Playing in place — click to show root motion';
    clips = motionOn ? playMotion : playInPlace;
    playClip(curClip);
  });
  moveBtn.hidden = true;
  container.appendChild(toolbar);
  setViewMode('iso');

  const clearOverlays = () => { container.querySelectorAll('.lb-placeholder,.lb-controls').forEach((e) => e.remove()); };
  const ensureCanvas = () => { if (!renderer.domElement.isConnected) container.appendChild(renderer.domElement); };
  const showPlaceholder = (text) => {
    if (obj) { scene.remove(obj); dispose(obj); obj = null; }
    renderer.domElement.remove();
    clearOverlays();
    const box = document.createElement('div');
    box.className = 'lb-placeholder';
    // Show the same Unity-rendered preview the grid card uses, when there is one, so
    // the viewer matches the card instead of dropping to a bare icon.
    if (asset.thumb === 'preview') {
      const img = new Image(); img.className = 'lb-preview'; img.src = thumbURL(asset.id);
      img.onerror = () => img.replaceWith(iconEl(asset.category));
      box.appendChild(img);
    } else {
      box.appendChild(iconEl(asset.category));
    }
    if (text) { const p = document.createElement('p'); p.textContent = text; box.appendChild(p); }
    container.appendChild(box);
  };

  const playClip = (i) => {
    if (action) action.stop();
    curClip = i;
    action = mixer.clipAction(clips[i]);
    action.reset(); action.play();
    clipDur = clips[i].duration || 0;
    playing = true;
    if (ctrls) ctrls.setClip(i);
  };

  const buildPlayback = (mixerRoot, cs, charInfo, rootRest, rmCs) => {
    let rootBoneName = null;
    mixerRoot.traverse((n) => { if (n.isBone && !rootBoneName && (!n.parent || !n.parent.isBone)) rootBoneName = n.name; });
    mixerRoot.traverse((o) => { if (o.isMesh) o.castShadow = true; });
    // Correct orientation and measure the framing box first, from the character's constant
    // reference (bind) pose — the shared prepareClipRig, so thumbnail and lightbox stay in
    // lockstep. It records the up axis (for in-place stripping); then the clip plays inside
    // the fixed frame. scale, centering and the ground stay fixed no matter what the clip does.
    const refBox = prepareClipRig(mixerRoot, rootRest);
    rawClips = cs; playRootName = rootBoneName; playUpAxis = mixerRoot.userData.upAxis;
    const rmClips = rmCs || [];
    // With a paired RM sibling, in-place is the native (non-RM) clips and the travel view is
    // the RM clips — both play on this same skeleton. Without one, fall back to stripping a
    // baked-motion clip in place algorithmically.
    playInPlace = rmClips.length ? cs : cs.map((c) => stripRootMotion(c, rootBoneName, playUpAxis));
    playMotion = rmClips.length ? rmClips : cs;
    clips = motionOn ? playMotion : playInPlace;
    moveBtn.hidden = !(rmClips.length || hasBakedMotion(asset.name));
    mixer = new THREE.AnimationMixer(mixerRoot);
    ctrls = makeControls();
    renderCharacter(charInfo);
    frameBox(refBox, camera, controls);
    placeGround(mixerRoot, refBox);
    eyeLevel(refBox);
    playClip(0);
  };

  // Load a chosen character and play the pending clip-only clips on it. Resolves
  // true on success, false if the character couldn't load — so callers can fall
  // back to another rig or the picker instead of leaving an empty viewer.
  const useCharacter = (item) =>
    loadModel(contentURL(item.id), item.ext).then(async (char) => {
      if (stopped) { dispose(char); return true; }
      CharRegistry.add({ id: item.id, name: item.name, ext: item.ext, bones: boneNames(char), vendor: item.vendor });
      const clips = await Promise.all(soloClips.map((c) => retargetedFor(c, asset.vendor, char)));
      let rmCs = null;
      const rmRaw = await loadRMClips(asset); // travel sibling, retargeted onto the same body
      if (rmRaw) rmCs = await Promise.all(rmRaw.map((c) => retargetedFor(c, asset.vendor, char)));
      if (stopped) { dispose(char); return true; }
      clearOverlays(); ensureCanvas();
      if (obj) { scene.remove(obj); dispose(obj); }
      obj = char; scene.add(char);
      mixer = null; action = null;
      buildPlayback(char, clips, { id: item.id, name: item.name }, asset.vendor === 'synty' ? null : soloRootRest, rmCs);
      return true;
    }).catch(() => false);

  // ---- character sidebar panel ----

  const SAVE_SVG = '<svg viewBox="0 0 24 24" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"><path d="M6 3h12v18l-6-4-6 4z"/></svg>';

  const useAndFallback = async (item, forBones) => {
    if (!(await useCharacter(item))) showCharacterChooser(forBones, 'That model could not be loaded. Try another.');
  };

  // characterSearch is a single autocomplete combobox: a text input whose dropdown of
  // matches appears only on focus/typing. On empty focus it suggests the rigs already
  // known to fit this clip (not a random dump); typing searches all models.
  function characterSearch(forBones, current) {
    const box = document.createElement('div'); box.className = 'lb-combo';
    const input = document.createElement('input');
    input.type = 'search'; input.className = 'lb-comboinput'; input.autocomplete = 'off';
    input.placeholder = current ? 'change character…' : 'search characters…';
    const drop = document.createElement('div'); drop.className = 'lb-drop'; drop.hidden = true;
    box.append(input, drop);

    let seq = 0, items = [], active = -1;
    const choose = (it) => { drop.hidden = true; useAndFallback(it, forBones); };
    const render = () => {
      drop.replaceChildren();
      if (!items.length) { drop.hidden = true; return; }
      items.forEach((it, i) => {
        const r = document.createElement('button'); r.type = 'button'; r.className = 'lb-result' + (i === active ? ' active' : '');
        const nm = document.createElement('span'); nm.className = 'lb-result-name'; nm.textContent = it.name.replace(/\.[^.]+$/, '');
        const sub = document.createElement('span'); sub.className = 'lb-result-sub'; sub.textContent = it.sub || [it.vendor, it.pack].filter(Boolean).join(' · ');
        r.append(nm, sub); r.title = it.name;
        r.addEventListener('mousedown', (e) => { e.preventDefault(); choose(it); }); // fire before blur
        drop.appendChild(r);
      });
      drop.hidden = false;
    };
    const suggestKnown = () => {
      const out = [], seen = new Set();
      if (current && current.name) seen.add(current.name);
      for (const e of CharRegistry.list()) {
        if ((current && e.id === current.id) || seen.has(e.name)) continue;
        if (forBones && !coversBones(e.bones, forBones)) continue;
        seen.add(e.name); out.push({ id: e.id, name: e.name, ext: e.ext, sub: 'fits this animation' });
        if (out.length >= 6) break;
      }
      return out;
    };
    const run = async (q) => {
      const my = ++seq;
      if (!q) { items = suggestKnown(); active = -1; render(); return; }
      try {
        const d = await (await fetch('/api/assets?type=model&limit=8&q=' + encodeURIComponent(q))).json();
        if (my === seq) { items = d.items || []; active = -1; render(); }
      } catch { /* ignore */ }
    };
    let t;
    input.addEventListener('input', () => { clearTimeout(t); t = setTimeout(() => run(input.value.trim()), 180); });
    input.addEventListener('focus', () => run(input.value.trim()));
    input.addEventListener('blur', () => setTimeout(() => { drop.hidden = true; }, 120));
    input.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowDown') { e.preventDefault(); if (items.length) { active = Math.min(active + 1, items.length - 1); render(); } }
      else if (e.key === 'ArrowUp') { e.preventDefault(); if (items.length) { active = Math.max(active - 1, 0); render(); } }
      else if (e.key === 'Enter') { e.preventDefault(); const it = items[active] || items[0]; if (it) choose(it); }
      else if (e.key === 'Escape') { drop.hidden = true; }
    });
    return box;
  }

  // renderCharacter shows the active character (charInfo) in the sidebar, or clears
  // it for self-contained models. The no-match chooser is shown separately.
  const renderCharacter = (charInfo) => { if (charInfo) showCharacterPanel(charInfo); else lb.character.replaceChildren(); };

  function showCharacterPanel(charInfo) {
    lb.character.replaceChildren();
    const bones = soloClips ? clipBones(soloClips[0]) : null;
    const panel = document.createElement('div'); panel.className = 'lb-charpanel';
    const label = document.createElement('div'); label.className = 'lb-charpanel-label'; label.textContent = 'Playing on';
    const name = document.createElement('div'); name.className = 'lb-charname';
    name.textContent = charInfo.name.replace(/\.[^.]+$/, ''); name.title = charInfo.name;

    const row = document.createElement('div'); row.className = 'lb-charrow';
    row.appendChild(characterSearch(bones, charInfo));
    // Save-as-default: an icon button beside the search; filled when this is the rig's default.
    const def = document.createElement('button'); def.type = 'button'; def.className = 'lb-savedefault'; def.innerHTML = SAVE_SVG;
    const refreshDef = () => {
      const on = CharRegistry.isPinned(charInfo.id);
      def.classList.toggle('on', on);
      def.title = on
        ? 'Default character for this rig — click to unset'
        : 'Save as the default character for this rig';
    };
    def.addEventListener('click', () => { CharRegistry.pin(charInfo.id, !CharRegistry.isPinned(charInfo.id)); refreshDef(); });
    refreshDef();
    row.appendChild(def);

    panel.append(label, name, row);
    lb.character.appendChild(panel);
  }

  function showCharacterChooser(forBones, note) {
    lb.character.replaceChildren();
    const panel = document.createElement('div'); panel.className = 'lb-charpanel';
    const label = document.createElement('div'); label.className = 'lb-charpanel-label'; label.textContent = 'Character';
    const hint = document.createElement('div'); hint.className = 'lb-charhint';
    hint.textContent = note || 'This animation has no mesh — search for a character to play it on:';
    panel.append(label, hint, characterSearch(forBones, null));
    lb.character.appendChild(panel);
  }

  function makeControls() {
    clearOverlays();
    const bar = document.createElement('div'); bar.className = 'lb-controls';
    const play = document.createElement('button'); play.type = 'button'; play.className = 'lb-play'; play.textContent = '⏸';
    const scrub = document.createElement('input'); scrub.type = 'range'; scrub.min = '0'; scrub.max = '1000'; scrub.value = '0'; scrub.className = 'lb-scrub';
    const time = document.createElement('span'); time.className = 'lb-time';
    bar.append(play, scrub, time);
    let sel = null;
    if (clips.length > 1) {
      sel = document.createElement('select'); sel.className = 'lb-clipsel';
      clips.forEach((c, i) => sel.appendChild(new Option(c.name || 'clip ' + (i + 1), String(i))));
      sel.addEventListener('change', () => playClip(+sel.value));
      bar.appendChild(sel);
    }
    const showTime = (t) => { time.textContent = `${t.toFixed(2)} / ${clipDur.toFixed(2)}s`; };
    play.addEventListener('click', () => { playing = !playing; play.textContent = playing ? '⏸' : '▶'; clock.getDelta(); });
    scrub.addEventListener('input', () => {
      if (!action) return;
      playing = false; play.textContent = '▶';
      const t = (+scrub.value / 1000) * clipDur;
      action.paused = false; action.time = t; mixer.update(0);
      showTime(t);
    });
    container.appendChild(bar);
    return {
      sync(t) { if (document.activeElement !== scrub) scrub.value = String(clipDur ? (t / clipDur) * 1000 : 0); showTime(t); },
      setClip(i) { if (sel) sel.value = String(i); },
    };
  }

  (async () => {
    let root;
    try { root = await loadModel(contentURL(asset.id), asset.ext); }
    catch { showPlaceholder('Could not load this model.'); return; }
    if (stopped) { dispose(root); return; }
    const cs = clipsForAsset(root, asset);
    if (isRenderable(root)) {
      obj = root; scene.add(root);
      CharRegistry.add({ id: asset.id, name: asset.name, ext: asset.ext, bones: boneNames(root), vendor: asset.vendor });
      // buildPlayback corrects orientation and frames from the reference box; do the same
      // for a static (clip-less) renderable so a Z-up model still stands upright and framed.
      if (cs.length) {
        const rmCs = await loadRMClips(asset); // travel sibling, if this animation ships one
        if (stopped) return;
        buildPlayback(root, cs, null, null, rmCs);
      } else frameBox(prepareClipRig(root, null), camera, controls);
      return;
    }
    if (!cs.length) { dispose(root); showPlaceholder('No mesh to preview (data file).'); return; }
    // clip-only: play on a rig it matches (AnimationClips survive disposing the source).
    soloClips = cs;
    soloRootRest = captureRootRest(root); // the clip file's root axis, for uprightRig
    const bones = clipBones(cs[0]);
    dispose(root);
    await CharRegistry.seed();
    if (stopped) return;
    // Play on the best-matching rig; a cached entry can go stale (its id changes
    // after a re-index), so a failed load evicts it and falls through to the next
    // match, then vendor discovery, then the manual picker — never a blank viewer.
    const playOnMatch = async () => {
      for (let m = CharRegistry.match(bones, asset.vendor); m && !stopped; m = CharRegistry.match(bones, asset.vendor)) {
        if (await useCharacter(m)) return true;
        CharRegistry.remove(m.id);
      }
      return false;
    };
    if (await playOnMatch()) return;
    if (stopped) return;
    await CharRegistry.discoverForVendor(asset.vendor, bones);
    if (await playOnMatch()) return;
    if (stopped) return;
    showPlaceholder('Animation clip — pick a character in the sidebar →');
    showCharacterChooser(bones);
  })();

  const onResize = () => {
    const nw = container.clientWidth, nh = container.clientHeight;
    if (nw && nh) { renderer.setSize(nw, nh); camera.aspect = nw / nh; camera.updateProjectionMatrix(); }
  };
  // A ResizeObserver (not a one-shot clientWidth read) so the canvas fills the viewer
  // whether it was created while the lightbox was still hidden (first open) or already
  // visible (navigating) — otherwise the initial size flip-flops between the two.
  const ro = new ResizeObserver(onResize);
  ro.observe(container);
  const loop = () => {
    raf = requestAnimationFrame(loop);
    if (mixer) { const dt = clock.getDelta(); if (playing) { mixer.update(dt); if (action && ctrls) ctrls.sync(action.time); } }
    controls.update();
    renderer.render(scene, camera);
    // orientation gizmo, top-left corner (in a row with the view toolbar)
    renderer.getSize(viewSize);
    renderer.autoClear = false;
    renderer.clearDepth();
    const g = 52, gx = 10, gy = viewSize.y - g - 8;
    renderer.setViewport(gx, gy, g, g);
    renderer.setScissor(gx, gy, g, g);
    renderer.setScissorTest(true);
    gizmoCam.position.copy(camera.position).sub(controls.target).normalize().multiplyScalar(3);
    gizmoCam.lookAt(0, 0, 0);
    renderer.render(gizmoScene, gizmoCam);
    renderer.setScissorTest(false);
    renderer.setViewport(0, 0, viewSize.x, viewSize.y);
    renderer.autoClear = true;
  };
  loop();

  return {
    stop() {
      stopped = true;
      cancelAnimationFrame(raf);
      ro.disconnect();
      controls.dispose();
      if (mixer) mixer.stopAllAction();
      if (obj) dispose(obj);
      if (ground) { ground.geometry.dispose(); ground.material.dispose(); }
      if (shadowPlane) { shadowPlane.geometry.dispose(); shadowPlane.material.dispose(); }
      gizmoScene.traverse((o) => { o.geometry?.dispose?.(); o.material?.dispose?.(); });
      renderer.dispose();
      renderer.domElement.remove();
    },
  };
}

document.getElementById('lb-close').addEventListener('click', closeLightbox);
lb.root.querySelector('.lb-backdrop').addEventListener('click', closeLightbox);
lb.prev.addEventListener('click', () => navLightbox(-1));
lb.next.addEventListener('click', () => navLightbox(1));
document.addEventListener('keydown', (e) => {
  if (lb.root.hidden) return;
  if (e.key === 'Escape') { closeLightbox(); return; }
  const t = e.target;
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
  if (e.key === 'ArrowLeft') { e.preventDefault(); navLightbox(-1); }
  else if (e.key === 'ArrowRight') { e.preventDefault(); navLightbox(1); }
});

// ---- misc ----

function humanSize(n) {
  if (!n) return '—';
  const u = ['B', 'KB', 'MB', 'GB'];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
}

let debounce;
els.q.addEventListener('input', () => { clearTimeout(debounce); debounce = setTimeout(reset, 220); });
for (const s of [els.sort, els.group]) s.addEventListener('change', reset);

new IntersectionObserver((entries) => {
  if (entries.some((e) => e.isIntersecting)) fetchPage();
}, { rootMargin: '600px' }).observe(els.sentinel);

loadPalette();
fetchPage();
