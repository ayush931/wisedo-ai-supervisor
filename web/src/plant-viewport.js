// Adapted from wiseDo Twin (layout2Scene) app/src/lib/viewport.ts at commit 928d0771
// Adapted from wiseDo Twin (layout2Scene) app/src/lib/bufferScene.ts at commit 928d0771
import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import { createPlantKit } from './plant-machines.js';
import { createCameraRig, frameFor } from './plant-camera.js';

export function mountPlant(canvas, stations, select) {
    const renderer=new THREE.WebGLRenderer({canvas,antialias:false,alpha:false});
    const gl=renderer.getContext(),debug=gl.getExtension('WEBGL_debug_renderer_info');
    const software=/swiftshader|llvmpipe|software/i.test(String(debug?gl.getParameter(debug.UNMASKED_RENDERER_WEBGL):gl.getParameter(gl.RENDERER)));
    renderer.setPixelRatio(software?1:Math.min(devicePixelRatio,1.5));
    const scene=new THREE.Scene();scene.background=new THREE.Color(0xe9eef2);
    const camera=new THREE.PerspectiveCamera(42,1,0.1,3000);
    const controls=new OrbitControls(camera,canvas);
    canvas.style.touchAction=""; // OrbitControls forces "none"; let style.css decide (pan-y on narrow screens so the page scrolls).
    controls.enableDamping=false;controls.maxPolarAngle=Math.PI*0.46;controls.minDistance=5;controls.maxDistance=1000;controls.screenSpacePanning=false;
    scene.add(new THREE.HemisphereLight(0xffffff,0x697c88,2.6));
    const light=new THREE.DirectionalLight(0xffffff,2);light.position.set(-10,25,15);scene.add(light);
    const kit=createPlantKit({software}),padGeometry=new THREE.BoxGeometry(4.5,0.2,5.2),padMaterial=software?new THREE.MeshLambertMaterial({color:0xd8e1e5}):new THREE.MeshStandardMaterial({color:0xd8e1e5,roughness:1});
    const groups=new Map(),batches=new Map(),pickables=[],labels=[];
    function paintLabel(sprite,text) {
        if(sprite.userData.text===text)return;sprite.userData.text=text;
        const c=sprite.material.map.image,ctx=c.getContext('2d');ctx.fillStyle='#f7fafc';ctx.fillRect(0,0,c.width,c.height);ctx.fillStyle='#203542';ctx.font='600 28px system-ui';ctx.textAlign='center';
        text.split('\n').slice(0,3).forEach((line,i)=>ctx.fillText(line,256,45+i*48,490));sprite.material.map.needsUpdate=true;
    }
    function label(text,x,y,z,width,station,line) {
        const c=document.createElement('canvas');c.width=512;c.height=192;
        const texture=new THREE.CanvasTexture(c),material=new THREE.SpriteMaterial({map:texture,depthTest:false});
        const sprite=new THREE.Sprite(material);sprite.position.set(x,y,z);sprite.scale.set(width,width*3/8,1);sprite.userData.station=station;sprite.userData.line=line;sprite.visible=false;paintLabel(sprite,text);scene.add(sprite);labels.push(sprite);if(station)pickables.push(sprite);
    }
    function collect(root,id,flags={}) {
        root.updateMatrixWorld(true);
        root.traverse(mesh=>{if(!mesh.isMesh)return;const status=!!mesh.userData.status,key=mesh.geometry.uuid+mesh.material.uuid+(status?':status':'');if(!batches.has(key))batches.set(key,{geometry:mesh.geometry,material:mesh.material,parts:[],status});batches.get(key).parts.push({matrix:mesh.matrixWorld.clone(),id,...flags});});
    }
    const seenLines=new Set(),kinds=['case','investigation','proposal_awaiting_approval','task','correction'];
    for(const s of stations) {
        const root=new THREE.Group();root.position.set(s.x,0,s.z);root.userData.station=s.id;groups.set(s.id,root);scene.add(root);
        const pad=new THREE.Mesh(padGeometry,padMaterial);pad.position.y=-0.13;root.add(pad);
        const machine=kit.machine(s.archetype,{statusColor:0xffffff});machine.scale.set(1.25,1.4,1.25);root.add(machine);collect(root,s.id);
        const person=kit.person({role:'operator'});person.scale.setScalar(1.55);person.position.set(s.x-1.7,0,s.z+2);collect(person,s.id,{person:true});
        for(const [i,kind] of kinds.entries()){
            const marker=kit.marker(kind);marker.position.set(s.x+(i-2)*0.75,0,s.z);collect(marker,s.id,{marker:kind});
            if(!seenLines.has(s.line)){const lineMarker=kit.marker(kind);lineMarker.position.set(s.x+3+(i-2)*0.75,0,s.z-2.5);collect(lineMarker,s.id,{lineMarker:kind});}
        }
        seenLines.add(s.line);
        // Keep original bounds for selection; only the batched meshes draw.
        root.visible=false;
        label(s.label??s.name,s.x,0.55,s.z+2.65,3.2,s.id);
    }
    for(const batch of batches.values()) {
        const mesh=new THREE.InstancedMesh(batch.geometry,batch.material,batch.parts.length);
        batch.mesh=mesh;scene.add(mesh);pickables.push(mesh);
    }
    for(const line of new Set(stations.map(s=>s.line))) {
        const row=stations.filter(s=>s.line===line);label(row[0].lineName.replace(' (name unavailable)',''),Math.min(...row.map(s=>s.x))+3,0.8,Math.min(...row.map(s=>s.z))-3,4,undefined,line);
    }
    function render(){renderer.render(scene,camera);}
    const rig=createCameraRig(camera,controls,{reducedMotion:matchMedia('(prefers-reduced-motion: reduce)').matches,onChange:render});
    let lastIDs,first=true,currentId=stations[0]?.id,labelStation=null,labelLine=null,hoverId=null;
    function showLabels(){for(const sprite of labels)sprite.visible=sprite.userData.station?([labelStation,hoverId].includes(sprite.userData.station)):sprite.userData.line===labelLine;render();}
    function update(next) {
        stations=next;const byId=new Map(stations.map(s=>[s.id,s])),color=new THREE.Color();
        for(const batch of batches.values()){
            const mesh=batch.mesh;let count=0;mesh.userData.stationIDs=[];
            for(const part of batch.parts){
                const s=byId.get(part.id);
                if(!s||(part.person&&!s.person)||(part.marker&&!s.marker?.includes(part.marker))||(part.lineMarker&&!s.lineMarker?.includes(part.lineMarker)))continue;
                mesh.setMatrixAt(count,part.matrix);if(batch.status)mesh.setColorAt(count,color.set(s.color??0x87949e));
                mesh.userData.stationIDs.push(part.lineMarker?undefined:part.id);count++;
            }
            mesh.count=count;mesh.visible=count>0;mesh.instanceMatrix.needsUpdate=true;if(mesh.instanceColor)mesh.instanceColor.needsUpdate=true;mesh.computeBoundingSphere();
        }
        for(const sprite of labels){const s=sprite.userData.station?byId.get(sprite.userData.station):stations.find(s=>s.line===sprite.userData.line);if(s)paintLabel(sprite,sprite.userData.station?s.label??s.name:s.lineName.replace(' (name unavailable)',''));}
        showLabels();
    }
    function focus(ids) {
        lastIDs=ids;
        const selected=stations.filter(s=>!ids?.length||ids.includes(s.id));if(!selected.length)return;
        const bounds={minX:Math.min(...selected.map(s=>s.x)),maxX:Math.max(...selected.map(s=>s.x)),minZ:Math.min(...selected.map(s=>s.z)),maxZ:Math.max(...selected.map(s=>s.z)),positions:selected};
        const mode=!ids?.length?'plant':selected.length===1?'station':'line';
        if(first){const pose=frameFor(bounds,camera.aspect,mode);camera.position.copy(pose.position);controls.target.copy(pose.target);controls.update();first=false;}
        else rig.frame(bounds,mode);
        labelStation=ids?.length===1?ids[0]:null;labelLine=ids?.length>1?selected[0].line:null;hoverId=null;
        if(ids?.length===1){currentId=ids[0];rig.highlight(groups.get(currentId),scene);}else rig.highlight(null,scene);
        showLabels();
    }
    function resize(){const r=canvas.getBoundingClientRect();renderer.setSize(Math.max(1,r.width),Math.max(1,r.height),false);camera.aspect=Math.max(1,r.width)/Math.max(1,r.height);camera.updateProjectionMatrix();focus(lastIDs);}
    const ray=new THREE.Raycaster(),pointer=new THREE.Vector2();let down=null;
    function activate(id){currentId=id;labelStation=id;showLabels();rig.highlight(groups.get(id)??null,scene);select(id);}
    function hover(e){if(e.buttons)return;const r=canvas.getBoundingClientRect();pointer.set((e.clientX-r.left)/r.width*2-1,-(e.clientY-r.top)/r.height*2+1);ray.setFromCamera(pointer,camera);const hit=ray.intersectObjects(pickables.filter(p=>p.isInstancedMesh&&p.visible),false)[0];const id=hit?.object.userData.stationIDs?.[hit.instanceId]??null;if(id!==hoverId){hoverId=id;showLabels();}}
    function leave(){hoverId=null;showLabels();}
    function start(e){down=[e.clientX,e.clientY];}
    function pick(e){if(!down||Math.hypot(e.clientX-down[0],e.clientY-down[1])>5)return;const r=canvas.getBoundingClientRect();pointer.set((e.clientX-r.left)/r.width*2-1,-(e.clientY-r.top)/r.height*2+1);ray.setFromCamera(pointer,camera);for(const hit of ray.intersectObjects(pickables.filter(p=>p.visible),false)){const id=hit.object.userData.stationIDs?.[hit.instanceId]??hit.object.userData.station;if(id){activate(id);return;}}}
    function step(delta){if(!stations.length)return;const i=stations.findIndex(s=>s.id===currentId);activate(stations[(i+delta+stations.length)%stations.length].id);}
    const detachKeys=rig.attachKeyboard(canvas,{next:()=>step(1),prev:()=>step(-1),activate:()=>currentId&&activate(currentId),presetPlant:()=>focus()});
    controls.addEventListener('change',render);canvas.addEventListener('pointerdown',start);canvas.addEventListener('pointerup',pick);canvas.addEventListener('pointermove',hover);canvas.addEventListener('pointerleave',leave);
    update(stations);
    const observer=new ResizeObserver(resize);observer.observe(canvas);resize();
    if(globalThis.__PLANT_PERF__)canvas.__plantView={renderer,scene,camera,controls};
    return {focus,update,dispose(){observer.disconnect();detachKeys();rig.dispose();controls.removeEventListener('change',render);controls.dispose();canvas.removeEventListener('pointerdown',start);canvas.removeEventListener('pointerup',pick);canvas.removeEventListener('pointermove',hover);canvas.removeEventListener('pointerleave',leave);for(const mesh of pickables)if(mesh.isInstancedMesh)mesh.dispose();for(const sprite of labels){sprite.material.map.dispose();sprite.material.dispose();}kit.dispose();padGeometry.dispose();padMaterial.dispose();renderer.dispose();delete canvas.__plantView;}};
}
