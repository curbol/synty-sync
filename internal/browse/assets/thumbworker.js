// Thumbnail worker: parses and renders every grid thumbnail off the main thread onto
// an OffscreenCanvas, returning a PNG blob. The main thread never parses a model or
// runs WebGL for the grid, so a 65 MB file can't freeze the page. The build logic
// mirrors the lightbox pipeline via the shared scene.js module.
// FBXLoader decodes embedded textures through DOM <img> elements, which a worker has
// no document for. Thumbnails render textureless clay (loadModel overrides every
// material), so stand in a no-op image that reports "loaded" immediately — letting such
// FBX parse in the worker. Runs at module init, before any job parses an FBX.
if (typeof document === 'undefined') {
  const fakeImage = () => {
    const on = {};
    return {
      style: {}, width: 1, height: 1, complete: true,
      addEventListener(t, f) { on[t] = f; },
      removeEventListener(t) { delete on[t]; },
      set src(v) { this._src = v; queueMicrotask(() => on.load && on.load()); },
      get src() { return this._src; },
    };
  };
  globalThis.document = { createElementNS: () => fakeImage(), createElement: () => fakeImage() };
}

import * as THREE from '/static/vendor/three/three.module.min.js';
import {
  loadModel, clipsForAsset, prepareClipRig, poseAt, stripRootMotion, isRenderable,
  captureRootRest, cloneRig, retargetedFor, dispose, CharRegistry, frameBox, contentURL,
} from '/static/scene.js';

const SIZE = 220;
const canvas = new OffscreenCanvas(SIZE, SIZE);
let renderer, scene, camera;

// Create the GL context lazily on the first job, not at worker load: creating it while
// the page is still initializing races the main thread and can fail (swiftshader's
// "BindToCurrentSequence failed"). Retry a few times to ride out a transient failure.
async function ensureRenderer() {
  if (renderer) return;
  for (let attempt = 0; ; attempt++) {
    try {
      renderer = new THREE.WebGLRenderer({ canvas, antialias: true, alpha: true });
      renderer.setSize(SIZE, SIZE, false);
      break;
    } catch (e) {
      if (attempt >= 4) throw e;
      await new Promise((r) => setTimeout(r, 150));
    }
  }
  scene = new THREE.Scene();
  scene.add(new THREE.HemisphereLight(0xffffff, 0x33343a, 2.6));
  const dir = new THREE.DirectionalLight(0xffffff, 2.2);
  dir.position.set(4, 6, 5);
  scene.add(dir);
  camera = new THREE.PerspectiveCamera(45, 1, 0.1, 1000);
}

const files = new Map(); // file path -> parsed+oriented file, reused across its split clips
const rigs = new Map();  // matched character id -> loaded rig, cloned per clip

function snap(object, box) {
  scene.add(object);
  frameBox(box, camera, null);
  renderer.render(scene, camera);
  scene.remove(object);
}

function rootBoneName(root) {
  let name = null;
  root.traverse((n) => { if (n.isBone && !name && (!n.parent || !n.parent.isBone)) name = n.name; });
  return name;
}

// build renders one thumbnail to the canvas and resolves true, or false when there is
// nothing to draw (a mesh-less clip with no matching rig).
async function build(asset) {
  const key = asset.source && asset.source.clip && asset.source.filePath;
  return key ? await buildShared(asset, key) : await buildStandalone(asset);
}

async function buildStandalone(asset) {
  const obj = await loadModel(contentURL(asset.id), asset.ext);
  const rootRest = captureRootRest(obj);
  if (isRenderable(obj)) {
    const cs = clipsForAsset(obj, asset);
    const refBox = prepareClipRig(obj, null);
    if (cs.length) poseAt(obj, stripRootMotion(cs[0], rootBoneName(obj), obj.userData.upAxis));
    snap(obj, refBox);
    dispose(obj);
    return true;
  }
  const clips = clipsForAsset(obj, asset);
  dispose(obj);
  return clips.length ? await buildPosed(clips[0], asset.vendor, rootRest) : false;
}

async function buildShared(asset, key) {
  let pending = files.get(key);
  if (!pending) {
    pending = loadSharedFile(asset);
    files.set(key, pending);
    evictFiles();
  }
  const ctx = await pending;
  if (!ctx) return false;
  if (ctx.renderable) {
    const cs = clipsForAsset(ctx.obj, asset);
    const mixer = cs.length ? poseAt(ctx.obj, stripRootMotion(cs[0], ctx.rootBoneName, ctx.upAxis)) : null;
    snap(ctx.obj, ctx.refBox);
    if (mixer) mixer.stopAllAction();
    return true;
  }
  const cs = clipsForAsset(ctx.obj, asset);
  return cs.length ? await buildPosed(cs[0], asset.vendor, ctx.rootRest) : false;
}

async function loadSharedFile(asset) {
  const obj = await loadModel(contentURL(asset.id), asset.ext);
  if (isRenderable(obj)) {
    const refBox = prepareClipRig(obj, null);
    return { renderable: true, obj, refBox, upAxis: obj.userData.upAxis, rootBoneName: rootBoneName(obj) };
  }
  return { renderable: false, obj, rootRest: captureRootRest(obj) };
}

function evictFiles() {
  const CAP = 6;
  while (files.size > CAP) {
    const oldest = files.keys().next().value;
    const pending = files.get(oldest);
    files.delete(oldest);
    Promise.resolve(pending).then((ctx) => { if (ctx && ctx.obj) dispose(ctx.obj); }).catch(() => {});
  }
}

async function rigFor(clip, vendor) {
  await CharRegistry.seed();
  let m = CharRegistry.match(clipBonesOf(clip), vendor);
  if (!m) { await CharRegistry.discoverForVendor(vendor, clipBonesOf(clip)); m = CharRegistry.match(clipBonesOf(clip), vendor); }
  if (!m) return null;
  if (!rigs.has(m.id)) {
    const rig = await loadModel(contentURL(m.id), m.ext)
      .then((r) => (isRenderable(r) ? r : (dispose(r), null)))
      .catch(() => null);
    rigs.set(m.id, rig);
  }
  return rigs.get(m.id);
}

function clipBonesOf(clip) {
  return [...new Set(clip.tracks.map((t) => t.name.split('.')[0]))];
}

async function buildPosed(clip, vendor, rootRest) {
  const template = await rigFor(clip, vendor);
  if (!template) return false;
  const rig = cloneRig(template);
  const refBox = prepareClipRig(rig, vendor === 'synty' ? null : rootRest);
  const posed = stripRootMotion(await retargetedFor(clip, vendor, rig), rootBoneName(rig), rig.userData.upAxis);
  const mixer = poseAt(rig, posed);
  snap(rig, refBox);
  mixer.stopAllAction();
  dispose(rig);
  return true;
}

// Serialize jobs: one render on the shared canvas at a time, converted to a blob before
// the next job overwrites the canvas.
let queue = Promise.resolve();
self.onmessage = (e) => {
  const { id, asset } = e.data;
  queue = queue.then(async () => {
    try {
      await ensureRenderer();
      if (!(await build(asset))) { self.postMessage({ id, blob: null }); return; }
      const blob = await canvas.convertToBlob({ type: 'image/png' });
      self.postMessage({ id, blob });
    } catch {
      self.postMessage({ id, blob: null });
    }
  });
};
