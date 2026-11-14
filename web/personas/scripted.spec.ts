import { test, expect, type Page } from "@playwright/test";
import { randomUUID } from "node:crypto";
import { clientNavigate, section } from "../tests/session";
import { call,deferred,ist,login,origin,root,save,site,users } from "./helpers";
import type { JobOrderBodyV1, ShiftPlanV1, StationProfileV1 } from "../../contracts/typescript/oee";

test("WIS-709 concurrent scripted facility personas",async({browser})=>{
 // Letters only: the tag reaches chat as cited SOP text, and a hex tag such as 265c13e1 reads as an exponent,
 // which chat's number guard refuses (STATEMENT_REJECTED). Same cause as the scenario machine name (WIS-709).
 const tag=randomUUID().replace(/-/g,"").slice(0,8).replace(/[0-9a-f]/g,c=>"abcdefghijklmnop"[parseInt(c,16)]);
 expect(tag,"persona tag must be letters only").toMatch(/^[a-p]{8}$/);
 const roles={...users,maintenance:users.planner,quality:users.operator,corrector:users.operator};
 const pages=Object.fromEntries(await Promise.all(Object.entries(roles).map(async([role,user])=>[role,await login(browser,user)] as const)));
 const unexpected:string[]=[], expectedHTTP:string[]=[], expectedConsole:string[]=[], knownProductDefects:string[]=[];
 // Known product defect on main 94dba219 (WIS-707): the server's CSP (style-src 'self') blocks the bundle's inline
 // styles. Each occurrence is labelled under known_product_defects AND fails the gate until the product fix
 // (PR #306, fix/WIS-707-csp-inline-styles) is merged; it is never allowed through.
 const cspInlineStyle=/^Applying inline style violates the following Content Security Policy directive 'style-src 'self''\./;
 // document-authority.tsx treats absent decisions as normal; ui.tsx lookup explicitly falls back on 404 for display names.
 const allowed404=(path:string)=>/\/(authority|currentness)$/.test(path)||/\/(entities|lines)\/[0-9a-f-]+$/.test(path);
 for(const [role,page]of Object.entries(pages)){
  page.on("pageerror",e=>unexpected.push(`${role}: ${e.message}`));
  page.on("response",r=>{if(new URL(r.url()).pathname.startsWith("/v1/")&&r.status()>=400){
   const text=`${role}: ${r.request().method()} ${new URL(r.url()).pathname} ${r.status()}`;
   if(r.status()===404&&r.request().method()==="GET"&&allowed404(new URL(r.url()).pathname))expectedHTTP.push(text);else unexpected.push(text);
  }});
  // Chromium also logs each failed resource load; only the allowlisted 404s above are expected.
  page.on("console",m=>{if(m.type()!=="error")return;const at=m.location().url,path=at?new URL(at,origin).pathname:"";const text=`${role}: console error: ${m.text()} ${path}`;
   if(/^Failed to load resource: the server responded with a status of 404\b/.test(m.text())&&path.startsWith("/v1/")&&allowed404(path))expectedConsole.push(text);
   else{if(cspInlineStyle.test(m.text()))knownProductDefects.push(text);unexpected.push(text);}});
 }
 const journal:{role:string;action:string;at:string}[]=[];
 const mark=(role:string,action:string)=>journal.push({role,action,at:new Date().toISOString()});
 const planReady=deferred<{plan:ShiftPlanV1;stations:string[];names:string[]}>();
 const approved=deferred<void>(),running=deferred<void>(),paused=deferred<void>(),maintained=deferred<void>(),operated=deferred<void>(),inspected=deferred<void>(),documentReady=deferred<{source:string;span:string}>(),claimReady=deferred<string>(),baselineReady=deferred<{source:string;span:string}>();
 const revisedReady=deferred<{source:string;entity:string}>(),correctionSubmitted=deferred<string>(),correctionApproved=deferred<void>(),snapshotPublished=deferred<void>(),baselinePublished=deferred<void>();
 // The pilot fixture answers from the first captured source and pins the first approved fact, each by ID order.
 // The baseline source and claim sort first (00000001-…, after the edge case's revoked 00000000-0000-0000-… IDs),
 // so the corrector's question gets an answered reply deterministically on a fresh facility.
 const firstSorting=()=>"00000001-"+randomUUID().slice(9);
 const baselineText=`Persona press ${tag} cycle time: 45 seconds. Fictional facility manual, reference copy.`;
 const stationPath=(id:string)=>`/sites/${site}/workstations/${id}`;
 const receipt=(p:Page,label:string)=>p.getByRole("status").filter({hasText:`Recorded: ${label}`});
 const toolbar=(p:Page)=>p.getByRole("toolbar",{name:"Record"});
 const tasks=[
  (async()=>{const p=pages.planner;mark("planner","begin");
   const stations=await call(p,"GET","/workstations");const seed=stations.workstations.find((w:{display_name:string})=>w.display_name==="press-01");expect(seed).toBeTruthy();
   const profile:StationProfileV1=await call(p,"GET",`/workstations/${seed.workstation_id}/profile`);
   let cursor="",order:JobOrderBodyV1|undefined;
   for(let i=0;i<50&&!order;i++){const list=await call(p,"GET",`/job-orders?limit=100&include=current${cursor?`&cursor=${cursor}`:""}`);order=list.job_orders.find((j:{current_order?:JobOrderBodyV1})=>j.current_order?.item_code==="Bracket A-100, op 10 Press")?.current_order;if(!list.next_cursor)break;cursor=list.next_cursor;}
   if(!order)throw new Error("BLOCKED: facility demo job absent");
   const line=randomUUID(),ids:string[]=[],names:string[]=[],assignments=[];
   for(let i=0;i<2;i++){
    const ws=randomUUID(),name=`Facility ${tag} station ${i+1}`;ids.push(ws);names.push(name);
    await call(p,"POST",`/workstations/${ws}/profile-revisions`,{...profile.revisions[profile.current_revision-1].profile,workstation_id:ws,line_id:line,display_name:name},0);
    const {routing_id:_r,step_number:_s,item_id:_i,due_at:_d,...copy}=order;
    const body={...copy,order_id:randomUUID(),operation_id:randomUUID(),item_code:`Facility persona ${tag}-${i}`};
    const job=await call(p,"POST","/job-orders",body);
    assignments.push({workstation_id:ws,job_order_id:job.id,job_order_revision:1,...body});
    delete (assignments[i] as Record<string,unknown>).schema_version;
    delete (assignments[i] as Record<string,unknown>).priority;
   }
   const plan:ShiftPlanV1=await call(p,"POST","/shift-plans",{schema_version:1,line_id:line,starts_at:ist(Date.now()-60_000),ends_at:ist(Date.now()+3600_000),timezone:"Asia/Kolkata",assignments});
   for(const actor of [pages.operator,pages.maintenance]){
    const subject=await actor.evaluate(async()=> (await(await fetch("/v1/me")).json()).subject);
    for(const ws of ids)await call(pages.approver,"POST",`/workstations/${ws}/authorizations`,{grantee_issuer:plan.revisions[0].submitted_by.issuer,grantee_subject:subject,action:"grant"},0);
   }
   planReady.resolve({plan,stations:ids,names});mark("planner","submitted plan");
   await approved.promise;await clientNavigate(p,`/sites/${site}/shifts/${plan.id}`);
   await p.getByRole("button",{name:"Start shift",exact:true}).click();await expect(p.getByRole("status").filter({hasText:/Started shift on plan r1/})).toBeVisible();mark("planner","started shift");running.resolve();
   const doc=await documentReady.promise,baseline=await baselineReady.promise;
   const entity=randomUUID(),claim=firstSorting();
   await call(p,"POST","/entities",{entity_id:entity,type:"asset",asset_kind:"machine",display_name:`Persona press ${tag}`,source_id:doc.source,source_revision:1},0);
   await call(p,"POST","/claims",{claim_id:claim,subject_id:entity,subject_revision:1,predicate:"cycle_time_seconds",literal:{type:"decimal",value:"45",unit:"s"},effective_from:"2026-09-01T00:00:00Z",evidence_class:"report",source_span_ids:[baseline.span]},0);claimReady.resolve(claim);
   await Promise.all([operated.promise,inspected.promise]);
   await p.getByRole("button",{name:"End shift",exact:true}).click();await p.getByRole("form",{name:"End this shift"}).getByRole("button",{name:"End shift",exact:true}).click();
   await expect(p.getByRole("status").filter({hasText:/Ended shift · summary r1 · handover draft r1/})).toBeVisible();await expect(p.getByRole("heading",{name:/^Handover r1/})).toBeVisible();mark("planner","summary and handover read");
  })(),
  (async()=>{const p=pages.approver;mark("approver","begin");const {plan}=await planReady.promise;await clientNavigate(p,`/sites/${site}/shifts/${plan.id}`);
   await p.getByRole("button",{name:/^Approve r1/}).click();await p.getByRole("dialog").getByRole("button",{name:/^Approve r1/}).click();await expect(p.getByText(/Approved r1/)).toBeVisible();mark("approver","approved exact scripted revision");approved.resolve();
   // WIS-703: a different person reviews the operator's correction; approval does not publish it.
   const candidate=await correctionSubmitted.promise;await section(p,"Facts to review");const card=p.getByRole("article",{name:`Correction ${candidate.slice(0,8)}`});
   await expect(card).toContainText("50 s");await card.getByLabel("Correction review reason").fill("Scripted fixture checked the revised fictional SOP");await card.getByRole("button",{name:"Approve correction"}).click();
   await expect(card).toContainText("Approved — awaiting snapshot");mark("approver","approved operator correction (not published)");correctionApproved.resolve();
  })(),
  (async()=>{const p=pages.operator;mark("operator","begin");const {stations}=await planReady.promise;await running.promise;await clientNavigate(p,stationPath(stations[0]));
   for(const [button,label]of [["Acknowledge","Acknowledged"],["Start","Started"]]){await toolbar(p).getByRole("button",{name:button,exact:true}).click();await expect(receipt(p,label)).toBeVisible();}
   await p.getByRole("button",{name:/\+ Good/}).click();await p.getByRole("button",{name:"Add 1 good",exact:true}).click();await p.getByRole("button",{name:/^Record/}).click();await expect(receipt(p,"Quantity")).toBeVisible();
   await toolbar(p).getByRole("button",{name:/Pause/}).click();await p.getByLabel("Reason",{exact:true}).fill("SYNTHETIC tool inspection stop");await p.getByRole("button",{name:/^Record/}).click();await expect(receipt(p,"Paused")).toBeVisible();mark("operator","count and stop recorded");paused.resolve();
   await maintained.promise;await toolbar(p).getByRole("button",{name:/Resume/}).click();await expect(receipt(p,"Resumed")).toBeVisible();mark("operator","resumed");operated.resolve();
  })(),
  (async()=>{const p=pages.maintenance;mark("maintenance","begin (supervisor fixture)");const {stations}=await planReady.promise;await paused.promise;await clientNavigate(p,stationPath(stations[0]));
   await toolbar(p).getByRole("button",{name:/Note/}).click();await p.getByRole("textbox",{name:"Note",exact:true}).fill("SYNTHETIC tooling inspected; observation only");await p.getByRole("button",{name:/^Record/}).click();await expect(receipt(p,"Note")).toBeVisible();mark("maintenance","inspection note recorded");maintained.resolve();
  })(),
  (async()=>{const p=pages.quality;mark("quality","begin (operator fixture)");const {stations}=await planReady.promise;await running.promise;await clientNavigate(p,stationPath(stations[1]));
   for(const [button,label]of [["Acknowledge","Acknowledged"],["Start","Started"]]){await toolbar(p).getByRole("button",{name:button,exact:true}).click();await expect(receipt(p,label)).toBeVisible();}
   await p.getByRole("button",{name:"Quality issue",exact:true}).click();await p.getByRole("textbox",{name:"Note",exact:true}).fill("SYNTHETIC Quality issue: hold for gauge calibration review");await p.getByRole("button",{name:/^Record/}).click();await expect(receipt(p,"Note")).toBeVisible();mark("quality","quality issue observation recorded; no release authority");inspected.resolve();
  })(),
  (async()=>{const p=pages.owner;mark("owner","begin");const title=`SYNTHETIC persona SOP ${tag}`,text=`Persona press ${tag} cycle time: 45 seconds. Fictional facility manual.`;
   await section(p,"Documents");const upload=p.getByRole("form",{name:"Upload a document"});await upload.getByRole("textbox",{name:/^Title\b/}).fill(title);await upload.getByLabel("File",{exact:true}).setInputFiles({name:"persona-sop.txt",mimeType:"text/plain",buffer:Buffer.from(text)});
   const uploaded=p.waitForResponse(r=>r.request().method()==="POST"&&new URL(r.url()).pathname.endsWith("/source-documents"));await p.getByRole("button",{name:"Upload",exact:true}).click();const response=await uploaded;expect(response.status()).toBe(200);const doc=await response.json();
   const panel=p.getByRole("group",{name:`Authority of ${title}`});await panel.getByLabel("Decision reason").fill("Scripted fixture checked fictional SOP");await panel.getByRole("button",{name:"Mark authoritative"}).click();await expect(panel.getByRole("status").filter({hasText:/\S/})).toHaveText(/^Marked authoritative/);
   await panel.getByLabel("Decision reason").fill("Scripted current revision");await panel.getByRole("button",{name:"Confirm current"}).click();await expect(panel.getByRole("status").filter({hasText:/\S/})).toHaveText(/^Confirmed current/);documentReady.resolve({source:doc.source.source_id,span:doc.spans[0].id});
   const base=await call(p,"POST","/sources",{source_id:firstSorting(),kind:"document",title:`SYNTHETIC persona SOP ${tag} reference`,text:baselineText},0);
   const baseSpan=await call(p,"POST",`/sources/${base.source_id}/spans`,{revision:1,start_char:0,end_char:Array.from(baselineText).length},1);
   const baseAuthority=await call(p,"POST",`/sources/${base.source_id}/authority-decisions`,{source_revision:1,expected_source_state_revision:1,verdict:"designate",reason:"Scripted fixture checked fictional SOP reference copy"},0);
   await call(p,"POST",`/sources/${base.source_id}/currentness-decisions`,{source_revision:1,expected_source_state_revision:1,authority_revision:1,authority_decision_id:baseAuthority.decision_id,verdict:"confirm_current",reason:"Scripted current revision"},0);
   baselineReady.resolve({source:base.source_id,span:baseSpan.id});
   const revised=await call(p,"POST","/sources",{source_id:randomUUID(),kind:"document",title:`SYNTHETIC persona SOP ${tag} revision`,text:`Persona press ${tag} B cycle time: 50 seconds. Revised fictional SOP.`},0);
   const authority=await call(p,"POST",`/sources/${revised.source_id}/authority-decisions`,{source_revision:1,expected_source_state_revision:1,verdict:"designate",reason:"Scripted fixture checked revised fictional SOP"},0);
   await call(p,"POST",`/sources/${revised.source_id}/currentness-decisions`,{source_revision:1,expected_source_state_revision:1,authority_revision:1,authority_decision_id:authority.decision_id,verdict:"confirm_current",reason:"Scripted current revision"},0);
   // A separate entity, so the corrected fact cannot conflict with the 45-second claim above.
   const corrected=randomUUID();await call(p,"POST","/entities",{entity_id:corrected,type:"asset",asset_kind:"machine",display_name:`Persona press ${tag} B`,source_id:revised.source_id,source_revision:1},0);
   revisedReady.resolve({source:revised.source_id,entity:corrected});
   const claim=await claimReady.promise;await section(p,"Facts to review");await expect(p.getByRole("list",{name:"Candidate facts"})).toBeVisible();const item=p.getByRole("listitem",{name:`Candidate fact ${claim.slice(0,8)}`});
   for(let i=0;i<40&&await item.count()===0;i++){await p.getByRole("button",{name:"Load more candidate facts"}).click();await expect(p.getByRole("button",{name:"Loading…"})).toHaveCount(0);}
   const card=item.getByRole("article");await expect(card.locator("blockquote")).toHaveText(baselineText);await card.getByLabel("Review reason").fill("Scripted exact SOP comparison");await card.getByRole("button",{name:"Approve",exact:true}).click();await expect(card.getByRole("status").filter({hasText:/\S/})).toHaveText(/^Approved\. Review/);
   const publish=async(reason:string)=>{await section(p,"Approved process");const form=p.getByRole("form",{name:"Publish approved knowledge"});await form.getByLabel("Snapshot reason").fill(reason);await form.getByRole("button",{name:"Publish snapshot"}).click();await expect(form.getByRole("status").filter({hasText:/\S/})).toHaveText(/^Snapshot .* published at knowledge revision/);};
   await publish("Scripted fixture reviewed knowledge");mark("owner","reviewed fact and published snapshot");baselinePublished.resolve();
   await correctionApproved.promise;await publish("Scripted fixture published the reviewed correction");mark("owner","published the reviewed correction");snapshotPublished.resolve();
  })(),
  (async()=>{const p=pages.corrector;mark("corrector","begin (operator fixture)");const {source,entity}=await revisedReady.promise;await baselinePublished.promise;
   // WIS-703 correction journey: operator flags an answer and proposes a sourced fact; it changes nothing until reviewed and published.
   await clientNavigate(p,`/sites/${site}/today`);await p.getByLabel("Your question").fill(`What is the cycle time of Persona press ${tag}?`);await p.getByRole("button",{name:"Ask",exact:true}).click();
   const wrong=p.getByRole("button",{name:"This is wrong"});await expect(wrong).toBeVisible({timeout:90_000});await wrong.click();
   await p.getByRole("textbox",{name:"Correction reason",exact:true}).fill("SYNTHETIC: the answer omits press B; its revised fictional SOP states 50 seconds.");
   await p.getByLabel("Expected factual answer (optional)").fill(`The Persona press ${tag} B cycle time is 50 seconds.`);
   await p.getByLabel("Add a proposed fact and source").check();await p.getByRole("combobox",{name:"Fact to correct",exact:true}).selectOption("new");
   await p.getByRole("combobox",{name:"Subject entity",exact:true}).selectOption(entity);await p.getByRole("textbox",{name:"Correct value (exact decimal)"}).fill("50");
   await p.getByRole("combobox",{name:"Evidence source",exact:true}).selectOption(source);
   const submission=p.waitForResponse(r=>r.request().method()==="POST"&&new URL(r.url()).pathname.endsWith("/feedback"));await p.getByRole("button",{name:"Submit correction"}).click();
   const submitted=await submission;expect(submitted.status()).toBe(201);const candidate:string=(await submitted.json()).candidate_id;
   await expect(p.getByRole("article",{name:`Correction ${candidate.slice(0,8)}`})).toBeVisible();mark("corrector","correction submitted");correctionSubmitted.resolve(candidate);
   await snapshotPublished.promise;const saved=await call(pages.owner,"GET",`/memory-candidates/${candidate}`);expect(saved.review_state).toBe("approved");expect(saved.published_snapshot_id).toBeTruthy();mark("corrector","correction published by the owner's snapshot");
  })(),
 ];
 let failure="";
 try{await Promise.all(tasks);expect(unexpected).toEqual([]);}catch(e){failure=String(e);throw e;}finally{
  await Promise.all(Object.entries(pages).map(async([role,p])=>{await p.screenshot({path:`${root}/${role}.png`,fullPage:true}).catch(()=>{});}));
  await save("scripted-report.json",{schema_version:1,synthetic:true,approvals:"scripted fixture identities, not independent human review",passed:!failure,unexpected_errors:unexpected,expected_http:expectedHTTP,expected_console:expectedConsole,known_product_defects:knownProductDefects,failure,journal,role_mapping:roles,limitations:["The correction journey checks submission, independent review and publication; it does not ask again for the corrected answer, because the pilot fixture provider answers from the first captured source, not the corrected fact (web/tests/feedback.spec.ts covers that on an isolated site)."]});
  await Promise.all(Object.values(pages).map(p=>p.context().close()));
 }
});
