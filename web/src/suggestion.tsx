import { CorrectOutput } from "./corrections";
import React, { useCallback, useState } from "react";
import type { CaseSuggestionV1 } from "../../contracts/typescript/oee";
import { ApiError, request } from "./api";
import { Badge, type BadgeProps } from "@astryxdesign/core/Badge";
import { Blockquote } from "@astryxdesign/core/Blockquote";
import { Card } from "@astryxdesign/core/Card";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from "@astryxdesign/core/Table";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import { Announce, Dec, Problem, Readiness, Time, short, usePolling } from "./ui";

// WIS-629 read-only advisor suggestion for one case. The server assembles it on read from the
// approved process snapshot the case pins, the case's window measurements and the stored
// investigation output. Every statement carries its label and citations; nothing here approves,
// dispatches or proposes anything (rule 05). Source text (names, passages, model hypotheses) is
// rendered as plain text, never as markup.

const pollMs = 30_000;

const predicateWords: Record<string, string> = {
    cycle_time_seconds: "cycle time",
    setup_time_seconds: "setup time",
    capacity_per_hour: "capacity per hour",
    batch_size: "batch size",
    yield_ratio: "yield",
    performs: "performs",
    precedes: "precedes",
    part_of_asset: "is part of",
    located_in: "is located in",
};
const metricWords: Record<string, string> = {
    expected_good_units: "Expected good units",
    actual_good_units: "Actual good units",
    deficit_good_units: "Deficit",
};

function Kind({ label }: { label: "approved_fact" | "observation" | "hypothesis" }) {
    const [text, icon, variant] = (
        {
            approved_fact: ["Approved fact", "✓", "green"],
            observation: ["Observation", "◎", "neutral"],
            hypothesis: ["Hypothesis (model, unverified)", "?", "orange"],
        } as const
    )[label] satisfies readonly [string, string, BadgeProps["variant"]];
    return (
        <span title={label}>
            <Badge
                variant={variant}
                label={
                    <>
                        <span aria-hidden="true">{icon}</span> {text}
                    </>
                }
            />
        </span>
    );
}

export function AdvisorSuggestion({ base }: { base: string }) {
    const [s, setS] = useState<CaseSuggestionV1 | null>(null);
    const [error, setError] = useState<unknown>(null);
    const load = useCallback(async () => {
        try {
            setS(await request<CaseSuggestionV1>(`${base}/suggestion`));
            setError(null);
        } catch (e) {
            setError(e);
        }
    }, [base]);
    usePolling(useCallback(() => void load(), [load]), pollMs);

    const off = error instanceof ApiError && error.problem.code === "ADVISOR_DISABLED";
    return (
        <Card padding={4}>
            <VStack as="section" aria-labelledby="advisor-title" gap={2}>
                <Heading level={2} id="advisor-title">
                    Advisor suggestion
                </Heading>
                <Text as="p" size="sm" color="secondary">
                    Read-only. It approves and dispatches nothing; any action goes through a proposal that a site supervisor approves, never the one who raised or edited it.
                </Text>
                {off ? <Text as="p">Advisor suggestions are switched off for this deployment.</Text> : <Problem error={error} context="The suggestion could not be refreshed." />}
                <div role="status">
                    <Announce text={!s && !error ? "Loading suggestion…" : ""} />
                </div>
                {!s && !error && <Skeleton height={120} />}
                {s && !off && <SuggestionBody s={s} />}
                {s?.investigation && ["completed", "canceled", "failed"].includes(s.investigation.state) && (
                    <VStack gap={1}>
                        <Text as="p">
                            Investigation result: {s.investigation.status} {s.investigation.reasons.join(", ")}
                        </Text>
                        <CorrectOutput key={s.investigation.run_id} subject={{ kind: "investigation", run_id: s.investigation.run_id }} />
                    </VStack>
                )}
            </VStack>
        </Card>
    );
}

const plainList = { listStyle: "none", margin: 0, padding: 0 } as const;

