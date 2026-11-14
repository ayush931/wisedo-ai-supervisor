import React, { useCallback, useEffect, useState } from "react";
import type { KnowledgeCoverageV1, KnowledgeDiscoveryInventoryPageV1 } from "../../contracts/typescript/oee";
import { allPages, request } from "./api";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { ProgressBar } from "@astryxdesign/core/ProgressBar";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import { label } from "./labels";
import { Link, Problem, Status, Time, useSession } from "./ui";

type Inventory = KnowledgeDiscoveryInventoryPageV1["inventories"][number];

// The value-stream stages in the order material flows, with the question each answers. The keys
// are the process_understanding template (WIS-611); a key the server adds later still shows, last.
const STAGES: [key: string, name: string, asks: string][] = [
    ["material_suppliers", "Suppliers", "Critical materials, who supplies them, lead time"],
    ["receiving_inspection", "Receiving", "Where material arrives and how it is checked"],
    ["storage_locations", "Storage", "Where raw material, work in progress and finished goods are kept"],
    ["item_routing", "Routing", "The operations each product goes through, in order"],
    ["cycle_setup_times", "Times", "Cycle and setup time of each operation"],
    ["in_process_quality", "Quality", "Checks during production: where, how, how many"],
    ["packing_finished_goods", "Packing", "How finished products are packed and stored"],
    ["dispatch_delivery", "Dispatch", "How orders are dispatched and delivered"],
    ["stage_responsibility", "Responsibility", "Who owns each stage and who to escalate to"],
];
const order = (key: string) => {
    const i = STAGES.findIndex(([k]) => k === key);
    return i < 0 ? STAGES.length : i;
};

