// Shared 3D pipeline — model loading, orientation, posing, retargeting, and rig
// matching — imported by both the main thread (app.js) and the OffscreenCanvas
// thumbnail worker. three is imported by absolute path so it resolves in a worker
// (which has no import map); it is the same file the document's import map points
// "three" at, so a single three instance is shared across both.
import * as THREE from '/static/vendor/three/three.module.min.js';
import { GLTFLoader } from '/static/vendor/three/jsm/loaders/GLTFLoader.js';
import { FBXLoader } from '/static/vendor/three/jsm/loaders/FBXLoader.js';

export const contentURL = (id) => '/api/content?id=' + encodeURIComponent(id);

// CharRegistry persists to localStorage on the main thread; a worker has none, so it
// falls back to an in-memory store (its rig cache then lasts the worker's lifetime).
const memStore = new Map();
const store = {
  get(k) { try { return typeof localStorage !== 'undefined' ? localStorage.getItem(k) : (memStore.has(k) ? memStore.get(k) : null); } catch { return memStore.has(k) ? memStore.get(k) : null; } },
  set(k, v) { try { if (typeof localStorage !== 'undefined') localStorage.setItem(k, v); else memStore.set(k, v); } catch { memStore.set(k, v); } },
};

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
  if (isFinite(tMin) && tMin > 1e-3) {
    // Clone each track's times before shifting. GLTFLoader shares one times buffer across
    // a clip's tracks (and across clips), so subtracting in place double-counts, drives the
    // shared array negative, and collapses durations to 0 (the NaN/0.00s scrubber on GLBs).
    for (const tr of clip.tracks) {
      const t = tr.times.slice();
      for (let i = 0; i < t.length; i++) t[i] -= tMin;
      tr.times = t;
    }
    clip.resetDuration();
  }
  return trimStaticTail(clip);
}

// trimStaticTail shortens a clip that ends by holding a pose — some libraries pad every
// animation to a fixed slot length (e.g. Quaternius Turn90 finishes at ~1.1s then holds
// the final pose to 2.0s). It finds the last keyframe any track actually changes and, when
// the dead tail exceeds ~0.3s, sets the clip duration there so playback and looping stop
// on the real end. Records the original on userData.trimmedFrom so the UI can show it.
function trimStaticTail(clip) {
  let lastMotion = 0;
  for (const tr of clip.tracks) {
    const vs = tr.getValueSize();
    const v = tr.values, times = tr.times, n = times.length;
    if (n < 2) continue;
    // Per-keyframe change, and the track's peak change. A frame counts as motion only if
    // it exceeds 5% of the track's own peak, so imperceptible end-of-clip jitter on one
    // bone doesn't keep the whole clip from trimming.
    let peak = 0;
    const chg = new Array(n).fill(0);
    for (let i = 1; i < n; i++) {
      let d = 0;
      for (let k = 0; k < vs; k++) d += Math.abs(v[i * vs + k] - v[(i - 1) * vs + k]);
      chg[i] = d;
      if (d > peak) peak = d;
    }
    if (peak < 1e-3) continue; // this track never really moves
    const thresh = peak * 0.05;
    for (let i = n - 1; i > 0; i--) {
      if (chg[i] > thresh) { if (times[i] > lastMotion) lastMotion = times[i]; break; }
    }
  }
  if (lastMotion > 0 && clip.duration - lastMotion > 0.3) {
    clip.userData = clip.userData || {};
    clip.userData.trimmedFrom = clip.duration;
    clip.duration = lastMotion;
  }
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

// clipsForAsset returns the animation clips to preview for an asset. A per-clip
// virtual asset (source.clip, from splitting a multi-animation model file like a
// Quaternius library) narrows to just that named clip; every other asset previews
// all of the file's clips. FBX prefixes a clip name with its take ("Armature|Walk"),
// so match the suffix too.
function clipsForAsset(obj, asset) {
  const all = obj.animations || [];
  const want = asset && asset.source && asset.source.clip;
  if (!want) return all;
  const hit = all.filter((c) => c.name === want || c.name.endsWith('|' + want));
  return hit.length ? hit : all;
}

// loadRMClips loads an animation's root-motion (travel) sibling file and returns its
// matching clips — the same clip name as the card. The RM and in-place variants share
// a skeleton, so the clips play on the already-loaded body; only the AnimationClips are
// kept (they outlive disposing the source object). Null when the asset has no sibling.
async function loadRMClips(asset) {
  if (!asset || !asset.rootMotionId) return null;
  try {
    const rmObj = await loadModel(contentURL(asset.rootMotionId), asset.ext);
    const cs = clipsForAsset(rmObj, asset);
    dispose(rmObj);
    return cs.length ? cs : null;
  } catch { return null; }
}

// hasBakedMotion detects a file that itself carries baked root motion (an _RM file
// opened without a separate in-place sibling), so the toggle can still strip it in
// place. Matches the "_RM" token bounded by _, space, brackets, dot, or ends.
function hasBakedMotion(name) {
  return /(?:^|[_\s[])rm(?:[_\s\].]|$)/i.test(name || '');
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
  const meshBox = new THREE.Box3().setFromObject(object);
  if (bones < 2 || box.isEmpty()) return meshBox;
  const boneSpan = box.getSize(_posedV).length();
  const meshSpan = meshBox.isEmpty() ? 0 : meshBox.getSize(new THREE.Vector3()).length();
  // Some rigs keep every bone at the armature origin at bind (they skin the mesh through
  // bind matrices only), so the bone box collapses to a point; frame the mesh instead
  // when the bones don't span it.
  if (meshSpan > 0 && boneSpan < meshSpan * 0.25) return meshBox;
  box.expandByScalar(boneSpan * 0.06 || 0);
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
  list() { try { return JSON.parse(store.get(this.key)) || []; } catch { return []; } },
  save(l) { try { store.set(this.key, JSON.stringify(l.slice(0, 40))); } catch { /* quota */ } },
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

export {
  loadModel, normalizeClip, boneNames, clipBones, clipsForAsset, loadRMClips, hasBakedMotion,
  coversBones, posedBox, frameBox, isRenderable, captureRootRest, uprightRig, prepareClipRig,
  cloneRig, poseAt, retargetedFor, stripRootMotion, dispose, CharRegistry, CLAY, _posedV,
};