function SuggestionBody({ s }: { s: CaseSuggestionV1 }) {
    return (
        <>
            <HStack align="center" gap={1} wrap="wrap">
                <Readiness value={s.readiness} reasons={s.reasons} />
            </HStack>
            {s.snapshot ? (
                <Text as="p" size="sm" color="secondary">
                    Approved process snapshot of <Time iso={s.snapshot.created_at} />
                    {s.snapshot.stale && <> · <strong>approved knowledge has changed since this snapshot</strong></>}
                </Text>
            ) : null}
            {s.readiness === "NOT_READY" ? (
                <Text as="p">{s.reasons.includes("NO_APPROVED_PLAN") || !s.reasons.includes("CONTEXT_NOT_ELIGIBLE") ? "No suggestion: the Supervisor cannot state this shortfall against a plan approved now." : "No suggestion: a window of this case changed since it was opened, so the shortfall cannot be stated."}</Text>
            ) : (
                <>
                    <Heading level={3}>Approved facts</Heading>
                    {s.facts.length ? (
                        <VStack as="ul" gap={2} aria-label="Approved facts" style={plainList}>
                            {s.facts.map((f) => (
                                <VStack as="li" gap={1} key={f.claim_id}>
                                    <Text as="p">
                                        <Kind label="approved_fact" />{" "}
                                        <strong>{f.subject_name || `entity ${short(f.subject_id)}`}</strong> {predicateWords[f.predicate] ?? f.predicate}{" "}
                                        {f.value !== null ? <strong>{f.value}{f.unit ? ` ${f.unit}` : ""}</strong> : <strong>{f.object_name || (f.object_entity_id ? `entity ${short(f.object_entity_id)}` : "")}</strong>}
                                        <Text size="sm" color="secondary"> · in effect from {f.effective_from}{f.effective_to ? ` until ${f.effective_to}` : ""}</Text>
                                    </Text>
                                    {s.passages.filter((p) => f.span_ids.includes(p.span_id)).map((p) => (
                                        <Blockquote key={p.span_id}>
                                            <span title={`Source ${p.source_id} revision ${p.revision}, passage ${p.span_id}`}>
                                                {p.text}
                                                <Text size="sm" color="secondary"> · source {short(p.source_id)} r{p.revision}</Text>
                                            </span>
                                        </Blockquote>
                                    ))}
                                </VStack>
                            ))}
                        </VStack>
                    ) : (
                        <Text as="p" color="secondary">No approved fact about this line, asset or order is available.</Text>
                    )}
                    <Heading level={3}>Observations</Heading>
                    <Table aria-label="Observations" density="compact">
                        <TableHeader>
                            <TableRow isHeaderRow>
                                <TableHeaderCell scope="col">Window</TableHeaderCell>
                                <TableHeaderCell scope="col">Measurement</TableHeaderCell>
                                <TableHeaderCell scope="col">Value</TableHeaderCell>
                            </TableRow>
                        </TableHeader>
                        <TableBody>
                            {s.observations.map((o) => (
                                <TableRow key={o.measurement_id}>
                                    <TableHeaderCell scope="row"><Time iso={o.window_start} />–<Time iso={o.window_end} /></TableHeaderCell>
                                    <TableCell><Kind label="observation" /> {metricWords[o.metric_name] ?? o.metric_name}</TableCell>
                                    <TableCell><Text hasTabularNumbers><Dec v={o.value_decimal} unit={o.unit} digits={0} /></Text></TableCell>
                                </TableRow>
                            ))}
                        </TableBody>
                    </Table>
                    <Heading level={3}>Hypotheses</Heading>
                    {s.hypotheses.length ? (
                        <VStack as="ul" gap={2} aria-label="Hypotheses" style={plainList}>
                            {s.hypotheses.map((h, i) => (
                                <li key={i}>
                                    <Kind label="hypothesis" /> {h.text}
                                    <span title={h.evidence_ids.join(", ")}>
                                        <Text size="sm" color="secondary"> · cites {h.evidence_ids.length} {h.evidence_ids.length === 1 ? "record" : "records"}</Text>
                                    </span>
                                </li>
                            ))}
                        </VStack>
                    ) : (
                        <Text as="p" color="secondary">No hypothesis available.</Text>
                    )}
                    {s.unknowns.length > 0 && (
                        <>
                            <Heading level={3}>Open questions</Heading>
                            <ul aria-label="Open questions">
                                {s.unknowns.map((u) => (
                                    <li key={u.field}>{u.question}{u.required ? " (required)" : ""}</li>
                                ))}
                            </ul>
                        </>
                    )}
                    {s.investigation && (
                        <Text as="p" size="sm" color="secondary">
                            From investigation run {short(s.investigation.run_id)} at case revision {s.investigation.case_revision}
                            {s.investigation.provider_actual ? "" : " · fixture or scripted model, not an actual model"}.
                        </Text>
                    )}
                </>
            )}
        </>
    );
}
