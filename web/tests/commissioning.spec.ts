import {
    expect,
    test,
    type Page,
    type Request,
    type Route,
} from "@playwright/test";
import { appRegions } from "./regions";

// WIS-644: Setup → Processes lets an owner register an area and a process, each citing a source,
// and start the value-stream checklist of a process. The API is stubbed (UI behaviour); the
// server's role, recent-sign-in, revision and idempotency checks are covered by the knowledge and
// discovery integration tests, and the real flow by the browser pilot (CASE=onboarding-pilot).

const site = "00000000-0000-4000-8000-000000000644";
const sop = "00000000-0000-4000-8000-0000000006a1";
const meFor = (role: string) => ({
    csrf_token: "test-csrf",
    display_name: "Stubbed member",
    subject: "member-subject",
    sites: [
        {
            id: site,
            name: "Stubbed Site",
            role,
            tenant_id: "00000000-0000-4000-8000-000000000640",
            agent_available: true,
        },
    ],
});
const problem = (status: number, code: string) => ({
    status,
    contentType: "application/problem+json",
    body: JSON.stringify({ code, status, title: code, type: "about:blank" }),
});
const source = (id: string, title: string, kind: string, revision: number) => ({
    source_id: id,
    title,
    kind,
    revision,
    state_revision: revision,
    access_epoch: 1,
    authority: "unverified",
    authority_class: "reported",
    byte_length: 10,
    rune_length: 10,
    sha256: "0".repeat(64),
    independence_group_id: id,
    model_extracted: false,
});
const entity = (b: Record<string, unknown>) => ({
    ...b,
    revision: 1,
    current_revision: 1,
    state: "active",
    sources_current: true,
    actor_issuer: "i",
    actor_subject: "member-subject",
    created_at: "2026-11-08T09:00:00Z",
});

type Reply = (
    r: Request,
) => Parameters<Route["fulfill"]>[0] | "abort" | undefined;
type Stub = {
    sources: ReturnType<typeof source>[];
    entities: ReturnType<typeof entity>[];
    inventories: Record<string, unknown>[];
    posts: Request[];
    reads: string[];
};

// open serves a small in-memory site. A hook may answer a POST itself (a refusal or a lost
// answer); otherwise the POST is applied and its receipt returned, as the server would.
async function open(
    page: Page,
    role: string,
    hook: Reply = () => undefined,
): Promise<Stub> {
    const s: Stub = {
        sources: [source(sop, "Bracket SOP", "document", 1)],
        entities: [],
        inventories: [],
        posts: [],
        reads: [],
    };
    await page.route("**/v1/**", async (route) => {
        const r = route.request();
        const path = new URL(r.url()).pathname;
        if (path === "/v1/me") return route.fulfill({ json: meFor(role) });
        const base = `/v1/sites/${site}`;
        if (r.method() === "GET") {
            s.reads.push(path);
            if (path === `${base}/entities`)
                return route.fulfill({ json: { entities: s.entities } });
            if (path === `${base}/discovery-inventories`)
                return route.fulfill({ json: { inventories: s.inventories } });
            if (path === `${base}/sources`)
                return route.fulfill({ json: { sources: s.sources } });
            return route.fulfill(problem(404, "NOT_FOUND"));
        }
        s.posts.push(r);
        const own = hook(r);
        if (own === "abort") return route.abort("connectionreset");
        if (own) return route.fulfill(own);
        const b = JSON.parse(r.postData() ?? "{}");
        if (path === `${base}/sources`) {
            const v = source(b.source_id, b.title, b.kind, 1);
            s.sources.push(v);
            return route.fulfill({ json: { ...v, text: b.text } });
        }
        if (path === `${base}/entities`) {
            const v = entity(b);
            s.entities.push(v);
            return route.fulfill({ json: v });
        }
        if (path === `${base}/discovery-inventories`) {
            const process = s.entities.find(
                (e) => e.entity_id === b.process_id,
            ) as { display_name: string } | undefined;
            s.inventories.push({
                inventory_id: b.inventory_id,
                process_id: b.process_id,
                process_display_name: process?.display_name,
                capability: b.capability,
                current_revision: 1,
                created_at: "",
                updated_at: "",
            });
            return route.fulfill({
                json: {
                    ...b,
                    revision: 1,
                    current_revision: 1,
                    requirements: [{}, {}, {}],
                    scope_current: true,
                },
            });
        }
        return route.fulfill(problem(404, "NOT_FOUND"));
    });
    await page.goto(`/sites/${site}/processes`);
    await expect(
        page.getByRole("heading", { name: "Processes", level: 1 }),
    ).toBeVisible();
    return s;
}

