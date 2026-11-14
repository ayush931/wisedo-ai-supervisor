import { expect, test, type Page, type Route } from "@playwright/test";
import { section } from "./session";

const siteId = "00000000-0000-4000-8000-000000000111";
const cursorError = (route: Route) => route.fulfill({ status: 400, contentType: "application/problem+json", json: { type: "about:blank", title: "Unexpected cursor", status: 400, code: "UNEXPECTED_CURSOR", correlation_id: "test" } });

async function stubSite(page: Page, handle: (url: URL, route: Route) => Promise<void>, workstations: { workstation_id: string; display_name: string; line_id: string; current_revision: number }[] = []) {
    await page.route("**/v1/**", async (route) => {
        const url = new URL(route.request().url());
        if (url.pathname === "/v1/me") {
            return route.fulfill({
                json: {
                    csrf_token: "test-csrf",
                    display_name: "Test Owner",
                    subject: "owner-subject",
                    sites: [{ id: siteId, name: "Pagination Site", role: "owner", tenant_id: "00000000-0000-4000-8000-000000000222" }],
                },
            });
        }
        if (url.pathname.endsWith("/workstations")) return route.fulfill({ json: { workstations } });
        if (url.pathname.endsWith("/members")) return route.fulfill({ json: { members: [] } });
        if (url.pathname.endsWith("/lines")) return route.fulfill({ json: { lines: [] } });
        if (url.pathname.endsWith("/items")) return route.fulfill({ json: { items: [] } });
        if (url.pathname.endsWith("/routings")) return route.fulfill({ json: { routings: [] } });
        if (url.pathname.endsWith("/shift-calendars")) return route.fulfill({ json: { shift_calendars: [] } });
        await handle(url, route);
    });
    await page.goto("/sites");
    await expect(page.getByRole("heading", { name: "Your sites" })).toBeVisible();
    await page.getByRole("listitem").getByRole("link").first().click();
    await expect(page.getByRole("navigation", { name: "Site sections" })).toBeVisible();
}

const shiftPlanId = "00000000-0000-4000-8000-000000000601";
const shiftLineId = "00000000-0000-4000-8000-000000000602";
const shiftStarts = "2040-01-01T07:00:00Z";
const shiftEnds = "2040-01-01T15:00:00Z";
const testActor = { issuer: "https://issuer.test", subject: "owner-subject" };

function shiftPlanDetail() {
    return {
        id: shiftPlanId,
        approved_revision: null,
        current_revision: 1,
        starts_at: shiftStarts,
        ends_at: shiftEnds,
        line_id: shiftLineId,
        replayed: false,
        state_revision: 1,
        revisions: [{
            approval: null,
            canonical_payload_digest: "sha256:plan",
            created_at: "2040-01-01T06:00:00Z",
            digest_schema_version: 1,
            plan: { assignments: [], starts_at: shiftStarts, ends_at: shiftEnds, line_id: shiftLineId, schema_version: 1, timezone: "UTC" },
            revision: 1,
            state: "draft",
            submitted_by: testActor,
        }],
    };
}

function shiftSummary(revision: number) {
    return {
        canonical_payload_digest: `sha256:summary-${revision}`,
        digest_schema_version: 1,
        generated_at: "2040-01-01T16:00:00Z",
        generated_by: testActor,
        plan_id: shiftPlanId,
        replayed: false,
        revision,
        summary: {
            as_of: "2040-01-01T16:00:00Z",
            ends_at: shiftEnds,
            formula_version: 1,
            jobs: { completed: 0, in_progress: 0, not_started: 0, planned: 0, remaining: 0, unknown: 0 },
            line_id: shiftLineId,
            notes: [],
            plan_id: shiftPlanId,
            plan_revision: 1,
            plan_revision_digest: "sha256:plan",
            policy: { revision: 1, stale_after_seconds: 60 },
            readiness: "READY",
            reasons: [],
            schema_version: 1,
            shift_ended: false,
            source_record_count: 0,
            starts_at: shiftStarts,
            stations: [],
        },
    };
}

