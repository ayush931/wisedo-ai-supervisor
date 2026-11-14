import { test, expect } from '@playwright/test';
import { archetype, layout } from '../src/plant-model';

test('WIS-711 station illustrations use bounded name hints and explicit templates', () => {
    for (const [name, template, expected] of [
        ['Press 4','production','press'], ['CNC 02','production','cnc'], ['Welding 1','production','welding'],
        ['Assembly bench','production','assembly'], ['Inspection','quality','inspection'], ['Packing-01','production','packing'],
        ['Unclassified','production','generic'], ['Compressor','production','generic'], ['', 'quality','inspection'], ['', 'material','material'],
    ]) expect(archetype(name, template)).toBe(expected);
});
test('WIS-711 layout is stable, nonoverlapping and preserves explicit line membership', () => {
    const stations = Array.from({length:48}, (_,i) => ({id:`s${String(i).padStart(2,'0')}`, line:`l${Math.floor(i/12)}`}));
    const placed = layout(stations);
    expect(layout([...stations].reverse())).toEqual(placed);
    expect(new Set(placed.map(s=>`${s.x},${s.z}`)).size).toBe(48);
    expect(placed.filter(s=>s.line==='l0').every(s=>s.z < placed.find(s=>s.line==='l1')!.z)).toBe(true);
});

import { stationState, type LiveStation } from '../src/plant-model';
import type { StationRecordV1 } from '../../contracts/typescript/oee';
const now=Date.parse('2026-11-11T12:00:00Z');
const station = {id:'s', current:{job_order_id:'job',plan_id:'plan',starts_at:'2026-11-11T08:00:00Z',ends_at:'2026-11-11T16:00:00Z'},run:{state:'started',started_at:'2026-11-11T08:00:00Z',ended_at:null}, records:[]} as unknown as LiveStation;
const record=(kind:string,at='2026-11-11T10:00:00Z',extra={})=>({id:kind,plan_id:'plan',state:'current',event_at:at,received_at:at,record:{kind,job_order_id:'job',...extra},recorded_by:{subject:'reporter',issuer:'oidc'}} as StationRecordV1);
test('WIS-711 unknown, empty, zero and missing lifecycle remain distinct',()=>{
    expect(stationState({...station,records:null},now).status).toBe('Unavailable');
    expect(stationState(station,now)).toMatchObject({status:'No data',good:null,person:null});
    expect(stationState({...station,records:[record('quantity',undefined,{good_quantity:0,reject_quantity:0})]},now)).toMatchObject({status:'Unknown',good:'0',reject:'0'});
    expect(stationState({...station,records:[record('note')]},now).good).toBeNull();
    expect(stationState({...station,current:null,records:[record('started')]},now)).toMatchObject({status:'No active assignment',person:null});
});
test('WIS-711 observed lifecycle and hold use current same-plan records without turning authors into live presence',()=>{
    const recs=[record('started'),record('blocked','2026-11-11T10:05:00Z',{comment:'Tool jam'}),record('quality_disposition_recorded','2026-11-11T10:06:00Z',{disposition:'hold'})];
    expect(stationState({...station,records:recs},now)).toMatchObject({status:'Stopped',reason:'Tool jam',hold:true,person:{subject:'reporter'}});
    expect(stationState({...station,records:[record('started'),{...record('blocked','2026-11-11T10:05:00Z'),state:'superseded'}]},now).status).toBe('Running');
    expect(stationState({...station,records:[{...record('started'),plan_id:'other'}]},now).status).toBe('No data');
    expect(stationState({...station,run:{...station.run!,state:'ended',ended_at:'2026-11-11T11:00:00Z'},records:[record('started')]},now)).toMatchObject({status:'Shift ended',person:null});
    expect(stationState({...station,records:[record('started')]},Date.parse(station.current!.ends_at)).status).toBe('Outside shift');
});

test('WIS-711 F1 holds remain recorded after acceptance and the latest disposition carries its time',()=>{
    const hold=record('quality_disposition_recorded','2026-11-11T10:06:00Z',{disposition:'hold'});
    const accept=record('quality_disposition_recorded','2026-11-11T10:20:00Z',{disposition:'accept'});
    expect(stationState({...station,records:[record('started'),hold,accept]},now)).toMatchObject({hold:true,holdAt:hold.event_at,disposition:{value:'accept',event_at:accept.event_at,recorded_by:accept.recorded_by}});
    expect(stationState({...station,records:[record('started'),accept]},now)).toMatchObject({hold:false,holdAt:null,disposition:{value:'accept',event_at:accept.event_at}});
    expect(stationState({...station,records:[hold,accept]},Date.parse('2026-11-11T10:10:00Z'))).toMatchObject({hold:true,disposition:{value:'hold',event_at:hold.event_at}});
});

import { replayTimes } from '../src/plant-model';
test('WIS-711 replay uses only authorized current assignment events without interpolation',()=>{
    const recs=[record('started'),record('quantity','2026-11-11T10:01:00Z',{good_quantity:5,reject_quantity:0}),record('blocked','2026-11-11T10:05:00Z',{comment:'Tool jam'}),{...record('quantity','2026-11-11T10:02:00Z',{good_quantity:99}),state:'superseded'} as StationRecordV1];
    const s={...station,records:recs};
    expect(replayTimes([s],now)).toEqual(['2026-11-11T10:00:00Z','2026-11-11T10:01:00Z','2026-11-11T10:05:00Z'].map(Date.parse));
    expect(stationState(s,Date.parse('2026-11-11T09:59:00Z'))).toMatchObject({status:'No data',good:null});
    expect(stationState(s,Date.parse('2026-11-11T10:01:00Z'))).toMatchObject({status:'Running',good:'5',reject:'0'});
    expect(stationState(s,Date.parse('2026-11-11T10:05:00Z')).status).toBe('Stopped');
    expect(replayTimes([{...s,current:null},{...s,records:null}],now)).toEqual([]);
    expect(replayTimes([{...s,records:[record('started','2026-11-12T10:00:00Z')]}],now)).toEqual([]);
});

test('WIS-711 whole-plant bays stay compact enough to read instead of a single long strip',()=>{
    const placed=layout(Array.from({length:48},(_,i)=>({id:`s${i}`,line:`l${Math.floor(i/8)}`})));
    const width=Math.max(...placed.map(s=>s.x))+5,depth=Math.max(...placed.map(s=>s.z))+6;
    expect(Math.max(width,depth)/Math.min(width,depth)).toBeLessThan(2);
});
