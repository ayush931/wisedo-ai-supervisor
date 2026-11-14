import { useCallback, useEffect, useRef, useState } from "react";
import type { CorrectionSummaryV1, EvidenceSourcePageV1, EvidenceSourceV1, EvidenceSpanV1, KnowledgeClaimPageV1, KnowledgeClaimV1, KnowledgeEntityPageV1, MemoryCandidatePageV1, MemoryCandidateV1, MemoryCandidateRevisionInputV1, OutcomeFeedbackInputV1, OutcomeFeedbackV1 } from "../../contracts/typescript/oee";
import { Intents, allPages, isConflict, mutate, request } from "./api";
import { Passage, literalText, predicates } from "./proposed-facts";
import { Link, Problem, Time, canSupervise, short, usePolling, useSession } from "./ui";

type Subject = OutcomeFeedbackInputV1["subject"];
type Claim = NonNullable<MemoryCandidateRevisionInputV1["claim"]>;
const properties: Record<string, string> = { cycle_time_seconds: "s", setup_time_seconds: "s", lead_time_days: "day", batch_size: "count", sample_size: "count", yield_ratio: "", inspection_method: "", objective: "", actual_step: "" };
const textProperties = new Set(["inspection_method", "objective", "actual_step"]);

export function CorrectOutput({ subject, question = "" }: { subject: Subject; question?: string }) {
    const [open, setOpen] = useState(false);
    const [saved, setSaved] = useState<MemoryCandidateV1 | null>(null);
    if (saved) return <CorrectionCard initial={saved} />;
    return <div className="stack">
        <button type="button" aria-expanded={open} onClick={() => setOpen(!open)}>This is wrong</button>
        {open && <CorrectionForm subject={subject} question={question} onSaved={setSaved} />}
    </div>;
}

