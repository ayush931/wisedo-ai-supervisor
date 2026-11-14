// Pure client rules (no browser, no backend): error mapping and idempotency-key reuse.
import { test, expect } from "@playwright/test";
import { actorLabel, compareInstant, duplicateNames, loadDirectory, roundDecimal, classify, Intents, zonedISO, toLocalInput, formatIn, match, formatInterval, formatTime, fieldOf, sameBody, allPages, mutate, AmbiguousError } from "../src/api";
import { label } from "../src/labels";

test("master data follows cursors and rejects repeated cursors and the page cap", async () => {
    const requested: (string | undefined)[] = [];
    const items = await allPages(async (cursor) => {
        requested.push(cursor);
        return cursor
            ? { items: ["second"] }
            : { items: ["first"], next_cursor: "page-2" };
    });
    expect(requested).toEqual([undefined, "page-2"]);
    expect(items).toEqual(["first", "second"]);

    await expect(allPages(async (cursor) => cursor
        ? { items: [], next_cursor: "page-2" }
        : { items: [], next_cursor: "page-2" },
    )).rejects.toThrow("List pagination returned a repeated cursor.");

    let pageCount = 0;
    await expect(allPages(async (cursor) => {
        pageCount++;
        return { items: [], next_cursor: `${cursor ?? "first"}-next` };
    })).rejects.toThrow("List pagination exceeded 50 pages.");
    expect(pageCount).toBe(50);
});

test("problem status maps to the WIS-278 user outcome", () => {
    const cases: [number, string, string][] = [
        [409, "LIFECYCLE_CONFLICT", "conflict"],
        [412, "REVISION_CONFLICT", "conflict"],
        [401, "AUTHENTICATION_REQUIRED", "signed_out"],
        [428, "REAUTHENTICATION_REQUIRED", "reauth"],
        [428, "REVISION_REQUIRED", "invalid"], // a missing If-Match is a client bug, not a reauth
        [403, "SELF_REVIEW_FORBIDDEN", "denied"],
        [404, "NOT_FOUND", "not_found"],
        [429, "RATE_LIMITED", "retry"],
        [503, "STORAGE_UNAVAILABLE", "retry"],
        [422, "INVALID_STATION_RECORD", "invalid"],
    ];
    for (const [status, code, want] of cases) expect(classify({ status, code }), `${status} ${code}`).toBe(want);
});

test("the same intent and body reuse one key; a changed body or finished intent gets a new one", () => {
    let n = 0;
    const intents = new Intents(() => `k${++n}`);
    const body = { kind: "quantity", good_quantity: 3 };
    expect(intents.key("record", body)).toBe("k1");
    expect(intents.key("record", { ...body })).toBe("k1"); // retry of the unchanged intent
    expect(intents.key("record", { ...body, good_quantity: 4 })).toBe("k2"); // edited: new intent
    expect(intents.key("approve", body)).toBe("k3"); // intents are independent
    intents.done("record");
    expect(intents.key("record", { ...body, good_quantity: 4 })).toBe("k4");
});

// Review fix: the key binds If-Match, so promoting r2 after an ambiguous r1 attempt is a new intent.
test("mutate reuses a key only for the same body and If-Match", async () => {
    const sent: string[] = [];
    const real = globalThis.fetch;
    globalThis.fetch = (async (_path: unknown, init?: RequestInit) => {
        sent.push((init?.headers as Record<string, string>)["Idempotency-Key"]);
        throw new TypeError("connection lost");
    }) as typeof fetch;
    try {
        let n = 0;
        const intents = new Intents(() => `k${++n}`);
        for (const ifMatch of [1, 1, 2]) await expect(mutate(intents, "promote", "/p", {}, ifMatch)).rejects.toBeInstanceOf(AmbiguousError);
    } finally {
        globalThis.fetch = real;
    }
    expect(sent).toEqual(["k1", "k1", "k2"]);
});

test("WIS-520 routing validation paths map to fields and unknown paths stay unmapped", () => {
    expect(fieldOf("steps[3].workstation_id")).toEqual({ step: 3, field: "workstation_id" });
    expect(fieldOf("line_id")).toEqual({ field: "line_id" });
    expect(fieldOf("steps[3].unknown_field")).toEqual({ step: 3, field: "unknown_field" });
    expect(fieldOf("steps[x].workstation_id")).toBeNull();
    expect(fieldOf("condition")).toBeNull();
});

test("WIS-520 routing bodies compare object keys independent of order but keep step order", () => {
    expect(sameBody({ name: "x", steps: [{ id: "a" }, { id: "b" }] }, { steps: [{ id: "a" }, { id: "b" }], name: "x" })).toBe(true);
    expect(sameBody({ name: "x", steps: [{ id: "a" }, { id: "b" }] }, { name: "x", steps: [{ id: "b" }, { id: "a" }] })).toBe(false);
});

