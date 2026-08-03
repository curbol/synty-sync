import * as THREE from 'three';
import { GLTFLoader } from 'three/addons/loaders/GLTFLoader.js';
import { FBXLoader } from 'three/addons/loaders/FBXLoader.js';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';

const PAGE = 200;

const els = {
  q: document.getElementById('q'),
  type: document.getElementById('type'),
  vendor: document.getElementById('vendor'),
  variant: document.getElementById('variant'),
  count: document.getElementById('count'),
  grid: document.getElementById('grid'),
  sentinel: document.getElementById('sentinel'),
  empty: document.getElementById('empty'),
};

const state = { offset: 0, total: 0, loading: false, done: false, facetsLoaded: false };

// ---- data ----

function query(extra = {}) {
  const p = new URLSearchParams();
  if (els.q.value.trim()) p.set('q', els.q.value.trim());
  if (els.type.value) p.set('type', els.type.value);
  if (els.vendor.value) p.set('vendor', els.vendor.value);
  if (els.variant.value) p.set('variant', els.variant.value);
  for (const [k, v] of Object.entries(extra)) p.set(k, v);
  return p;
}

const contentURL = (id) => '/api/content?id=' + encodeURIComponent(id);
const thumbURL = (id) => '/api/thumb?id=' + encodeURIComponent(id);

async function fetchPage() {
  if (state.loading || state.done) return;
  state.loading = true;
  const p = query({ offset: state.offset, limit: PAGE });
  const res = await fetch('/api/assets?' + p.toString());
  const data = await res.json();
  if (!state.facetsLoaded) { populateFacets(data.facets); state.facetsLoaded = true; }
  state.total = data.total;
  for (const a of data.items) els.grid.appendChild(card(a));
  state.offset += data.items.length;
  if (data.items.length === 0 || state.offset >= data.total) state.done = true;
  els.count.textContent = state.total + (state.total === 1 ? ' asset' : ' assets');
  els.empty.hidden = state.total !== 0;
  state.loading = false;
}

function reset() {
  state.offset = 0; state.total = 0; state.done = false; state.loading = false;
  els.grid.replaceChildren();
  fetchPage();
}

function populateFacets(facets) {
  fillSelect(els.type, 'all types', facets.categories);
  fillSelect(els.vendor, 'all vendors', facets.vendors);
  fillSelect(els.variant, 'all variants', facets.variants, true);
}

function fillSelect(sel, allLabel, values, isVariant = false) {
  sel.replaceChildren();
  const none = new Option(allLabel, '');
  sel.appendChild(none);
  for (const f of values) {
    const label = (f.value === '' ? (isVariant ? '(loose / unknown)' : '(none)') : f.value) + ' (' + f.count + ')';
    sel.appendChild(new Option(label, f.value));
  }
}

// ---- cards ----

function card(a) {
  const el = document.createElement('div');
  el.className = 'card';
  el.tabIndex = 0;

  const thumb = document.createElement('div');
  thumb.className = 'thumb';
  thumb.appendChild(thumbContent(a));
  const badge = document.createElement('span');
  badge.className = 'ext-badge';
  badge.textContent = a.ext || a.category;
  thumb.appendChild(badge);

  const body = document.createElement('div');
  body.className = 'body';
  const name = document.createElement('div');
  name.className = 'name';
  name.textContent = a.name;
  const sub = document.createElement('div');
  sub.className = 'sub';
  sub.innerHTML = `<span class="cat">${a.category}</span><span>${a.variant || a.vendor}</span>`;
  const copy = document.createElement('button');
  copy.className = 'copy';
  copy.textContent = 'copy path';
  copy.addEventListener('click', (e) => { e.stopPropagation(); copyPath(a.copyPath, copy); });

  body.append(name, sub, copy);
  el.append(thumb, body);
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
    const holder = iconEl(a.category);
    modelThumbs.observe(holder, a);
    return holder;
  }
  return iconEl(a.category);
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
  material: '<circle cx="12" cy="12" r="9"/><path d="M4 12a8 8 0 0 1 16 0"/>',
  scene: '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 9h18"/>',
  animation: '<circle cx="12" cy="12" r="9"/><path d="M10 8l6 4-6 4z"/>',
  audio: '<path d="M4 9v6h4l5 4V5L8 9z"/><path d="M16 8a5 5 0 0 1 0 8"/>',
  script: '<path d="M8 4h9l3 3v13H8z"/><path d="M4 8v12h11"/>',
  doc: '<path d="M6 2h8l4 4v16H6z"/><path d="M14 2v4h4"/>',
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

async function loadModel(url, ext) {
  if (ext === 'glb' || ext === 'gltf') {
    const gltf = await new GLTFLoader(loadingManager).loadAsync(url);
    return gltf.scene;
  }
  const obj = await new FBXLoader(loadingManager).loadAsync(url);
  obj.traverse((o) => { if (o.isMesh) o.material = CLAY; });
  return obj;
}

