// Procedural, low-poly plant props for the 3-D plant view: machines per archetype, people and
// AI-work markers. Everything is built from a few shared unit geometries scaled per mesh, and one
// material per colour, so a plant of hundreds of stations costs a handful of GPU resources.
// No textures, no loaders, no randomness: the same calls give the same scene graph.
import * as THREE from 'three';

const STEEL = 0x526f80, PALE = 0xb7c9d2, DARK = 0x293e4b, GLASS = 0x6f97ab, CARD = 0xba9466,
    WOOD = 0x9c7a52, ROBOT = 0xc49a35, SAFETY = 0xd9b13b, GRANITE = 0x5a6166, SKIN = 0xcbb997,
    RACK = 0x2f5d8a, BEAM = 0xd55e00, NEUTRAL = 0x87949e;
// Okabe-Ito colours, each paired with a distinct shape so colour is never the only cue.
const MARKER_COLOR = {case: 0xd55e00, investigation: 0x0072b2, proposal_awaiting_approval: 0xe69f00, task: 0x009e73, correction: 0xcc79a7};
const VEST = {operator: 0xe69f00, supervisor: 0x0072b2};
const ARCHETYPES = ['press', 'cnc', 'welding', 'assembly', 'inspection', 'packing', 'material', 'generic'];

