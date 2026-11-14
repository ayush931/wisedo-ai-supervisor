import { test, expect } from '@playwright/test';
import { relatedActivity, proposalActivity, loadActivity, type Activity } from '../src/plant-activity';
import * as activity from '../src/plant-activity';
test('WIS-711 AI markers bind exact station IDs or explicit line scope, never names or unrelated sites',()=>{
    const items:Activity[]=[{id:'a',kind:'Task',title:'Press s1',body:'',href:'/tasks/a',stationIDs:['s2'],line:'line1'}, {id:'b',kind:'Case',title:'Line case',body:'',href:'/cases/b',stationIDs:[],line:'line1'}, {id:'c',kind:'Chat',title:'s1',body:'Press',href:'/today',stationIDs:[]}];
    expect(relatedActivity(items,{id:'s1',line:'line1'}).map(a=>a.id)).toEqual(['b']);
    expect(relatedActivity(items,{id:'s2',line:'line2'}).map(a=>a.id)).toEqual(['a']);
});

test('WIS-711 F7 line markers draw once while explicit station markers remain on their station',()=>{
    const items:Activity[]=[{id:'case',kind:'Case',title:'Case needs_input',body:'',href:'/cases/c',stationIDs:[],line:'l1'},{id:'task',kind:'Task',title:'Approved task',body:'',href:'/tasks/t',stationIDs:['s2'],line:'l1'}];
    const stations=[{id:'s1',line:'l1'},{id:'s2',line:'l1'},{id:'s3',line:'l2'}];
    expect(activity.sceneMarkers(items,stations)).toEqual([{marker:[],lineMarker:['case']},{marker:['task'],lineMarker:[]},{marker:[],lineMarker:[]}]);
});

test('WIS-711 F3 task summaries supply line scope without per-task detail reads',async()=>{
    const original=globalThis.fetch,paths:string[]=[];
    globalThis.fetch=async input=>{const path=String(input);paths.push(path);const body=path.includes('/tasks?')?{items:[{id:'t',line_id:'l',state:'acknowledged',created_at:'2026-11-11T10:00:00Z',due_at:'2026-11-12T10:00:00Z'}]}:path.includes('/chat-conversations')?{conversations:[]}:path.includes('/memory-candidates')?{candidates:[]}:{items:[]};return new Response(JSON.stringify(body),{headers:{'Content-Type':'application/json'}});};
    try{const result=await loadActivity('site','supervisor');expect(result.errors).toEqual([]);expect(result.items).toEqual([expect.objectContaining({id:'t',kind:'Task',line:'l',stationIDs:[],title:'Approved task · acknowledged'})]);expect(paths.some(p=>p.includes('/tasks/'))).toBe(false);}finally{globalThis.fetch=original;}
});
test('WIS-711 expired pending proposals cannot be counted as awaiting human approval',()=>{
    const p={id:'p',case_id:'c',state:'pending',expires_at:'2026-11-11T10:00:00Z',proposal_revision:1,revision:1};
    expect(proposalActivity(p,'site',[],Date.parse('2026-11-11T10:00:00Z')).title).toBe('Expired proposal');
    expect(proposalActivity(p,'site',[],Date.parse('2026-11-11T09:00:00Z')).title).toBe('Awaiting human approval');
});

import { markerKinds } from '../src/plant-activity';
test('WIS-711 marker kinds preserve investigation, approval, task and correction distinctions',()=>{
    const entry=(kind:string,title:string):Activity=>({id:title,kind,title,body:'',href:'/today',stationIDs:[]});
    expect(markerKinds([entry('Case','Investigation running'),entry('Case','Case needs_input'),entry('Proposal','Awaiting human approval'),entry('Proposal','Expired proposal'),entry('Task','Approved task · created'),entry('Correction','Correction · pending'),entry('Chat','Question')])).toEqual(['investigation','case','proposal_awaiting_approval','task','correction']);
});

test('WIS-711 R2 correction annotations shorten reference IDs without changing message text',async()=>{
    const original=globalThis.fetch,claim='12345678-1234-4000-8000-123456789abc';
    globalThis.fetch=async input=>{
        const path=String(input),body=path.includes('/messages?')?{messages:[{message_id:'m',author_kind:'agent',state:'answered',body:'Recorded answer',corrections:[{claim_id:claim,reviewed_at:'2026-11-11T10:00:00Z'}]}]}:path.includes('/chat-conversations?')?{conversations:[{conversation_id:'c',updated_at:'2026-11-11T10:00:00Z'}]}:{items:[]};
        return new Response(JSON.stringify(body),{headers:{'Content-Type':'application/json'}});
    };
    try{const {items,errors}=await loadActivity('site','operator');expect(errors).toEqual([]);expect(items[0].body).toContain('Recorded answer');expect(items[0].body).toContain('(claim 12345678)');expect(items[0].body).not.toContain(claim);}finally{globalThis.fetch=original;}
});