function frame(object, camera, controls, offset = 1.5) {
  const box = new THREE.Box3().setFromObject(object);
  if (box.isEmpty()) return;
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
  return !new THREE.Box3().setFromObject(object).isEmpty();
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

// ---- lazy 3D thumbnails: one shared renderer, sequential queue, cached ----

class ModelThumbnails {
  constructor(size = 220) {
    this.size = size;
    this.cache = new Map();
    this.queue = Promise.resolve();
    this.observer = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (e.isIntersecting) {
          this.observer.unobserve(e.target);
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
    }
  }
  async build(asset) {
    try {
      this.ensureRenderer();
      const obj = await loadModel(contentURL(asset.id), asset.ext);
      if (!isRenderable(obj)) { dispose(obj); return null; }
      this.scene.add(obj);
      frame(obj, this.camera, null);
      this.renderer.render(this.scene, this.camera);
      const dataURL = this.renderer.domElement.toDataURL('image/png');
      this.scene.remove(obj);
      dispose(obj);
      return dataURL;
    } catch (e) {
      return null;
    }
  }
}
const modelThumbs = new ModelThumbnails();

// ---- lightbox ----

const lb = {
  root: document.getElementById('lightbox'),
  view: document.getElementById('lb-view'),
  name: document.getElementById('lb-name'),
  fields: document.getElementById('lb-fields'),
  path: document.getElementById('lb-path'),
  copybtn: document.getElementById('lb-copybtn'),
};
let activeViewer = null;

function openLightbox(a) {
  lb.name.textContent = a.name;
  lb.fields.innerHTML = [
    ['Category', a.category], ['Format', a.ext || '—'], ['Vendor', a.vendor],
    ['Pack', a.pack || '—'], ['Variant', a.variant || '(loose)'], ['Size', humanSize(a.size)],
    ['Path', a.relPath],
  ].map(([k, v]) => `<dt>${k}</dt><dd>${escapeHTML(v)}</dd>`).join('');
  lb.path.textContent = a.copyPath;
  lb.copybtn.onclick = () => copyPath(a.copyPath, lb.copybtn);

  lb.view.replaceChildren();
  if (a.category === 'model' && (a.ext === 'glb' || a.ext === 'gltf' || a.ext === 'fbx')) {
    activeViewer = startViewer(lb.view, a);
  } else if (a.thumb === 'image') {
    const img = new Image(); img.src = contentURL(a.id); lb.view.appendChild(img);
  } else if (a.thumb === 'preview') {
    const img = new Image(); img.src = thumbURL(a.id); lb.view.appendChild(img);
  } else {
    lb.view.appendChild(iconEl(a.category));
  }
  lb.root.hidden = false;
}

function closeLightbox() {
  lb.root.hidden = true;
  if (activeViewer) { activeViewer.stop(); activeViewer = null; }
  lb.view.replaceChildren();
}

function startViewer(container, asset) {
  const w = container.clientWidth || 600, h = container.clientHeight || 500;
  const renderer = new THREE.WebGLRenderer({ antialias: true });
  renderer.setSize(w, h);
  renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
  renderer.setClearColor(0x14161d, 1);
  container.appendChild(renderer.domElement);
  const scene = new THREE.Scene();
  scene.add(new THREE.HemisphereLight(0xffffff, 0x2a2c33, 3.0));
  const dir = new THREE.DirectionalLight(0xffffff, 2.4); dir.position.set(4, 6, 5); scene.add(dir);
  const fill = new THREE.DirectionalLight(0xffffff, 1.0); fill.position.set(-4, 2, -3); scene.add(fill);
  const camera = new THREE.PerspectiveCamera(45, w / h, 0.1, 5000);
  const controls = new OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;

  let raf = 0, obj = null, stopped = false;
  const showPlaceholder = (text) => {
    cancelAnimationFrame(raf);
    renderer.domElement.remove();
    const box = document.createElement('div');
    box.className = 'lb-placeholder';
    box.appendChild(iconEl(asset.category));
    if (text) { const p = document.createElement('p'); p.textContent = text; box.appendChild(p); }
    container.appendChild(box);
  };
  loadModel(contentURL(asset.id), asset.ext).then((o) => {
    if (stopped) { dispose(o); return; }
    if (!isRenderable(o)) { dispose(o); showPlaceholder('No mesh to preview (animation / data file).'); return; }
    obj = o; scene.add(o); frame(o, camera, controls);
  }).catch(() => showPlaceholder('Could not load this model.'));
  const onResize = () => {
    const nw = container.clientWidth, nh = container.clientHeight;
    if (nw && nh) { renderer.setSize(nw, nh); camera.aspect = nw / nh; camera.updateProjectionMatrix(); }
  };
  window.addEventListener('resize', onResize);
  const loop = () => { raf = requestAnimationFrame(loop); controls.update(); renderer.render(scene, camera); };
  loop();

  return {
    stop() {
      stopped = true;
      cancelAnimationFrame(raf);
      window.removeEventListener('resize', onResize);
      controls.dispose();
      if (obj) dispose(obj);
      renderer.dispose();
      renderer.domElement.remove();
    },
  };
}

document.getElementById('lb-close').addEventListener('click', closeLightbox);
lb.root.querySelector('.lb-backdrop').addEventListener('click', closeLightbox);
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !lb.root.hidden) closeLightbox(); });

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
for (const s of [els.type, els.vendor, els.variant]) s.addEventListener('change', reset);

new IntersectionObserver((entries) => {
  if (entries.some((e) => e.isIntersecting)) fetchPage();
}, { rootMargin: '600px' }).observe(els.sentinel);

fetchPage();
