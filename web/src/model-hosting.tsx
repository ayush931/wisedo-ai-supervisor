import React, { useCallback, useEffect, useMemo, useState } from "react";
import type { SiteModelHostingV1 } from "../../contracts/typescript/oee";
import { Intents, isConflict, mutate, request } from "./api";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Card } from "@astryxdesign/core/Card";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Skeleton } from "@astryxdesign/core/Skeleton";
import { Text } from "@astryxdesign/core/Text";
import { TextArea } from "@astryxdesign/core/TextArea";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { Problem, Time, lengthError, useSession } from "./ui";

// ModelHosting (WIS-623) is the site's switch for hosted model inference (rule 08). It is off
// unless an owner of this site turns it on with a data processing agreement reference; turning
// it off stops the next model call, including work already queued. Supervisors read it.
export function ModelHosting() {
    const { site, online } = useSession();
    const path = `/v1/sites/${site.id}/model-hosting`;
    const [state, setState] = useState<SiteModelHostingV1 | null>(null);
    const [loadError, setLoadError] = useState<unknown>(null);
    const [dpa, setDpa] = useState("");
    const [reason, setReason] = useState("");
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<unknown>(null);
    const [done, setDone] = useState("");
    const intents = useMemo(() => new Intents(), []);
    const isOwner = site.role === "owner";

    const load = useCallback(() => {
        request<SiteModelHostingV1>(path).then(
            (v) => {
                setState(v);
                setLoadError(null);
            },
            setLoadError,
        );
    }, [path]);
    useEffect(load, [load]);

    // Astryx disables with aria-disabled and marks required fields without the native attribute,
    // so Enter in a field would still submit: the same guard the disabled button shows.
    const reasonTooLong = lengthError(reason, 2000);
    const blocked = !state || busy || !online || !reason.trim() || !!reasonTooLong || (!state.enabled && !dpa.trim());
    async function decide(e: React.FormEvent) {
        e.preventDefault();
        if (!state || blocked) return;
        const enable = !state.enabled;
        setBusy(true);
        setError(null);
        setDone("");
        try {
            const body = enable ? { enabled: true, dpa_reference: dpa.trim(), reason: reason.trim() } : { enabled: false, reason: reason.trim() };
            const v = await mutate<SiteModelHostingV1>(intents, "decide", `${path}/decisions`, body, state.revision);
            setReason("");
            setDpa("");
            if (v.replayed) {
                // A replay is the receipt of the decision this key recorded earlier, not the switch
                // now: another decision may have followed it. Read the current state instead.
                setDone(`This decision was already recorded at revision ${v.revision}. The current setting is shown above.`);
                load();
            } else {
                setState(v);
                setDone(v.enabled ? "Hosted model inference is on for this site." : "Hosted model inference is off for this site. The next model call uses no hosted endpoint.");
            }
        } catch (err) {
            setError(err);
            if (isConflict(err)) load();
        } finally {
            setBusy(false);
        }
    }

    return (
        <VStack as="section" aria-labelledby="hosting-title" gap={3} maxWidth={720}>
            <Heading level={1} id="hosting-title">
                Model hosting
            </Heading>
            <Text as="p" size="sm" color="secondary">
                Off by default: the agent uses only this site's own model server. An owner can allow Qwen on Alibaba Cloud Model Studio (Singapore). Names the Supervisor knows are
                replaced before sending; a name written only inside a document is not.
            </Text>
            {loadError ? <Problem error={loadError} context="The model hosting setting could not be loaded." /> : null}
            {!state && !loadError && <Skeleton height={96} />}
            {state && (
                <>
                    <Card padding={3}>
                        <HStack role="status" aria-label="Current setting" align="center" gap={2} wrap="wrap">
                            <Badge variant={state.enabled ? "warning" : "neutral"} label={<strong>{state.enabled ? "On" : "Off"}</strong>} />
                            <Text size="sm" color="secondary">
                                {state.enabled && state.dpa_reference ? `data processing agreement ${state.dpa_reference} · ` : ""}revision {state.revision}
                            </Text>
                        </HStack>
                    </Card>
                    {isOwner ? (
                        <Card padding={3}>
                            <VStack as="form" gap={2} onSubmit={decide} aria-label={state.enabled ? "Turn hosted model off" : "Turn hosted model on"}>
                                {!state.enabled && (
                                    <TextInput id="hosting-dpa" label="Data processing agreement reference" isRequired value={dpa} {...{ maxLength: 200 }} onChange={(v) => setDpa(v)} />
                                )}
                                <TextArea id="hosting-reason" label="Reason" isRequired status={reasonTooLong} rows={2} value={reason} onChange={(v) => setReason(v)} />
                                <HStack>
                                    <Button
                                        type="submit"
                                        variant={state.enabled ? "destructive" : "primary"}
                                        label={state.enabled ? "Turn hosted model off" : "Turn hosted model on"}
                                        isDisabled={blocked}
                                    >
                                        {busy ? "Submitting…" : state.enabled ? "Turn hosted model off" : "Turn hosted model on"}
                                    </Button>
                                </HStack>
                            </VStack>
                        </Card>
                    ) : (
                        <Text as="p" size="sm" color="secondary">
                            Only an owner of this site can change this.
                        </Text>
                    )}
                    {done && (
                        <Text as="p" role="status" weight="semibold">
                            {done}
                        </Text>
                    )}
                    <Problem error={error} />
                    <Heading level={2}>Decisions</Heading>
                    {state.decisions.length === 0 ? (
                        <Text as="p" size="sm" color="secondary">
                            Never turned on.
                        </Text>
                    ) : (
                        <VStack as="ol" gap={2} aria-label="Model hosting decisions" style={{ listStyle: "none", margin: 0, padding: 0 }}>
                            {state.decisions.map((d) => (
                                <li key={d.revision}>
                                    <HStack align="center" gap={2} wrap="wrap">
                                        <Badge variant={d.enabled ? "warning" : "neutral"} label={d.enabled ? "Turned on" : "Turned off"} />
                                        <Text size="sm" color="secondary">
                                            revision {d.revision} · <Time iso={d.created_at} />
                                        </Text>
                                    </HStack>
                                    <Text as="p" size="sm">
                                        Reason: {d.reason}
                                    </Text>
                                </li>
                            ))}
                        </VStack>
                    )}
                </>
            )}
        </VStack>
    );
}