function shiftHandover(revision: number, summaryRevision: number) {
    return {
        adoption: null,
        canonical_payload_digest: `sha256:handover-${revision}`,
        digest_schema_version: 1,
        generated_at: "2040-01-01T16:00:00Z",
        generated_by: testActor,
        handover: {
            ends_at: shiftEnds,
            formula_version: 1,
            jobs: [],
            line_id: shiftLineId,
            next_shift: { starts_at: "2040-01-01T15:00:00Z", ends_at: "2040-01-01T23:00:00Z", line_id: shiftLineId, timezone: "UTC" },
            notes: [],
            plan_id: shiftPlanId,
            plan_revision: 1,
            plan_revision_digest: "sha256:plan",
            proposed_plan: null,
            schema_version: 1,
            source: { as_of: "2040-01-01T16:00:00Z", readiness: "READY", reasons: [], shift_ended: true, source_record_count: 0 },
            starts_at: shiftStarts,
            summary_revision: summaryRevision,
        },
        plan_id: shiftPlanId,
        replayed: false,
        revision,
        status: "draft",
    };
}

test("draft count and list follow the returned cursor", async ({ page }) => {
    const draftCountCursors: (string | null)[] = [];
    const listCursors: (string | null)[] = [];
    const firstDraft = "00000001-0000-4000-8000-000000000301";
    const secondDraft = "00000002-0000-4000-8000-000000000302";
    const draft = (id: string) => ({
        draft_id: id,
        kind: "line",
        target_object_id: id,
        target_expected_revision: 0,
        author_kind: "agent",
        status: "draft",
        citation_count: 0,
        unavailable_citation_count: 0,
        current_revision: 1,
        updated_at: "2026-10-25T12:00:00Z",
    });

    // stubbed list page: return a cursor on page one for both the badge count and the drafts table.
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith("/shift-plans")) return route.fulfill({ json: { plans: [] } });
        if (!url.pathname.endsWith("/configuration-drafts")) return cursorError(route);
        const cursor = url.searchParams.get("cursor");
        if (url.searchParams.get("limit") === "100") {
            draftCountCursors.push(cursor);
            if (!cursor) return route.fulfill({ json: { drafts: [draft(firstDraft)], next_cursor: "draft-count-page-2" } });
            if (cursor === "draft-count-page-2") return route.fulfill({ json: { drafts: [draft(secondDraft)] } });
            return cursorError(route);
        }
        listCursors.push(cursor);
        if (!cursor) return route.fulfill({ json: { drafts: [draft(firstDraft)], next_cursor: "draft-list-page-2" } });
        if (cursor === "draft-list-page-2") return route.fulfill({ json: { drafts: [draft(secondDraft)] } });
        return cursorError(route);
    });

    await section(page, "Master data");
    const draftsTab = page.getByRole("button", { name: /^Drafts/ });
    await expect(draftsTab.locator(".master-count")).toHaveText("2");
    expect(draftCountCursors).toEqual([null, "draft-count-page-2"]);

    await draftsTab.click();
    await expect(page.getByRole("button", { name: "Load more" })).toBeVisible();
    await page.getByRole("button", { name: "Load more" }).click();
    await expect(page.getByRole("row").filter({ hasText: "00000002" })).toBeVisible();
    expect(listCursors).toEqual([null, "draft-list-page-2"]);
});

test("Today, shifts and jump palette read shift plans beyond the first page", async ({ page }) => {
    const overviewCursors: (string | null)[] = [];
    const listCursors: (string | null)[] = [];
    const overviewPlan = { id: "overview-plan", line_id: "line-1111", starts_at: "2040-01-01T07:00:00Z", ends_at: "2040-01-01T15:00:00Z", approved_revision: null, current_revision: 1, state_revision: 1 };
    const nextPlan = { id: "later-plan", line_id: "line-2222", starts_at: "2040-02-02T07:00:00Z", ends_at: "2040-02-02T15:00:00Z", approved_revision: null, current_revision: 1, state_revision: 1 };

    // stubbed list page: the overview, palette and shifts table each receive a second page cursor.
    await stubSite(page, async (url, route) => {
        if (!url.pathname.endsWith("/shift-plans")) return cursorError(route);
        const cursor = url.searchParams.get("cursor");
        if (url.searchParams.get("limit") === "100") {
            overviewCursors.push(cursor);
            if (!cursor) return route.fulfill({ json: { plans: [overviewPlan], next_cursor: "overview-plans-page-2" } });
            if (cursor === "overview-plans-page-2") return route.fulfill({ json: { plans: [nextPlan] } });
            return cursorError(route);
        }
        listCursors.push(cursor);
        if (!cursor) return route.fulfill({ json: { plans: [overviewPlan], next_cursor: "shift-list-page-2" } });
        if (cursor === "shift-list-page-2") return route.fulfill({ json: { plans: [nextPlan] } });
        return cursorError(route);
    });

    // Today lists every plan awaiting approval, from both pages.
    await expect(page.getByRole("article", { name: /awaiting approval$/ })).toHaveCount(2);
    expect(overviewCursors).toEqual([null, "overview-plans-page-2"]);

    await section(page, "Shifts");
    await page.getByRole("button", { name: "Next page" }).click();
    await expect(page.getByRole("table").getByText("…2222")).toBeVisible();
    expect(listCursors).toEqual([null, "shift-list-page-2"]);

    await page.keyboard.press("Control+k");
    const palette = page.getByRole("dialog", { name: "Jump to" });
    await expect(palette).toBeVisible();
    await palette.getByRole("combobox", { name: "Search workstations and shifts" }).fill("Thu 2 Feb");
    await expect(palette.getByRole("option", { name: /Thu 2 Feb/ })).toBeVisible();
    expect(overviewCursors).toEqual([null, "overview-plans-page-2", null, "overview-plans-page-2"]);
});