test("wall time in an IANA zone carries that zone's offset, including across DST", () => {
    expect(zonedISO("2026-10-21T06:00", "Asia/Kolkata")).toBe("2026-10-21T06:00:00+05:30");
    expect(zonedISO("2026-01-15T08:00", "Europe/Berlin")).toBe("2026-01-15T08:00:00+01:00");
    expect(zonedISO("2026-07-15T08:00", "Europe/Berlin")).toBe("2026-07-15T08:00:00+02:00");
    expect(zonedISO("2026-07-15T08:00", "UTC")).toBe("2026-07-15T08:00:00+00:00");
    expect(toLocalInput("2026-10-21T00:30:00Z", "Asia/Kolkata")).toBe("2026-10-21T06:00");
});

// Regression (WIS-278): dateStyle/timeStyle with timeZoneName threw "Invalid option" and blanked every page.
test("formatIn renders an instant with its zone and keeps unknown visible", () => {
    expect(formatIn("2026-10-21T00:30:00Z", "Asia/Kolkata")).toMatch(/21 Oct 2026.*06:00:00.*GMT\+5:30/);
    expect(formatIn(null)).toBe("unknown");
});

test("intervals and record times keep a short zone name and add the day only when needed", () => {
    expect(formatInterval("2026-10-21T00:30:00Z", "2026-10-21T08:30:00Z", "Asia/Kolkata")).toBe("Wed 21 Oct · 06:00–14:00 IST");
    expect(formatInterval("2026-10-21T16:30:00Z", "2026-10-22T00:30:00Z", "Asia/Kolkata")).toBe("Wed 21 Oct · 22:00–Thu 22 Oct 06:00 IST");
    const now = new Date("2026-10-21T06:00:00Z");
    expect(formatTime("2026-10-21T05:29:00Z", "Asia/Kolkata", now)).toBe("10:59 IST");
    expect(formatTime("2026-10-20T05:29:00Z", "Asia/Kolkata", now)).toBe("Tue 20 Oct 10:59 IST");
    expect(formatTime(null, "Asia/Kolkata", now)).toBe("unknown");
});

test("codes get human labels; an unknown code is shown verbatim, never hidden", () => {
    expect(label("NOT_READY").text).toBe("Not ready");
    expect(label("SUPERSEDED_PLAN_BINDING").text).toBe("Plan revised since");
    expect(label("in_progress").text).toBe("In progress");
    expect(label("SOMETHING_NEW")).toMatchObject({ text: "SOMETHING_NEW", code: "SOMETHING_NEW" });
});

// Security (WIS-278): route ids must be canonical UUIDs so a crafted link cannot steer API calls
// (client-side path traversal); the encoded traversal below previously decoded into "../../v1/...".
test("route params accept only canonical UUIDs and are never decoded", () => {
    const site = "00000000-0000-4000-8000-000000000291", station = "09e874ca-505a-59ed-b294-cb21dc03dea5";
    expect(match("/sites/:siteId/workstations/:stationId", `/sites/${site}/workstations/${station}`)).toEqual({ siteId: site, stationId: station });
    expect(match("/sites/:siteId/workstations/:stationId", `/sites/${site}/workstations/..%2F..%2Fv1%2Fme`)).toBeNull();
    expect(match("/sites/:siteId/workstations/:stationId", `/sites/${site}/workstations/${station.toUpperCase()}`)).toBeNull();
    expect(match("/sites/:siteId/shifts/:shiftId", `/sites/${site}/shifts/new`)).toBeNull();
    expect(match("/sites/:siteId/shifts", `/sites/${site}/shifts/`)).toEqual({ siteId: site });
});

test("WIS-290 actor label: you, then the directory name, then the subject prefix; never blank", () => {
    const names = new Map([["5c89aa00-0000-4000-8000-000000000001", "Operator One"]]);
    expect(actorLabel({ subject: "5c89aa00-0000-4000-8000-000000000001" }, "me", names)).toBe("Operator One · 5c89");
    expect(actorLabel({ subject: "5c89aa00-0000-4000-8000-000000000001" }, "5c89aa00-0000-4000-8000-000000000001", names)).toBe("you");
    expect(actorLabel({ subject: "9f00bb11-0000-4000-8000-000000000002" }, "me", names)).toBe("member 9f00bb11");
    expect(actorLabel(null, "me", names)).toBe("unknown");
    expect(actorLabel(undefined, "me", new Map())).toBe("unknown");
});

