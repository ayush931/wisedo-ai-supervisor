import { expect, type Browser, type Page } from "@playwright/test";
import { mkdir, writeFile, rename } from "node:fs/promises";
import { openSite, signIn } from "../tests/session";
const artifacts = process.env.FACILITY_ARTIFACTS;
if (!artifacts) throw new Error("BLOCKED: FACILITY_ARTIFACTS is required (scripts/supervisor/facility.sh sets it)");
export const root = artifacts;
export const origin = "http://127.0.0.1:62376";
export const site = "00000000-0000-4000-8000-000000000291";
export const users = { owner: "foundation-reviewer", planner: "shift-lead", approver: "shift-approver", operator: "operator-1" };
export async function login(browser: Browser, username: string) {
 const context = await browser.newContext({baseURL:origin});const page=await context.newPage();
 await signIn(page,username);await openSite(page);return page;
}
export async function call(page: Page, method: string, path: string, body?: unknown, match?: number) {
 const r=await page.evaluate(async({method,path,body,match,site})=>{
  const me=await(await fetch("/v1/me")).json();
  if(me.sites.length!==1||me.sites[0].id!==site)throw new Error("Unexpected facility site");
  const headers: Record<string,string>={};
  if(method!=="GET"){headers["Content-Type"]="application/json";headers["X-CSRF-Token"]=me.csrf_token;headers["Idempotency-Key"]=crypto.randomUUID();if(match!==undefined)headers["If-Match"]=`"${match}"`;}
  const r=await fetch(`/v1/sites/${site}${path}`,{method,headers,body:body===undefined?undefined:JSON.stringify(body)});
  return {status:r.status,body:await r.json()};
 },{method,path,body,match,site});
 expect([200,201,202],`${method} ${path}: ${r.status} ${r.body?.code||""}`).toContain(r.status);return r.body;
}
export async function save(name: string,value: unknown){await mkdir(root,{recursive:true,mode:0o700});await writeFile(`${root}/${name}`,JSON.stringify(value,null,2)+"\n",{mode:0o600});}
export async function saveSessions(pages: Record<string,Page>){
 const cookies:Record<string,string>={};for(const [role,page]of Object.entries(pages)){const c=(await page.context().cookies(origin)).find(c=>c.name==="supervisor");if(!c)throw new Error("Missing fixture browser session");cookies[role]=c.value;}
 await save(".sessions.json",cookies);await rename(`${root}/.sessions.json`,`${root}/sessions.json`);
}
export function deferred<T>(){let resolve!:(value:T)=>void;const promise=new Promise<T>(r=>resolve=r);return {promise,resolve};}
export const ist=(ms:number)=>new Date(Math.floor(ms/1000)*1000+19800e3).toISOString().slice(0,19)+"+05:30";