test("shift detail selects the latest summary and handover from later pages", async ({ page }) => {
    const summaryCursors: (string | null)[] = [];
    const handoverCursors: (string | null)[] = [];
    const base = `/v1/sites/${siteId}/shift-plans/${shiftPlanId}`;
    const planRow = { id: shiftPlanId, starts_at: shiftStarts, ends_at: shiftEnds, line_id: shiftLineId, approved_revision: null, current_revision: 1, state_revision: 1 };

    // stubbed list page: latest summary and handover revisions are on page two.
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith("/shift-plans")) {
            return route.fulfill({ json: { plans: url.searchParams.get("limit") === "100" ? [] : [planRow] } });
        }
        if (url.pathname === base) return route.fulfill({ json: shiftPlanDetail() });
        if (url.pathname === `${base}/summaries`) {
            const cursor = url.searchParams.get("cursor");
            summaryCursors.push(cursor);
            if (!cursor) return route.fulfill({ json: { summaries: [{ revision: 1, plan_revision: 1, readiness: "READY", generated_at: shiftEnds, generated_by: testActor, canonical_payload_digest: "sha256:summary-1" }], next_cursor: "summaries-page-2" } });
            if (cursor === "summaries-page-2") return route.fulfill({ json: { summaries: [{ revision: 7, plan_revision: 1, readiness: "READY", generated_at: shiftEnds, generated_by: testActor, canonical_payload_digest: "sha256:summary-7" }] } });
            return cursorError(route);
        }
        if (url.pathname === `${base}/handovers`) {
            const cursor = url.searchParams.get("cursor");
            handoverCursors.push(cursor);
            if (!cursor) return route.fulfill({ json: { handovers: [{ revision: 2, summary_revision: 1, status: "draft", generated_at: shiftEnds, generated_by: testActor, canonical_payload_digest: "sha256:handover-2" }], next_cursor: "handovers-page-2" } });
            if (cursor === "handovers-page-2") return route.fulfill({ json: { handovers: [{ revision: 9, summary_revision: 7, status: "draft", generated_at: shiftEnds, generated_by: testActor, canonical_payload_digest: "sha256:handover-9" }] } });
            return cursorError(route);
        }
        const summaryRevision = url.pathname.match(/\/summaries\/(\d+)$/)?.[1];
        if (summaryRevision) return route.fulfill({ json: shiftSummary(Number(summaryRevision)) });
        const handoverRevision = url.pathname.match(/\/handovers\/(\d+)$/)?.[1];
        if (handoverRevision) return route.fulfill({ json: shiftHandover(Number(handoverRevision), Number(handoverRevision) === 9 ? 7 : 1) });
        return cursorError(route);
    });

    await section(page, "Shifts");
    await page.getByRole("link", { name: /Shift ·/ }).click();
    await expect(page.getByText(/Summary r7 of plan r1/)).toBeVisible();
    await expect(page.getByRole("heading", { name: /Handover r9/ })).toBeVisible();
    await expect(page.getByText(/from summary r7/)).toBeVisible();
    expect(summaryCursors).toEqual([null, "summaries-page-2"]);
    expect(handoverCursors).toEqual([null, "handovers-page-2"]);
});

