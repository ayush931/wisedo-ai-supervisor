import { useCallback, useEffect, useRef, useState } from "react";
import type { EvidenceSpanV1, KnowledgeClaimV1, KnowledgeEntityV1, KnowledgeReviewInputV1, KnowledgeReviewV1 } from "../../contracts/typescript/oee";
import { Intents, isConflict, mutate, request } from "./api";
import { Banner } from "@astryxdesign/core/Banner";
import { Blockquote } from "@astryxdesign/core/Blockquote";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { Token, type TokenColor } from "@astryxdesign/core/Token";
import { VStack } from "@astryxdesign/core/VStack";
import { Problem, canSupervise, short, useSession } from "./ui";

// WIS-616: the candidate entities and claims an onboarding agent turn proposed (WIS-612), with the
// passages each claim cites, and the human review of each claim. Every model-worded name and value
// renders as plain text (rule 08). A review is shown only from the server's receipt and the claim
// is then reloaded; nothing reads as approved before the server says so (rule 05).

export const entityTypes: Record<string, string> = {
    material: "Material", operation: "Operation", supplier: "Supplier", customer: "Customer", location: "Location",
    inspection_point: "Inspection point", tool: "Tool", role: "Role", process: "Process",
};
export const predicates: Record<string, string> = {
    supplied_by: "supplied by", received_at: "received at", stored_at: "stored at", inspected_at: "inspected at", ships_to: "ships to",
    consumes: "consumes", produces: "produces", precedes: "precedes", part_of: "is part of", requires_tool: "requires tool",
    responsible_role: "responsible role", lead_time_days: "lead time", cycle_time_seconds: "cycle time", setup_time_seconds: "setup time",
    batch_size: "batch size", yield_ratio: "yield", sample_size: "sample size", inspection_method: "inspection method",
    objective: "objective", actual_step: "actual step",
};
const states: Record<string, string> = { candidate: "Candidate", approved: "Approved", rejected: "Rejected", contested: "Contested" };
const stateColor: Record<string, TokenColor> = { candidate: "blue", approved: "green", rejected: "red", contested: "orange" };
const bare = { listStyle: "none", padding: 0, margin: 0 } as const;

// Unavailable names one item that could not be read, with the server's problem under it.
function Unavailable({ text, error }: { text: string; error: unknown }) {
    return (
        <VStack gap={1} role="group" aria-label={text}>
            <Banner status="warning" title={text} />
            <Problem error={error} />
        </VStack>
    );
}

type Loaded<T> = { value?: T; error?: unknown };

// useEntity reads one entity by ID, for its display name and type.
function useEntity(id: string | undefined) {
    const { site } = useSession();
    const [entity, setEntity] = useState<Loaded<KnowledgeEntityV1>>({});
    useEffect(() => {
        if (!id) return;
        let live = true;
        request<KnowledgeEntityV1>(`/v1/sites/${site.id}/entities/${id}`).then(
            (value) => live && setEntity({ value }),
            (error: unknown) => live && setEntity({ error }),
        );
        return () => {
            live = false;
        };
    }, [id, site.id]);
    return entity;
}

const entityName = (id: string, e: Loaded<KnowledgeEntityV1>) => e.value?.display_name ?? `entity ${short(id)}`;

export function literalText(literal: { value: unknown; unit?: string }) {
    const value = typeof literal.value === "string" ? literal.value : JSON.stringify(literal.value);
    return literal.unit ? `${value} ${literal.unit}` : value;
}

export function ProposedFacts({ entityIDs, claimIDs }: { entityIDs: readonly string[]; claimIDs: readonly string[] }) {
    if (entityIDs.length === 0 && claimIDs.length === 0) return null;
    return (
        <VStack as="section" gap={2} aria-labelledby="proposed-facts-title">
            <HStack gap={2} align="center">
                <Heading level={3} id="proposed-facts-title">
                    Proposed by the agent
                </Heading>
                <Token label="Candidates until reviewed" color="blue" size="sm" />
            </HStack>
            <Text as="p" color="secondary">
                Nothing here is a fact until a reviewer approves it against its cited passage.
            </Text>
            <Heading level={4}>Entities {entityIDs.length}</Heading>
            <HStack as="ul" gap={1} wrap="wrap" style={bare}>
                {entityIDs.map((id) => (
                    <ProposedEntity key={id} id={id} />
                ))}
            </HStack>
            <Heading level={4}>Claims {claimIDs.length}</Heading>
            {claimIDs.map((id) => (
                <ProposedClaim key={id} id={id} />
            ))}
        </VStack>
    );
}

function ProposedEntity({ id }: { id: string }) {
    const entity = useEntity(id);
    if (entity.error)
        return (
            <li>
                <Unavailable text={`Entity ${short(id)} could not be loaded.`} error={entity.error} />
            </li>
        );
    if (!entity.value)
        return (
            <li>
                <Text color="secondary">Loading entity {short(id)}…</Text>
            </li>
        );
    return (
        <li>
            <Card padding={2}>
                <HStack gap={1} align="center">
                    <Text size="sm" color="secondary">
                        {entityTypes[entity.value.type] ?? entity.value.type}
                    </Text>
                    <Text weight="semibold">{entity.value.display_name}</Text>
                </HStack>
            </Card>
        </li>
    );
}

