import * as THREE from 'three';
import { GLTFLoader } from 'three/addons/loaders/GLTFLoader.js';
import { FBXLoader } from 'three/addons/loaders/FBXLoader.js';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';

const PAGE = 200;

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

const contentURL = (id) => '/api/content?id=' + encodeURIComponent(id);
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
const BLANK_PIXEL = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==';
const loadingManager = new THREE.LoadingManager();
loadingManager.setURLModifier((url) => (url.includes('/api/content') ? url : BLANK_PIXEL));
const CLAY = new THREE.MeshStandardMaterial({ color: 0xc7ccd6, roughness: 0.72, metalness: 0.0 });

// normalizeClip rebases a clip to its first real keyframe. Synty source FBX keep each
// clip at its position on a shared master timeline, so a clip can begin with seconds
// of empty (static) lead-in that no real import would play; trimming it makes playback
// and posed thumbnails start on the actual motion. A no-op for clips already at 0.
function normalizeClip(clip) {
  let tMin = Infinity;
  for (const tr of clip.tracks) if (tr.times.length) tMin = Math.min(tMin, tr.times[0]);
  if (!isFinite(tMin) || tMin <= 1e-3) return clip;
  // Clone each track's times before shifting. GLTFLoader shares one times buffer across
  // a clip's tracks (and across clips), so subtracting in place double-counts, drives the
  // shared array negative, and collapses durations to 0 (the NaN/0.00s scrubber on GLBs).
  for (const tr of clip.tracks) {
    const t = tr.times.slice();
    for (let i = 0; i < t.length; i++) t[i] -= tMin;
    tr.times = t;
  }
  clip.resetDuration();
  return clip;
}

// loadModel returns the display root with its animation clips attached as
// root.animations (GLTFLoader keeps clips on gltf.animations, not the scene).
async function loadModel(url, ext) {
  if (ext === 'glb' || ext === 'gltf') {
    const gltf = await new GLTFLoader(loadingManager).loadAsync(url);
    const root = gltf.scene;
    root.animations = (gltf.animations || []).map(normalizeClip);
    return root;
  }
  const obj = await new FBXLoader(loadingManager).loadAsync(url);
  obj.traverse((o) => { if (o.isMesh) o.material = CLAY; });
  if (obj.animations) obj.animations = obj.animations.map(normalizeClip);
  return obj;
}

function boneNames(root) {
  const names = [];
  root.traverse((o) => { if (o.isBone) names.push(o.name); });
  return names;
}

// clipBones returns the distinct bone names a clip's tracks drive.
function clipBones(clip) {
  return [...new Set(clip.tracks.map((t) => t.name.split('.')[0]))];
}

// coversBones reports whether a rig can actually play a clip: the clip must drive
// most of the rig (≥60% of the rig's bones — so nearly the whole body animates) AND
// cover a good part of the clip (≥45% of its bones). Requiring both rejects the
// small/partial rigs that share a handful of names but pose a full clip into garbage
// (the shredded animation thumbnails), and superset showcase rigs, while still
// letting a body play a richer clip.
function coversBones(have, want) {
  if (!have || !have.length || !want || !want.length) return false;
  const set = new Set(have);
  let hit = 0;
  for (const b of want) if (set.has(b)) hit++;
  return hit / have.length >= 0.6 && hit / want.length >= 0.45;
}

// posedBox returns the object's bounds from its posed skeleton (bone world positions), not
// Box3.setFromObject — which for a skinned mesh uses the bind pose at the origin and so
// ignores the animation, leaving displaced/animated poses off-centre. Falls back to
// geometry bounds for a static model with no skeleton. Padded to cover skin past the joints.
const _posedV = new THREE.Vector3();
function posedBox(object) {
  object.updateMatrixWorld(true);
  const box = new THREE.Box3();
  let bones = 0;
  object.traverse((n) => { if (n.isBone) { box.expandByPoint(n.getWorldPosition(_posedV)); bones++; } });
  if (bones < 2) { box.setFromObject(object); return box; }
  const pad = box.getSize(_posedV).length() * 0.06;
  box.expandByScalar(pad || 0);
  return box;
}