test("shift detail shows a page-two error without presenting a partial summary", async ({ page }) => {
    const summaryCursors: (string | null)[] = [];
    const summaryDetails: number[] = [];
    const base = `/v1/sites/${siteId}/shift-plans/${shiftPlanId}`;
    const planRow = { id: shiftPlanId, starts_at: shiftStarts, ends_at: shiftEnds, line_id: shiftLineId, approved_revision: null, current_revision: 1, state_revision: 1 };

    // stubbed list page: summary page two fails after a valid first page.
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith("/shift-plans")) return route.fulfill({ json: { plans: url.searchParams.get("limit") === "100" ? [] : [planRow] } });
        if (url.pathname === base) return route.fulfill({ json: shiftPlanDetail() });
        if (url.pathname === `${base}/summaries`) {
            const cursor = url.searchParams.get("cursor");
            summaryCursors.push(cursor);
            if (!cursor) return route.fulfill({ json: { summaries: [{ revision: 1, plan_revision: 1, readiness: "READY", generated_at: shiftEnds, generated_by: testActor, canonical_payload_digest: "sha256:summary-1" }], next_cursor: "summary-page-2" } });
            return cursorError(route);
        }
        if (url.pathname === `${base}/handovers`) return route.fulfill({ json: { handovers: [] } });
        const summaryRevision = url.pathname.match(/\/summaries\/(\d+)$/)?.[1];
        if (summaryRevision) {
            summaryDetails.push(Number(summaryRevision));
            return route.fulfill({ json: shiftSummary(Number(summaryRevision)) });
        }
        return cursorError(route);
    });

    await section(page, "Shifts");
    await page.getByRole("link", { name: /Shift ·/ }).click();
    const error = page.getByRole("alert").filter({ hasText: /\S/ });
    await expect(error).toBeVisible();
    await expect(error).toContainText("UNEXPECTED_CURSOR");
    await expect(page.getByRole("heading", { name: /Shift ·/ })).toHaveCount(0);
    await expect(page.getByText(/Summary r1 of plan r1/)).toHaveCount(0);
    expect(summaryCursors).toEqual([null, "summary-page-2"]);
    expect(summaryDetails).toEqual([]);
});

test("workstation history loads records after the first page", async ({ page }) => {
    const stationId = "00000000-0000-4000-8000-000000000411";
    const jobOrderId = "00000000-0000-4000-8000-000000000412";
    const planId = "00000000-0000-4000-8000-000000000413";
    const recordCursors: (string | null)[] = [];
    const record = (id: string, comment: string) => ({
        canonical_payload_digest: "sha256:test",
        digest_schema_version: 1,
        event_at: "2026-10-25T12:00:00Z",
        id,
        late: false,
        plan_id: planId,
        plan_revision: 1,
        profile_revision: 1,
        received_at: "2026-10-25T12:00:01Z",
        record: { event_at: "2026-10-25T12:00:00Z", job_order_id: jobOrderId, job_order_revision: 1, kind: "note", comment, schema_version: 1, workstation_id: stationId },
        recorded_by: { issuer: "https://issuer.test", subject: "owner-subject" },
        replayed: false,
        source: "manual_tablet",
        state: "current",
    });

    // stubbed list page: history requests the exact cursor returned with the first 100 records.
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith("/shift-plans")) return route.fulfill({ json: { plans: [] } });
        if (url.pathname.endsWith("/current-job-order")) {
            return route.fulfill({ json: {
                current_revision: 1,
                ends_at: "2026-10-25T15:00:00Z",
                job_order_id: jobOrderId,
                line_id: "00000000-0000-4000-8000-000000000414",
                order: null,
                pinned_revision: 1,
                plan_id: planId,
                plan_revision: 1,
                starts_at: "2026-10-25T07:00:00Z",
                state: "needs_replan",
                target_quantity: 10,
                timezone: "America/Detroit",
                workstation_id: stationId,
            } });
        }
        if (url.pathname.endsWith("/profile")) return route.fulfill({ status: 404, json: { type: "about:blank", title: "NOT_FOUND", status: 404, code: "NOT_FOUND", correlation_id: "test" } });
        if (url.pathname.endsWith("/records")) {
            const cursor = url.searchParams.get("cursor");
            recordCursors.push(cursor);
            if (!cursor) return route.fulfill({ json: { records: [record("record-first", "first page record")], next_cursor: "records-page-2" } });
            if (cursor === "records-page-2") return route.fulfill({ json: { records: [record("record-second", "second page record")] } });
            return cursorError(route);
        }
        return cursorError(route);
    }, [{ workstation_id: stationId, display_name: "Station A", line_id: "00000000-0000-4000-8000-000000000414", current_revision: 1 }]);

    await section(page, "Workstations");
    await page.getByRole("link", { name: "Station A" }).click();
    await page.getByRole("button", { name: /^History/ }).click();
    await expect(page.getByRole("heading", { name: "Records this shift" })).toBeVisible();
    await expect(page.getByText("first page record")).toBeVisible();
    await page.getByRole("button", { name: "Load more records" }).click();
    await expect(page.getByText("second page record")).toBeVisible();
    expect(recordCursors).toEqual([null, "records-page-2"]);
});

