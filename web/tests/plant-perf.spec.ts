import { expect, test, type Page } from "@playwright/test";

// WIS-711: draw-call and frame-time budget for the 3-D plant view (/sites/{site}/plant).
//
// Page source: like the other stubbed specs, the HTML and bundle come from the real server at
// SUPERVISOR_PUBLIC_ORIGIN (default http://127.0.0.1:58176); only /v1/** is stubbed here. On a host
// without the Go server, tests/plant-perf-server.mjs serves a built web/dist with the same SPA
// fallback and security headers (CSP included) as httpapi.spa()/Boundary().
//
// Renderer: GPU-less headless Chromium, WebGL through SwiftShader (asserted, not assumed).
// Viewport 1280x800 at deviceScaleFactor 1; the canvas is the page's own CSS size.
//
// Review baseline: 48 procedural stations, 24 recorder figures, line-scoped AI markers,
// antialiasing off, Lambert shading on SwiftShader, DPR 1. On this 24-core host:
// 25 draw calls / 23,690 triangles; 120 samples, median 8.0 ms / p95 10.2 ms.
// The unchanged budgets below remain the gates; final measured runs are saved separately.
//
// A frame is one synchronous orbit step: a pointermove drives OrbitControls, whose 'change'
// handler renders, and a 1x1 readPixels forces SwiftShader to finish before the clock stops.
// It is not a rAF interval: the viewport renders only on control changes and resizes.

// Instancing target for 48 stations: one draw per material/geometry batch, not per box.
const MAX_DRAW_CALLS = 400;
// Triangle ceiling for 48 stations once the machine kit lands (kit meshes are richer than boxes).
const MAX_TRIANGLES = 300_000;
// Frame budgets on the CI GPU-less runner (SwiftShader). See the baseline above before changing.
const MAX_MEDIAN_FRAME_MS = 16.7;
const MAX_P95_FRAME_MS = 33;
const FRAMES = 120;
const WARMUP_FRAMES = 20;

