import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { VStack } from "@astryxdesign/core/VStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Text } from "@astryxdesign/core/Text";
import type { KnowledgeClaimPageV1 } from "../../contracts/typescript/oee";
import { CorrectionQueue } from "./corrections";
import { request } from "./api";
import { ProposedClaim } from "./proposed-facts";
import { Problem, canSupervise, short, useSession } from "./ui";

// WIS-641: every candidate claim of the site, whoever proposed it (an onboarding agent turn, an
// interview or a person), for a reviewer who was not the one interviewed. An interview page shows
// its own run's proposals only to its participant; this page lists them for the whole site. Each
// claim uses the same card and review controls as the interview page (WIS-616): the cited passages
// load through the evidence read, which checks revocation, and a review shows only from the
// server's receipt. The list comes from GET /claims?state=candidate, which leaves out claims whose
// sources are revoked; a claim reviewed here stays on screen, with its receipt, until reload.
export function ReviewQueue() {
    const { site } = useSession();
    const [ids, setIds] = useState<string[] | null>(null);
    const [next, setNext] = useState<string | undefined>();
    const [error, setError] = useState<unknown>(null);
    const [loading, setLoading] = useState(false);
    const sequence = useRef(0);

    const load = useCallback(
        async (cursor?: string) => {
            const n = ++sequence.current;
            setLoading(true);
            setError(null);
            const query = new URLSearchParams({ state: "candidate", limit: "50" });
            if (cursor) query.set("cursor", cursor);
            try {
                const page = await request<KnowledgeClaimPageV1>(`/v1/sites/${site.id}/claims?${query}`);
                if (n !== sequence.current) return;
                const fresh = page.claims.map((c) => c.claim_id);
                setIds((prior) => (cursor && prior ? [...prior, ...fresh.filter((id) => !prior.includes(id))] : fresh));
                setNext(page.next_cursor || undefined);
            } catch (e) {
                if (n === sequence.current) setError(e);
            } finally {
                if (n === sequence.current) setLoading(false);
            }
        },
        [site.id],
    );
    useEffect(() => {
        void load();
    }, [load]);

    return (
        <VStack as="section" aria-labelledby="review-queue-title" gap={3}>
            <Heading level={2} id="review-queue-title">
                Facts to review
            </Heading>
            {!canSupervise(site.role) && (
                <Text as="p" size="sm" color="secondary">
                    Reviewing needs the owner or supervisor role at this site; you can read the candidates.
                </Text>
            )}
            {error !== null && (
                <VStack gap={1}>
                    <Text as="p" weight="semibold">
                        {ids === null ? "The candidate facts could not be loaded." : "More candidate facts could not be loaded."}
                    </Text>
                    <Problem error={error} />
                </VStack>
            )}
            {ids === null && error === null && <Skeleton height={160} aria-label="Loading candidate facts" />}
            {ids !== null && ids.length === 0 && (
                <Card variant="muted" padding={3}>
                    <Text as="p" color="secondary">
                        No candidate facts to review. New ones appear here when an interview, a document or the agent proposes them.
                    </Text>
                </Card>
            )}
            {ids !== null && ids.length > 0 && (
                <VStack gap={2} role="list" aria-label="Candidate facts">
                    {ids.map((id) => (
                        // Two candidates can read the same; the short ID tells them apart.
                        <div role="listitem" key={id} aria-label={`Candidate fact ${short(id)}`}>
                            <ProposedClaim id={id} />
                        </div>
                    ))}
                </VStack>
            )}
            <CorrectionQueue />
            {next && (
                <HStack>
                    <Button label="Load more candidate facts" onClick={() => void load(next)} isDisabled={loading}>
                        {loading ? "Loading…" : "Load more candidate facts"}
                    </Button>
                </HStack>
            )}
        </VStack>
    );
}
