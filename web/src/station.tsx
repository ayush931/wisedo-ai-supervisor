import React, { useEffect, useState } from "react";
import type { EvidenceSourceV1, StationAuthorizationPageV1 } from "../../contracts/typescript/oee";
import { allPages, request } from "./api";
import { Badge } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { EmptyState } from "@astryxdesign/core/EmptyState";
import { Heading } from "@astryxdesign/core/Heading";
import { List, ListItem } from "@astryxdesign/core/List";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { HStack, VStack } from "@astryxdesign/core/Stack";
import { Text } from "@astryxdesign/core/Text";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { Link, Problem, useSession, useStations, type Station } from "./ui";

// WIS-278 operator surfaces: the stations an operator may record at, and the job's procedure
// shown from its exact approved source revision.

const stationKey = (site: string) => `supervisor.station.${site}`;
function read(key: string): string | null {
    try {
        return localStorage.getItem(key);
    } catch {
        return null;
    }
}
function write(key: string, value: string) {
    try {
        localStorage.setItem(key, value);
    } catch {
        // storage unavailable: the choice is simply not remembered
    }
}

// MyStation lists the stations the signed-in person holds an active grant for.
export function MyStation() {
    const { site, me } = useSession();
    const { list, error } = useStations();
    const [granted, setGranted] = useState<Station[] | null>(null);
    const [unchecked, setUnchecked] = useState(0);
    useEffect(() => {
        if (!list) return;
        let live = true;
        // ponytail: one authorizations read per station; fine up to ~100 stations. Beyond that
        // the API needs a "my grants" read.
        Promise.allSettled(
            list.map((w) =>
                allPages(async (cursor) => {
                    const page = await request<StationAuthorizationPageV1>(
                        `/v1/sites/${site.id}/workstations/${w.id}/authorizations?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
                    );
                    return { items: page.authorizations, next_cursor: page.next_cursor };
                }).then((grants) => grants.some((g) => g.active && g.grantee.subject === me.subject)),
            ),
        ).then((results) => {
            if (!live) return;
            setGranted(list.filter((_, i) => results[i].status === "fulfilled" && (results[i] as PromiseFulfilledResult<boolean>).value));
            setUnchecked(results.filter((r) => r.status === "rejected").length);
        });
        return () => {
            live = false;
        };
    }, [list, site.id, me.subject]);
    const remembered = read(stationKey(site.id));
    const last = granted?.find((w) => w.id === remembered);
    const remember = (id: string) => () => write(stationKey(site.id), id);
    return (
        <VStack gap={4} maxWidth={720}>
            <Heading level={1}>My station</Heading>
            <Problem error={error} />
            {!granted && !error && (
                <div role="status">
                    <Text color="secondary">Checking your stations…</Text>
                    <Skeleton height={48} />
                </div>
            )}
            {last && (
                <Button
                    label={`Continue at ${last.name}`}
                    variant="primary"
                    size="lg"
                    href={`/sites/${site.id}/workstations/${last.id}`}
                    onClick={remember(last.id)}
                />
            )}
            {granted && granted.length > 0 && (
                <Card padding={1}>
                    <List aria-label="Your stations" hasDividers>
                        {granted.map((w) => (
                            <ListItem
                                key={w.id}
                                label={
                                    <Link to={`/sites/${site.id}/workstations/${w.id}`} onNavigate={remember(w.id)}>
                                        {w.name}
                                    </Link>
                                }
                            />
                        ))}
                    </List>
                </Card>
            )}
            {granted && !granted.length && !unchecked && (
                <EmptyState title="No station assigned" description="A supervisor can give you access to a workstation." headingLevel={2} />
            )}
            {unchecked > 0 && (
                <div role="alert">
                    <Banner
                        status="warning"
                        title={`${unchecked} station${unchecked === 1 ? "" : "s"} could not be checked and ${unchecked === 1 ? "is" : "are"} not listed. Try again shortly.`}
                    />
                </div>
            )}
        </VStack>
    );
}

export type Step = { title: string; body: string[] };
// parseSteps splits an instruction's plain text into steps without adding any words: markdown
// headings start a step (lines under them are its body); without headings every non-empty
// line is a step.
export function parseSteps(text: string): Step[] {
    const lines = text
        .split(/\r?\n/)
        .map((l) => l.trim())
        .filter(Boolean);
    const heading = /^#{1,6}\s+(.*)$/;
    if (!lines.some((l) => heading.test(l))) return lines.map((title) => ({ title, body: [] }));
    const steps: Step[] = [];
    for (const l of lines) {
        const m = heading.exec(l);
        if (m) steps.push({ title: m[1], body: [] });
        else if (steps.length) steps[steps.length - 1].body.push(l);
        else steps.push({ title: l, body: [] });
    }
    return steps;
}

// Procedure shows the job's instruction document at the exact revision the job pins, and only
// when its checksum matches; otherwise it says why and shows no text (rule 06).
export function Procedure({ jobKey, documentId, revision, sha256 }: { jobKey: string; documentId: string; revision: number; sha256: string }) {
    const { site } = useSession();
    const [doc, setDoc] = useState<EvidenceSourceV1 | null>(null);
    const [error, setError] = useState<unknown>(null);
    const key = `supervisor.procedure.${jobKey}.${documentId}.${revision}`;
    const [done, setDone] = useState<number[]>(() => {
        try {
            const v = JSON.parse(read(key) ?? "[]");
            return Array.isArray(v) ? v.filter((n) => Number.isInteger(n)) : [];
        } catch {
            return [];
        }
    });
    const [at, setAt] = useState<number | null>(null);
    useEffect(() => {
        let live = true;
        setDoc(null);
        setError(null);
        request<EvidenceSourceV1>(`/v1/sites/${site.id}/sources/${encodeURIComponent(documentId)}?revision=${revision}`).then(
            (d) => live && setDoc(d),
            (e) => live && setError(e),
        );
        return () => {
            live = false;
        };
    }, [site.id, documentId, revision]);
    const head = (
        <Heading level={2} id="guide-title">
            How to do this job
        </Heading>
    );
    const shell = (children: React.ReactNode) => (
        <Card role="region" aria-labelledby="guide-title" padding={4}>
            <VStack gap={3}>{children}</VStack>
        </Card>
    );
    if (error)
        return shell(
            <>
                {head}
                <Text as="p" color="secondary">
                    The procedure could not be loaded, so no steps are shown.
                </Text>
                <Problem error={error} />
            </>,
        );
    if (!doc)
        return shell(
            <>
                {head}
                <div role="status">
                    <VisuallyHidden>Loading the procedure…</VisuallyHidden>
                    <Skeleton height={96} />
                </div>
            </>,
        );
    if (doc.sha256 !== sha256 || doc.revision !== revision)
        return shell(
            <>
                {head}
                <div role="alert">
                    <Banner status="warning" title="This document does not match the revision approved for this job, so no steps are shown. Ask your supervisor." />
                </div>
            </>,
        );
    const steps = parseSteps(doc.text);
    if (!steps.length)
        return shell(
            <>
                {head}
                <Text as="p" color="secondary">
                    The procedure has no text.
                </Text>
            </>,
        );
    const k = Math.min(at ?? Math.max(0, steps.findIndex((_, i) => !done.includes(i))), steps.length - 1);
    const markDone = () => {
        const next = done.includes(k) ? done : [...done, k];
        setDone(next);
        write(key, JSON.stringify(next));
        setAt(Math.min(k + 1, steps.length - 1));
    };
    return shell(
        <>
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                {head}
                <Badge variant={done.length === steps.length ? "green" : "neutral"} label={`${done.length} of ${steps.length} done`} />
            </HStack>
            <HStack as="ol" gap={2} wrap="wrap" aria-label="Steps">
                {steps.map((s, i) => (
                    <li key={i}>
                        <Button
                            label={`Step ${i + 1}${done.includes(i) ? ", done" : ""}: ${s.title}`}
                            variant={i === k ? "primary" : done.includes(i) ? "secondary" : "ghost"}
                            size="sm"
                            aria-current={i === k ? "step" : undefined}
                            tooltip={s.title}
                            onClick={() => setAt(i)}
                        >
                            {done.includes(i) ? "✓" : i + 1} {s.title.length > 28 ? `${s.title.slice(0, 27)}…` : s.title}
                        </Button>
                    </li>
                ))}
            </HStack>
            <VStack gap={1} aria-live="polite">
                <Text size="sm" color="secondary" hasTabularNumbers>
                    Step {k + 1} of {steps.length}
                </Text>
                <Heading level={3}>{steps[k].title}</Heading>
                {steps[k].body.map((line, i) => (
                    <Text as="p" key={i}>
                        {line}
                    </Text>
                ))}
            </VStack>
            <HStack justify="between" align="center" gap={2} wrap="wrap">
                <Text size="sm" color="secondary">
                    From {doc.title}, rev {revision}
                </Text>
                <HStack gap={2}>
                    <Button label="Back" isDisabled={k === 0} onClick={() => setAt(k - 1)} />
                    <Button label={k === steps.length - 1 ? "Done" : "Done, next step"} variant="primary" onClick={markDone} />
                </HStack>
            </HStack>
        </>,
    );
}
