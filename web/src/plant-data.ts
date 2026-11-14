import type { LinePageV1, StationProfilePageV1, StationProfileV1 } from '../../contracts/typescript/oee';
import { allPages, loadShiftPlans, request } from './api';
import { archetype, layout, type PlantStation } from './plant-model';

// Four in-flight reads keep a 48-station refresh from flooding the shared backend.
export async function parallelMap<T,R>(items: readonly T[], read: (item:T)=>Promise<R>): Promise<R[]> {
    const out:R[]=[];
    let next=0;
    await Promise.all(Array.from({length:Math.min(4,items.length)},async()=>{
        while(next<items.length) { const i=next++; out[i]=await read(items[i]); }
    }));
    return out;
}
export async function loadPlant(site:string,previous:readonly PlantStation[]=[]):Promise<PlantStation[]> {
    const base=`/v1/sites/${site}`;
    const [stations,lines]=await Promise.all([
        allPages(async cursor=>{const p=await request<StationProfilePageV1>(`${base}/workstations?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.workstations,next_cursor:p.next_cursor};}),
        allPages(async cursor=>{const p=await request<LinePageV1>(`${base}/lines?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.lines,next_cursor:p.next_cursor};}),
    ]);
    return layout(await parallelMap(stations,async s=>{
        const lineName=(line:string)=>lines.find(l=>l.id===line)?.body.display_name??(line?`Line ${line.slice(0,8)} (name unavailable)`:'Unassigned line');
        const cached=previous.find(p=>p.id===s.workstation_id&&!p.error&&p.profileRevision===s.current_revision);
        if(cached)return {...cached,lineName:lineName(cached.line)};
        try{
            const p=await request<StationProfileV1>(`${base}/workstations/${s.workstation_id}/profile`);
            const profile=p.revisions.find(r=>r.revision===p.current_revision)?.profile;
            if(!profile)throw new Error('Current station profile unavailable.');
            return {id:s.workstation_id,name:profile.display_name,line:profile.line_id,lineName:lineName(profile.line_id),template:profile.template,archetype:archetype(profile.display_name,profile.template),profileRevision:p.current_revision};
        }catch(error){return {id:s.workstation_id,name:s.display_name??s.workstation_id,line:s.line_id??'',lineName:lineName(s.line_id??''),template:'Unavailable',archetype:'generic' as const,error};}
    }));
}

import type { CurrentJobOrderV1, ShiftRunV1, ShiftPlanV1, StationRecordPageV1 } from '../../contracts/typescript/oee';
import { ApiError } from './api';
import type { LiveStation } from './plant-model';
export async function loadLive(site:string,stations:readonly PlantStation[]):Promise<LiveStation[]> {
    const runs=new Map<string,Promise<ShiftRunV1>>();
    const now=Date.now();
    const plans=(await loadShiftPlans(site)).filter(p=>p.approved_revision!==null&&Date.parse(p.starts_at)<=now&&now<Date.parse(p.ends_at));
    const assigned=new Set((await parallelMap(plans,p=>request<ShiftPlanV1>(`/v1/sites/${site}/shift-plans/${p.id}`))).flatMap(p=>p.revisions.find(r=>r.revision===p.approved_revision)?.plan.assignments.map(a=>a.workstation_id)??[]));
    return parallelMap(stations,async s=>{
        if(s.error)return {...s,current:null,run:null,records:null};
        try {
            const base=`/v1/sites/${site}/workstations/${s.id}`;
            const current=assigned.has(s.id)?await request<CurrentJobOrderV1>(`${base}/current-job-order`).catch(e=>{
                if(e instanceof ApiError&&e.problem.code==='NO_CURRENT_JOB_ORDER')return null;
                throw e;
            }):null;
            if(current&&!runs.has(current.plan_id))runs.set(current.plan_id,request<ShiftRunV1>(`/v1/sites/${site}/shift-plans/${current.plan_id}/run`));
            const [records,run]=await Promise.all([
                current?allPages(async cursor=>{const p=await request<StationRecordPageV1>(`${base}/records?limit=100&job_order_id=${current.job_order_id}${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.records,next_cursor:p.next_cursor};}):request<StationRecordPageV1>(`${base}/records?limit=1`).then(p=>p.records),
                current?runs.get(current.plan_id)!:Promise.resolve(null),
            ]);
            return {...s,current,run,records};
        } catch(error){return {...s,current:null,run:null,records:null,error};}
    });
}