function CorrectionForm({ subject, question = "", candidate, onSaved }: { subject?: Subject; question?: string; candidate?: MemoryCandidateV1; onSaved: (c: MemoryCandidateV1) => void }) {
    const { site, online } = useSession();
    const base = `/v1/sites/${site.id}`;
    const [reason, setReason] = useState(candidate?.content.reason ?? "");
    const [q, setQ] = useState(candidate?.content.question ?? question);
    const [expected, setExpected] = useState(candidate?.content.expected_answer ?? "");
    const [details, setDetails] = useState(!!candidate?.content.claim);
    const [claim, setClaim] = useState<Claim | undefined>(candidate?.content.claim);
    const [revision, setRevision] = useState(candidate?.content.expected_base_revision ?? 0);
    const [mode, setMode] = useState(candidate?.content.claim ? "existing" : "");
    const [claims, setClaims] = useState<KnowledgeClaimPageV1["claims"]>([]);
    const [entities, setEntities] = useState<KnowledgeEntityPageV1["entities"]>([]);
    const [sources, setSources] = useState<EvidenceSourcePageV1["sources"]>([]);
    const [source, setSource] = useState<EvidenceSourceV1 | null>(null);
    const [sourceID, setSourceID] = useState("");
    const [start, setStart] = useState("0");
    const [end, setEnd] = useState("0");
    const [value, setValue] = useState(candidate?.content.claim?.literal ? String(candidate.content.claim.literal.value) : "");
    const [error, setError] = useState<unknown>(null);
    const [busy, setBusy] = useState(false);
    const intents = useRef(new Intents()).current;
    const savedSpan = useRef<{ body: string; span: EvidenceSpanV1 } | null>(null);
    const savedFeedback = useRef<{ body: string; id: string } | null>(null);
    useEffect(() => {
        if (!details) return;
        let live = true;
        Promise.all([
            allPages(async (cursor) => { const p = await request<KnowledgeClaimPageV1>(`${base}/claims?state=approved&limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`); return { items: p.claims, next_cursor: p.next_cursor }; }),
            allPages(async (cursor) => { const p = await request<EvidenceSourcePageV1>(`${base}/sources?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`); return { items: p.sources, next_cursor: p.next_cursor }; }),
            allPages(async (cursor) => { const p = await request<KnowledgeEntityPageV1>(`${base}/entities?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`); return { items: p.entities, next_cursor: p.next_cursor }; }),
        ]).then(([c, s, e]) => { if (live) { setClaims(c); setSources(s); setEntities(e); } }, (e: unknown) => live && setError(e));
        return () => { live = false; };
    }, [details, base]);
    useEffect(() => {
        setSource(null);
        if (!sourceID) return;
        let live = true;
        request<EvidenceSourceV1>(`${base}/sources/${sourceID}`).then((s) => { if (live) { setSource(s); setStart("0"); setEnd(String(s.rune_length)); } }, (e: unknown) => live && setError(e));
        return () => { live = false; };
    }, [sourceID, base]);
    async function selectClaim(id: string) {
        setBusy(true); setError(null);
        try {
            const c = await request<KnowledgeClaimV1>(`${base}/claims/${id}`);
            setClaim({ claim_id: c.claim_id, subject_id: c.subject_id, subject_revision: c.subject_revision, predicate: c.predicate, object_entity_id: c.object_entity_id, object_revision: c.object_revision, literal: c.literal, source_span_ids: c.source_span_ids, evidence_class: "report", effective_from: c.effective_from, effective_to: c.effective_to });
            setRevision(c.revision); setValue(c.literal ? String(c.literal.value) : "");
        } catch (e) { setError(e); } finally { setBusy(false); }
    }
    function newClaim(entityID: string) {
        const e = entities.find((x) => x.entity_id === entityID);
        setClaim(e ? { claim_id: crypto.randomUUID(), subject_id: e.entity_id, subject_revision: e.revision, predicate: "cycle_time_seconds", literal: { type: "decimal", value: "", unit: "s" }, source_span_ids: [], evidence_class: "report", effective_from: new Date().toISOString() } : undefined);
        setRevision(0); setValue("");
    }
    async function submit(e: React.FormEvent) {
        e.preventDefault(); setBusy(true); setError(null);
        try {
            let proposed = details ? claim : undefined;
            if (proposed?.literal) proposed = { ...proposed, literal: { ...proposed.literal, value } };
            if (proposed && source) {
                const body = { revision: source.revision, start_char: Number(start), end_char: Number(end) };
                const key = JSON.stringify({ source_id: source.source_id, ...body });
                const span = savedSpan.current?.body === key ? savedSpan.current.span : await mutate<EvidenceSpanV1>(intents, "correction-span", `${base}/sources/${source.source_id}/spans`, body, source.revision);
                savedSpan.current = { body: key, span };
                proposed = { ...proposed, source_span_ids: [span.id] };
            }
            if (details && !proposed) throw new Error("Choose a proposed fact or turn off optional details.");
            const content: MemoryCandidateRevisionInputV1 = { reason: reason.trim(), question: q.trim(), expected_answer: expected.trim(), expected_base_revision: proposed ? revision : 0, ...(proposed ? { claim: proposed } : {}) };
            if (candidate) onSaved(await mutate<MemoryCandidateV1>(intents, `amend:${candidate.candidate_id}`, `${base}/memory-candidates/${candidate.candidate_id}/revisions`, content, candidate.revision));
            else if (subject) {
                const input: OutcomeFeedbackInputV1 = { schema_version: 1, subject, kind: "correction", verdict: "unhelpful", reason: content.reason, evidence_ids: [], proposed_correction: content };
                const key = JSON.stringify(input);
                let id = savedFeedback.current?.body === key ? savedFeedback.current.id : undefined;
                if (!id) {
                    const out = await mutate<OutcomeFeedbackV1>(intents, "correction", `${base}/feedback`, input);
                    if (!out.candidate_id) throw new Error("The correction did not return a review candidate.");
                    id = out.candidate_id;
                    savedFeedback.current = { body: key, id };
                }
                onSaved(await request<MemoryCandidateV1>(`${base}/memory-candidates/${id}`));
            }
        } catch (e) { setError(e); } finally { setBusy(false); }
    }
    const numeric = claim?.literal?.type === "decimal";
    return <form className="card stack" aria-label="Correct this output" onSubmit={(e) => void submit(e)}>
        {(subject?.kind === "answer" || candidate?.kind === "answer") && <p>This shares only the selected question and answer with authorized site reviewers. Your other conversation turns remain private.</p>}
        <label>Correction reason<textarea required maxLength={2000} value={reason} onChange={(e) => setReason(e.target.value)} /></label>
        <label>Regression question (optional)<input maxLength={4000} value={q} onChange={(e) => setQ(e.target.value)} /></label>
        <label>Expected factual answer (optional)<textarea maxLength={8000} value={expected} onChange={(e) => setExpected(e.target.value)} /></label>
        <p className="meta">A question, a complete proposed fact with current evidence, and the expected factual answer are required before approval. Submitting a reason alone keeps the correction incomplete.</p>
        <label><input type="checkbox" checked={details} onChange={(e) => setDetails(e.target.checked)} />Add a proposed fact and source</label>
        {details && <fieldset disabled={busy}><legend>Proposed fact</legend>
            <label>Fact to correct<select value={mode} onChange={(e) => { setMode(e.target.value); setClaim(undefined); setRevision(0); }}><option value="">Choose</option><option value="existing">An existing approved fact</option><option value="new">A new fact</option></select></label>
            {mode === "existing" && <label>Approved fact<select value={claim?.claim_id ?? ""} onChange={(e) => e.target.value && void selectClaim(e.target.value)}><option value="">Choose a fact cited by this output</option>{claims.map((c) => <option key={c.claim_id} value={c.claim_id}>{predicates[c.predicate] ?? c.predicate} · {short(c.claim_id)} · r{c.revision}</option>)}</select></label>}
            {mode === "new" && <label>Subject entity<select value={claim?.subject_id ?? ""} onChange={(e) => newClaim(e.target.value)}><option value="">Choose</option>{entities.map((e) => <option key={e.entity_id} value={e.entity_id}>{e.display_name} · {e.type}</option>)}</select></label>}
            {claim && <>
                <p>Claim {short(claim.claim_id)} · base revision {revision}</p>
                {mode === "new" && <label>Property<select value={claim.predicate} onChange={(e) => setClaim({ ...claim, predicate: e.target.value, literal: { type: textProperties.has(e.target.value) ? "string" : "decimal", value, unit: properties[e.target.value] || undefined } })}>{Object.keys(properties).map((p) => <option key={p} value={p}>{predicates[p] ?? p}</option>)}</select></label>}
                {claim.literal ? <label>Correct value{numeric ? " (exact decimal)" : " (text)"}<input required value={value} inputMode={numeric ? "decimal" : "text"} pattern={numeric ? "-?(0|[1-9][0-9]*)(\\.[0-9]{1,6})?" : undefined} onChange={(e) => setValue(e.target.value)} /><span className="meta">{claim.literal.unit || "No unit"}</span></label> : <label>Correct target entity<select value={claim.object_entity_id ?? ""} onChange={(e) => { const target = entities.find((x) => x.entity_id === e.target.value); if (target) setClaim({ ...claim, object_entity_id: target.entity_id, object_revision: target.revision }); }}>{entities.map((e) => <option key={e.entity_id} value={e.entity_id}>{e.display_name}</option>)}</select></label>}
                <label>Evidence source<select value={sourceID} onChange={(e) => setSourceID(e.target.value)}><option value="">{claim.source_span_ids.length ? "Keep existing cited passages" : "Choose current evidence"}</option>{sources.map((s) => <option key={s.source_id} value={s.source_id}>{s.title} · r{s.revision}</option>)}</select></label>
                {source && <><label>Passage start character<input type="number" required min={0} max={source.rune_length - 1} value={start} onChange={(e) => setStart(e.target.value)} /></label><label>Passage end character<input type="number" required min={Number(start) + 1} max={source.rune_length} value={end} onChange={(e) => setEnd(e.target.value)} /></label><blockquote>{Array.from(source.text).slice(Number(start), Number(end)).join("")}</blockquote></>}
            </>}
        </fieldset>}
        <Problem error={error} />
        <button type="submit" disabled={busy || !online || !reason.trim() || (!!sourceID && !source)}>{busy ? "Saving…" : candidate ? "Save correction revision" : "Submit correction"}</button>
    </form>;
}

