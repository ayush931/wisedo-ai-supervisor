// Adapted from wiseDo Twin (layout2Scene) app/src/lib/viewport.ts at commit 928d0771
import type { MarkerKind } from './plant-machines';
import type { PlantStation } from './plant-model';
export type SceneStation = PlantStation & {color?:number;person?:boolean;marker?:readonly MarkerKind[];lineMarker?:readonly MarkerKind[];label?:string};
export function mountPlant(canvas:HTMLCanvasElement, stations:readonly SceneStation[], select:(id:string)=>void): {
    dispose():void;
    focus(ids?:readonly string[]):void;
    update(stations:readonly SceneStation[]):void;
};