const env = (globalThis as typeof globalThis & { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};
test.use({
    viewport: { width: 1280, height: 800 },
    deviceScaleFactor: 1,
    // A test.use launchOptions replaces the config's, so the pinned executable is passed again.
    launchOptions: {
        executablePath: env.PLAYWRIGHT_CHROMIUM_EXECUTABLE,
        args: ["--use-angle=swiftshader", "--enable-unsafe-swiftshader", "--ignore-gpu-blocklist"],
    },
});

const site = "00000000-0000-4000-8000-000000000a01";
const actor = (n: number) => ({ issuer: "https://idp.test", subject: `operator-${n}` });
const LINES = 6, PER_LINE = 8;
// Display names drive the archetype regex in plant-model.ts; templates cover material/generic.
const kinds: [string, string][] = [["Press", "production"], ["CNC", "production"], ["Welding", "production"], ["Assembly", "production"],
    ["Inspection", "quality"], ["Packing", "production"], ["Feeder", "material"], ["Compressor", "production"]];
const id = (n: number) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const lineId = (l: number) => id(1000 + l);
const stations = Array.from({ length: LINES * PER_LINE }, (_, i) => {
    const l = Math.floor(i / PER_LINE), [kind, template] = kinds[(i + l) % kinds.length];
    // Every other station has a started job: its last recorder is drawn as an operator figure.
    return { id: id(2000 + i), line: l, name: `${kind} ${l + 1}-${(i % PER_LINE) + 1}`, template, staffed: i % 2 === 0 };
});
const problem = (status: number, code: string) => ({ status, contentType: "application/problem+json", body: JSON.stringify({ type: "about:blank", title: code, status, code, correlation_id: "test" }) });

async function stubPlant(page: Page) {
    await page.route("**/v1/**", async (route) => {
        const path = new URL(route.request().url()).pathname;
        const base = `/v1/sites/${site}`;
        if (path === "/v1/me") return route.fulfill({ json: { csrf_token: "test-csrf", display_name: "Perf", subject: "perf", sites: [{ id: site, name: "Perf Plant", role: "supervisor", tenant_id: id(1), agent_available: false }] } });
        if (path === `${base}/workstations`) return route.fulfill({ json: { workstations: stations.map((s) => ({ workstation_id: s.id, display_name: s.name, line_id: lineId(s.line), current_revision: 1 })) } });
        if (path === `${base}/lines`) return route.fulfill({ json: { lines: Array.from({ length: LINES }, (_, l) => ({ id: lineId(l), current_revision: 1, body: { active: true, display_name: `Line ${l + 1}`, schema_version: 1 } })) } });
        if (path === `${base}/shift-plans`) return route.fulfill({json:{plans:Array.from({length:LINES},(_,l)=>({id:id(4000+l),line_id:lineId(l),approved_revision:1,current_revision:1,starts_at:'2000-01-01T00:00:00Z',ends_at:'2100-01-01T00:00:00Z'}))}});
        const planDetail=path.match(/\/shift-plans\/([^/]+)$/);
        if(planDetail){const l=Array.from({length:LINES},(_,i)=>i).find(l=>id(4000+l)===planDetail[1]);return route.fulfill({json:{id:planDetail[1],approved_revision:1,revisions:[{revision:1,plan:{assignments:stations.filter(s=>s.line===l&&s.staffed).map(s=>({workstation_id:s.id}))}}]}});}
        if(path===`${base}/cases`)return route.fulfill({json:{items:Array.from({length:LINES},(_,l)=>({id:id(6000+l),line_id:lineId(l),state:l%2?'investigating':'needs_input',detected_at:'2026-01-01T08:00:00Z'}))}});
        if(path===`${base}/proposals`)return route.fulfill({json:{items:Array.from({length:LINES},(_,l)=>({id:id(7000+l),case_id:id(6000+l),state:'pending',expires_at:'2100-01-01T00:00:00Z'}))}});
        if(path===`${base}/tasks`)return route.fulfill({json:{items:[]}});
        if(path===`${base}/chat-conversations`)return route.fulfill({json:{conversations:[]}});
        if(path===`${base}/memory-candidates`)return route.fulfill({json:{candidates:[]}});
        if(path===`${base}/members`)return route.fulfill({json:{items:[]}});
        const m = path.match(/\/workstations\/([^/]+)\/(profile|current-job-order|records)$/);
        const s = stations.find((x) => x.id === m?.[1]);
        if (m && s) {
            const job = id(3000 + stations.indexOf(s)), plan = id(4000 + s.line);
            if (m[2] === "profile") return route.fulfill({ json: { current_revision: 1, replayed: false, revisions: [{ revision: 1, profile: { display_name: s.name, line_id: lineId(s.line), template: s.template, fields: [] } }] } });
            if (m[2] === "current-job-order") return s.staffed
                ? route.fulfill({ json: { job_order_id: job, plan_id: plan, line_id: lineId(s.line), starts_at: "2000-01-01T00:00:00Z", ends_at: "2100-01-01T00:00:00Z", current_revision: 1, order: null } })
                : route.fulfill(problem(404, "NO_CURRENT_JOB_ORDER"));
            const at = "2026-01-01T08:00:00Z";
            const records=s.staffed?[{id:id(5000+stations.indexOf(s)),plan_id:plan,state:'current',event_at:at,received_at:at,recorded_by:actor(stations.indexOf(s)),record:{kind:'started',job_order_id:job}}]:[];
            if(s===stations[0])for(const [n,disposition] of ['hold','accept'].entries())records.push({id:id(9000+n),plan_id:plan,state:'current',event_at:`2026-01-01T08:0${n+1}:00Z`,received_at:at,recorded_by:actor(0),record:{kind:'quality_disposition_recorded',job_order_id:job,disposition} as typeof records[number]['record']});
            return route.fulfill({json:{records}});
        }
        const run = path.match(/\/shift-plans\/([^/]+)\/run$/);
        if (run) return route.fulfill({ json: { plan_id: run[1], state: "started", started_at: "2000-01-01T00:00:00Z", ended_at: null } });
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
}

// Counts every draw the page issues at the WebGL level (equivalent to renderer.info, and still
// correct after instancing), and keeps the context so a frame can be forced to finish.
function installGLProbe() {
    const w = window as unknown as { __gl?: WebGL2RenderingContext; __draws: { calls: number; triangles: number; clears: number } };
    w.__draws = { calls: 0, triangles: 0, clears: 0 };
    const get = HTMLCanvasElement.prototype.getContext;
    HTMLCanvasElement.prototype.getContext = function (this: HTMLCanvasElement, ...a: Parameters<typeof get>) {
        const c = get.apply(this, a);
        if (c && (a[0] === "webgl2" || a[0] === "webgl")) w.__gl = c as WebGL2RenderingContext;
        return c;
    } as typeof get;
    const P = WebGL2RenderingContext.prototype as unknown as Record<string, (...x: number[]) => void>;
    const tri = (mode: number, count: number, instances = 1) => {
        w.__draws.calls++;
        if (mode === WebGL2RenderingContext.TRIANGLES) w.__draws.triangles += Math.floor(count / 3) * instances;
    };
    for (const [name, countArg, instArg] of [["drawArrays", 2, -1], ["drawElements", 1, -1], ["drawArraysInstanced", 2, 3], ["drawElementsInstanced", 1, 4]] as const) {
        const orig = P[name];
        P[name] = function (this: unknown, ...x: number[]) { tri(x[0], x[countArg], instArg < 0 ? 1 : x[instArg]); return orig.apply(this, x); };
    }
    const clear = P.clear;
    P.clear = function (this: unknown, ...x: number[]) { w.__draws.clears++; return clear.apply(this, x); };
}

type Sample = { ms: number[]; calls: number[]; triangles: number[]; clears: number[]; renderer: string };
// Rotates the camera FRAMES+WARMUP_FRAMES times about the target (OrbitControls left-drag) and times
// each synchronous render. A real mouse press gives OrbitControls a captured pointer; the moves are
// dispatched in-page so Playwright's round trip is not part of the frame.
async function orbit(page: Page): Promise<Sample> {
    const canvas = page.locator("canvas.plant-canvas");
    await canvas.scrollIntoViewIfNeeded();
    const box = (await canvas.boundingBox())!;
    const cx = box.x + box.width / 2, cy = box.y + box.height / 2;
    await page.evaluate(canvas_pointer);
    await page.mouse.move(cx, cy);
    await page.mouse.down();
    const out = await page.evaluate(({ n, cx, cy }) => {
        const w = window as unknown as { __gl: WebGL2RenderingContext; __draws: { calls: number; triangles: number; clears: number }; __pointerId: number };
        const c = document.querySelector("canvas.plant-canvas")!, gl = w.__gl, px = new Uint8Array(4);
        const dbg = gl.getExtension("WEBGL_debug_renderer_info");
        const s = { ms: [] as number[], calls: [] as number[], triangles: [] as number[], clears: [] as number[], renderer: String(dbg ? gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER)) };
        for (let i = 1; i <= n; i++) {
            w.__draws.calls = w.__draws.triangles = w.__draws.clears = 0;
            const t0 = performance.now();
            c.dispatchEvent(new PointerEvent("pointermove", { pointerId: w.__pointerId, pointerType: "mouse", isPrimary: true, buttons: 1, clientX: cx + i * 3, clientY: cy, bubbles: true }));
            gl.readPixels(0, 0, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, px);
            s.ms.push(performance.now() - t0);
            s.calls.push(w.__draws.calls); s.triangles.push(w.__draws.triangles); s.clears.push(w.__draws.clears);
        }
        return s;
    }, { n: FRAMES + WARMUP_FRAMES, cx, cy });
    await page.mouse.move(cx + 400, cy + 100); // away from the press point, so pointerup never selects a station
    await page.mouse.up();
    for (const k of ["ms", "calls", "triangles", "clears"] as const) out[k] = out[k].slice(WARMUP_FRAMES);
    return out;
}
// Remembers the real mouse pointerId the canvas receives (OrbitControls captures that id).
function canvas_pointer() {
    document.querySelector("canvas.plant-canvas")!.addEventListener("pointerdown", (e) => { (window as unknown as { __pointerId: number }).__pointerId = (e as PointerEvent).pointerId; }, { capture: true });
}

const pct = (xs: number[], p: number) => { const s = [...xs].sort((a, b) => a - b); return s[Math.min(s.length - 1, Math.ceil(p * s.length) - 1)]; };

async function openPlant(page: Page) {
    const violations: string[] = [], foreign: string[] = [];
    const origin = new URL(test.info().project.use.baseURL ?? "http://127.0.0.1:58176").origin;
    page.on("console", (m) => { if (/Content Security Policy|Refused to/i.test(m.text())) violations.push(m.text()); });
    page.on("request", (r) => { const u = new URL(r.url()); if (/^https?:$/.test(u.protocol) && u.origin !== origin) foreign.push(r.url()); });
    await page.addInitScript(() => document.addEventListener("securitypolicyviolation", (e) => console.error(`Refused to load: CSP ${e.violatedDirective} ${e.blockedURI}`)));
    await stubPlant(page);
    await page.goto(`/sites/${site}/plant`);
    await expect(page.getByRole("heading", { name: "Plant view", exact: true })).toBeVisible();
    await expect(page.getByRole("table", { name: "Plant stations" }).getByRole("row")).toHaveCount(stations.length + 1);
    await expect(page.locator("canvas.plant-canvas")).toBeVisible();
    await expect(page.getByText("3D unavailable")).toHaveCount(0);
    return { violations, foreign };
}

test("WIS-711 plant view GL-level draw calls, triangles and orbit frame time (48 stations)", async ({ page }) => {
    await page.addInitScript(installGLProbe);
    const { violations, foreign } = await openPlant(page);
    const s = await orbit(page);
    expect(s.renderer).toMatch(/SwiftShader/i); // measured on the GPU-less renderer, not a real GPU
    expect(s.clears.every((n) => n === 1), "each orbit step renders exactly one frame").toBe(true);
    const calls = Math.max(...s.calls), triangles = Math.max(...s.triangles), median = pct(s.ms, 0.5), p95 = pct(s.ms, 0.95);
    const result = { renderer: s.renderer, frames: s.ms.length, drawCalls: calls, triangles, medianMs: +median.toFixed(2), p95Ms: +p95.toFixed(2) };
    console.log(`plant-perf ${JSON.stringify(result)}`);
    await test.info().attach("plant-perf.json", { body: JSON.stringify({ ...result, samplesMs: s.ms }, null, 2), contentType: "application/json" });
    expect(calls).toBeGreaterThan(0);
    expect(violations).toEqual([]);
    expect(foreign).toEqual([]);
    expect.soft(calls, `draw calls for ${stations.length} stations`).toBeLessThanOrEqual(MAX_DRAW_CALLS);
    expect.soft(triangles, "triangles").toBeLessThanOrEqual(MAX_TRIANGLES);
    expect.soft(median, "median frame ms").toBeLessThanOrEqual(MAX_MEDIAN_FRAME_MS);
    expect.soft(p95, "p95 frame ms").toBeLessThanOrEqual(MAX_P95_FRAME_MS);
});

test('WIS-711 F2 refresh and replay retain the renderer, camera and keyboard selection',async({page})=>{
    await page.addInitScript(()=>{(globalThis as any).__PLANT_PERF__=true;});
    await openPlant(page);
    const canvas=page.locator('canvas.plant-canvas');await canvas.focus();await canvas.press('ArrowLeft');await canvas.press(']');
    const selected=await page.getByText(/^Selected:/).innerText();
    await page.evaluate(()=>{const w=globalThis as any,v=(document.querySelector('canvas.plant-canvas') as any).__plantView;w.savedPlant={view:v,position:v.camera.position.toArray(),target:v.controls.target.toArray()};});
    await page.route(url=>url.pathname===`/v1/sites/${site}/cases`,route=>route.fulfill({json:{items:[]}}));
    for(const button of ['Refresh now','Replay first record']){
        await page.getByRole('button',{name:button,exact:true}).click();
        await expect(page.locator('.plant-number').filter({hasText:'open cases'}).locator('strong')).toHaveText('0');
        const retained=await page.evaluate(()=>{const saved=(globalThis as any).savedPlant,v=(document.querySelector('canvas.plant-canvas') as any).__plantView;return {same:v===saved.view,position:v.camera.position.toArray(),target:v.controls.target.toArray(),before:saved.position,beforeTarget:saved.target};});
        expect(retained.same).toBe(true);expect(retained.position).toEqual(retained.before);expect(retained.target).toEqual(retained.beforeTarget);
        expect(await page.evaluate(()=>{const v=(document.querySelector('canvas.plant-canvas') as any).__plantView;return v.scene.children.filter((o:any)=>o.isInstancedMesh&&o.geometry.type==='OctahedronGeometry').reduce((n:number,o:any)=>n+o.count,0);})).toBe(0);
        await expect(page.getByText(selected,{exact:true})).toBeVisible();
    }
});

test('WIS-711 F1 F8 recorded holds and latest acceptance are accessible',async({page})=>{
    await openPlant(page);
    await expect(page.locator('.plant-number').filter({hasText:'recorded holds'}).locator('strong')).toHaveText('1');
    const row=page.getByRole('table',{name:'Plant stations'}).getByRole('row').filter({has:page.getByRole('link',{name:stations[0].name,exact:true})});
    await expect(row).toContainText('accept recorded');await expect(row).toContainText('Hold recorded');
    await expect(row.locator('time')).toHaveCount(3);
});

test('WIS-711 F6 canvas instructions and announced selection match the keyboard',async({page})=>{
    await openPlant(page);
    const canvas=page.locator('canvas.plant-canvas');await expect.soft(canvas).toHaveAttribute('aria-label',/Enter selects/);
    await canvas.focus();await canvas.press(']');await expect(page.getByRole('status').filter({hasText:'Selected:'})).toBeVisible();
});

test('WIS-711 F5 software rendering disables antialiasing and F7 case markers draw once per line',async({page})=>{
    await page.addInitScript(()=>{(globalThis as any).__PLANT_PERF__=true;});await openPlant(page);
    const result=await page.evaluate(()=>{const v=(document.querySelector('canvas.plant-canvas') as any).__plantView;return {antialias:v.renderer.getContext().getContextAttributes().antialias,cases:v.scene.children.filter((o:any)=>o.isInstancedMesh&&o.geometry.type==='OctahedronGeometry').reduce((n:number,o:any)=>n+o.count,0)};});
    expect.soft(result.antialias).toBe(false);expect(result.cases).toBe(3);
});

// Cross-check the GL probe against three.js counters exposed only when __PLANT_PERF__ is set.
test("WIS-711 plant view renderer.info budget through the __plantView hook", async ({ page }) => {
    await page.addInitScript(() => { (globalThis as unknown as { __PLANT_PERF__: boolean }).__PLANT_PERF__ = true; });
    const { violations, foreign } = await openPlant(page);
    const s = await page.evaluate(({ n, warm }) => {
        type View = { renderer: { render(s: unknown, c: unknown): void; info: { render: { calls: number; triangles: number } }; getContext(): WebGL2RenderingContext };
            scene: unknown; camera: { position: { sub(v: unknown): unknown; add(v: unknown): unknown; applyAxisAngle(a: unknown, r: number): unknown }; up: unknown }; controls: { target: unknown; update(): void } };
        const v = (document.querySelector("canvas.plant-canvas") as unknown as { __plantView?: View }).__plantView;
        if (!v) throw new Error("FAIL: canvas.__plantView hook missing");
        const gl = v.renderer.getContext(), px = new Uint8Array(4), out = { ms: [] as number[], calls: [] as number[], triangles: [] as number[] };
        for (let i = 0; i < n + warm; i++) {
            v.camera.position.sub(v.controls.target); v.camera.position.applyAxisAngle(v.camera.up, (2 * Math.PI) / n); v.camera.position.add(v.controls.target);
            const t0 = performance.now();
            v.controls.update(); // emits 'change', which renders once
            gl.readPixels(0, 0, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, px);
            if (i < warm) continue;
            out.ms.push(performance.now() - t0); out.calls.push(v.renderer.info.render.calls); out.triangles.push(v.renderer.info.render.triangles);
        }
        return out;
    }, { n: FRAMES, warm: WARMUP_FRAMES });
    expect(Math.min(...s.calls)).toBeGreaterThan(0);
    expect(Math.max(...s.calls)).toBeLessThanOrEqual(MAX_DRAW_CALLS);
    expect(Math.max(...s.triangles)).toBeLessThanOrEqual(MAX_TRIANGLES);
    expect(pct(s.ms, 0.5)).toBeLessThanOrEqual(MAX_MEDIAN_FRAME_MS);
    expect(pct(s.ms, 0.95)).toBeLessThanOrEqual(MAX_P95_FRAME_MS);
    expect(violations).toEqual([]);
    expect(foreign).toEqual([]);
});

test('WIS-711 R2 scene comes first with one toolbar and a keyboard reachable key',async({page})=>{
    await page.setViewportSize({width:1600,height:1100});await openPlant(page);
    const canvas=page.locator('canvas.plant-canvas'),box=(await canvas.boundingBox())!;
    expect.soft(box.y).toBeLessThanOrEqual(260);expect.soft(box.height).toBeGreaterThanOrEqual(600);
    const metrics=(await page.locator('.plant-metrics').boundingBox())!,toolbar=(await page.locator('.plant-controls').boundingBox())!;
    expect(metrics.y+metrics.height).toBeLessThanOrEqual(toolbar.y);expect(toolbar.y+toolbar.height).toBeLessThanOrEqual(box.y);
    expect(toolbar.height).toBeLessThanOrEqual(48);
    for(const name of ['Whole plant','Focus selected station','Table only','Replay first record','Live observations'])await expect(page.locator('.plant-controls').getByRole('button',{name,exact:true})).toBeVisible();
    const slider=(await page.getByRole('slider',{name:'Recorded event'}).boundingBox())!;expect(slider.y).toBeGreaterThanOrEqual(box.y+box.height);
    const key=page.locator('.plant-key summary');await key.focus();await key.press('Enter');
    await expect(page.locator('.plant-key')).toHaveAttribute('open','');await expect(page.locator('.plant-key')).toContainText('Hold recorded');
    await key.press('Enter');await expect(page.locator('.plant-key')).not.toHaveAttribute('open','');
});

test('WIS-711 R2 plant preset makes typical machines and recorder figures readable',async({page})=>{
    await page.setViewportSize({width:1600,height:1100});await page.addInitScript(()=>{(globalThis as any).__PLANT_PERF__=true;});await openPlant(page);
    const scale=await page.evaluate(()=>{
        const canvas=document.querySelector('canvas.plant-canvas') as any,v=canvas.__plantView,height=canvas.getBoundingClientRect().height,machines:number[]=[],people:number[]=[];
        for(const root of v.scene.children){
            const machine=root.children.find((o:any)=>o.userData.kind==='machine');if(!machine)continue;
            const ys:number[]=[];machine.traverse((mesh:any)=>{if(!mesh.isMesh)return;mesh.geometry.computeBoundingBox();const b=mesh.geometry.boundingBox;
                for(const x of [b.min.x,b.max.x])for(const y of [b.min.y,b.max.y])for(const z of [b.min.z,b.max.z])ys.push(mesh.position.clone().set(x,y,z).applyMatrix4(mesh.matrixWorld).project(v.camera).y);
            });machines.push((Math.max(...ys)-Math.min(...ys))*height/2);
        }
        // The orange hard-hat cylinder's top to the ground measures each actual recorder instance.
        for(const mesh of v.scene.children.filter((o:any)=>o.isInstancedMesh&&o.geometry.type==='CylinderGeometry'&&o.material.color.getHex()===0xe69f00))for(let i=0;i<mesh.count;i++){
            const matrix=v.camera.matrix.clone();mesh.getMatrixAt(i,matrix);const top=mesh.position.clone().set(0,0.5,0).applyMatrix4(matrix),foot=top.clone().setY(0);
            people.push(Math.abs(top.project(v.camera).y-foot.project(v.camera).y)*height/2);
        }
        const median=(ns:number[])=>ns.sort((a,b)=>a-b)[Math.floor(ns.length/2)];
        const delta=v.camera.position.clone().sub(v.controls.target);
        return {machine:median(machines),person:median(people),pitch:Math.atan2(delta.y,Math.hypot(delta.x,delta.z))*180/Math.PI};
    });
    console.log(`plant-scale ${JSON.stringify(scale)}`);
    expect.soft(scale.machine).toBeGreaterThanOrEqual(40);expect.soft(scale.person).toBeGreaterThanOrEqual(20);expect(scale.pitch).toBeGreaterThanOrEqual(35);expect(scale.pitch).toBeLessThanOrEqual(45);
});

test('WIS-711 R2 out-of-inventory feed lines use a short honest fallback',async({page})=>{
    await openPlant(page);const outside='75558ba0-c12d-5fd4-8d80-10fa3565c2c9';
    await page.route(url=>url.pathname===`/v1/sites/${site}/cases`,route=>route.fulfill({json:{items:[{id:id(6000),line_id:outside,state:'escalated',detected_at:'2026-01-01T08:00:00Z'}]}}));
    await page.getByRole('button',{name:'Refresh now',exact:true}).click();
    const feed=page.getByRole('complementary',{name:'Current AI activity'});
    await expect(feed).toContainText('Line not in this plant view · 75558ba0');await expect(feed).not.toContainText(outside);
});

// WIS-711 re-review N1: on a phone the canvas must leave room to scroll to the table, and vertical swipes must scroll.
test('WIS-711 phone canvas fits the screen and lets vertical swipes scroll the page',async({page})=>{
    await page.setViewportSize({width:390,height:844});await openPlant(page);
    const canvas=page.locator('canvas.plant-canvas'),box=(await canvas.boundingBox())!;
    expect(box.height).toBeLessThanOrEqual(844*0.6+1);expect(box.height).toBeGreaterThanOrEqual(320);
    expect(await canvas.evaluate(el=>getComputedStyle(el).touchAction)).toBe('pan-y');
});