function frameBox(box, camera, controls, offset = 1.5) {
  if (!box || box.isEmpty()) return;
  const size = box.getSize(new THREE.Vector3());
  const center = box.getCenter(new THREE.Vector3());
  const maxDim = Math.max(size.x, size.y, size.z) || 1;
  const fov = camera.fov * Math.PI / 180;
  const dist = (maxDim / 2) / Math.tan(fov / 2) * offset;
  camera.near = dist / 100;
  camera.far = dist * 100;
  camera.position.set(center.x + dist * 0.7, center.y + dist * 0.5, center.z + dist);
  camera.lookAt(center);
  camera.updateProjectionMatrix();
  if (controls) { controls.target.copy(center); controls.update(); }
}

// isRenderable reports whether an object has any geometry to show. Synty ANIMATION
// packs ship mesh-less FBX (skeleton + keyframes only), which would otherwise render
// as an empty void.
function isRenderable(object) {
  const box = new THREE.Box3().setFromObject(object);
  if (box.isEmpty()) return false;
  // A real model has volume. Synty morph-animation FBX ship a flat 3-vertex stub
  // mesh (a degenerate plane) alongside the skeleton; treat that as a clip to pose
  // on a rig, not a stray triangle to render.
  const s = box.getSize(new THREE.Vector3());
  const dims = [s.x, s.y, s.z].sort((a, b) => a - b);
  return dims[0] > dims[2] * 1e-3;
}

// captureRootMotionRest returns the clip source's top-level bone rest quaternion — the
// bone's local rotation before any animation. It encodes the file's own axis convention,
// so a mesh-less clip posed on a body from a different file can be corrected regardless of
// what the character is doing (see uprightRig).
function captureRootRest(obj) {
  let rr = null;
  obj.traverse((n) => { if (n.isBone && !rr && (!n.parent || !n.parent.isBone)) rr = n.quaternion.clone(); });
  return rr;
}

// uprightRig rotates the whole object so the character stands +Y-up, fixing the axis
// flips from files authored Z-up (kevdev bodies, some explosive clips) or from a mesh-less
// clip whose root-bone axis differs from the body it plays on (kevdev clips on HumanF_Model).
// It measures the character's up (hips->head) at a straight *reference* pose — the bind
// pose, with the root bone forced to the clip's own rest (rootRest) so a cross-file clip's
// root convention is anticipated — then snaps that up to the nearest cardinal axis and
// rotates that axis to +Y. Snapping (not exact hips->head alignment) is deliberate: an
// already-upright rig keeps its natural forward spine lean instead of being tilted back,
// while a 90/180 flip is still fully corrected. It leaves the skeleton in the reference
// pose, so the caller can read a stable framing box (posedBox) before animating. rootRest
// is null for self-contained and retargeted (synty) clips: then it reads the rig's own bind.
function uprightRig(root, rootRest) {
  let rootBone = null, hips = null, head = null, neck = null;
  root.traverse((o) => {
    if (!o.isBone) return;
    const n = o.name.toLowerCase();
    if (!rootBone && (!o.parent || !o.parent.isBone)) rootBone = o;
    if (!hips && (n.includes('hips') || n.includes('pelvis'))) hips = o;
    if (!head && n.includes('head')) head = o;
    if (!neck && n.includes('neck')) neck = o;
  });
  const top = head || neck;
  if (!rootBone || !hips || !top) return;
  root.traverse((o) => { if (o.isSkinnedMesh && o.skeleton) o.skeleton.pose(); });
  if (rootRest) rootBone.quaternion.copy(rootRest);
  root.updateMatrixWorld(true);
  const up = top.getWorldPosition(new THREE.Vector3()).sub(hips.getWorldPosition(new THREE.Vector3()));
  if (up.lengthSq() < 1e-6) return;
  up.normalize();
  const ax = Math.abs(up.x), ay = Math.abs(up.y), az = Math.abs(up.z);
  const card = ax >= ay && ax >= az ? new THREE.Vector3(Math.sign(up.x), 0, 0)
    : ay >= az ? new THREE.Vector3(0, Math.sign(up.y), 0)
      : new THREE.Vector3(0, 0, Math.sign(up.z));
  // The character's up axis in this file's track space; stripRootMotion keeps this axis and
  // zeroes the two horizontal ones (a Z-up file's forward is on Y, not the Y-up default).
  root.userData.upAxis = card.clone();
  root.quaternion.premultiply(new THREE.Quaternion().setFromUnitVectors(card, new THREE.Vector3(0, 1, 0)));
  root.updateMatrixWorld(true);
}

