// Adapted from wiseDo Twin (layout2Scene) app/src/lib/viewport.ts at commit 928d0771
// Camera framing, keyboard navigation and an accessible selection marker for the plant view.
import * as THREE from 'three';

const FOV = 42;
const PAD = 3;        // half a station footprint (slab 4.5 x 5.2): bounds are station centres
const HEIGHT = 5;     // tallest station geometry, metres
const AZIMUTH = THREE.MathUtils.degToRad(31);
const MODES = {
    plant: {pitch: 42, margin: 1.025, minHalf: 3},
    line: {pitch: 40, margin: 1.02, minHalf: 3},
    station: {pitch: 38, margin: 1.015, minHalf: 3},
};
const DURATION = 400;
const EDITABLE = new Set(['INPUT', 'TEXTAREA', 'SELECT', 'BUTTON']);

/** Pure: the camera pose that keeps the padded bounds (0..HEIGHT m tall) inside the frustum. */
export function frameFor(bounds, aspect, mode, fov = FOV) {
    const m = MODES[mode];
    if (!m) throw new Error(`unknown frame mode ${mode}`);
    const cx = (bounds.minX + bounds.maxX) / 2, cz = (bounds.minZ + bounds.maxZ) / 2;
    const hx = Math.max((bounds.maxX - bounds.minX) / 2 + PAD, m.minHalf);
    const hz = Math.max((bounds.maxZ - bounds.minZ) / 2 + PAD, m.minHalf);
    const halfV = THREE.MathUtils.degToRad(fov) / 2;
    const halfH = Math.atan(Math.tan(halfV) * aspect);
    const pitch = THREE.MathUtils.degToRad(m.pitch);
    const direction = new THREE.Vector3(Math.sin(AZIMUTH)*Math.cos(pitch),Math.sin(pitch),Math.cos(AZIMUTH)*Math.cos(pitch));
    const right = new THREE.Vector3(Math.cos(AZIMUTH),0,-Math.sin(AZIMUTH));
    const up = new THREE.Vector3().crossVectors(direction,right);
    let fit = 0;
    // Sparse bays must not reserve room for unoccupied corners of the plant rectangle.
    const corners=bounds.positions?.length?bounds.positions.flatMap(p=>[-PAD,PAD].flatMap(x=>[-PAD,PAD].map(z=>({x:p.x-cx+x,z:p.z-cz+z})))):[-hx,hx].flatMap(x=>[-hz,hz].map(z=>({x,z})));
    for(const {x,z} of corners)for(const y of [0,HEIGHT]) {
        const corner = new THREE.Vector3(x,y,z);
        fit = Math.max(fit,corner.dot(direction)+Math.max(Math.abs(corner.dot(right))/Math.tan(halfH),Math.abs(corner.dot(up))/Math.tan(halfV)));
    }
    const distance = fit*m.margin;
    const flat = distance * Math.cos(pitch);
    return {
        target: {x: cx, y: 0, z: cz},
        position: {x: cx + flat * Math.sin(AZIMUTH), y: distance * Math.sin(pitch), z: cz + flat * Math.cos(AZIMUTH)},
    };
}

const ease = t => 1 - (1 - t) ** 3;

