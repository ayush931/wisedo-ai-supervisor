import type { CasePageV1, ChatConversationPageV1, ChatMessagePageV1, KnowledgeClaimV1, MemoryCandidatePageV1, ProposalPageV1, TaskPageV1 } from '../../contracts/typescript/oee';
import { allPages, request } from './api';
import { parallelMap } from './plant-data';
export type Activity = {id:string;kind:string;title:string;body:string;href:string;stationIDs:string[];line?:string;time?:string};
export const relatedActivity=(items:readonly Activity[],s:{id:string;line:string})=>items.filter(a=>a.stationIDs.length?a.stationIDs.includes(s.id):!!a.line&&a.line===s.line);
export function proposalActivity(p:ProposalPageV1['items'][number],site:string,cases:CasePageV1['items'],now:number):Activity {
    return {id:p.id,kind:'Proposal',title:p.state==='pending'?(Date.parse(p.expires_at)<=now?'Expired proposal':'Awaiting human approval'):`Proposal ${p.state}`,body:`Expires ${p.expires_at}`,href:`/sites/${site}/approvals`,stationIDs:[],line:cases.find(c=>c.id===p.case_id)?.line_id};
}
export async function loadActivity(site:string,role:string):Promise<{items:Activity[];errors:{section:string;error:unknown}[]}> {
    const base=`/v1/sites/${site}`,link=`/sites/${site}`,items:Activity[]=[],errors:{section:string;error:unknown}[]=[];
    const read=async<T>(section:string,fn:()=>Promise<T>,fallback:T):Promise<T>=>{try{return await fn();}catch(error){errors.push({section,error});return fallback;}};
    const cases=await read('Cases',()=>allPages(async cursor=>{const p=await request<CasePageV1>(`${base}/cases?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.items,next_cursor:p.next_cursor??undefined};}),[]);
    for(const c of cases.filter(c=>!['resolved','canceled'].includes(c.state)))items.push({id:c.id,kind:'Case',title:c.state==='investigating'?'Investigation running':`Case ${c.state}`,body:'Line scope; no individual station cause established.',href:`${link}/cases/${c.id}`,stationIDs:[],line:c.line_id,time:c.detected_at});
    await read('Proposals',async()=>{
        const proposals=await allPages(async cursor=>{const p=await request<ProposalPageV1>(`${base}/proposals?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.items,next_cursor:p.next_cursor??undefined};});
        items.push(...proposals.map(p=>proposalActivity(p,site,cases,Date.now())));
    },undefined);
    await read('Tasks',async()=>{
        const tasks=await allPages(async cursor=>{const p=await request<TaskPageV1>(`${base}/tasks?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.items,next_cursor:p.next_cursor??undefined};});
        // Approved task assets are line IDs; the list supplies the explicit line scope.
        items.push(...tasks.map(t=>({id:t.id,kind:'Task',title:`Approved task · ${t.state}`,body:`Due ${t.due_at}`,href:`${link}/tasks/${t.id}`,stationIDs:[],line:t.line_id,time:t.created_at})));
    },undefined);
    await read('Your chat',async()=>{
        const conversations=await allPages(async cursor=>{const p=await request<ChatConversationPageV1>(`${base}/chat-conversations?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.conversations,next_cursor:p.next_cursor};});
        const recent=[...conversations].sort((a,b)=>b.updated_at.localeCompare(a.updated_at)).slice(0,5);
        const claims=new Map<string,Promise<KnowledgeClaimV1>>();
        const chats=await parallelMap(recent,async c=>{
            const messages=await allPages(async cursor=>{const p=await request<ChatMessagePageV1>(`${base}/chat-conversations/${c.conversation_id}/messages?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.messages,next_cursor:p.next_cursor};});
            return parallelMap(messages.slice(-40),async m=>{
                const stationIDs:string[]=[];
                for(const citation of m.claim_citations??[]) {
                    const key=`${citation.claim_id}:${citation.revision}`;
                    if(!claims.has(key))claims.set(key,request<KnowledgeClaimV1>(`${base}/claims/${citation.claim_id}?revision=${citation.revision}`));
                    const claim=await claims.get(key)!;
                    const rev=claim.revision===citation.revision?claim:null;
                    if(rev){stationIDs.push(rev.subject_id);if(rev.object_entity_id)stationIDs.push(rev.object_entity_id);}
                }
                return {id:m.message_id,kind:'Chat',title:`${m.author_kind==='human'?'Question':'Answer'} · ${m.state}`,body:[m.body??m.reason_code??'Text unavailable',m.snapshot_stale?'Newer knowledge edits are not approved.':'',...(m.corrections??[]).map(c=>`Correction reviewed ${c.reviewed_at} (claim ${c.claim_id.slice(0,8)})`)].filter(Boolean).join('\n'),href:`${link}/today`,stationIDs,time:m.created_at};
            });
        });
        items.push(...chats.flat());
    },undefined);
    if(role==='owner'||role==='supervisor')await read('Corrections',async()=>{
        const corrections=await allPages(async cursor=>{const p=await request<MemoryCandidatePageV1>(`${base}/memory-candidates?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);return {items:p.candidates,next_cursor:p.next_cursor};});
        items.push(...corrections.map(c=>({id:c.candidate_id,kind:'Correction',title:`Correction · ${c.review_state}`,body:c.content.reason,href:`${link}/review`,stationIDs:[c.content.claim?.subject_id,c.content.claim?.object_entity_id].filter((id):id is string=>!!id),time:c.created_at})));
    },undefined);
    return {items,errors};
}

export type MarkerKind = 'case' | 'investigation' | 'proposal_awaiting_approval' | 'task' | 'correction';
export function markerKinds(items:readonly Activity[]):MarkerKind[] {
    const kinds:MarkerKind[]=[];
    for(const a of items){
        if(a.kind==='Case')kinds.push(a.title==='Investigation running'?'investigation':'case');
        else if(a.kind==='Proposal'&&a.title==='Awaiting human approval')kinds.push('proposal_awaiting_approval');
        else if(a.kind==='Task')kinds.push('task');
        else if(a.kind==='Correction')kinds.push('correction');
    }
    return [...new Set(kinds)];
}

export function sceneMarkers(items:readonly Activity[],stations:readonly {id:string;line:string}[]) {
    const seen=new Set<string>();
    return stations.map(s=>{
        const lineMarker=seen.has(s.line)?[]:markerKinds(items.filter(a=>!a.stationIDs.length&&a.line===s.line));seen.add(s.line);
        return {marker:markerKinds(items.filter(a=>a.stationIDs.includes(s.id))),lineMarker};
    });
}