export function CorrectionCard({ initial }: { initial: MemoryCandidateV1 }) {
    const { site, me, online } = useSession();
    const [c, setC] = useState(initial);
    const [reason, setReason] = useState("");
    const [error, setError] = useState<unknown>(null);
    const [busy, setBusy] = useState(false);
    const [edit, setEdit] = useState(false);
    const intents = useRef(new Intents()).current;
    const self = c.proposer_subject === me.subject;
    const mayReview = canSupervise(site.role) && !self;
    const reload = async () => setC(await request<MemoryCandidateV1>(`/v1/sites/${site.id}/memory-candidates/${c.candidate_id}`));
    async function decide(verdict: "approve" | "reject") {
        setBusy(true); setError(null);
        try { setC(await mutate<MemoryCandidateV1>(intents, `review-correction:${c.candidate_id}`, `/v1/sites/${site.id}/memory-candidates/${c.candidate_id}/review`, { verdict, reason: reason.trim(), expected_revision: c.revision }, c.revision)); }
        catch (e) { setError(e); if (isConflict(e)) await reload().catch(setError); }
        finally { setBusy(false); }
    }
    return <article className="claim stack" aria-label={`Correction ${short(c.candidate_id)}`}>
        <h3>Correction {short(c.candidate_id)} · revision {c.revision}</h3>
        <p>{c.content.reason}</p>
        {c.original.available ? <div><h4>Original output</h4>{c.original.question && <p>{c.original.question}</p>}{c.original.answer && <blockquote>{c.original.answer}</blockquote>}{c.original.outcome_status && <p>{c.original.outcome_status}: {c.original.outcome_reasons?.join(", ")}</p>}{c.original.investigation_rationale && <blockquote>{c.original.investigation_rationale}</blockquote>}</div> : <p>Original evidence is unavailable. Its text is withheld.</p>}
        {c.original.provenance.run_id && <p className="meta">Original run {short(c.original.provenance.run_id)}</p>}
        {c.content.claim && <div><h4>Proposed fact</h4><p>{predicates[c.content.claim.predicate] ?? c.content.claim.predicate}: {c.content.claim.literal ? literalText(c.content.claim.literal) : short(c.content.claim.object_entity_id ?? "")} · claim {short(c.content.claim.claim_id)} · base revision {c.content.expected_base_revision}</p>{c.content.claim.source_span_ids.map((id) => <Passage key={id} id={id} />)}</div>}
        <p>Regression question: {c.content.question || "Missing"}</p><p>Expected answer: {c.content.expected_answer || "Missing"}</p>
        {!c.review && c.review_state === "superseded" && <p role="status">Replaced by a newer correction from its author. It can be rejected but not approved.</p>}
        {c.missing_requirements.length > 0 && <p role="status">Still needed: {c.missing_requirements.map((x) => x.replaceAll("_", " ")).join(", ")}.</p>}
        {c.review ? <><p role="status">{c.review.verdict === "approve" ? c.published_snapshot_id ? "Approved — published" : "Approved — awaiting snapshot" : "Rejected"}. Review {short(c.review.review_id)} · <Time iso={c.review.created_at} /></p><p>{c.review.reason}</p>{c.review.verdict === "approve" && !c.published_snapshot_id && <Link to={`/sites/${site.id}/process`}>Publish approved knowledge</Link>}</> : <>
            {self && <><p>A different owner or supervisor must review your correction.</p><button type="button" aria-expanded={edit} onClick={() => setEdit(!edit)}>Amend correction</button>{edit && <CorrectionForm candidate={c} onSaved={(value) => { setC(value); setEdit(false); }} />}</>}
            {canSupervise(site.role) && <div className="review"><label>Correction review reason<input maxLength={2000} value={reason} onChange={(e) => setReason(e.target.value)} /></label><button type="button" disabled={!mayReview || busy || !online || !reason.trim() || c.missing_requirements.length > 0 || c.review_state === "superseded"} onClick={() => void decide("approve")}>Approve correction</button><button type="button" disabled={!mayReview || busy || !online || !reason.trim()} onClick={() => void decide("reject")}>Reject correction</button></div>}
        </>}
        <button type="button" onClick={() => void reload().catch(setError)} disabled={busy}>Refresh correction status</button><Problem error={error} />
    </article>;
}