test("History polling updates the live job and tally while retaining loaded history pages", async ({ page }) => {
    await page.clock.install();
    const stationId = "00000000-0000-4000-8000-000000000611";
    const oldJobOrderId = "00000000-0000-4000-8000-000000000612";
    const newJobOrderId = "00000000-0000-4000-8000-000000000613";
    const planId = "00000000-0000-4000-8000-000000000614";
    const lineId = "00000000-0000-4000-8000-000000000615";
    let currentReads = 0;
    let newOrderPageReads = 0;
    const current = (jobOrderId: string, target: number, itemCode: string) => ({
        current_revision: 1,
        ends_at: "2026-10-25T15:00:00Z",
        job_order_id: jobOrderId,
        line_id: lineId,
        order: {
            canonical_payload_digest: "sha256:test",
            created_at: "2026-10-25T12:00:00Z",
            digest_schema_version: 1,
            order: { fields: [], instruction_document_id: lineId, instruction_revision: 1, instruction_sha256: "sha256:test", item_code: itemCode, operation_id: lineId, order_id: jobOrderId, schema_version: 1, target_quantity: target, template: "template" },
            revision: 1,
            state: "current",
            submitted_by: testActor,
        },
        pinned_revision: 1,
        plan_id: planId,
        plan_revision: 1,
        starts_at: "2026-10-25T07:00:00Z",
        state: "current",
        target_quantity: target,
        timezone: "America/Detroit",
        workstation_id: stationId,
    });
    const quantity = (id: string, jobOrderId: string, good: number) => ({
        canonical_payload_digest: "sha256:test",
        digest_schema_version: 1,
        event_at: "2026-10-25T12:00:00Z",
        id,
        late: false,
        plan_id: planId,
        plan_revision: 1,
        profile_revision: 1,
        received_at: "2026-10-25T12:00:01Z",
        record: { event_at: "2026-10-25T12:00:00Z", good_quantity: good, job_order_id: jobOrderId, job_order_revision: 1, kind: "quantity", reject_quantity: 0, schema_version: 1, workstation_id: stationId },
        recorded_by: testActor,
        replayed: false,
        source: "manual_tablet",
        state: "current",
    });

    // stubbed list page: a poll changes job order/counts after History opens; loaded pages remain in the dialog.
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith("/shift-plans")) return route.fulfill({ json: { plans: [] } });
        if (url.pathname.endsWith("/current-job-order")) {
            const response = currentReads++ === 0 ? current(oldJobOrderId, 10, "ITEM-OLD") : current(newJobOrderId, currentReads > 2 ? 20 : 10, "ITEM-NEW");
            return route.fulfill({ json: response });
        }
        if (url.pathname.endsWith("/profile")) return route.fulfill({ status: 404, json: { type: "about:blank", title: "NOT_FOUND", status: 404, code: "NOT_FOUND", correlation_id: "test" } });
        if (url.pathname.endsWith("/records")) {
            const jobOrderId = url.searchParams.get("job_order_id");
            const cursor = url.searchParams.get("cursor");
            if (cursor === "history-page-2") return route.fulfill({ json: { records: [{
                canonical_payload_digest: "sha256:test", digest_schema_version: 1, event_at: "2026-10-25T11:00:00Z", id: "record-second-page", late: false,
                plan_id: planId, plan_revision: 1, profile_revision: 1, received_at: "2026-10-25T11:00:01Z",
                record: { event_at: "2026-10-25T11:00:00Z", job_order_id: newJobOrderId, job_order_revision: 1, kind: "note", comment: "second page record", schema_version: 1, workstation_id: stationId },
                recorded_by: testActor, replayed: false, source: "manual_tablet", state: "current",
            }] } });
            if (jobOrderId === oldJobOrderId) return route.fulfill({ json: { records: [quantity("record-old-job", oldJobOrderId, 1)] } });
            if (jobOrderId === newJobOrderId) {
                newOrderPageReads++;
                if (newOrderPageReads === 1) return route.fulfill({ json: { records: [quantity("record-new-job", newJobOrderId, 7)], next_cursor: "history-page-2" } });
                return route.fulfill({ json: { records: [quantity("record-new-count", newJobOrderId, 9)] } });
            }
        }
        return cursorError(route);
    }, [{ workstation_id: stationId, display_name: "Station A", line_id: lineId, current_revision: 1 }]);

    await section(page, "Workstations");
    await page.getByRole("link", { name: "Station A" }).click();
    const good = page.getByLabel("Recorded this shift").locator(".kpi").filter({ hasText: /^Good/ }).locator("strong");
    const target = page.getByLabel("Recorded this shift").locator(".kpi").nth(3).locator("strong");
    await expect(good).toHaveText("1");
    await page.getByRole("button", { name: /^History/ }).click();
    const history = page.getByRole("dialog", { name: "Records this shift" });
    await page.clock.fastForward(10000);
    await expect(page.getByText("ITEM-NEW", { exact: true })).toBeVisible();
    await expect(history.getByText("good 7 · reject 0")).toBeVisible();
    await page.getByRole("button", { name: "Load more records" }).click();
    await expect(history.getByText("second page record")).toBeVisible();

    await page.clock.fastForward(10000);
    await expect.poll(() => currentReads).toBeGreaterThanOrEqual(3);
    await expect(target).toHaveText("20");
    await expect(good).toHaveText("9");
    await expect(history.getByText("good 7 · reject 0")).toBeVisible();
    await expect(history.getByText("second page record")).toBeVisible();
    await expect(history.getByText("good 9 · reject 0")).toHaveCount(0);
});