// prepareClipRig orients a rig for a clip and returns its constant reference box. It is the
// single shared setup for both the grid thumbnail and the lightbox, so the two can't drift
// apart — the recurring "one is right, the other is sideways" was two separate code paths.
// Both feed it a pristine skeleton (the lightbox a freshly loaded body, the thumbnail a
// fresh clone) because re-measuring orientation on a reused, already-posed skeleton is
// unreliable. rootRest is already resolved (null for synty / self-contained; the clip's own
// root rest for a cross-file body).
function prepareClipRig(rig, rootRest) {
  uprightRig(rig, rootRest);
  return posedBox(rig);
}

// cloneRig deep-clones a skinned character (three's Object3D.clone shares the skeleton, so
// posing one clone would move them all). Same algorithm as three's SkeletonUtils.clone: clone
// the hierarchy, then rebind each SkinnedMesh to a cloned skeleton whose bones point at the
// cloned nodes. Lets the thumbnails reuse one loaded body but pose each clip on a fresh
// skeleton, matching the lightbox's fresh-load pipeline.
function cloneRig(source) {
  const srcLookup = new Map(), cloneLookup = new Map();
  const clone = source.clone();
  (function walk(a, b) { srcLookup.set(b, a); cloneLookup.set(a, b); for (let i = 0; i < a.children.length; i++) walk(a.children[i], b.children[i]); })(source, clone);
  clone.traverse((node) => {
    if (!node.isSkinnedMesh) return;
    const src = srcLookup.get(node);
    node.skeleton = src.skeleton.clone();
    node.bindMatrix.copy(src.bindMatrix);
    node.skeleton.bones = src.skeleton.bones.map((b) => cloneLookup.get(b));
    node.bind(node.skeleton, node.bindMatrix);
  });
  return clone;
}

// poseAt drives root to a representative mid-clip frame and returns the mixer, so a
// still shows real motion instead of the bind (T-)pose. skeleton.pose() first clears
// any prior binding on a reused rig. A zero/NaN-duration clip falls back to frame 0.
function poseAt(root, clip) {
  root.traverse((o) => { if (o.isSkinnedMesh && o.skeleton) o.skeleton.pose(); });
  const mixer = new THREE.AnimationMixer(root);
  mixer.clipAction(clip).play();
  mixer.setTime(clip.duration > 0 ? clip.duration * 0.5 : 0);
  return mixer;
}

// ---- rig-agnostic clip retargeting for the mesh-less Synty animation clips ----
// The Synty animation clips are authored on one shared rig whose neutral is the T-pose
// clip A_TPose_Neut. Playing a clip's raw local rotations on a different character rig
// distorts it (their bind poses differ); rebasing each rotation through the shared
// neutral — rigBind · neutral⁻¹ · sourceFrame — makes any Synty T-pose character play any
// clip cleanly. syntyNeutral loads that neutral once (per-bone local quaternions).
let syntyNeutralPromise = null;
function syntyNeutral() {
  if (syntyNeutralPromise) return syntyNeutralPromise;
  syntyNeutralPromise = (async () => {
    try {
      const r = await fetch('/api/assets?vendor=synty&limit=8&group=0&q=' + encodeURIComponent('A_TPose_Neut'));
      const items = (await r.json()).items || [];
      const it = items.find((x) => x.name === 'A_TPose_Neut.fbx') || items[0];
      if (!it) return null;
      const o = await loadModel(contentURL(it.id), it.ext);
      const clip = (o.animations || [])[0];
      if (clip) { const m = new THREE.AnimationMixer(o); m.clipAction(clip).play(); m.setTime((clip.duration || 0) * 0.5); }
      const map = new Map();
      o.traverse((n) => { if (n.isBone) map.set(n.name, n.quaternion.clone()); });
      dispose(o);
      return map.size ? map : null;
    } catch { return null; }
  })();
  return syntyNeutralPromise;
}

