// Labelled API stubs test the actual create/revise form, not database validation.
import { test, expect, type Page } from "@playwright/test";
const site="00000000-0000-4000-8000-000000000211", line="00000000-0000-4000-8000-000000000212", station="00000000-0000-4000-8000-000000000213", item="00000000-0000-4000-8000-000000000214", routing="00000000-0000-4000-8000-000000000215", job="00000000-0000-4000-8000-000000000216", source="00000000-0000-4000-8000-000000000217";
async function setup(page: Page, conflict = false) {
 let conflicted = false;
 const writes: {body:any;headers:Record<string,string>;path:string}[]=[];let saved:any;
 await page.route("**/v1/**",async route=>{
  const path=new URL(route.request().url()).pathname;
  if(path==="/v1/me") return route.fulfill({json:{csrf_token:"stub",subject:"lead",sites:[{id:site,name:"Stubbed routing site",role:"supervisor",tenant_id:source}]}});
  if(path.endsWith("/workstations"))return route.fulfill({json:{workstations:[{workstation_id:station,line_id:line,display_name:"Press",current_revision:1}]}});
  if(path.endsWith("/routings"))return route.fulfill({json:{routings:[{id:routing,current_revision:1,body:{schema_version:1,display_name:"Press route",active:true,line_id:line,item_id:item,steps:[{step_number:1,workstation_id:station}]}}]}});
  if(path.endsWith("/items"))return route.fulfill({json:{items:[{id:item,current_revision:1,body:{schema_version:1,display_name:"Press part",active:true,unit:"count"}}]}});
  if(route.request().method()==="POST"){
   const body=route.request().postDataJSON(),headers=route.request().headers();writes.push({body,headers,path});
   if(conflict && !conflicted && path.endsWith("/revisions")) {
    conflicted=true; saved={...saved,current_revision:2,current_order:{...saved.current_order,target_quantity:60}};
    return route.fulfill({status:412,contentType:"application/problem+json",json:{status:412,code:"REVISION_CONFLICT",correlation_id:"stub"}});
   }
   saved={id:job,current_revision:writes.length,current_order:body};return route.fulfill({status:201,json:saved});
  }
  if(path.endsWith("/job-orders"))return route.fulfill({json:{job_orders:saved?[saved]:[]}});
  return route.fulfill({json:{}});
 });
 await page.goto(`/sites/${site}/job-orders`);
 await expect(page.getByRole("heading",{name:"Job orders",exact:true})).toBeVisible();
 await page.getByRole("button",{name:"New job order",exact:true}).click();
 await page.getByLabel("Instruction document ID",{exact:true}).fill(source);
 await page.getByLabel("Instruction SHA-256").fill("a".repeat(64));
 await page.getByLabel("Cycle time (seconds)").fill("3");
 return writes;
}
test("stubbed create and revise preserve the optional routing step and revision precondition",async({page})=>{
 const writes=await setup(page);
 await page.getByRole("combobox",{name:"Routing (optional)",exact:true}).selectOption(routing);
 await expect(page.getByRole("combobox",{name:"Item",exact:true})).toHaveValue(item);
 await page.getByRole("button",{name:"Create job order",exact:true}).click();
 expect(writes).toEqual([]);
 await expect(page.getByRole("heading",{name:"Create job order",exact:true})).toBeVisible();
 await page.getByRole("combobox",{name:"Routing step",exact:true}).selectOption("1");
 await page.getByRole("button",{name:"Create job order",exact:true}).click();
 await expect(page.getByRole("status").filter({hasText:"saved at revision"})).toContainText("saved at revision 1");
 expect(writes).toHaveLength(1);expect(writes[0].body).toMatchObject({item_id:item,routing_id:routing,step_number:1,item_code:"Press part"});expect(writes[0].headers["idempotency-key"]).toBeTruthy();
 await page.getByRole("button",{name:/Revise /}).click();
 await expect(page.getByRole("combobox",{name:"Routing (optional)",exact:true})).toHaveValue(routing);
 await expect(page.getByRole("combobox",{name:"Routing step",exact:true})).toHaveValue("1");
 await page.getByLabel("Target quantity",{exact:true}).fill("25");
 await page.getByRole("button",{name:"Save job revision"}).click();
 await expect(page.getByRole("status").filter({hasText:"saved at revision"})).toContainText("saved at revision 2");
 expect(writes).toHaveLength(2);expect(writes[1].headers["if-match"]).toBe('"1"');expect(writes[1].body).toMatchObject({...writes[0].body,target_quantity:25});
});
test("stubbed manual job omits all three routing fields; unlinking omits them on revise",async({page})=>{
 const writes=await setup(page);
 await page.getByRole("combobox",{name:"Routing (optional)",exact:true}).selectOption(routing);
 await page.getByRole("combobox",{name:"Routing step",exact:true}).selectOption("1");
 await page.getByRole("combobox",{name:"Routing (optional)",exact:true}).selectOption("");
 await page.getByLabel("Item code",{exact:true}).fill("LEGACY-42");
 await page.getByRole("button",{name:"Create job order",exact:true}).click();
 await expect(page.getByRole("status").filter({hasText:"saved at revision"})).toContainText("saved at revision 1");
 expect(writes[0].body.item_id).toBeUndefined();expect(writes[0].body.routing_id).toBeUndefined();expect(writes[0].body.step_number).toBeUndefined();expect(writes[0].body.item_code).toBe("LEGACY-42");
 await page.getByRole("button",{name:/Revise /}).click();
 await page.getByRole("combobox",{name:"Routing (optional)",exact:true}).selectOption(routing);await page.getByRole("combobox",{name:"Routing step",exact:true}).selectOption("1");
 await page.getByRole("button",{name:"Save job revision"}).click();await expect(page.getByRole("status").filter({hasText:"saved at revision"})).toContainText("saved at revision 2");
 await page.getByRole("button",{name:/Revise /}).click();await page.getByRole("combobox",{name:"Routing (optional)",exact:true}).selectOption("");
 await page.getByRole("button",{name:"Save job revision"}).click();await expect(page.getByRole("status").filter({hasText:"saved at revision"})).toContainText("saved at revision 3");
 expect(writes[2].body.item_id).toBeUndefined();expect(writes[2].body.routing_id).toBeUndefined();expect(writes[2].body.step_number).toBeUndefined();expect(writes[2].body.item_code).toBe("LEGACY-42");expect(writes[2].headers["if-match"]).toBe('"2"');
});

