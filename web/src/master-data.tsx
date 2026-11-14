import React, { useEffect, useState } from "react";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table as AstryxTable, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import type { ConfigurationDraftPageV1, ItemPageV1, LinePageV1, RoutingPageV1, ShiftCalendarPageV1 } from "../../contracts/typescript/oee";
import { allPages, request } from "./api";
import { ConfigurationDrafts } from "./configuration-drafts";
import { ConfigurationImport } from "./configuration-import";
import { canSupervise, Problem, useSession, useStations } from "./ui";

type Resource = "lines" | "items" | "routings" | "calendars";
type Catalog = {
    lines: LinePageV1["lines"];
    items: ItemPageV1["items"];
    routings: RoutingPageV1["routings"];
    calendars: ShiftCalendarPageV1["shift_calendars"];
};

const labels: Record<Resource, string> = {
    lines: "Lines",
    items: "Items",
    routings: "Routings",
    calendars: "Shift calendars",
};

// draftLink lets another workspace open one draft here (the interview's "Open draft" link). The
// app router has no query or hash routes, so it is a one-shot hand-off read on mount.
export const draftLink = { open: "" };

export function MasterDataPage() {
    const { site } = useSession();
    const stations = useStations();
    const [resource, setResource] = useState<Resource>("lines");
    const [initialDraft] = useState(() => {
        const id = draftLink.open;
        draftLink.open = "";
        return id;
    });
    const [panel, setPanel] = useState<"" | "import" | "drafts">(initialDraft ? "drafts" : "");
    const showPanel = panel !== "";
    const [openDrafts, setOpenDrafts] = useState<string>("…");
    // Selecting the Drafts tab again returns from a draft to the list.
    const [draftsView, setDraftsView] = useState(0);
    const [catalogVersion, setCatalogVersion] = useState(0);
    const [catalog, setCatalog] = useState<Catalog | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [pendingObject, setPendingObject] = useState("");
    const canImport = canSupervise(site.role);
    const objectViews: Record<string, Resource> = { line: "lines", item: "items", routing: "routings", shift_calendar: "calendars" };

    useEffect(() => {
        let active = true;
        setCatalog(null);
        setError(null);
        const base = `/v1/sites/${site.id}`;
        const query = (cursor?: string) => `?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`;
        Promise.all([
            allPages(async (cursor) => {
                const page = await request<LinePageV1>(`${base}/lines${query(cursor)}`);
                return { items: page.lines, next_cursor: page.next_cursor };
            }),
            allPages(async (cursor) => {
                const page = await request<ItemPageV1>(`${base}/items${query(cursor)}`);
                return { items: page.items, next_cursor: page.next_cursor };
            }),
            allPages(async (cursor) => {
                const page = await request<RoutingPageV1>(`${base}/routings${query(cursor)}`);
                return { items: page.routings, next_cursor: page.next_cursor };
            }),
            allPages(async (cursor) => {
                const page = await request<ShiftCalendarPageV1>(`${base}/shift-calendars${query(cursor)}`);
                return { items: page.shift_calendars, next_cursor: page.next_cursor };
            }),
        ]).then(([lines, items, routings, calendars]) => {
            if (active) setCatalog({ lines, items, routings, calendars });
        }).catch((reason: unknown) => {
            if (active) setError(reason);
        });
        // Drafts are owner/supervisor only; a viewer never requests them.
        if (canImport)
            allPages(async (cursor) => {
                const page = await request<ConfigurationDraftPageV1>(`${base}/configuration-drafts?status=draft&limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
                return { items: page.drafts, next_cursor: page.next_cursor };
            }).then(
                (drafts) => active && setOpenDrafts(String(drafts.length)),
                () => active && setOpenDrafts("—"),
            );
        return () => {
            active = false;
        };
    }, [site.id, catalogVersion, canImport]);

    useEffect(() => {
        if (!catalog || showPanel || !pendingObject) return;
        const row = document.getElementById(`master-object-${pendingObject}`);
        if (row) {
            row.scrollIntoView({ behavior: "smooth", block: "center" });
            row.focus({ preventScroll: true });
            setPendingObject("");
        }
    }, [catalog, showPanel, pendingObject]);

    const openObject = (kind: string, id: string) => {
        const view = objectViews[kind];
        if (!view) return;
        setPendingObject(id);
        setPanel("");
        setResource(view);
        setCatalog(null);
        setError(null);
        setCatalogVersion((v) => v + 1);
    };
    const objectNames = React.useMemo(() => new Map([...names(catalog?.lines ?? []), ...names(catalog?.items ?? []), ...names(catalog?.routings ?? [])]), [catalog]);
    const canShowRoutes = resource !== "routings" || !!stations.list;
    const current = catalog?.[resource] ?? [];

    return (
        <section aria-labelledby="master-title">
            <VStack gap={3}>
                <HStack gap={2} align="center" wrap="wrap">
                    <Heading level={1} id="master-title">Master data</Heading>
                    {!canImport && <Badge variant="neutral" label="Read only" />}
                </HStack>
                <nav aria-label="Master data types">
                    <HStack gap={1} wrap="wrap">
                        {(Object.keys(labels) as Resource[]).map((key) => (
                            <Chip key={key} label={labels[key]} pressed={!showPanel && resource === key} onClick={() => { setPanel(""); setResource(key); }}>
                                {labels[key]} <Count>{catalog ? catalog[key].length : error ? "—" : "…"}</Count>
                            </Chip>
                        ))}
                        {canImport && <Chip label="Drafts" pressed={panel === "drafts"} onClick={() => { setPanel("drafts"); setDraftsView((v) => v + 1); }}>Drafts <Count>{openDrafts}</Count></Chip>}
                        {canImport && <Chip label="Import CSV" pressed={panel === "import"} onClick={() => setPanel("import")}>Import CSV</Chip>}
                    </HStack>
                </nav>
                {panel === "drafts" && canImport ? (
                    <ConfigurationDrafts key={draftsView} initialOpen={draftsView === 0 ? initialDraft : ""} names={objectNames} onChanged={() => setCatalogVersion((v) => v + 1)} onOpenObject={openObject} />
                ) : panel === "import" && canImport ? (
                    <ConfigurationImport onApplied={() => setCatalogVersion((v) => v + 1)} onOpenObject={openObject} />
                ) : (
                    <>
                        <Problem error={error} />
                        {resource === "routings" && <Problem error={stations.error} />}
                        {!catalog && !error && <div role="status" aria-label="Loading master data"><Skeleton height={200} /></div>}
                        {catalog && resource === "routings" && !stations.list && !stations.error && <div role="status" aria-label="Loading workstation names"><Skeleton height={200} /></div>}
                        {catalog && canShowRoutes && <CatalogTable resource={resource} catalog={catalog} stations={stations.list ?? []} />}
                    </>
                )}
            </VStack>
        </section>
    );
}

// Chip is one pressed-state type selector. ToggleButton renders its children twice (to keep its
// width stable), which would duplicate .master-count, so this is a Button with aria-pressed.
function Chip({ label, pressed, onClick, children }: { label: string; pressed: boolean; onClick: () => void; children: React.ReactNode }) {
    return <Button label={label} variant={pressed ? "primary" : "secondary"} aria-pressed={pressed} onClick={onClick}>{children}</Button>;
}

// Count is the number on a type chip; .master-count is what the specs read.
function Count({ children }: { children: React.ReactNode }) {
    return <Text size="sm" color="inherit" hasTabularNumbers className="master-count">{children}</Text>;
}

const state = (active: boolean) => <Badge variant={active ? "green" : "neutral"} label={active ? "Active" : "Inactive"} />;

function CatalogTable({ resource, catalog, stations }: { resource: Resource; catalog: Catalog; stations: readonly { id: string; name: string }[] }) {
    if (resource === "lines")
        return <Table label={labels.lines} empty="No lines have been configured." headings={["Name", "State", "Revision"]}>
            {catalog.lines.map((line) => <TableRow id={`master-object-${line.id}`} tabIndex={-1} key={line.id}><TableHeaderCell scope="row">{line.body.display_name}</TableHeaderCell><TableCell>{state(line.body.active)}</TableCell><TableCell>r{line.current_revision}</TableCell></TableRow>)}
        </Table>;
    if (resource === "items")
        return <Table label={labels.items} empty="No items have been configured." headings={["Name", "Unit", "State", "Revision"]}>
            {catalog.items.map((item) => <TableRow id={`master-object-${item.id}`} tabIndex={-1} key={item.id}><TableHeaderCell scope="row">{item.body.display_name}</TableHeaderCell><TableCell>{item.body.unit}</TableCell><TableCell>{state(item.body.active)}</TableCell><TableCell>r{item.current_revision}</TableCell></TableRow>)}
        </Table>;
    if (resource === "routings") {
        const lineNames = names(catalog.lines);
        const itemNames = names(catalog.items);
        const stationNames = new Map(stations.map((station) => [station.id, station.name]));
        return <Table label={labels.routings} empty="No routings have been configured." headings={["Routing", "Line", "Item", "Ordered steps", "State", "Revision"]}>
            {catalog.routings.map((routing) => <TableRow id={`master-object-${routing.id}`} tabIndex={-1} key={routing.id}>
                <TableHeaderCell scope="row">{routing.body.display_name}</TableHeaderCell>
                <TableCell>{lineNames.get(routing.body.line_id) ?? routing.body.line_id}</TableCell>
                <TableCell>{itemNames.get(routing.body.item_id) ?? routing.body.item_id}</TableCell>
                <TableCell><ol className="master-steps" style={{ listStyle: "none", margin: 0, padding: 0 }}>{[...routing.body.steps].sort((a, b) => a.step_number - b.step_number).map((step) =>
                    <li key={step.step_number}><span className="master-step-number">{step.step_number}</span>{" "}{stationNames.get(step.workstation_id) ?? step.workstation_id}</li>,
                )}</ol></TableCell>
                <TableCell>{state(routing.body.active)}</TableCell>
                <TableCell>r{routing.current_revision}</TableCell>
            </TableRow>)}
        </Table>;
    }
    return <Table label={labels.calendars} empty="No shift calendars have been configured." headings={["Calendar", "Time zone", "Weekly shifts", "Revision"]}>
        {catalog.calendars.map((calendar) => <TableRow id={`master-object-${calendar.id}`} tabIndex={-1} key={calendar.id}>
            <TableHeaderCell scope="row">{calendar.body.display_name}</TableHeaderCell>
            <TableCell>{calendar.body.time_zone}</TableCell>
            <TableCell><ul style={{ listStyle: "none", margin: 0, padding: 0 }}>{[...calendar.body.shifts].sort((a, b) => a.day_of_week - b.day_of_week || a.start_local.localeCompare(b.start_local)).map((shift, i) =>
                <li key={`${shift.day_of_week}-${shift.start_local}-${i}`}><span>{["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"][shift.day_of_week - 1] ?? `Day ${shift.day_of_week}`}</span> {shift.start_local}–{shift.end_local}</li>,
            )}</ul></TableCell>
            <TableCell>r{calendar.current_revision}</TableCell>
        </TableRow>)}
    </Table>;
}

function names(values: readonly { id: string; body: { display_name: string } }[]) {
    return new Map(values.map((value) => [value.id, value.body.display_name]));
}

function Table({ label, empty, headings, children }: { label: string; empty: string; headings: string[]; children: React.ReactNode }) {
    if (!React.Children.count(children)) return <Text as="p" color="secondary">{empty}</Text>;
    return (
        <AstryxTable aria-label={`${label} master data`} density="compact">
            <TableHeader><TableRow isHeaderRow>{headings.map((heading) => <TableHeaderCell key={heading} scope="col">{heading}</TableHeaderCell>)}</TableRow></TableHeader>
            <TableBody>{children}</TableBody>
        </AstryxTable>
    );
}