// retargetClip rebuilds clip's rotation tracks onto rig's rest pose through the neutral.
// Position/scale tracks pass through unchanged so vertical motion (a squat's hip drop, a
// jump) survives; horizontal locomotion is handled later by stripRootMotion. Returns the
// original clip when no rotation maps (so a native rig still plays).
function retargetClip(clip, neutral, rig) {
  rig.traverse((o) => { if (o.isSkinnedMesh && o.skeleton) o.skeleton.pose(); });
  const bind = new Map();
  rig.traverse((n) => { if (n.isBone) bind.set(n.name, n.quaternion.clone()); });
  const src = new THREE.Quaternion(), delta = new THREE.Quaternion(), inv = new THREE.Quaternion(), out = new THREE.Quaternion();
  const tracks = [];
  let rotated = 0;
  for (const tr of clip.tracks) {
    if (!tr.name.endsWith('.quaternion')) { tracks.push(tr); continue; }
    const bone = tr.name.slice(0, -'.quaternion'.length);
    const nq = neutral.get(bone), bq = bind.get(bone);
    if (!nq || !bq) continue;
    inv.copy(nq).invert();
    const v = tr.values, vv = new Float32Array(v.length);
    for (let i = 0; i < v.length; i += 4) {
      src.set(v[i], v[i + 1], v[i + 2], v[i + 3]);
      delta.copy(inv).multiply(src);
      out.copy(bq).multiply(delta);
      vv[i] = out.x; vv[i + 1] = out.y; vv[i + 2] = out.z; vv[i + 3] = out.w;
    }
    tracks.push(new THREE.QuaternionKeyframeTrack(tr.name, tr.times, vv));
    rotated++;
  }
  return rotated ? new THREE.AnimationClip(clip.name, clip.duration, tracks) : clip;
}

// retargetedFor returns a clip playable on rig: the Synty mesh-less clips are rebased
// through the shared neutral; everything else plays as-is.
async function retargetedFor(clip, vendor, rig) {
  if (vendor !== 'synty') return clip;
  const neutral = await syntyNeutral();
  return neutral ? retargetClip(clip, neutral, rig) : clip;
}

// stripRootMotion zeroes the horizontal locomotion on the root bone's position track, so a
// walk/run/dash plays in place instead of drifting out of frame, while keeping the vertical
// axis (so squats, jumps, and hip bob still read). upAxis is the character's up in the clip's
// track space (from uprightRig); the two axes that aren't it are the horizontal ones. Defaults
// to Y-up — a Z-up file's forward travel is on Y, which the default would wrongly keep.
function stripRootMotion(clip, rootName, upAxis) {
  if (!rootName) return clip;
  const up = upAxis || new THREE.Vector3(0, 1, 0);
  const keep = [Math.abs(up.x) >= 0.5, Math.abs(up.y) >= 0.5, Math.abs(up.z) >= 0.5];
  let changed = false;
  const tracks = clip.tracks.map((tr) => {
    if (tr.name !== rootName + '.position') return tr;
    const v = tr.values.slice();
    for (let i = 0; i < v.length; i += 3) { if (!keep[0]) v[i] = 0; if (!keep[1]) v[i + 1] = 0; if (!keep[2]) v[i + 2] = 0; }
    changed = true;
    return new THREE.VectorKeyframeTrack(tr.name, tr.times, v);
  });
  return changed ? new THREE.AnimationClip(clip.name, clip.duration, tracks) : clip;
}

function dispose(object) {
  object.traverse((o) => {
    if (o.geometry) o.geometry.dispose();
    if (o.material) {
      const mats = Array.isArray(o.material) ? o.material : [o.material];
      for (const m of mats) {
        for (const k in m) { if (m[k] && m[k].isTexture) m[k].dispose(); }
        m.dispose();
      }
    }
  });
}