test("plan form uses current_order from both job-order pages without detail requests", async ({ page }) => {
    const jobOrderIds = Array.from({ length: 101 }, (_, index) => `00000000-0000-4000-8000-${String(index + 1).padStart(12, "0")}`);
    const cursors: (string | null)[] = [];
    const detailRequests: string[] = [];
    const workstationId = "00000000-0000-4000-8000-000000000511";
    const lineId = "00000000-0000-4000-8000-000000000512";
    const currentOrder = (id: string, index: number) => ({
        id,
        current_revision: index === 100 ? 3 : 1,
        current_order: {
            order_id: id,
            item_code: `ITEM-${index}`,
            operation_id: lineId,
            instruction_document_id: lineId,
            instruction_revision: 1,
            instruction_sha256: "sha256:test",
            schema_version: 1,
            target_quantity: index + 101,
            template: "template",
            fields: [],
        },
    });

    // stubbed list page: the picker reads both current_order pages and never fetches each detail.
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith("/shift-plans")) return route.fulfill({ json: { plans: [] } });
        if (url.pathname.endsWith("/job-orders")) {
            const cursor = url.searchParams.get("cursor");
            cursors.push(cursor);
            expect(url.searchParams.get("limit")).toBe("100");
            expect(url.searchParams.get("include")).toBe("current");
            if (!cursor) return route.fulfill({ json: { job_orders: jobOrderIds.slice(0, 100).map(currentOrder), next_cursor: "job-orders-page-2" } });
            if (cursor === "job-orders-page-2") return route.fulfill({ json: { job_orders: jobOrderIds.slice(100).map((id, index) => currentOrder(id, index + 100)) } });
            return cursorError(route);
        }
        const detail = url.pathname.match(/\/job-orders\/[^/]+$/);
        if (detail) detailRequests.push(url.pathname);
        return cursorError(route);
    }, [{ workstation_id: workstationId, display_name: "Station A", line_id: lineId, current_revision: 1 }]);

    await section(page, "Shifts");
    await page.getByRole("link", { name: "New plan", exact: true }).click();
    const form = page.getByRole("form", { name: "Create plan" });
    await expect(form).toBeVisible();
    const picker = form.getByLabel("Row 1 job order");
    await expect(picker.locator("option", { hasText: "ITEM-100" })).toHaveCount(1);
    await picker.selectOption(jobOrderIds[100]);
    await expect(form.getByLabel("Row 1 target quantity")).toHaveValue("201");
    expect(cursors).toEqual([null, "job-orders-page-2"]);
    expect(detailRequests).toEqual([]);
});