test("WIS-290 security: a directory name cannot pose as a UI label, and shared names are flagged", () => {
    const s = (n: number) => `${n}abc0000-0000-4000-8000-00000000000${n}`;
    // A name that equals or starts with a reserved label falls back to the subject prefix.
    for (const name of ["you", " You ", "unknown", "Unknown person", "member 1a2b3c4d", "MEMBER x", "  ", "Ｙｏｕ", "ｍｅｍｂｅｒ 1"]) {
        expect(actorLabel({ subject: s(1) }, s(9), new Map([[s(1), name]])), name).toBe(`member ${s(1).slice(0, 8)}`);
    }
    // Review finding: a name carrying its own fake subject tag ("Shift Approver · 9f3c") would
    // render as "Shift Approver · 9f3c · 1abc" and pass for the real approver. A separator dot
    // (or a lookalike) next to a space is refused; a Catalan l·l inside a word is kept.
    for (const name of ["Shift Approver · 9f3c", "Shift Approver ·9f3c", "Shift Approver• 9f3c", "Shift Approver ∙ 9f3c", "Shift Approver ・ 9f3c"]) {
        expect(actorLabel({ subject: s(1) }, s(9), new Map([[s(1), name]])), name).toBe(`member ${s(1).slice(0, 8)}`);
    }
    expect(actorLabel({ subject: s(1) }, s(9), new Map([[s(1), "Col·legi"]]))).toBe("Col·legi · 1abc");
    // Ordinary names that merely begin with the same letters are kept.
    expect(actorLabel({ subject: s(1) }, s(9), new Map([[s(1), "Youssef Amin"]]))).toBe("Youssef Amin · 1abc");
    // "you" only for the signed-in subject, never for a look-alike name.
    expect(actorLabel({ subject: s(1) }, s(1), new Map([[s(1), "Shift Approver"]]))).toBe("you");
    const dir = new Map([
        [s(1), "Shift Approver"],
        [s(2), " shift approver"],
        [s(3), "Operator One"],
        [s(4), "Ａｎａ"],
        [s(5), "ana"],
    ]);
    // Fullwidth lookalikes fold (NFKC) onto the same name and are flagged too.
    expect([...duplicateNames(dir)].sort()).toEqual([s(1), s(2), s(4), s(5)]);
    expect(duplicateNames(new Map([[s(3), "Operator One"]])).size).toBe(0);
});

test("decimals are rounded half-even for display only, exactly, without floats", () => {
    const cases: [string, number, string][] = [
        ["30.316667", 1, "30.3"],
        ["30.35", 1, "30.4"], // tie, odd last digit rounds up
        ["30.25", 1, "30.2"], // tie, even last digit stays
        ["30.250001", 1, "30.3"],
        ["0.05", 1, "0.0"],
        ["0.95", 1, "1.0"],
        ["9.96", 1, "10.0"],
        ["-0.04", 1, "0.0"],
        ["-1.26", 1, "-1.3"],
        ["31.5", 1, "31.5"],
        ["12", 1, "12"],
        ["2.5", 0, "2"],
        ["3.5", 0, "4"],
        ["not-a-number", 1, "not-a-number"],
    ];
    for (const [v, d, want] of cases) expect(roundDecimal(v, d), `${v}/${d}`).toBe(want);
});

// Regression: lifecycle state came from sorting timestamps as text, so a seeded "started" at
// "…39.4Z" sorted after "acknowledged" at "…39.401Z" and the tablet offered Acknowledge again.
test("instants order by time, not by text, whatever the fraction length", () => {
    const cases: [string, string, number][] = [
        ["2026-10-21T06:30:39.4Z", "2026-10-21T06:30:39.401Z", -1],
        ["2026-10-21T06:30:39Z", "2026-10-21T06:30:39.000001Z", -1],
        ["2026-10-21T06:30:39.000002Z", "2026-10-21T06:30:39.000001Z", 1],
        ["2026-10-21T06:30:39.5Z", "2026-10-21T06:30:39.500000Z", 0],
        ["2026-10-21T06:30:40Z", "2026-10-21T06:30:39.999999Z", 1],
    ];
    for (const [a, b, want] of cases) expect(Math.sign(compareInstant(a, b)), `${a} vs ${b}`).toBe(want);
});

test("WIS-290 review: a member directory that fails on any page is unknown, not partial", async () => {
    const pages: Record<string, { members: { subject: string; display_name: string | null }[]; next_cursor: string | null }> = {
        "": { members: [{ subject: "a", display_name: "Ana" }], next_cursor: "p2" },
        p2: { members: [{ subject: "b", display_name: "ana" }, { subject: "c", display_name: null }], next_cursor: null },
    };
    const full = await loadDirectory(async (cursor) => pages[cursor]);
    expect([...full]).toEqual([["a", "Ana"], ["b", "ana"]]);
    expect([...duplicateNames(full)].sort()).toEqual(["a", "b"]);
    // Page 2 fails: the lookalike on page 2 would go unflagged in a partial map.
    const failed = await loadDirectory(async (cursor) => {
        if (cursor === "p2") throw new Error("network");
        return pages[cursor];
    });
    expect(failed.size).toBe(0);
    // More pages than the bound is also incomplete.
    const endless = await loadDirectory(async (cursor) => ({ members: [{ subject: cursor || "first", display_name: "x" }], next_cursor: cursor + "n" }));
    expect(endless.size).toBe(0);
});
