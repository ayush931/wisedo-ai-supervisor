// Pure client rule (no browser, no backend): the station list follows next_cursor to the end.
// Regression for the PR #148 follow-up: one limit=100 page silently truncated sites with >100 stations.
import { test, expect } from "@playwright/test";
import { loadStations } from "../src/ui";

const ws = (n: number) => ({ workstation_id: `ws-${n}`, display_name: `Station ${n}`, line_id: "line-1", current_revision: 1 });
const page1 = { workstations: Array.from({ length: 100 }, (_, i) => ws(i)), next_cursor: "c1" };
const page2 = { workstations: [ws(100)] };

test("all pages are merged in order, one request per page", async () => {
    const cursors: string[] = [];
    const list = await loadStations(async (cursor) => {
        cursors.push(cursor);
        return cursor === "" ? page1 : page2;
    });
    expect(cursors).toEqual(["", "c1"]);
    expect(list).toHaveLength(101);
    expect(list.map((s) => s.id)).toEqual(Array.from({ length: 101 }, (_, i) => `ws-${i}`));
    expect(list[100]).toEqual({ id: "ws-100", name: "Station 100", line_id: "line-1", revision: 1 });
});

test("a failed later page or a repeated cursor rejects instead of returning a partial list", async () => {
    await expect(
        loadStations(async (cursor) => {
            if (cursor === "c1") throw new Error("network");
            return page1;
        }),
    ).rejects.toThrow("network");
    let calls = 0;
    await expect(
        loadStations(async () => {
            calls++;
            return page1; // always next_cursor "c1"
        }),
    ).rejects.toThrow("STATION_CURSOR_LOOP");
    expect(calls).toBe(2);
});