export function ProposedClaim({ id }: { id: string }) {
    const { site } = useSession();
    const [claim, setClaim] = useState<Loaded<KnowledgeClaimV1>>({});
    const sequence = useRef(0);
    const reload = useCallback(() => {
        const n = ++sequence.current;
        request<KnowledgeClaimV1>(`/v1/sites/${site.id}/claims/${id}`).then(
            (value) => n === sequence.current && setClaim({ value }),
            // A failed reload keeps the claim last read (and so the review receipt under it) on
            // screen with the error beside it; only a first load that fails shows no claim.
            (error: unknown) => n === sequence.current && setClaim((prior) => ({ value: prior.value, error })),
        );
    }, [id, site.id]);
    useEffect(reload, [reload]);
    const subject = useEntity(claim.value?.subject_id);
    const object = useEntity(claim.value?.object_entity_id);

    if (claim.error && !claim.value)
        return (
            <Card padding={3} role="article" aria-label={`Claim ${short(id)}`}>
                <Unavailable text={`Claim ${short(id)} could not be loaded.`} error={claim.error} />
            </Card>
        );
    if (!claim.value)
        return (
            <Card padding={3} role="article" aria-label={`Claim ${short(id)}`}>
                <Text color="secondary">Loading claim {short(id)}…</Text>
            </Card>
        );
    const c = claim.value;
    const predicate = predicates[c.predicate] ?? c.predicate.replaceAll("_", " ");
    const target = c.object_entity_id ? entityName(c.object_entity_id, object) : c.literal ? literalText(c.literal) : "";
    const sentence = `${entityName(c.subject_id, subject)} ${predicate} ${target}`.trim();
    const ocr = c.corroboration.model_extracted_sources;
    return (
        <Card padding={3} role="article" aria-label={`Claim: ${sentence}`}>
            <VStack gap={2}>
                <HStack gap={2} align="center" wrap="wrap">
                    <Text weight="semibold">{sentence}</Text>
                    <Token label={states[c.state] ?? c.state} color={stateColor[c.state] ?? "gray"} size="sm" />
                </HStack>
                {subject.error !== undefined && <Unavailable text="The subject of this claim could not be loaded." error={subject.error} />}
                {c.object_entity_id && object.error !== undefined && <Unavailable text="The object of this claim could not be loaded." error={object.error} />}
                {claim.error !== undefined && <Unavailable text="The claim could not be refreshed; what is shown may be out of date." error={claim.error} />}
                {ocr > 0 && (
                    <Banner
                        status="warning"
                        title={`${ocr === 1 ? "A cited source is" : `${ocr} cited sources are`} model-extracted text (OCR of a scanned page). Check the passage against the original page before approving.`}
                    />
                )}
                {c.source_span_ids.map((spanID) => (
                    <Passage key={spanID} id={spanID} />
                ))}
                <ReviewClaim claim={c} onReload={reload} />
            </VStack>
        </Card>
    );
}

export function Passage({ id }: { id: string }) {
    const { site } = useSession();
    const [span, setSpan] = useState<Loaded<EvidenceSpanV1>>({});
    useEffect(() => {
        let live = true;
        request<EvidenceSpanV1>(`/v1/sites/${site.id}/source-spans/${id}`).then(
            (value) => live && setSpan({ value }),
            (error: unknown) => live && setSpan({ error }),
        );
        return () => {
            live = false;
        };
    }, [id, site.id]);
    if (span.error) return <Unavailable text={`Cited passage unavailable (${short(id)}).`} error={span.error} />;
    if (!span.value) return <Text color="secondary">Loading cited passage…</Text>;
    return <Blockquote style={{ whiteSpace: "pre-wrap" }}>{span.value.text}</Blockquote>;
}

// ReviewClaim records an approve or reject decision for a candidate claim. The decision binds the
// claim revision on screen (If-Match), so a claim that changed underneath is reloaded, not decided.
function ReviewClaim({ claim, onReload }: { claim: KnowledgeClaimV1; onReload: () => void }) {
    const { site, online } = useSession();
    // Only an owner or supervisor may review a claim (internal/knowledge reviews); the control is
    // gated here, not only by whoever renders it.
    const mayReview = canSupervise(site.role);
    const [reason, setReason] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [receipt, setReceipt] = useState<KnowledgeReviewV1 | null>(null);
    const intents = useRef(new Intents()).current;

    async function decide(verdict: "approve" | "reject") {
        setBusy(true);
        setError(null);
        const body: KnowledgeReviewInputV1 = { claim_revision: claim.revision, verdict, reason: reason.trim() };
        try {
            const out = await mutate<KnowledgeReviewV1>(intents, `review:${claim.claim_id}`, `/v1/sites/${site.id}/claims/${claim.claim_id}/reviews`, body, claim.revision);
            setReceipt(out);
            onReload();
        } catch (e) {
            setError(e);
            if (isConflict(e)) onReload();
        } finally {
            setBusy(false);
        }
    }

    const decided = receipt && receipt.claim_id === claim.claim_id;
    const blocked = busy || !online || reason.trim() === "";
    return (
        <>
            {decided && (
                <div role="status">
                    <Banner status="success" title={`${receipt.verdict === "approve" ? "Approved" : "Rejected"}. Review ${short(receipt.review_id)} recorded.`} />
                </div>
            )}
            {mayReview && claim.state === "candidate" && !decided && (
                <HStack gap={2} align="end" wrap="wrap">
                    <TextInput label="Review reason" value={reason} {...{ maxLength: 2000 }} onChange={(v) => setReason(v)} width={320} />
                    <Button label="Approve" variant="primary" isDisabled={blocked} onClick={() => decide("approve")} />
                    <Button label="Reject" variant="destructive" isDisabled={blocked} onClick={() => decide("reject")} />
                </HStack>
            )}
            <Problem error={error} />
        </>
    );
}
