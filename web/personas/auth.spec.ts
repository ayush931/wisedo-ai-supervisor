import { test } from "@playwright/test";
import { login,saveSessions,users } from "./helpers";
test("refresh four real facility fixture sessions",async({browser})=>{
 const entries=await Promise.all(Object.entries(users).map(async([role,user])=>[role,await login(browser,user)] as const));
 try{await saveSessions(Object.fromEntries(entries));}finally{await Promise.all(entries.map(([,p])=>p.context().close()));}
});
