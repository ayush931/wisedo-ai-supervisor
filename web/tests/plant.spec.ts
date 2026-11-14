import { test, expect } from '@playwright/test';
import { openSite, signIn } from './session';

test.use({viewport:{width:1600,height:1100}});

test('WIS-711 real plant inventory, no-data station, read-only offline assets and fallback',async({page,baseURL})=>{
    await page.emulateMedia({reducedMotion:'reduce'});
    await signIn(page,'shift-lead');
    // An isolated build preview may share the facility's identity callback origin.
    await page.goto('/');await openSite(page);await page.waitForLoadState('networkidle');
    const errors:string[]=[],foreign:string[]=[],writes:string[]=[];
    page.on('console',m=>{if(m.type()==='error')errors.push(`${m.location().url}: ${m.text()}`);});
    page.on('pageerror',e=>errors.push(e.message));
    page.on('request',r=>{const u=new URL(r.url());if(u.origin!==new URL(baseURL!).origin)foreign.push(r.url());if(u.pathname.startsWith('/v1/')&&r.method()!=='GET')writes.push(`${r.method()} ${u.pathname}`);});
    await page.evaluate(()=>document.addEventListener('securitypolicyviolation',e=>{(window as unknown as {csp:string[]}).csp??=[];(window as unknown as {csp:string[]}).csp.push(e.violatedDirective);}));
    await page.getByRole('navigation',{name:'Site sections'}).getByRole('link',{name:'Plant view',exact:true}).click();
    await expect(page.getByRole('heading',{name:'Plant view',exact:true})).toBeVisible();
    const inventory=await page.evaluate(async()=>{
        const me=await(await fetch('/v1/me')).json();const site=location.pathname.split('/')[2];
        if(!me.sites.some((s:{id:string})=>s.id===site))throw new Error('Unauthorized site');
        const rows:{workstation_id:string;display_name:string}[]=[];let cursor='';
        do{const p=await(await fetch(`/v1/sites/${site}/workstations?limit=100${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`)).json();rows.push(...p.workstations);cursor=p.next_cursor??'';}while(cursor);
        let empty='';for(const s of rows){const r=await(await fetch(`/v1/sites/${site}/workstations/${s.workstation_id}/records?limit=1`)).json();if(r.records?.length===0){empty=s.display_name;break;}}
        return {rows,empty};
    });
    expect(inventory.rows.length).toBeGreaterThan(0);
    const table=page.getByRole('table',{name:'Plant stations'});
    await expect(table.getByRole('row')).toHaveCount(inventory.rows.length+1,{timeout:60000});
    for(const s of inventory.rows)await expect(table.getByRole('link',{name:s.display_name,exact:true})).toBeVisible();
    expect(inventory.empty,'Seeded backend must include a station with no records').toBeTruthy();
    await expect(table.getByRole('row').filter({has:page.getByRole('link',{name:inventory.empty,exact:true})})).toContainText('No data');
    await expect(page.locator('canvas.plant-canvas')).toBeVisible();
    const toggle=await page.getByRole('button',{name:'Table only',exact:true}).boundingBox();
    const canvasBox=await page.locator('canvas.plant-canvas').boundingBox();
    expect(toggle!.width).toBeLessThan(canvasBox!.width/2);
    await page.locator('canvas.plant-canvas').scrollIntoViewIfNeeded();
    await page.screenshot({path:'../.claude-flow/plant3d-shots/plant-desktop.png'});
    await table.getByRole('button',{name:`Focus ${inventory.empty}`,exact:true}).click();
    await expect(page.getByText(`Selected: ${inventory.empty}`,{exact:true})).toBeVisible();
    await page.locator('canvas.plant-canvas').focus();await page.locator('canvas.plant-canvas').press(']');
    await expect(page.getByText(`Selected: ${inventory.empty}`,{exact:true})).toHaveCount(0);
    await page.getByRole('button',{name:'Whole plant',exact:true}).click();
    if(await page.getByRole('button',{name:'Replay first record'}).isEnabled()){
        await page.getByRole('button',{name:'Replay first record'}).click();
        await expect(page.getByRole('heading',{name:'Recorded activity replay',exact:true})).toBeVisible();
        await expect(page.getByRole('slider',{name:'Recorded event'})).toHaveValue('0');
        await page.getByRole('slider',{name:'Recorded event'}).press('End');
        await page.getByRole('button',{name:'Live observations',exact:true}).click();
        await expect(page.getByRole('heading',{name:'Live observations',exact:true})).toBeVisible();
    }
    await page.getByRole('button',{name:`Focus ${inventory.rows[0].display_name}`,exact:true}).click();
    await page.locator('canvas.plant-canvas').scrollIntoViewIfNeeded();
    await page.screenshot({path:'../.claude-flow/plant3d-shots/plant-station.png'});
    await page.getByRole('button',{name:'Table only',exact:true}).click();
    await expect(page.locator('canvas.plant-canvas')).toHaveCount(0);
    await expect(table.getByRole('row')).toHaveCount(inventory.rows.length+1);
    await page.screenshot({path:'../.claude-flow/plant3d-shots/plant-table.png',fullPage:true});
    expect(await page.evaluate(()=>(window as unknown as {csp?:string[]}).csp??[])).toEqual([]);
    await page.getByRole('button',{name:'Show 3D',exact:true}).click();
    await page.getByRole('button',{name:'Whole plant',exact:true}).click();
    await page.setViewportSize({width:390,height:844});
    await page.locator('canvas.plant-canvas').scrollIntoViewIfNeeded();
    await page.screenshot({path:'../.claude-flow/plant3d-shots/plant-mobile.png'});
    await page.locator('canvas.plant-canvas').dispatchEvent('webglcontextlost');
    await expect(page.getByText('3D unavailable. All plant facts remain in the station table.')).toBeVisible();
    await expect(table.getByRole('row')).toHaveCount(inventory.rows.length+1);
    expect(errors).toEqual([]);expect(foreign).toEqual([]);expect(writes).toEqual([]);
});
