import { test, expect } from '@playwright/test';
// @ts-ignore three ships without types in this package
import * as THREE from 'three';
// @ts-ignore three ships without types in this package
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import { createCameraRig, frameFor, type FrameBounds, type FrameMode } from '../src/plant-camera';
import { layout } from '../src/plant-model';

const modes: FrameMode[] = ['plant', 'line', 'station'];
const plant: FrameBounds = {minX:0, maxX:25, minZ:0, maxZ:40};
const line: FrameBounds = {minX:0, maxX:25, minZ:10, maxZ:16};
const station: FrameBounds = {minX:10, maxX:10, minZ:16, maxZ:16};

function cameraAt(aspect:number, pose:ReturnType<typeof frameFor>) {
    const camera = new THREE.PerspectiveCamera(42, aspect, 0.1, 5000);
    camera.position.set(pose.position.x, pose.position.y, pose.position.z);
    camera.lookAt(pose.target.x, pose.target.y, pose.target.z);
    camera.updateMatrixWorld();
    return new THREE.Frustum().setFromProjectionMatrix(new THREE.Matrix4().multiplyMatrices(camera.projectionMatrix, camera.matrixWorldInverse));
}
const dist = (p:{position:any;target:any}) => Math.hypot(p.position.x-p.target.x, p.position.y-p.target.y, p.position.z-p.target.z);

test('WIS-711 frameFor keeps padded, full-height bounds inside the frustum', () => {
    for (const aspect of [0.5, 1, 2]) for (const mode of modes) for (const b of [plant, line, station]) {
        const frustum = cameraAt(aspect, frameFor(b, aspect, mode));
        for (const x of [b.minX-3, b.maxX+3]) for (const z of [b.minZ-3, b.maxZ+3]) for (const y of [0, 3])
            expect(frustum.containsPoint(new THREE.Vector3(x, y, z)), `${mode} aspect ${aspect} corner ${x},${y},${z}`).toBe(true);
    }
    expect(() => frameFor(plant, 1, 'aisle' as FrameMode)).toThrow();
});

test('WIS-711 station framing is closer than line, which is closer than plant', () => {
    for (const aspect of [0.5, 1, 2]) {
        expect(dist(frameFor(station, aspect, 'station'))).toBeLessThan(dist(frameFor(line, aspect, 'line')));
        expect(dist(frameFor(line, aspect, 'line'))).toBeLessThan(dist(frameFor(plant, aspect, 'plant')));
        for (const b of [plant, line, station]) {   // the mode alone also orders distance for the same bounds
            expect(dist(frameFor(b, aspect, 'station'))).toBeLessThanOrEqual(dist(frameFor(b, aspect, 'line')));
            expect(dist(frameFor(b, aspect, 'line'))).toBeLessThanOrEqual(dist(frameFor(b, aspect, 'plant')));
        }
    }
});

function fakeFrames() {
    const queue = new Map<number, (t:number)=>void>(); let id = 0;
    (globalThis as any).requestAnimationFrame = (cb:(t:number)=>void) => { queue.set(++id, cb); return id; };
    (globalThis as any).cancelAnimationFrame = (n:number) => queue.delete(n);
    return {queue, run(t:number) { const cbs = [...queue.values()]; queue.clear(); for (const cb of cbs) cb(t); }};
}
function rig(reducedMotion:boolean) {
    const camera = new THREE.PerspectiveCamera(42, 1.6, 0.1, 5000);
    camera.position.set(0, 50, 50);
    const controls = new OrbitControls(camera);
    controls.maxDistance = 5000;
    let changes = 0;
    const r = createCameraRig(camera, controls, {reducedMotion, onChange: () => changes++});
    return {camera, controls, r, changes: () => changes};
}
const near = (v:any, p:{x:number;y:number;z:number}) => expect(v.distanceTo(new THREE.Vector3(p.x, p.y, p.z))).toBeLessThan(1e-6);

test('WIS-711 reduced motion lands in one call without animation frames', () => {
    const frames = fakeFrames();
    const {camera, controls, r, changes} = rig(true);
    r.frame(line, 'line');
    expect(frames.queue.size).toBe(0);
    const pose = frameFor(line, 1.6, 'line');
    near(camera.position, pose.position); near(controls.target, pose.target);
    expect(changes()).toBe(1);
});

test('WIS-711 motion eases within 400 ms and a second frame() cancels the first', () => {
    const frames = fakeFrames();
    const {camera, controls, r} = rig(false);
    r.frame(plant, 'plant');
    expect(frames.queue.size).toBe(1);
    const stale = [...frames.queue.values()][0];
    frames.run(1000); frames.run(1200);                       // first transition half-way
    r.frame(station, 'station');                              // cancels it
    const mid = camera.position.clone();
    stale(1300);                                              // a stale callback must not move the camera
    expect(camera.position.distanceTo(mid)).toBe(0);
    frames.run(2000); frames.run(2400);                       // 400 ms later the second transition has landed
    const pose = frameFor(station, 1.6, 'station');
    near(camera.position, pose.position); near(controls.target, pose.target);
    expect(frames.queue.size).toBe(0);
    expect(mid.distanceTo(camera.position)).toBeGreaterThan(1);
    r.frame(plant, 'plant'); r.dispose();                     // dispose cancels too
    expect(frames.queue.size).toBe(0);
});

test('WIS-711 a user drag cancels a running transition', () => {
    const frames = fakeFrames();
    const {r, controls} = rig(false);
    r.frame(plant, 'plant');
    controls.dispatchEvent({type:'start'});
    expect(frames.queue.size).toBe(0);
});