// ValueStreamChecklist (WIS-618) shows, for each value-stream discovery inventory, which of the nine
// stages are approved, waiting for review or still missing, read from the server's coverage. It is
// read-only: a stage is filled by interviews and documents, and approved by a reviewer.
export function ValueStreamChecklist() {
    const { site } = useSession();
    const [inventories, setInventories] = useState<Inventory[] | null>(null);
    const [error, setError] = useState<unknown>(null);
    const load = useCallback(async () => {
        setError(null);
        setInventories(null);
        try {
            const all = await allPages(async (cursor) => {
                const page = await request<KnowledgeDiscoveryInventoryPageV1>(`/v1/sites/${site.id}/discovery-inventories?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
                return { items: page.inventories, next_cursor: page.next_cursor };
            });
            setInventories(all.filter((inventory) => inventory.capability === "process_understanding"));
        } catch (e) {
            setError(e);
        }
    }, [site.id]);
    useEffect(() => {
        void load();
    }, [load]);

    return (
        <section aria-labelledby="checklist-title">
            <VStack gap={3}>
                <Heading level={1} id="checklist-title">Onboarding checklist</Heading>
                {error !== null && (
                    <>
                        <Problem error={error} />
                        <div>
                            <Button label="Retry" onClick={() => void load()} />
                        </div>
                    </>
                )}
                {inventories === null && error === null && (
                    <div role="status" aria-label="Loading checklists…">
                        <Skeleton height={240} />
                    </div>
                )}
                {inventories?.length === 0 && (
                    <EmptyState
                        title="No value-stream checklist is registered at this site yet."
                        description="A site owner registers one per process (capability “value stream”); interviews about that process then ask its questions."
                        headingLevel={2}
                    />
                )}
                {inventories?.map((inventory) => <Checklist key={inventory.inventory_id} inventory={inventory} />)}
            </VStack>
        </section>
    );
}

function Checklist({ inventory }: { inventory: Inventory }) {
    const { site } = useSession();
    const [coverage, setCoverage] = useState<KnowledgeCoverageV1 | null>(null);
    const [error, setError] = useState<unknown>(null);
    const load = useCallback(async () => {
        setError(null);
        try {
            setCoverage(await request<KnowledgeCoverageV1>(`/v1/sites/${site.id}/discovery-inventories/${inventory.inventory_id}/coverage`));
        } catch (e) {
            setError(e);
        }
    }, [site.id, inventory.inventory_id]);
    useEffect(() => {
        void load();
    }, [load]);

    const heading = `${inventory.process_display_name}`;
    return (
        <article aria-label={`Checklist: ${heading}`}>
            <Card padding={4}>
                <VStack gap={2}>
                    <Heading level={2}>{heading}</Heading>
                    {error !== null ? (
                        // One checklist that cannot be read is shown as unavailable, never as empty.
                        <>
                            <Problem error={error} context="This checklist is unavailable right now." />
                            <div>
                                <Button label="Retry checklist" onClick={() => void load()} />
                            </div>
                        </>
                    ) : coverage === null ? (
                        <div role="status" aria-label="Loading…">
                            <Skeleton height={160} />
                        </div>
                    ) : (
                        <>
                            <HStack gap={2} align="center" wrap="wrap">
                                <strong>
                                    {coverage.approved_count} of {coverage.applicable_count} stages approved
                                </strong>
                                {coverage.knowledge_ready && <Badge variant="green" label="Ready" />}
                                {!coverage.scope_current && <Badge variant="orange" label="Process or area changed since registration" />}
                                <Text size="sm" color="secondary">
                                    evaluated <Time iso={coverage.evaluated_at} />
                                </Text>
                            </HStack>
                            {coverage.applicable_count > 0 && (
                                <ProgressBar label="Approved" isLabelHidden value={coverage.approved_count} max={coverage.applicable_count} variant="success" />
                            )}
                            <Table aria-label={`Stages: ${heading}`} density="compact">
                                <TableHeader>
                                    <TableRow isHeaderRow>
                                        <TableHeaderCell scope="col">Stage</TableHeaderCell>
                                        <TableHeaderCell scope="col">What it covers</TableHeaderCell>
                                        <TableHeaderCell scope="col">State</TableHeaderCell>
                                        <TableHeaderCell scope="col">Asked of</TableHeaderCell>
                                    </TableRow>
                                </TableHeader>
                                <TableBody>
                                    {[...coverage.requirements]
                                        .sort((a, b) => order(a.key) - order(b.key))
                                        .map((cell) => {
                                            const stage = STAGES.find(([k]) => k === cell.key);
                                            return (
                                                <TableRow key={cell.requirement_id}>
                                                    <TableHeaderCell scope="row">{stage?.[1] ?? cell.key}</TableHeaderCell>
                                                    <TableCell>{stage?.[2] ?? ""}</TableCell>
                                                    <TableCell>
                                                        <HStack gap={1} align="center" wrap="wrap">
                                                            {cell.state === "candidate" ? (
                                                                // A bound stage still waiting for qualification; worded here, not in the shared
                                                                // table, where "candidate" names a claim state.
                                                                <span title="candidate">
                                                                    <Badge
                                                                        variant="orange"
                                                                        label={
                                                                            <>
                                                                                <span aria-hidden="true">◐</span> Waiting for review
                                                                            </>
                                                                        }
                                                                    />
                                                                </span>
                                                            ) : (
                                                                <Status code={cell.state} />
                                                            )}
                                                            {cell.state !== "approved" && cell.reason_code && (
                                                                <Text size="sm" color="secondary">{label(cell.reason_code).text}</Text>
                                                            )}
                                                        </HStack>
                                                    </TableCell>
                                                    <TableCell>{label(cell.interview_role).text}</TableCell>
                                                </TableRow>
                                            );
                                        })}
                                </TableBody>
                            </Table>
                            <Text as="p" size="sm">
                                <Link to={`/sites/${site.id}/interviews`}>Start an interview</Link> to fill missing stages, or{" "}
                                <Link to={`/sites/${site.id}/documents`}>upload a document</Link>.
                            </Text>
                        </>
                    )}
                </VStack>
            </Card>
        </article>
    );
}
