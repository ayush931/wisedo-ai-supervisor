import { test, expect } from '@playwright/test';
import { loadLive, parallelMap } from '../src/plant-data';
import type { PlantStation } from '../src/plant-model';

test('WIS-711 unassigned stations read history without expected-error current-job requests',async()=>{
    const paths:string[]=[];const original=globalThis.fetch;
    globalThis.fetch=async input=>{const path=String(input);paths.push(path);if(path.includes('current-job-order'))return new Response(JSON.stringify({status:404,code:'NO_CURRENT_JOB_ORDER'}),{status:404,headers:{'Content-Type':'application/problem+json'}});return new Response(JSON.stringify(path.includes('/shift-plans')?{plans:[]}:{records:[]}),{status:200,headers:{'Content-Type':'application/json'}});};
    try {const out=await loadLive('site',[{id:'station'} as PlantStation]);expect(out[0]).toMatchObject({current:null,run:null,records:[]});expect(paths.some(p=>p.includes('current-job-order'))).toBe(false);}finally{globalThis.fetch=original;}
});

test('WIS-711 F3 unassigned history reads only one record even when more pages exist',async()=>{
    const paths:string[]=[],original=globalThis.fetch;
    globalThis.fetch=async input=>{const path=String(input);paths.push(path);return new Response(JSON.stringify(path.includes('/shift-plans')?{plans:[]}:path.includes('cursor=')?{records:[]}:{records:[{id:'record'}],next_cursor:'older'}),{headers:{'Content-Type':'application/json'}});};
    try{const out=await loadLive('site',[{id:'station'} as PlantStation]);expect(out[0].records).toHaveLength(1);expect(paths.filter(p=>p.includes('/records'))).toEqual(['/v1/sites/site/workstations/station/records?limit=1']);}finally{globalThis.fetch=original;}
});
test('WIS-711 station reads bound concurrent requests and retain ordering',async()=>{
    let active=0,max=0;
    const out=await parallelMap([0,1,2,3,4,5,6,7],async n=>{active++;max=Math.max(max,active);await new Promise<void>(resolve=>setTimeout(resolve,2));active--;return n*2;});
    expect(max).toBeLessThanOrEqual(4);expect(out).toEqual([0,2,4,6,8,10,12,14]);
});

test('WIS-711 F3 unchanged profile revisions are reused and changed profiles are reauthorized',async()=>{
    const original=globalThis.fetch;let revision=1,reads=0;
    globalThis.fetch=async input=>{const path=String(input);if(path.includes('/profile'))reads++;return new Response(JSON.stringify(path.includes('/profile')?{current_revision:revision,revisions:[{revision,profile:{display_name:`CNC ${revision}`,line_id:'line',template:'production'}}]}:path.includes('/workstations')?{workstations:[{workstation_id:'station',current_revision:revision}]}:{lines:[]}),{headers:{'Content-Type':'application/json'}});};
    try{const first=await loadPlant('site');const second=await loadPlant('site',first);expect(reads).toBe(1);revision=2;const third=await loadPlant('site',second);expect(reads).toBe(2);expect(third[0].name).toBe('CNC 2');}finally{globalThis.fetch=original;}
});

test('WIS-711 F4 missing station profiles isolate unavailable rows and are retried',async()=>{
    const original=globalThis.fetch;let missing=true;
    globalThis.fetch=async input=>{const path=String(input);if(path.includes('/bad/profile')&&missing)return new Response(JSON.stringify({status:404,code:'NOT_FOUND'}),{status:404,headers:{'Content-Type':'application/problem+json'}});return new Response(JSON.stringify(path.includes('/profile')?{current_revision:1,revisions:[{revision:1,profile:{display_name:path.includes('/bad/')?'Recovered':'CNC',line_id:'line',template:'production'}}]}:path.includes('/workstations')?{workstations:['good','bad'].map(id=>({workstation_id:id,display_name:id,line_id:'line',current_revision:1}))}:path.includes('/shift-plans')?{plans:[]}:{lines:[]}),{headers:{'Content-Type':'application/json'}});};
    try{const first=await loadPlant('site');expect(first).toHaveLength(2);expect(first.find(s=>s.id==='good')?.name).toBe('CNC');const bad=first.find(s=>s.id==='bad')!;expect(bad.error).toBeTruthy();expect((await loadLive('site',[bad]))[0].records).toBeNull();missing=false;expect((await loadPlant('site',first)).find(s=>s.id==='bad')?.name).toBe('Recovered');}finally{globalThis.fetch=original;}
});

import { loadPlant } from '../src/plant-data';
test('WIS-711 a missing line name does not turn a known line ID into an unassigned station',async()=>{
    const original=globalThis.fetch;
    globalThis.fetch=async input=>new Response(JSON.stringify(String(input).includes('/profile')?{current_revision:1,revisions:[{revision:1,profile:{display_name:'CNC 1',line_id:'known-line',template:'production'}}]}:String(input).includes('/workstations')?{workstations:[{workstation_id:'station'}]}:{lines:[]}),{headers:{'Content-Type':'application/json'}});
    try{const rows=await loadPlant('site');expect(rows[0].line).toBe('known-line');expect(rows[0].lineName).toContain('name unavailable');expect(rows[0].lineName).not.toBe('Unassigned line');}finally{globalThis.fetch=original;}
});