function fakeElement() {
    const attrs = new Map<string,string>(); const listeners = new Set<(e:any)=>void>();
    return {listeners, el: {
        tagName:'DIV', hasAttribute:(k:string)=>attrs.has(k), setAttribute:(k:string,v:string)=>attrs.set(k,v), getAttribute:(k:string)=>attrs.get(k),
        addEventListener:(_:string,f:(e:any)=>void)=>listeners.add(f), removeEventListener:(_:string,f:(e:any)=>void)=>listeners.delete(f),
    }};
}

test('WIS-711 keyboard maps keys to handlers and camera moves, and ignores form fields', () => {
    fakeFrames();
    const {camera, controls, r} = rig(true);
    r.frame(line, 'line');
    const {el, listeners} = fakeElement();
    const calls: string[] = [];
    const detach = r.attachKeyboard(el as unknown as HTMLElement, {next:()=>calls.push('next'), prev:()=>calls.push('prev'), activate:()=>calls.push('activate'), presetPlant:()=>calls.push('plant')});
    expect(el.getAttribute('tabindex')).toBe('0');
    const press = (key:string, target:any = el, extra = {}) => { let prevented = false; for (const f of listeners) f({key, target, preventDefault:()=>{prevented=true;}, ...extra}); return prevented; };

    for (const k of [']', '[', 'Enter', 'Home']) expect(press(k)).toBe(true);
    expect(calls).toEqual(['next', 'prev', 'activate', 'plant']);

    const d0 = camera.position.distanceTo(controls.target);
    press('+'); expect(camera.position.distanceTo(controls.target)).toBeCloseTo(d0*0.8, 6);
    press('_', el, {shiftKey:true}); expect(camera.position.distanceTo(controls.target)).toBeCloseTo(d0, 6);
    const t0 = controls.target.clone();
    press('ArrowUp'); expect(controls.target.distanceTo(t0)).toBeGreaterThan(0);
    expect(controls.target.y).toBe(0);

    for (const target of [{tagName:'INPUT'}, {tagName:'TEXTAREA'}, {tagName:'SELECT'}, {tagName:'DIV', isContentEditable:true}])
        expect(press(']', target)).toBe(false);
    expect(press(']', el, {ctrlKey:true})).toBe(false);
    expect(press('a')).toBe(false);
    expect(calls).toHaveLength(4);
    detach();
    expect(listeners.size).toBe(0);
});

test('WIS-711 highlight adds a shape cue, clears it and dispose frees its resources', () => {
    const {r} = rig(true);
    const scene = new THREE.Scene();
    const group = new THREE.Group(); group.position.set(10, 0, 16);
    const body = new THREE.Mesh(new THREE.BoxGeometry(4.5, 3, 5.2)); body.position.y = 1.5; group.add(body); scene.add(group);
    const base = scene.children.length;
    r.highlight(group, scene);
    const added = scene.children.slice(base);
    expect(added.map((o:any) => o.type).sort()).toEqual(['LineSegments', 'Mesh']);
    const ring = added.find((o:any) => o.type === 'Mesh');
    expect(ring.position.x).toBeCloseTo(10); expect(ring.position.z).toBeCloseTo(16);
    expect(ring.scale.x).toBeLessThanOrEqual(2.25);                       // inside the footprint, no neighbour overlap
    r.highlight(null, scene);
    expect(scene.children.length).toBe(base);
    r.highlight(group, scene);
    const freed: string[] = [];
    for (const o of added) for (const res of [o.geometry, o.material]) res.addEventListener('dispose', () => freed.push(res.type));
    r.dispose();
    expect(freed).toHaveLength(4);
    expect(scene.children.length).toBe(base);
});

test('WIS-711 plant preset fills at least 70 percent of the narrower viewport axis',()=>{
    const pose=frameFor(plant,1.6,'plant');
    const camera=new THREE.PerspectiveCamera(42,1.6,0.1,5000);
    camera.position.copy(pose.position);camera.lookAt(pose.target.x,pose.target.y,pose.target.z);camera.updateMatrixWorld();
    const points=[];
    for(const x of [plant.minX-3,plant.maxX+3])for(const z of [plant.minZ-3,plant.maxZ+3])for(const y of [0,5])points.push(new THREE.Vector3(x,y,z).project(camera));
    expect((Math.max(...points.map(p=>p.y))-Math.min(...points.map(p=>p.y)))/2).toBeGreaterThanOrEqual(0.7);
});

test('WIS-711 sparse line bays fill the plant viewport without fitting empty corners',()=>{
    const positions=layout(Array.from({length:50},(_,i)=>({id:String(i),line:String(Math.floor(i/8))})));
    const bounds={minX:Math.min(...positions.map(p=>p.x)),maxX:Math.max(...positions.map(p=>p.x)),minZ:Math.min(...positions.map(p=>p.z)),maxZ:Math.max(...positions.map(p=>p.z)),positions};
    const pose=frameFor(bounds,1.6,'plant'),camera=new THREE.PerspectiveCamera(42,1.6,0.1,5000);
    camera.position.copy(pose.position);camera.lookAt(pose.target.x,pose.target.y,pose.target.z);camera.updateMatrixWorld();
    const points=positions.flatMap(p=>[-2.25,2.25].flatMap(x=>[-2.6,2.6].map(z=>new THREE.Vector3(p.x+x,0,p.z+z).project(camera))));
    const width=(Math.max(...points.map(p=>p.x))-Math.min(...points.map(p=>p.x)))/2;
    const height=(Math.max(...points.map(p=>p.y))-Math.min(...points.map(p=>p.y)))/2;
    expect(Math.max(width,height)).toBeGreaterThanOrEqual(0.8);
    for(const p of points){expect(Math.abs(p.x)).toBeLessThan(1);expect(Math.abs(p.y)).toBeLessThan(1);}
});