const body = (r: Request) => JSON.parse(r.postData() ?? "{}");
const at = (s: Stub, suffix: string) =>
    s.posts.filter((r) => new URL(r.url()).pathname.endsWith(suffix));

async function addProcess(page: Page, name: string, cite: string) {
    const form = page.getByRole("form", { name: "Add an area or process" });
    await form.getByLabel("Process", { exact: true }).check();
    await form.getByLabel("Name").fill(name);
    await form.getByLabel("Cites").selectOption({ label: cite });
    await form.getByRole("button", { name: "Add process" }).click();
}

test("stubbed owner registers a process citing the SOP, an area from a note, and starts the checklist", async ({
    page,
}) => {
    const s = await open(page, "owner");
    const add = page.getByRole("form", { name: "Add an area or process" });
    await expect(
        add.getByRole("button", { name: "Add process" }),
    ).toBeDisabled(); // a name and a source are required

    await addProcess(page, "Bracket process", "Bracket SOP · document · r1");
    await expect(add.getByRole("status").filter({ hasText: /\S/ })).toHaveText(
        'Process "Bracket process" added (revision 1).',
    );
    const [proc] = at(s, "/entities");
    expect(body(proc)).toEqual({
        entity_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
        type: "process",
        display_name: "Bracket process",
        source_id: sop,
        source_revision: 1,
    });
    expect(proc.headers()["if-match"]).toBe('"0"');
    expect(proc.headers()["idempotency-key"]).toMatch(/^[0-9a-f-]{36}$/);
    await expect(page.getByRole("list", { name: "Processes" })).toContainText(
        "no value-stream checklist yet",
    );

    // An area citing a commissioning note: the note is stored first, then the area cites it.
    await add.getByLabel("Area", { exact: true }).check();
    await add.getByLabel("Name").fill("Press area");
    await add
        .getByLabel("Cites")
        .selectOption({ label: "A commissioning note I write now" });
    await expect(add.getByRole("button", { name: "Add area" })).toBeDisabled(); // the note text is required
    await add
        .getByRole("textbox", { name: "Commissioning note" })
        .fill("Press shop, bay 2, per the commissioning walk-through.");
    await add.getByRole("button", { name: "Add area" }).click();
    await expect(add.getByRole("status").filter({ hasText: /\S/ })).toHaveText(
        'Area "Press area" added (revision 1).',
    );
    const [note] = at(s, "/sources");
    expect(body(note)).toEqual({
        source_id: expect.any(String),
        kind: "manual",
        title: "Commissioning note: Press area",
        text: "Press shop, bay 2, per the commissioning walk-through.",
    });
    const area = at(s, "/entities")[1];
    expect(body(area)).toMatchObject({
        type: "area",
        display_name: "Press area",
        source_id: body(note).source_id,
        source_revision: 1,
    });

    const start = page.getByRole("form", {
        name: "Start a value-stream checklist",
    });
    await start
        .getByLabel("Process")
        .selectOption({ label: "Bracket process" });
    await start.getByLabel("Area").selectOption({ label: "Press area" });
    await start.getByLabel("Reason").fill("Pilot value-stream interview");
    await start.getByRole("button", { name: "Start checklist" }).click();
    await expect(start.getByRole("status").filter({ hasText: /\S/ })).toHaveText(
        'Value-stream checklist started for "Bracket process" with 3 questions to cover.',
    );
    const [inv] = at(s, "/discovery-inventories");
    expect(body(inv)).toEqual({
        inventory_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
        area_id: body(area).entity_id,
        area_revision: 1,
        process_id: body(proc).entity_id,
        process_revision: 1,
        capability: "process_understanding",
        reason: "Pilot value-stream interview",
    });
    expect(inv.headers()["if-match"]).toBe('"0"');
    await expect(page.getByRole("list", { name: "Processes" })).toContainText(
        "value-stream checklist started",
    );
    // A second value-stream checklist for the same process is not offered.
    await start
        .getByLabel("Process")
        .selectOption({ label: "Bracket process" });
    await expect(
        start.getByText("This process already has a value-stream checklist"),
    ).toBeVisible();
    await expect(
        start.getByRole("button", { name: "Start checklist" }),
    ).toBeDisabled();
});