test("plan form reports the held second job-order page and blocks submit until both pages load", async ({ page }) => {
    const jobOrderIds = Array.from({ length: 2 }, (_, index) => `00000000-0000-4000-8000-${String(index + 701).padStart(12, "0")}`);
    const lineId = "00000000-0000-4000-8000-000000000712";
    const cursors: (string | null)[] = [];
    const detailRequests: string[] = [];
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    let markSecondStarted!: () => void;
    const secondStarted = new Promise<void>((resolve) => { markSecondStarted = resolve; });
    const currentOrder = (id: string, index: number) => ({
        id,
        current_revision: index === 1 ? 3 : 1,
        current_order: {
            order_id: id,
            item_code: `ITEM-${index}`,
            operation_id: lineId,
            instruction_document_id: lineId,
            instruction_revision: 1,
            instruction_sha256: "sha256:test",
            schema_version: 1,
            target_quantity: index + 20,
            template: "template",
            fields: [],
        },
    });

    // stubbed list page: the second current_order page stays held while progress and submit state remain visible.
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith("/shift-plans")) return route.fulfill({ json: { plans: [] } });
        if (url.pathname.endsWith("/job-orders")) {
            const cursor = url.searchParams.get("cursor");
            cursors.push(cursor);
            expect(url.searchParams.get("limit")).toBe("100");
            expect(url.searchParams.get("include")).toBe("current");
            if (!cursor) return route.fulfill({ json: { job_orders: [currentOrder(jobOrderIds[0], 0)], next_cursor: "job-orders-page-2" } });
            if (cursor === "job-orders-page-2") {
                markSecondStarted();
                await gate;
                return route.fulfill({ json: { job_orders: [currentOrder(jobOrderIds[1], 1)] } });
            }
            return cursorError(route);
        }
        if (/\/job-orders\/[^/]+$/.test(url.pathname)) detailRequests.push(url.pathname);
        return cursorError(route);
    });

    try {
        await section(page, "Shifts");
        await page.getByRole("link", { name: "New plan", exact: true }).click();
        const form = page.getByRole("form", { name: "Create plan" });
        // The plan form also has a status region for assignment suggestions; target the job-order load status.
        const status = form.locator("#pf-jobs-load[role=status]");
        const submit = form.getByRole("button", { name: "Create draft plan" });
        await secondStarted;
        await expect(status).toHaveText("Loading job orders (page 2)…");
        await expect(submit).toBeDisabled();
        expect(detailRequests).toEqual([]);

        release();
        await expect(status).toHaveCount(0);
        await expect(submit).toBeEnabled();
        const picker = form.getByLabel("Row 1 job order");
        await expect(picker.locator("option", { hasText: "ITEM-0" })).toHaveCount(1);
        await expect(picker.locator("option", { hasText: "ITEM-1" })).toHaveCount(1);
        await picker.selectOption(jobOrderIds[1]);
        await expect(form.getByLabel("Row 1 target quantity")).toHaveValue("21");
        expect(cursors).toEqual([null, "job-orders-page-2"]);
        expect(detailRequests).toEqual([]);
    } finally {
        release();
    }
});

test("plan form shows a job order without current_order by short ID and refuses it on submit", async ({ page }) => {
    const bareId = "00000000-0000-4000-8000-000000000801";
    // stubbed list page: the one job order arrives without its current_order body.
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith("/shift-plans")) return route.fulfill({ json: { plans: [] } });
        if (url.pathname.endsWith("/job-orders")) return route.fulfill({ json: { job_orders: [{ id: bareId, current_revision: 1 }] } });
        return cursorError(route);
    });

    await section(page, "Shifts");
    await page.getByRole("link", { name: "New plan", exact: true }).click();
    const form = page.getByRole("form", { name: "Create plan" });
    const picker = form.getByLabel("Row 1 job order");
    await expect(picker.locator("option", { hasText: bareId.slice(0, 8) })).toHaveCount(1);
    await picker.selectOption(bareId);
    await expect(form.getByLabel("Row 1 target quantity")).toHaveValue("");
    await form.getByRole("button", { name: "Create draft plan" }).click();
    await expect(form.getByRole("alert").filter({ hasText: /\S/ })).toContainText("Row 1: job order details are not loaded");
});