// ---- character registry: match a clip-only animation to a rig it can play on ----
// A skinned character mesh whose bone names cover a clip's tracks can play that clip
// directly (proven for the Synty rig: the native body and the clips share a rest
// pose). Different rigs (e.g. the goblin A-pose rig) match a different body, or none
// — in-browser retargeting of these mesh-less clips is unreliable, so a non-matching
// rig falls back to the manual picker rather than a distorted pose.
const CharRegistry = {
  key: 'browsePreviewChars',
  seeded: false,
  list() { try { return JSON.parse(localStorage.getItem(this.key)) || []; } catch { return []; } },
  save(l) { try { localStorage.setItem(this.key, JSON.stringify(l.slice(0, 40))); } catch { /* quota */ } },
  add(entry) {
    if (!entry.bones || entry.bones.length < 10) return;
    const l = this.list().filter((e) => e.id !== entry.id);
    l.unshift(entry);
    this.save(l);
  },
  remove(id) { this.save(this.list().filter((e) => e.id !== id)); },
  // match picks the registered character whose skeleton best covers a clip's bones.
  // A pinned character that covers the clip wins over a higher-coverage unpinned one,
  // so pinning a body for a rig makes it the default for every clip on that rig.
  // Auto-match is scoped to the clip's own vendor: cross-vendor skeletons share enough
  // bone names to pass the coverage bar but differ in rest pose, posing a clip into a
  // shredded/T-posed garbage still. A legacy entry with no recorded vendor is a wildcard
  // until it is re-registered (see register), so old caches keep working.
  match(bones, vendor) {
    const want = new Set(bones);
    if (!want.size) return null;
    let best = null, bestScore = -1, bestPinned = false;
    for (const e of this.list()) {
      if (vendor && e.vendor && e.vendor !== vendor) continue;
      if (e.bones.length > new Set(e.bones).size * 1.4) continue; // legacy cache: skip multi-skeleton showcase meshes, whose bones repeat per character (register now rejects them)
      const have = new Set(e.bones);
      let hit = 0;
      for (const b of want) if (have.has(b)) hit++;
      if (hit / have.size < 0.6 || hit / want.size < 0.45) continue; // rig must fit the clip
      const pinned = !!e.pinned;
      // Rank by absolute shared bones: prefer the fullest matching body.
      if ((pinned && !bestPinned) || (pinned === bestPinned && hit > bestScore)) {
        best = e; bestScore = hit; bestPinned = pinned;
      }
    }
    return best;
  },
  pin(id, on) {
    const l = this.list();
    const e = l.find((x) => x.id === id);
    if (!e) return;
    e.pinned = on;
    this.save(l);
  },
  isPinned(id) { return !!(this.list().find((x) => x.id === id) || {}).pinned; },
  async register(item) {
    const known = this.list().find((e) => e.id === item.id);
    if (known) { // already loaded; only backfill a missing vendor so legacy caches scope
      if (item.vendor && known.vendor !== item.vendor) { known.vendor = item.vendor; this.save(this.list().map((e) => e.id === item.id ? known : e)); }
      return true;
    }
    let root;
    try { root = await loadModel(contentURL(item.id), item.ext); } catch { return false; }
    const bones = boneNames(root);
    // A showcase FBX packs several characters into one mesh (multiple skeletons, duplicate
    // bone names like two "Hips"); those can't be posed cleanly — the retarget bind map and
    // three.js property bindings resolve the name ambiguously and shred the character. Only
    // register a single-skeleton body (a clean rig may still carry a few incidental duplicate
    // helper-bone names, so gate on skeleton count, not on any duplicate).
    const skels = new Set();
    root.traverse((n) => { if (n.isSkinnedMesh && n.skeleton) skels.add(n.skeleton); });
    const rigged = isRenderable(root) && bones.length >= 10 && skels.size <= 1;
    dispose(root);
    if (rigged) this.add({ id: item.id, name: item.name, ext: item.ext, bones, vendor: item.vendor });
    return rigged;
  },
  // Lazily discover a few character bodies by name so auto-match works before the
  // user has opened a matching character. Runs once per session; bounded; add()
  // dedups. It does NOT early-out on a non-empty registry — an already-registered
  // character may be the wrong rig for this clip, so the known bodies still get
  // discovered to cover their rigs.
  async seed() {
    if (this.seeded) return;
    this.seeded = true;
    const terms = ['PolygonSyntyCharacter', 'Character', 'SK_Mannequin', '_Model', 'Base_Mesh', 'SM_Chr'];
    let added = 0;
    for (const t of terms) {
      if (added >= 3) return;
      try {
        const r = await fetch('/api/assets?type=model&limit=4&q=' + encodeURIComponent(t));
        const items = (await r.json()).items;
        for (const it of items.slice(0, 2)) { if (await this.register(it)) added++; if (added >= 3) return; }
      } catch { /* skip */ }
    }
  },
  // When the seed didn't cover a clip's rig, look for a body in the clip's own vendor
  // (a clip's native character usually ships in the same vendor's packs).
  async discoverForVendor(vendor, want) {
    if (!vendor) return;
    // Load candidate bodies until one actually covers this clip (a single showcase mesh can
    // win by bone count yet be a multi-skeleton mesh register now rejects, and some bodies
    // ship a different skeleton family that shares no bone names). Stop at the first covering
    // rig; bound the loads so a vendor without a match doesn't scan the whole library.
    const seen = new Set(this.list().map((e) => e.id));
    let loaded = 0;
    for (const t of ['Character', 'Hero', 'Human', 'Knight', 'Warrior', 'Body', 'Model', 'Base']) {
      let items;
      try { items = (await (await fetch(`/api/assets?type=model&limit=8&vendor=${encodeURIComponent(vendor)}&q=${encodeURIComponent(t)}`)).json()).items || []; } catch { continue; }
      for (const it of items) {
        if (loaded >= 14) return;
        if (seen.has(it.id)) continue;
        seen.add(it.id); loaded++;
        await this.register(it);
        if (want && this.match(want, vendor)) return;
      }
    }
  },
};