export function createCameraRig(camera, controls, opts) {
    const {reducedMotion, onChange} = opts;
    let raf = 0, generation = 0;
    const raf$ = cb => globalThis.requestAnimationFrame(cb);
    function cancel() {
        generation++;
        if (raf) globalThis.cancelAnimationFrame(raf);
        raf = 0;
    }
    function land(position, target) {
        camera.position.copy(position);
        controls.target.copy(target);
        controls.update();
        onChange();
    }
    function frame(bounds, mode) {
        cancel();
        const pose = frameFor(bounds, camera.aspect, mode, camera.fov);
        const toP = new THREE.Vector3().copy(pose.position), toT = new THREE.Vector3().copy(pose.target);
        if (reducedMotion) return land(toP, toT);
        const fromP = camera.position.clone(), fromT = controls.target.clone();
        const mine = generation;
        let start = null;
        const step = now => {
            if (mine !== generation) return;
            if (start === null) start = now;
            const t = Math.min(1, (now - start) / DURATION), k = ease(t);
            land(fromP.clone().lerp(toP, k), fromT.clone().lerp(toT, k));
            raf = t < 1 ? raf$(step) : 0;
        };
        raf = raf$(step);
    }
    // A user drag takes the camera back from any running transition.
    controls.addEventListener('start', cancel);

    function pan(dx, dz) {
        cancel();
        const forward = new THREE.Vector3().subVectors(controls.target, camera.position).setY(0);
        if (forward.lengthSq() === 0) forward.set(0, 0, -1);
        forward.normalize();
        const right = new THREE.Vector3(-forward.z, 0, forward.x);
        const step = camera.position.distanceTo(controls.target) * 0.1;
        const move = forward.multiplyScalar(dz * step).addScaledVector(right, dx * step);
        camera.position.add(move); controls.target.add(move);
        controls.update(); onChange();
    }
    function dolly(f) {
        cancel();
        const t = controls.target;
        const d = camera.position.distanceTo(t);
        const next = Math.min(controls.maxDistance, Math.max(controls.minDistance, d * f));
        camera.position.sub(t).setLength(next).add(t);
        controls.update(); onChange();
    }
    function attachKeyboard(el, handlers) {
        if (!el.hasAttribute('tabindex')) el.setAttribute('tabindex', '0');
        const onKey = e => {
            if (e.altKey || e.ctrlKey || e.metaKey) return;
            const target = e.target;
            if (target && target !== el && (EDITABLE.has(target.tagName) || target.isContentEditable)) return;
            const actions = {
                ArrowUp: () => pan(0, 1), ArrowDown: () => pan(0, -1),
                ArrowLeft: () => pan(-1, 0), ArrowRight: () => pan(1, 0),
                '+': () => dolly(0.8), '=': () => dolly(0.8), '-': () => dolly(1.25), '_': () => dolly(1.25),
                '[': handlers.prev, ']': handlers.next, Enter: handlers.activate, Home: handlers.presetPlant,
            };
            const action = actions[e.key];
            if (!action) return;
            e.preventDefault();
            action();
        };
        el.addEventListener('keydown', onKey);
        return () => el.removeEventListener('keydown', onKey);
    }

    // Selection is shown by shape, not colour alone: a ground ring and a raised box outline.
    const ringGeometry = new THREE.RingGeometry(0.92, 1, 64).rotateX(-Math.PI / 2);
    const boxGeometry = new THREE.BoxGeometry(1, 1, 1);
    const edgeGeometry = new THREE.EdgesGeometry(boxGeometry);
    boxGeometry.dispose();
    const ringMaterial = new THREE.MeshBasicMaterial({color: 0x10202b, side: THREE.DoubleSide, depthWrite: false});
    const edgeMaterial = new THREE.LineBasicMaterial({color: 0x10202b});
    const ring = new THREE.Mesh(ringGeometry, ringMaterial);
    const outline = new THREE.LineSegments(edgeGeometry, edgeMaterial);
    ring.name = 'plant-selection-ring'; outline.name = 'plant-selection-outline';
    ring.renderOrder = outline.renderOrder = 10;
    function highlight(group, scene) {
        ring.removeFromParent(); outline.removeFromParent();
        if (group) {
            const box = new THREE.Box3().setFromObject(group);
            const size = box.getSize(new THREE.Vector3()), centre = box.getCenter(new THREE.Vector3());
            // Inscribed in the footprint so it cannot overlap a neighbour; lifted above the floor stripe.
            const r = Math.min(size.x, size.z) / 2;
            ring.scale.set(r, 1, r); ring.position.set(centre.x, Math.max(box.min.y, 0) + 0.1, centre.z);
            outline.scale.set(size.x + 0.2, size.y + 0.3, size.z + 0.2);
            outline.position.set(centre.x, centre.y + 0.15, centre.z);
            scene.add(ring, outline);
        }
        onChange();
    }

    return {
        frame, attachKeyboard, highlight,
        dispose() {
            cancel();
            controls.removeEventListener('start', cancel);
            ring.removeFromParent(); outline.removeFromParent();
            ringGeometry.dispose(); edgeGeometry.dispose(); ringMaterial.dispose(); edgeMaterial.dispose();
        },
    };
}