test("stubbed linked job keeps its human code through create, display and revision",async({page})=>{
 const writes=await setup(page);
 await page.getByLabel("Item code",{exact:true}).fill("SKU-PRESS-42");
 await page.getByRole("combobox",{name:"Routing (optional)",exact:true}).selectOption(routing);
 await page.getByRole("combobox",{name:"Routing step",exact:true}).selectOption("1");
 await expect(page.getByLabel("Item code",{exact:true})).toHaveValue("SKU-PRESS-42");
 await page.getByRole("button",{name:"Create job order",exact:true}).click();
 await expect(page.getByRole("cell",{name:"SKU-PRESS-42",exact:true})).toBeVisible();
 expect(writes[0].body).toMatchObject({item_id:item,item_code:"SKU-PRESS-42",routing_id:routing,step_number:1});
 await page.getByRole("button",{name:/Revise /}).click();
 await expect(page.getByRole("combobox",{name:"Item",exact:true})).toHaveValue(item);
 await page.getByLabel("Item code",{exact:true}).fill("SKU-PRESS-43");
 await page.getByRole("button",{name:"Save job revision"}).click();
 await expect(page.getByRole("cell",{name:"SKU-PRESS-43",exact:true})).toBeVisible();
 expect(writes[1].body).toMatchObject({item_id:item,item_code:"SKU-PRESS-43",routing_id:routing,step_number:1});
 expect(writes[1].headers["if-match"]).toBe('"1"');
});

test("stubbed stale revision requires reloading the current job before another save",async({page})=>{
 const writes=await setup(page,true);
 await page.getByRole("combobox",{name:"Routing (optional)",exact:true}).selectOption(routing);
 await page.getByRole("combobox",{name:"Routing step",exact:true}).selectOption("1");
 await page.getByRole("button",{name:"Create job order",exact:true}).click();
 await expect(page.getByRole("status").filter({hasText:"saved at revision"})).toContainText("saved at revision 1");
 await page.getByRole("button",{name:/Revise /}).click();await page.getByLabel("Target quantity",{exact:true}).fill("25");
 await page.getByRole("button",{name:"Save job revision"}).click();
 await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText("This job order changed. Cancel and reopen it");
 expect(writes).toHaveLength(2);expect(writes[1].headers["if-match"]).toBe('"1"');
 await expect(page.getByLabel("Target quantity",{exact:true})).toHaveValue("25");
 await page.getByRole("button",{name:"Cancel",exact:true}).click();await page.getByRole("button",{name:/Revise /}).click();
 await expect(page.getByLabel("Target quantity",{exact:true})).toHaveValue("60");
 await page.getByLabel("Target quantity",{exact:true}).fill("25");await page.getByRole("button",{name:"Save job revision"}).click();
 await expect(page.getByRole("status").filter({hasText:"saved at revision"})).toContainText("saved at revision 3");
 expect(writes).toHaveLength(3);expect(writes[2].headers["if-match"]).toBe('"2"');
});