// ---- lazy 3D thumbnails: one shared renderer, sequential queue, cached ----

class ModelThumbnails {
  constructor(size = 220) {
    this.size = size;
    this.cache = new Map();
    this.rigs = new Map(); // matched character id -> loaded rig, reused to pose clips
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
  enqueue(holder, asset) {
    this.queue = this.queue.then(() => this.render(holder, asset)).catch(() => {});
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
      const obj = await loadModel(contentURL(asset.id), asset.ext);
      const rootRest = captureRootRest(obj);
      if (isRenderable(obj)) {
        const cs = obj.animations || [];
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
      const clips = obj.animations || [];
      dispose(obj);
      return clips.length ? await this.buildPosed(clips[0], asset.vendor, rootRest) : null;
    } catch (e) {
      return null;
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
    clips = motionOn ? rawClips : rawClips.map((c) => stripRootMotion(c, playRootName, playUpAxis));
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

  const buildPlayback = (mixerRoot, cs, charInfo, rootRest) => {
    let rootBoneName = null;
    mixerRoot.traverse((n) => { if (n.isBone && !rootBoneName && (!n.parent || !n.parent.isBone)) rootBoneName = n.name; });
    mixerRoot.traverse((o) => { if (o.isMesh) o.castShadow = true; });
    // Correct orientation and measure the framing box first, from the character's constant
    // reference (bind) pose — the shared prepareClipRig, so thumbnail and lightbox stay in
    // lockstep. It records the up axis (for in-place stripping); then the clip plays inside
    // the fixed frame. scale, centering and the ground stay fixed no matter what the clip does.
    const refBox = prepareClipRig(mixerRoot, rootRest);
    rawClips = cs; playRootName = rootBoneName; playUpAxis = mixerRoot.userData.upAxis;
    clips = motionOn ? cs : cs.map((c) => stripRootMotion(c, rootBoneName, playUpAxis));
    moveBtn.hidden = !/rootmotion|_rm\b|\[rm\]/i.test(asset.name || '');
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
      if (stopped) { dispose(char); return true; }
      clearOverlays(); ensureCanvas();
      if (obj) { scene.remove(obj); dispose(obj); }
      obj = char; scene.add(char);
      mixer = null; action = null;
      buildPlayback(char, clips, { id: item.id, name: item.name }, asset.vendor === 'synty' ? null : soloRootRest);
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
    const cs = root.animations || [];
    if (isRenderable(root)) {
      obj = root; scene.add(root);
      CharRegistry.add({ id: asset.id, name: asset.name, ext: asset.ext, bones: boneNames(root), vendor: asset.vendor });
      // buildPlayback corrects orientation and frames from the reference box; do the same
      // for a static (clip-less) renderable so a Z-up model still stands upright and framed.
      if (cs.length) buildPlayback(root, cs, null, null);
      else frameBox(prepareClipRig(root, null), camera, controls);
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
