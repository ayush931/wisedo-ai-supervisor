// Pure unit spec: builds the kit in Node, no backend or browser needed.
import { test, expect } from '@playwright/test';
import { Box3 } from 'three';
import { createPlantKit } from '../src/plant-machines.js';

const ARCHETYPES = ['press', 'cnc', 'welding', 'assembly', 'inspection', 'packing', 'material', 'generic'] as const;
const MARKERS = ['case', 'investigation', 'proposal_awaiting_approval', 'task', 'correction'] as const;

test('WIS-711 F5 software kit uses Lambert materials',()=>{
    const kit=createPlantKit({software:true});
    for(const group of [kit.machine('press'),kit.person(),kit.marker('case')])for(const mesh of meshes(group))expect(mesh.material.type).toBe('MeshLambertMaterial');
    kit.dispose();
});

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Obj = any;
function meshes(group: Obj): Obj[] { const out: Obj[] = []; group.traverse((o: Obj) => { if (o.isMesh) out.push(o); }); return out; }
function triangles(group: Obj): number {
    return meshes(group).reduce((n, m) => n + (m.geometry.index ? m.geometry.index.count : m.geometry.attributes.position.count) / 3, 0);
}
// Shape signature: the geometry types of a group's meshes, independent of colour.
const signature = (group: Obj) => meshes(group).map(m => m.geometry.type).sort().join('+');

test('WIS-711 every archetype builds a tagged machine within mesh and triangle budgets', () => {
    const kit = createPlantKit();
    const report: Record<string, string> = {};
    for (const a of ARCHETYPES) {
        const g: Obj = kit.machine(a);
        expect(g.userData).toEqual({kind: 'machine', archetype: a});
        expect(g.position.toArray()).toEqual([0, 0, 0]);
        const n = meshes(g).length, t = triangles(g);
        expect(n).toBeGreaterThan(4);
        expect(n).toBeLessThanOrEqual(40);
        expect(t).toBeLessThanOrEqual(3000);
        report[a] = `${n} meshes / ${t} tris`;
    }
    console.log(JSON.stringify(report));
    kit.dispose();
});

test('WIS-711 status strip uses the caller colour and unknown archetypes fall back to generic', () => {
    const kit = createPlantKit();
    const colours = (g: Obj) => meshes(g).map(m => m.material.color.getHex());
    expect(colours(kit.machine('press', {statusColor: 0x208775}))).toContain(0x208775);
    expect(colours(kit.machine('press'))).not.toContain(0x208775);
    const unknown: Obj = kit.machine('compressor');
    expect(unknown.userData).toEqual({kind: 'machine', archetype: 'generic'});
    expect(signature(unknown)).toBe(signature(kit.machine('generic')));
    kit.dispose();
});

test('WIS-711 machines, people and markers share geometries and one material per colour', () => {
    const kit = createPlantKit();
    const a: Obj = kit.machine('cnc', {statusColor: 0xc27c31}), b: Obj = kit.machine('cnc', {statusColor: 0xc27c31});
    const [ma, mb] = [meshes(a), meshes(b)];
    expect(ma.length).toBe(mb.length);
    ma.forEach((m, i) => { expect(mb[i].material).toBe(m.material); expect(mb[i].geometry).toBe(m.geometry); });
    // Across everything the kit builds, a colour maps to exactly one material instance.
    const all = [a, b, ...ARCHETYPES.map(x => kit.machine(x, {statusColor: 0xc27c31})), kit.person(), kit.person({role: 'supervisor'}), ...MARKERS.map(k => kit.marker(k))].flatMap(meshes);
    const byColour = new Map<number, Set<unknown>>();
    for (const m of all) byColour.set(m.material.color.getHex(), (byColour.get(m.material.color.getHex()) ?? new Set()).add(m.material));
    for (const set of byColour.values()) expect(set.size).toBe(1);
    expect(new Set(all.map(m => m.geometry)).size).toBeLessThanOrEqual(6);
    kit.dispose();
});

test('WIS-711 the same calls give the same geometry', () => {
    const shape = (g: Obj) => meshes(g).map(m => { m.updateMatrixWorld(true); return [m.geometry.type, m.matrixWorld.elements.map((e: number) => e.toFixed(6))]; });
    const [k1, k2] = [createPlantKit(), createPlantKit()];
    for (const a of ARCHETYPES) expect(shape(k2.machine(a))).toEqual(shape(k1.machine(a)));
    k1.dispose(); k2.dispose();
});

test('WIS-711 person is about 1.7 units tall and the vest differs by role', () => {
    const kit = createPlantKit();
    const op: Obj = kit.person(), sup: Obj = kit.person({role: 'supervisor'});
    expect(op.userData).toEqual({kind: 'person', role: 'operator'});
    expect(sup.userData).toEqual({kind: 'person', role: 'supervisor'});
    const height = (g: Obj) => { let top = 0; for (const m of meshes(g)) { m.geometry.computeBoundingBox(); m.updateMatrixWorld(true); top = Math.max(top, m.geometry.boundingBox.clone().applyMatrix4(m.matrixWorld).max.y); } return top; };
    expect(height(op)).toBeGreaterThan(1.6);
    expect(height(op)).toBeLessThan(1.8);
    const colours = (g: Obj) => new Set(meshes(g).map(m => m.material.color.getHex()));
    expect([...colours(op)].sort()).not.toEqual([...colours(sup)].sort());
    kit.dispose();
});

test('WIS-711 marker kinds differ in shape, not only colour, and float above the machine', () => {
    const kit = createPlantKit();
    const groups = MARKERS.map(k => kit.marker(k) as Obj);
    groups.forEach((g, i) => expect(g.userData).toEqual({kind: 'marker', marker: MARKERS[i]}));
    expect(new Set(groups.map(signature)).size).toBe(MARKERS.length);
    const box = (g: Obj) => { g.updateMatrixWorld(true); const b = new Box3().setFromObject(g); return b; };
    const tallestMachine = Math.max(...ARCHETYPES.map(a => box(kit.machine(a)).max.y));
    for (const g of groups) expect(box(g).min.y).toBeGreaterThan(3.5); // press crown top; markers float clear of it
    expect(tallestMachine).toBeLessThan(3.8);
    expect(() => kit.marker('unknown' as never)).toThrow(/unknown marker kind/);
    kit.dispose();
});

test('WIS-711 dispose frees every geometry and material the kit handed out', () => {
    const kit = createPlantKit();
    const built = [...ARCHETYPES.map(a => kit.machine(a, {statusColor: 0x208775})), kit.person(), kit.person({role: 'supervisor'}), ...MARKERS.map(k => kit.marker(k))].flatMap(meshes);
    const resources = new Set<Obj>(built.flatMap(m => [m.geometry, m.material]));
    const disposed = new Set<Obj>();
    for (const r of resources) { const orig = r.dispose.bind(r); r.dispose = () => { disposed.add(r); orig(); }; }
    kit.dispose();
    expect([...resources].filter(r => !disposed.has(r))).toEqual([]);
});