test("stubbed changed source is refused, nothing is shown as added, and the sources reload", async ({
    page,
}) => {
    let s: Stub;
    s = await open(page, "owner", (r) => {
        if (!new URL(r.url()).pathname.endsWith("/entities")) return undefined;
        s.sources[0] = source(sop, "Bracket SOP", "document", 2); // someone uploaded a new revision
        return problem(409, "SOURCE_CHANGED");
    });
    await addProcess(page, "Bracket process", "Bracket SOP · document · r1");
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText(
        "changed since it was read",
    );
    await expect(
        appRegions(
            page.getByRole("form", { name: "Add an area or process" }),
            "status",
        ),
    ).toHaveCount(0);
    await expect(
        page
            .getByLabel("Cites")
            .locator("option", { hasText: "Bracket SOP · document · r2" }),
    ).toHaveCount(1);
    await expect(page.getByText("No processes registered yet.")).toBeVisible();
});

test("stubbed checklist needing a recent sign-in asks the owner to confirm their identity", async ({
    page,
}) => {
    const s = await open(page, "owner", (r) =>
        new URL(r.url()).pathname.endsWith("/discovery-inventories")
            ? problem(428, "REAUTHENTICATION_REQUIRED")
            : undefined,
    );
    s.entities.push(
        entity({
            entity_id: "00000000-0000-4000-8000-0000000006b1",
            type: "process",
            display_name: "Bracket process",
            source_id: sop,
            source_revision: 1,
        }),
    );
    s.entities.push(
        entity({
            entity_id: "00000000-0000-4000-8000-0000000006b2",
            type: "area",
            display_name: "Press area",
            source_id: sop,
            source_revision: 1,
        }),
    );
    await page.reload();
    const start = page.getByRole("form", {
        name: "Start a value-stream checklist",
    });
    await start
        .getByLabel("Process")
        .selectOption({ label: "Bracket process" });
    await start.getByLabel("Area").selectOption({ label: "Press area" });
    await start.getByLabel("Reason").fill("Pilot");
    await start.getByRole("button", { name: "Start checklist" }).click();
    await expect(
        page.getByRole("button", { name: "Confirm identity" }),
    ).toBeVisible();
    await expect(appRegions(start, "status")).toHaveCount(0);
    await expect(page.getByRole("list", { name: "Processes" })).toContainText(
        "no value-stream checklist yet",
    );
});

test("stubbed lost answer keeps the same entity ID and Idempotency-Key for the retry", async ({
    page,
}) => {
    let first = true;
    const s = await open(page, "owner", (r) => {
        if (!new URL(r.url()).pathname.endsWith("/entities") || !first)
            return undefined;
        first = false;
        return "abort";
    });
    await addProcess(page, "Bracket process", "Bracket SOP · document · r1");
    await expect(page.getByRole("alert").filter({ hasText: /\S/ })).toContainText(
        "The outcome is unknown",
    );
    await page.getByRole("button", { name: "Add process" }).click();
    await expect(page.getByRole("status").filter({ hasText: /\S/ })).toHaveText(
        'Process "Bracket process" added (revision 1).',
    );
    const [lost, retry] = at(s, "/entities");
    expect(retry.headers()["idempotency-key"]).toBe(
        lost.headers()["idempotency-key"],
    );
    expect(body(retry).entity_id).toBe(body(lost).entity_id);
    expect(s.entities).toHaveLength(1);
});

for (const role of ["supervisor", "operator"]) {
    test(`stubbed ${role} reads the processes but cannot add or start anything`, async ({
        page,
    }) => {
        const s = await open(page, role);
        await expect(
            page.getByText(
                "Only an owner of this site adds areas, processes and checklists.",
            ),
        ).toBeVisible();
        await expect(page.getByRole("form")).toHaveCount(0);
        expect(s.reads.some((p) => p.endsWith("/sources"))).toBe(false); // the source picker is the owner's
        expect(s.posts).toHaveLength(0);
    });
}