export function CorrectionQueue() {
    const { site } = useSession();
    const [items, setItems] = useState<MemoryCandidateV1[] | null>(null);
    const [error, setError] = useState<unknown>(null);
    useEffect(() => {
        let live = true;
        allPages(async (cursor) => { const p = await request<MemoryCandidatePageV1>(`/v1/sites/${site.id}/memory-candidates?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`); return { items: p.candidates, next_cursor: p.next_cursor }; }).then((p) => live && setItems(p), (e: unknown) => live && setError(e));
        return () => { live = false; };
    }, [site.id]);
    return <section aria-label="Corrections to review"><h2>Corrections</h2><p>Corrections stay pending until a different human reviewer approves them. Publication is a separate step.</p><Problem error={error} />{items?.length === 0 && <p>No corrections submitted.</p>}{items?.map((c) => <CorrectionCard key={c.candidate_id} initial={c} />)}</section>;
}

export function CorrectionCount() {
    const { site } = useSession();
    const [count, setCount] = useState<CorrectionSummaryV1 | null>(null);
    const [error, setError] = useState<unknown>(null);
    usePolling(useCallback(() => { void request<CorrectionSummaryV1>(`/v1/sites/${site.id}/correction-summary`).then((c) => { setCount(c); setError(null); }, setError); }, [site.id]), 30000);
    return <div>{count && <p>{count.approved_count} corrections approved this month <span className="meta">({count.timezone}; includes approvals awaiting snapshot)</span></p>}<Problem error={error} context="The correction count could not be refreshed." /></div>;
}