export function createPlantKit({software=false}={}) {
    const geo = {
        box: new THREE.BoxGeometry(1, 1, 1),
        cyl: new THREE.CylinderGeometry(0.5, 0.5, 1, 12),
        sphere: new THREE.SphereGeometry(0.5, 10, 8),
        cone: new THREE.ConeGeometry(0.5, 1, 12),
        octa: new THREE.OctahedronGeometry(0.5),
        torus: new THREE.TorusGeometry(0.4, 0.08, 8, 20),
    };
    const materials = new Map();
    const mat = color => {
        if (!materials.has(color)) materials.set(color, software?new THREE.MeshLambertMaterial({color}):new THREE.MeshStandardMaterial({color, roughness: 0.8, metalness: 0.05}));
        return materials.get(color);
    };
    // One mesh: shared geometry, scaled and placed. Rotation is optional Euler radians.
    function add(parent, g, color, [x, y, z], [sx, sy, sz], [rx, ry, rz] = [0, 0, 0]) {
        const mesh = new THREE.Mesh(geo[g], mat(color));
        mesh.position.set(x, y, z); mesh.scale.set(sx, sy, sz); mesh.rotation.set(rx, ry, rz);
        parent.add(mesh); return mesh;
    }
    const joint = (parent, [x, y, z], rz) => { const j = new THREE.Group(); j.position.set(x, y, z); j.rotation.z = rz; parent.add(j); return j; };

    const builders = {
        press(g) {
            add(g, 'box', STEEL, [0, 0.35, 0], [2.4, 0.7, 1.5]);                 // base
            add(g, 'box', PALE, [0, 0.76, 0], [2.0, 0.12, 1.2]);                 // bolster
            for (const x of [-1.05, 1.05]) add(g, 'box', STEEL, [x, 1.85, 0], [0.35, 2.3, 1.2]); // columns
            add(g, 'box', STEEL, [0, 3.2, 0], [2.8, 0.6, 1.6]);                  // crown
            add(g, 'cyl', DARK, [0, 2.6, 0], [0.3, 0.6, 0.3]);                   // ram rod
            add(g, 'box', PALE, [0, 2.05, 0], [1.6, 0.5, 1.0]);                  // ram
            add(g, 'cyl', DARK, [1.45, 3.2, 0], [1.0, 0.2, 1.0], [0, 0, Math.PI / 2]); // flywheel
            add(g, 'box', DARK, [-1.45, 1.2, 0.55], [0.3, 0.5, 0.2]);            // two-hand control box
        },
        cnc(g) {
            add(g, 'box', DARK, [0, 0.1, 0], [2.7, 0.2, 1.7]);                   // plinth
            add(g, 'box', PALE, [0, 1.15, 0], [2.6, 1.9, 1.6]);                  // enclosure
            add(g, 'box', STEEL, [-0.35, 1.3, 0.8], [1.45, 1.05, 0.04]);         // door frame
            add(g, 'box', GLASS, [-0.35, 1.3, 0.83], [1.25, 0.85, 0.03]);        // door window
            add(g, 'box', STEEL, [0.45, 1.3, 0.86], [0.06, 0.6, 0.06]);          // handle
            add(g, 'box', DARK, [1.05, 1.35, 0.88], [0.5, 0.8, 0.14]);           // control panel
            add(g, 'box', GLASS, [1.05, 1.5, 0.96], [0.36, 0.28, 0.02]);         // screen
            for (const x of [0.92, 1.05, 1.18]) add(g, 'box', STEEL, [x, 1.1, 0.96], [0.08, 0.08, 0.02]); // keys
            add(g, 'box', STEEL, [-1.55, 0.4, 0.1], [0.5, 0.6, 1.2]);            // chip conveyor
            add(g, 'cyl', DARK, [1.1, 2.25, -0.5], [0.12, 0.3, 0.12]);           // beacon post
        },
        welding(g) {
            add(g, 'box', STEEL, [0.55, 0.4, 0.15], [1.2, 0.8, 0.8]);            // weld table
            add(g, 'box', DARK, [0.55, 0.82, 0.15], [1.3, 0.05, 0.9]);           // table top
            add(g, 'box', PALE, [0.55, 0.92, 0.15], [0.5, 0.15, 0.3]);           // workpiece
            add(g, 'cyl', DARK, [-0.6, 0.25, -0.3], [0.7, 0.5, 0.7]);            // robot base
            add(g, 'cyl', ROBOT, [-0.6, 0.6, -0.3], [0.5, 0.2, 0.5]);            // turntable
            const shoulder = joint(g, [-0.6, 0.7, -0.3], -0.5);
            add(shoulder, 'box', ROBOT, [0, 0.55, 0], [0.24, 1.1, 0.24]);        // lower arm
            const elbow = joint(shoulder, [0, 1.1, 0], -1.4);
            add(elbow, 'cyl', ROBOT, [0, 0, 0], [0.3, 0.3, 0.3], [Math.PI / 2, 0, 0]); // elbow
            add(elbow, 'box', ROBOT, [0, 0.45, 0], [0.18, 0.9, 0.18]);           // upper arm
            add(elbow, 'cyl', DARK, [0, 1.0, 0], [0.08, 0.25, 0.08]);            // torch
            for (const [x, z] of [[-1.4, -1.3], [1.4, -1.3], [-1.4, 1.1], [1.4, 1.1]]) add(g, 'box', SAFETY, [x, 0.9, z], [0.1, 1.8, 0.1]); // fence posts
            for (const y of [0.9, 1.75]) {                                       // rails: back and sides, front open
                add(g, 'box', SAFETY, [0, y, -1.3], [2.8, 0.06, 0.06]);
                for (const x of [-1.4, 1.4]) add(g, 'box', SAFETY, [x, y, -0.1], [0.06, 0.06, 2.4]);
            }
        },
        assembly(g) {
            add(g, 'box', PALE, [0, 0.9, 0], [2.8, 0.1, 1.2]);                   // bench top
            for (const [x, z] of [[-1.3, -0.5], [1.3, -0.5], [-1.3, 0.5], [1.3, 0.5]]) add(g, 'box', STEEL, [x, 0.43, z], [0.08, 0.85, 0.08]);
            add(g, 'box', STEEL, [0, 0.25, 0], [2.6, 0.05, 1.0]);                // lower shelf
            add(g, 'box', STEEL, [0, 1.4, -0.58], [2.8, 0.9, 0.06]);             // tool board
            add(g, 'box', PALE, [0, 1.9, -0.45], [2.4, 0.06, 0.2]);              // light bar
            for (const x of [-0.8, 0, 0.8]) {
                add(g, 'box', DARK, [x, 1.02, 0.1], [0.45, 0.14, 0.4]);          // fixture
                add(g, 'cyl', ROBOT, [x, 1.15, 0.1], [0.1, 0.14, 0.1]);          // clamp
            }
            for (const x of [-0.9, 0, 0.9]) add(g, 'box', RACK, [x, 0.36, 0], [0.5, 0.18, 0.6]); // part bins
        },
        inspection(g) {
            add(g, 'box', STEEL, [0, 0.2, 0], [2.2, 0.4, 1.4]);                  // stand
            add(g, 'box', GRANITE, [0, 0.52, 0], [2.6, 0.25, 1.8]);              // granite table
            for (const x of [-1.1, 1.1]) add(g, 'box', PALE, [x, 1.35, -0.2], [0.18, 1.4, 0.25]); // bridge legs
            add(g, 'box', PALE, [0, 2.1, -0.2], [2.4, 0.22, 0.3]);               // bridge beam
            add(g, 'box', DARK, [0.3, 2.1, -0.2], [0.35, 0.35, 0.4]);            // carriage
            add(g, 'box', STEEL, [0.3, 1.55, -0.2], [0.1, 0.8, 0.1]);            // quill
            add(g, 'sphere', BEAM, [0.3, 1.12, -0.2], [0.09, 0.09, 0.09]);       // probe tip
            add(g, 'box', CARD, [0.3, 0.8, -0.2], [0.6, 0.3, 0.4]);              // part
            add(g, 'cyl', STEEL, [-1.5, 0.7, 0.6], [0.08, 1.4, 0.08]);           // monitor pole
            add(g, 'box', DARK, [-1.5, 1.45, 0.6], [0.55, 0.38, 0.05], [0, 0.4, 0]); // monitor
        },
        packing(g) {
            for (const [x, z] of [[-1.3, -0.3], [1.3, -0.3], [-1.3, 0.3], [1.3, 0.3]]) add(g, 'box', STEEL, [x, 0.36, z], [0.08, 0.72, 0.08]);
            add(g, 'box', STEEL, [0, 0.75, 0], [3.0, 0.12, 0.8]);                // frame
            add(g, 'box', DARK, [0, 0.83, 0], [2.9, 0.04, 0.7]);                 // belt
            for (const x of [-1.45, 1.45]) add(g, 'cyl', DARK, [x, 0.78, 0], [0.18, 0.78, 0.18], [Math.PI / 2, 0, 0]); // end rollers
            for (const z of [-0.4, 0.4]) add(g, 'box', PALE, [0, 0.95, z], [3.0, 0.08, 0.04]); // guide rails
            add(g, 'box', CARD, [-0.9, 1.05, 0], [0.5, 0.4, 0.45]);              // cartons on belt
            add(g, 'box', CARD, [0.1, 1.0, 0], [0.4, 0.3, 0.4]);
            add(g, 'box', CARD, [0.95, 1.07, 0], [0.55, 0.45, 0.5]);
            add(g, 'box', WOOD, [0.6, 0.08, -1.35], [1.1, 0.15, 1.0]);           // pallet
            for (const [x, y, z] of [[0.35, 0.4, -1.6], [0.85, 0.4, -1.6], [0.35, 0.4, -1.1], [0.85, 0.4, -1.1], [0.6, 0.9, -1.35]]) add(g, 'box', CARD, [x, y, z], [0.48, 0.48, 0.48]); // stacked cartons
        },
        material(g) {
            for (const [x, z] of [[-1.3, -0.5], [1.3, -0.5], [-1.3, 0.5], [1.3, 0.5]]) add(g, 'box', RACK, [x, 1.2, z], [0.1, 2.4, 0.1]); // uprights
            for (const y of [1.0, 2.0]) for (const z of [-0.5, 0.5]) add(g, 'box', BEAM, [0, y, z], [2.7, 0.12, 0.08]); // beams
            for (const y of [0, 1.06, 2.06]) {                                   // pallets with loads on three levels
                add(g, 'box', WOOD, [0, y + 0.08, 0], [1.1, 0.14, 0.9]);
                add(g, 'box', CARD, [0, y + 0.45, 0], [1.0, 0.6, 0.8]);
            }
            add(g, 'box', PALE, [0, 0.02, 1.1], [2.6, 0.04, 0.1]);               // floor bay line
        },
        generic(g) {
            add(g, 'box', DARK, [0, 0.05, 0], [1.9, 0.1, 1.1]);                  // plinth
            add(g, 'box', PALE, [0, 1.0, 0], [1.8, 1.8, 1.0]);                   // cabinet
            add(g, 'box', DARK, [0, 1.0, 0.51], [0.02, 1.7, 0.02]);              // door seam
            for (const y of [0.4, 0.55, 0.7]) add(g, 'box', DARK, [-0.45, y, 0.51], [0.6, 0.05, 0.02]); // vents
            add(g, 'box', STEEL, [0.45, 1.4, 0.52], [0.5, 0.4, 0.03]);           // panel
            add(g, 'cyl', STEEL, [0.6, 2.05, -0.2], [0.12, 0.3, 0.12]);          // beacon post
        },
    };

    const markerBuilders = {
        case: g => add(g, 'octa', MARKER_COLOR.case, [0, 4.30, 0], [0.7, 0.9, 0.7]),
        investigation: g => {                                                    // magnifier: ring and handle
            add(g, 'torus', MARKER_COLOR.investigation, [0, 4.40, 0], [1, 1, 1]);
            add(g, 'cyl', MARKER_COLOR.investigation, [0.37, 4.03, 0], [0.12, 0.45, 0.12], [0, 0, Math.PI / 4]);
        },
        proposal_awaiting_approval: g => {                                       // hourglass: waiting on a human
            add(g, 'cone', MARKER_COLOR.proposal_awaiting_approval, [0, 4.55, 0], [0.6, 0.5, 0.6], [Math.PI, 0, 0]);
            add(g, 'cone', MARKER_COLOR.proposal_awaiting_approval, [0, 4.05, 0], [0.6, 0.5, 0.6]);
        },
        task: g => {                                                             // flag
            add(g, 'cyl', DARK, [0, 4.20, 0], [0.06, 1.2, 0.06]);
            add(g, 'box', MARKER_COLOR.task, [0.3, 4.60, 0], [0.55, 0.36, 0.04]);
        },
        correction: g => {                                                       // map pin
            add(g, 'sphere', MARKER_COLOR.correction, [0, 4.50, 0], [0.5, 0.5, 0.5]);
            add(g, 'cone', MARKER_COLOR.correction, [0, 4.10, 0], [0.3, 0.6, 0.3], [Math.PI, 0, 0]);
        },
    };

    return {
        machine(archetype, opts = {}) {
            const kind = ARCHETYPES.includes(archetype) ? archetype : 'generic';
            const g = new THREE.Group();
            g.userData = {kind: 'machine', archetype: kind};
            builders[kind](g);
            add(g, 'box', opts.statusColor ?? NEUTRAL, [0, 0.03, 1.45], [2.6, 0.06, 0.16]).userData.status=true;
            return g;
        },
        person(opts = {}) {
            const role = opts.role === 'supervisor' ? 'supervisor' : 'operator';
            const g = new THREE.Group();
            g.userData = {kind: 'person', role};
            for (const x of [-0.1, 0.1]) add(g, 'cyl', DARK, [x, 0.42, 0], [0.15, 0.84, 0.15]); // legs
            add(g, 'box', VEST[role], [0, 1.13, 0], [0.42, 0.6, 0.24]);                       // torso / vest
            for (const x of [-0.27, 0.27]) add(g, 'cyl', DARK, [x, 1.1, 0], [0.1, 0.6, 0.1]);  // arms
            add(g, 'sphere', SKIN, [0, 1.55, 0], [0.24, 0.26, 0.24]);                          // head
            add(g, 'cyl', VEST[role], [0, 1.68, 0], [0.27, 0.06, 0.27]);                       // hard-hat brim
            return g;
        },
        marker(kind) {
            if (!markerBuilders[kind]) throw new Error(`unknown marker kind: ${kind}`);
            const g = new THREE.Group();
            g.userData = {kind: 'marker', marker: kind};
            markerBuilders[kind](g);
            return g;
        },
        dispose() {
            for (const g of Object.values(geo)) g.dispose();
            for (const m of materials.values()) m.dispose();
            materials.clear();
        },
    };
}
