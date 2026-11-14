// Adapted from wiseDo Twin (layout2Scene) app/src/lib/viewport.ts at commit 928d0771
// three ships without types here (@types/three is not installed), so the rig is typed structurally.
type Vec3 = {x:number;y:number;z:number};
/** A THREE.Vector3 as the rig uses it. */
type Vector3Like = Vec3 & {copy(v:Vec3):unknown};
/** A THREE.PerspectiveCamera. */
export type RigCamera = {fov:number;aspect:number;position:Vector3Like};
/** An OrbitControls instance. Its update() still applies minDistance, maxDistance and maxPolarAngle. */
export type RigControls = {
    target:Vector3Like;minDistance:number;maxDistance:number;update():unknown;
    addEventListener(type:'start',listener:()=>void):void;removeEventListener(type:'start',listener:()=>void):void;
};
/** A THREE.Object3D (a station group) and a THREE.Scene. */
export type RigObject = {position:Vec3};
export type RigScene = RigObject & {add(...objects:unknown[]):unknown};

/** Station-centre bounds in world metres; positions let sparse bays fit their occupied footprints. */
export type FrameBounds = {minX:number;maxX:number;minZ:number;maxZ:number;positions?:readonly {x:number;z:number}[]};
export type FrameMode = 'plant'|'line'|'station';
export type CameraPose = {position:Vec3;target:Vec3};
export type KeyboardHandlers = {next():void;prev():void;activate():void;presetPlant():void};

export type Rig = {
    /** Fits the camera to the bounds; jumps under reduced motion, otherwise eases over 400 ms. A new call, a drag, a key pan or zoom cancels a running transition. */
    frame(bounds:FrameBounds, mode:FrameMode):void;
    /** Arrows pan, + / - zoom, [ / ] previous/next station, Enter activates, Home frames the plant. Ignores keys typed in form fields. Adds tabindex="0" when missing. Returns detach. */
    attachKeyboard(el:HTMLElement, handlers:KeyboardHandlers):()=>void;
    /** Shows a ground ring and a raised box outline around the group (null clears it). */
    highlight(group:RigObject|null, scene:RigScene):void;
    /** Cancels any transition, removes the highlight and frees its geometry and materials. Detach keyboards separately. */
    dispose():void;
};

/** Pure framing: the whole padded bounds stay inside the frustum for the given aspect and vertical fov (default 42). */
export function frameFor(bounds:FrameBounds, aspect:number, mode:FrameMode, fov?:number):CameraPose;
export function createCameraRig(camera:RigCamera, controls:RigControls, opts:{reducedMotion:boolean;onChange:()=>void}):Rig;
