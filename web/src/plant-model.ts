export type Archetype = 'press' | 'cnc' | 'welding' | 'assembly' | 'inspection' | 'packing' | 'material' | 'generic';
export function archetype(name: string, template: string): Archetype {
    const hints: [RegExp, Archetype][] = [[/\bpress\b/i,'press'],[/\b(cnc|lathe|milling)\b/i,'cnc'],[/\bweld(ing)?\b/i,'welding'],[/\bassembly\b/i,'assembly'],[/\b(inspection|quality)\b/i,'inspection'],[/\b(pack(ing)?|packaging)\b/i,'packing']];
    return hints.find(([pattern])=>pattern.test(name))?.[1] ?? (template==='quality'?'inspection':template==='material'?'material':'generic');
}
export function layout<T extends {id:string; line:string}>(stations: readonly T[]): (T & {x:number;z:number})[] {
    const lines=[...new Set(stations.map(s=>s.line))].sort();
    const bayRows=Math.max(1,Math.ceil(Math.sqrt(lines.length)));
    const depth=Math.max(1,...lines.map(line=>Math.ceil(stations.filter(s=>s.line===line).length/4)))*6+4;
    return lines.flatMap((line,index)=>{
        const row=stations.filter(s=>s.line===line).sort((a,b)=>a.id.localeCompare(b.id));
        const out=row.map((s,i)=>({...s,x:Math.floor(index/bayRows)*22+(i%4)*5,z:(index%bayRows)*depth+Math.floor(i/4)*6}));
        return out;
    });
}
export type PlantStation = {id:string;name:string;line:string;lineName:string;template:string;archetype:Archetype;x:number;z:number;profileRevision?:number;error?:unknown};

import type { CurrentJobOrderV1, ShiftRunV1, StationRecordV1 } from '../../contracts/typescript/oee';
import { compareInstant } from './api';
export type LiveStation = PlantStation & {current:CurrentJobOrderV1|null; run:ShiftRunV1|null; records:readonly StationRecordV1[]|null};
export function stationState(s:LiveStation, now:number) {
    const out:{status:string;reason:string;hold:boolean;holdAt:string|null;disposition:{value:string;event_at:string;recorded_by:StationRecordV1['recorded_by']}|null;good:string|null;reject:string|null;person:StationRecordV1['recorded_by']|null;event:string|null;color:number}={status:'Unavailable',reason:'',hold:false,holdAt:null,disposition:null,good:null,reject:null,person:null,event:null,color:0x87949e};
    if(!s.records)return out;
    if(!s.records.length)return {...out,status:'No data'};
    if(!s.current)return {...out,status:'No active assignment'};
    const rows=s.records.filter(r=>r.state==='current'&&r.plan_id===s.current!.plan_id&&r.record.job_order_id===s.current!.job_order_id&&Date.parse(r.event_at)<=now).sort((a,b)=>compareInstant(a.event_at,b.event_at)||compareInstant(a.received_at,b.received_at)||a.id.localeCompare(b.id));
    if(!rows.length)return {...out,status:'No data'};
    const last=rows.at(-1)!;
    out.event=last.event_at;
    const quantity=rows.filter(r=>r.record.kind==='quantity');
    for(const [field,key] of [['good_quantity','good'],['reject_quantity','reject']] as const) {
        const values=quantity.map(r=>r.record[field]).filter((v):v is number=>v!==undefined);
        if(values.length&&values.every(Number.isSafeInteger))out[key]=values.reduce((sum,n)=>sum+BigInt(n),0n).toString();
    }
    const disposition=rows.filter(r=>r.record.kind==='quality_disposition_recorded').at(-1);
    const hold=rows.find(r=>r.record.kind==='quality_disposition_recorded'&&r.record.disposition==='hold');
    out.hold=!!hold;out.holdAt=hold?.event_at??null;
    if(disposition?.record.disposition)out.disposition={value:disposition.record.disposition,event_at:disposition.event_at,recorded_by:disposition.recorded_by};
    if(now<Date.parse(s.current.starts_at)||now>=Date.parse(s.current.ends_at))return {...out,status:'Outside shift'};
    if(!s.run)return {...out,status:'Shift unavailable'};
    if(s.run.ended_at&&now>=Date.parse(s.run.ended_at))return {...out,status:'Shift ended'};
    if(!s.run.started_at||now<Date.parse(s.run.started_at))return {...out,status:'Shift not started'};
    const lifecycle=rows.filter(r=>['acknowledged','started','resumed','paused','blocked','completion_evidence'].includes(r.record.kind)).at(-1);
    out.status=({started:'Running',resumed:'Running',paused:'Stopped',blocked:'Stopped',acknowledged:'Acknowledged',completion_evidence:'Completed'} as Record<string,string>)[lifecycle?.record.kind??'']??'Unknown';
    out.reason=out.status==='Stopped'?(lifecycle?.record.comment??'Reason not recorded'):'';
    out.person=last.recorded_by;
    out.color=out.hold?0xa76cbd:out.status==='Running'?0x208775:out.status==='Stopped'?0xc27c31:0x87949e;
    return out;
}

// Replay is today's authorized, corrected record set, not an archived authority snapshot.
export function replayTimes(stations:readonly LiveStation[],now:number):number[] {
    return [...new Set(stations.flatMap(s=>s.current?(s.records??[]).filter(r=>r.state==='current'&&r.plan_id===s.current!.plan_id&&r.record.job_order_id===s.current!.job_order_id).map(r=>Date.parse(r.event_at)):[]))].filter(t=>Number.isFinite(t)&&t<=now).sort((a,b)=>a-b);
}