test("palette ignores a superseded shift-plan load", async ({ page }) => {
    let planRequests = 0;
    let releaseOld!: () => void;
    let releaseNew!: () => void;
    let oldFinished!: () => void;
    const oldGate = new Promise<void>((resolve) => { releaseOld = resolve; });
    const newGate = new Promise<void>((resolve) => { releaseNew = resolve; });
    const oldSettled = new Promise<void>((resolve) => { oldFinished = resolve; });
    const oldPlan = { id: "old-palette-plan", line_id: "line-1111", starts_at: "2040-01-01T07:00:00Z", ends_at: "2040-01-01T15:00:00Z", approved_revision: null, current_revision: 1, state_revision: 1 };
    const newPlan = { id: "new-palette-plan", line_id: "line-2222", starts_at: "2040-02-02T07:00:00Z", ends_at: "2040-02-02T15:00:00Z", approved_revision: null, current_revision: 1, state_revision: 1 };

    // stubbed list page: the older palette response must not replace or finish the newer load.
    await stubSite(page, async (url, route) => {
        if (!url.pathname.endsWith("/shift-plans")) return cursorError(route);
        planRequests++;
        if (planRequests === 1) return route.fulfill({ json: { plans: [] } });
        if (planRequests === 2) {
            await oldGate;
            const result = await route.fulfill({ json: { plans: [oldPlan] } });
            oldFinished();
            return result;
        }
        if (planRequests === 3) {
            await newGate;
            return route.fulfill({ json: { plans: [newPlan] } });
        }
        return cursorError(route);
    });
    await expect.poll(() => planRequests).toBe(1);
    const palette = page.getByRole("dialog", { name: "Jump to" });
    await page.keyboard.press("Control+k");
    await expect.poll(() => planRequests).toBe(2);
    await page.keyboard.press("Control+k");
    await expect.poll(() => planRequests).toBe(3);
    try {
        releaseOld();
        await oldSettled;
        await expect(palette.getByRole("status").filter({ hasText: /\S/ })).toBeVisible();
        releaseNew();
        await expect(palette.getByRole("option", { name: /Thu 2 Feb/ })).toBeVisible();
        await expect(palette.getByRole("option", { name: /1 Jan/ })).toHaveCount(0);
    } finally {
        releaseOld();
        releaseNew();
    }
});

// WIS-273 regressions: the case inbox poll replaced the whole list with the first page, so cases
// loaded with "Load more cases" vanished; then (review of #187) a poll that refreshed only page 1
// hid a case pushed off page 1 by a newer one. Every poll now re-reads all loaded pages.
test("Case inbox polling re-reads every loaded page, so a new case and a displaced one both show", async ({ page }) => {
    await page.clock.install();
    const lineId = "00000000-0000-4000-8000-000000000701";
    const orderId = "00000000-0000-4000-8000-000000000702";
    const caseRow = (n: number, state = "investigating") => ({
        id: `${String(n).padStart(8, "0")}-0000-4000-8000-000000000800`, // unique 8-character prefix, as the UI shows it
        line_id: lineId,
        order_id: orderId,
        state,
        episode: 1,
        detected_at: "2040-01-01T07:30:00Z",
        latest_window_end: "2040-01-01T07:45:00Z",
    });
    // After the change a new case 0 sorts first, pushing case 50 onto page 2, and case 51 resolves.
    let changed = false;
    let firstPageReads = 0;
    await stubSite(page, async (url, route) => {
        if (url.pathname.endsWith(`/entities/${lineId}`)) return route.fulfill({ json: { id: lineId, display_name: "Bracket line 1" } });
        if (url.pathname.endsWith(`/entities/${orderId}`)) return route.fulfill({ json: { id: orderId, display_name: "Order 4711" } });
        if (url.pathname.endsWith("/cases")) {
            if (url.searchParams.get("cursor") === "cases-page-2")
                return route.fulfill({ json: { items: changed ? [caseRow(50), caseRow(51, "resolved")] : [caseRow(51)] } });
            firstPageReads++;
            const first = changed ? [caseRow(0), ...Array.from({ length: 49 }, (_, i) => caseRow(i + 1))] : Array.from({ length: 50 }, (_, i) => caseRow(i + 1));
            return route.fulfill({ json: { items: first, next_cursor: "cases-page-2" } });
        }
        return cursorError(route);
    });

    await section(page, "Cases");
    const table = page.getByRole("table", { name: "Cases" });
    await expect(table.getByRole("row")).toHaveCount(51); // header + 50
    await page.getByRole("button", { name: "Load more cases" }).click();
    await expect(table.getByRole("row")).toHaveCount(52);
    await expect(page.getByRole("button", { name: "Load more cases" })).toHaveCount(0);

    changed = true;
    await page.clock.fastForward(30_000);
    await expect.poll(() => firstPageReads).toBeGreaterThanOrEqual(2);
    await expect(table.getByRole("row")).toHaveCount(53); // header + 52
    for (const n of [0, 50, 51]) await expect(table.getByRole("link", { name: `Case ${caseRow(n).id.slice(0, 8)}` })).toBeVisible();
    // The later page is refreshed too: case 51 shows its new state.
    await expect(table.getByRole("row").filter({ hasText: `Case ${caseRow(51).id.slice(0, 8)}` })).toContainText("Resolved");
    await expect(page.getByRole("button", { name: "Load more cases" })).toHaveCount(0);
});
