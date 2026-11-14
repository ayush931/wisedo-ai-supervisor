// three 0.169.0 ships no type declarations here, so returned objects are typed as opaque
// THREE.Group instances: add them to a scene and set position/rotation/visible as usual.
import type { Archetype } from './plant-model';

export type MarkerKind = 'case' | 'investigation' | 'proposal_awaiting_approval' | 'task' | 'correction';
export type PersonRole = 'operator' | 'supervisor';

/** A THREE.Group whose origin is the station pad centre (y = 0 is the pad top). */
export interface PlantGroup {
    readonly isGroup: true;
    userData:
        | {kind: 'machine'; archetype: Archetype}
        | {kind: 'person'; role: PersonRole}
        | {kind: 'marker'; marker: MarkerKind};
    [key: string]: unknown;
}

export interface PlantKit {
    /** Low-poly machine about 2.5 to 3 units wide. Unknown archetypes build `generic`.
     *  `statusColor` colours the thin strip at the machine's front (+z); the caller decides it. */
    machine(archetype: Archetype | string, opts?: {statusColor?: number}): PlantGroup;
    /** Standing figure about 1.7 units tall; vest colour by role (default operator). */
    person(opts?: {role?: PersonRole}): PlantGroup;
    /** Static marker floating at y ~3.6 to 4.9 (clear of the press crown at 3.5), distinct in shape and colour per kind:
     *  case octahedron, investigation magnifier ring, proposal hourglass, task flag, correction pin.
     *  Throws on an unknown kind. */
    marker(kind: MarkerKind): PlantGroup;
    /** Frees every geometry and material the kit created. Groups built by it must not be rendered afterwards. */
    dispose(): void;
}

export function createPlantKit(options?:{software?:boolean}): PlantKit;
