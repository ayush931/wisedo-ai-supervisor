import React, { createContext, useContext, useEffect, useState } from "react";
import { Badge, type BadgeProps } from "@astryxdesign/core/Badge";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { MetadataList, MetadataListItem } from "@astryxdesign/core/MetadataList";
import { Text } from "@astryxdesign/core/Text";
import { VisuallyHidden } from "@astryxdesign/core/VisuallyHidden";
import { AmbiguousError, ApiError, actorLabel, classify, duplicateNames, loadDirectory, roundDecimal, formatIn, formatTime, outcomeText, request, type Me, type Site } from "./api";
import { label, problemText } from "./labels";
import type { SiteMemberPageV1, StationProfilePageV1 } from "../../contracts/typescript/oee";

export type Session = {
    me: Me;
    site: Site;
    navigate: (to: string) => void;
    reauthenticate: () => void;
    online: boolean;
};
export const SessionContext = createContext<Session | null>(null);
export const useSession = () => useContext(SessionContext)!;
export const canSupervise = (role: string) => role === "owner" || role === "supervisor";

export function Link({
    to,
    children,
    className,
    current,
    onNavigate,
}: {
    to: string;
    children: React.ReactNode;
    className?: string;
    current?: boolean;
    onNavigate?: () => void;
}) {
    const navigate = useContext(NavigateContext);
    return (
        <a
            href={to}
            className={className}
            aria-current={current ? "page" : undefined}
            onClick={(e) => {
                onNavigate?.();
                if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
                e.preventDefault();
                navigate(to);
            }}
        >
            {children}
        </a>
    );
}
export const NavigateContext = createContext<(to: string) => void>(() => {});

// RouterAnchor is the LinkProvider component: every Astryx link (SideNavItem, BreadcrumbItem,
// Button with href) to an in-app path navigates client-side like Link. /auth/ and /v1/ paths,
// modified clicks and targeted links stay ordinary browser navigations.
export function RouterAnchor({ href, onClick, target, ...rest }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { ref?: React.Ref<HTMLAnchorElement> }) {
    const navigate = useContext(NavigateContext);
    return (
        <a
            href={href}
            target={target}
            {...rest}
            onClick={(e) => {
                onClick?.(e);
                if (e.defaultPrevented || target || e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
                if (!href?.startsWith("/") || href.startsWith("//") || /^\/(auth|v1)\//.test(href)) return;
                e.preventDefault();
                navigate(href);
            }}
        />
    );
}

export function useOnline() {
    const [online, setOnline] = useState(navigator.onLine);
    useEffect(() => {
        const update = () => setOnline(navigator.onLine);
        addEventListener("online", update);
        addEventListener("offline", update);
        return () => {
            removeEventListener("online", update);
            removeEventListener("offline", update);
        };
    }, []);
    return online;
}

// usePolling runs refresh now, every intervalMs while the page is visible and online, and
// again when the page becomes visible or the connection returns.
export function usePolling(refresh: () => void, intervalMs: number | null) {
    useEffect(() => {
        refresh();
        if (intervalMs === null) return;
        const tick = () => {
            if (!document.hidden && navigator.onLine) refresh();
        };
        const timer = setInterval(tick, intervalMs);
        document.addEventListener("visibilitychange", tick);
        addEventListener("online", tick);
        return () => {
            clearInterval(timer);
            document.removeEventListener("visibilitychange", tick);
            removeEventListener("online", tick);
        };
    }, [refresh, intervalMs]);
}

// Problem keeps role="alert" and title={code} on its own wrapper: Banner moves role="alert" to an
// inner node and has no title attribute, and specs select [role="alert"][title=CODE].
export function Problem({ error, context }: { error: unknown; context?: string }) {
    const session = useContext(SessionContext);
    if (!error) return null;
    if (error instanceof AmbiguousError)
        return (
            <div role="alert">
                <Banner
                    status="error"
                    title={<strong>Connection lost before the server answered.</strong>}
                    description={`${context ? `${context} ` : ""}The outcome is unknown; nothing is shown as saved until the server confirms it.`}
                />
            </div>
        );
    if (!(error instanceof ApiError))
        return (
            <div role="alert">
                <Banner status="error" title="Unexpected error. Nothing was confirmed." />
            </div>
        );
    const p = error.problem;
    const outcome = classify(p);
    const specific = Object.hasOwn(problemText, p.code) ? problemText[p.code] : undefined;
    const facts = (
        <MetadataList>
            <MetadataListItem label="Status">{p.status}</MetadataListItem>
            <MetadataListItem label="Code">
                <span className="identifier">{p.code}</span>
            </MetadataListItem>
            {p.path && (
                <MetadataListItem label="Field">
                    <span className="identifier">{p.path}</span>
                </MetadataListItem>
            )}
            {p.correlation_id && (
                <MetadataListItem label="Reference">
                    <span className="identifier">{p.correlation_id}</span>
                </MetadataListItem>
            )}
        </MetadataList>
    );
    // A known code gets its own plain sentence; the code itself moves into Details.
    return (
        <div role="alert" title={p.code}>
            <Banner
                status="error"
                collapsible={false}
                title={<strong>{specific ?? (p.code.endsWith("_UNCHANGED") ? "Nothing changed since the latest revision." : outcomeText[outcome])}</strong>}
                endContent={
                    outcome === "reauth" && session ? <Button label="Confirm identity" size="sm" onClick={session.reauthenticate} /> : undefined
                }
            >
                {specific ? (
                    <details className="ids">
                        <summary>Details</summary>
                        {facts}
                    </details>
                ) : (
                    facts
                )}
            </Banner>
        </div>
    );
}

// Status is icon + human label; the server code stays in the tooltip. Never colour alone.
// The label is passed as a fragment so Badge sets no title of its own; the wrapper's title is the code.
const toneVariant: Record<string, BadgeProps["variant"]> = { ok: "green", warn: "orange", bad: "red", muted: "neutral" };
export function Status({ code, prefix }: { code: string; prefix?: string }) {
    const l = label(code);
    return (
        <span title={code}>
            <Badge
                variant={toneVariant[l.tone]}
                label={
                    <>
                        <span aria-hidden="true">{l.icon}</span> {prefix}
                        {l.text}
                    </>
                }
            />
        </span>
    );
}

// Reasons shows up to three reason chips and "+N" for the rest (all codes in its tooltip).
export function Reasons({ codes }: { codes?: readonly string[] }) {
    if (!codes?.length) return null;
    const rest = codes.slice(3);
    return (
        <span className="chiplist">
            {codes.slice(0, 3).map((c) => (
                <span key={c} title={c}>
                    <Badge label={<>{label(c).text}</>} />
                </span>
            ))}
            {rest.length > 0 && (
                <span title={rest.map((c) => `${label(c).text} (${c})`).join(", ")}>
                    <Badge label={<>+{rest.length}</>} />
                </span>
            )}
        </span>
    );
}

export function Readiness({ value, reasons }: { value: string; reasons?: readonly string[] }) {
    return (
        <>
            <Status code={value} />
            <Reasons codes={reasons} />
        </>
    );
}

// Site member directory (WIS-290): fetched once per site; a subject it does not know triggers at
// most one refetch per site every 30 s, so a revoked member's rows cannot cause a fetch loop. A
// failed fetch, on any page, leaves every name unknown (subject prefix) and is retried on the
// same schedule: a partial directory would silently drop duplicate-name markers (rule 06).
const memberCache = new Map<string, { at: number; names: Promise<Map<string, string>> }>();
function loadMembers(site: string) {
    return loadDirectory((cursor) =>
        request<SiteMemberPageV1>(`/v1/sites/${site}/members?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`),
    );
}
function members(site: string, unknown?: string) {
    const cached = memberCache.get(site);
    if (cached && !(unknown && Date.now() - cached.at > 30000)) return cached.names;
    const names = (cached?.names ?? Promise.resolve(new Map<string, string>())).then((known) =>
        cached && unknown && known.has(unknown) ? known : loadMembers(site),
    );
    memberCache.set(site, { at: Date.now(), names });
    return names;
}

// Actor renders who did something: "you", "Name · 1a2b" (subject in the tooltip), or
// "member 1a2b3c4d". A display name shared by two site members carries a visible marker.
export function Actor({ a }: { a: { subject: string } | null | undefined }) {
    const { me, site } = useSession();
    const [names, setNames] = useState<ReadonlyMap<string, string>>(new Map());
    const dups = React.useMemo(() => duplicateNames(names), [names]);
    const subject = a?.subject;
    useEffect(() => {
        let live = true;
        members(site.id).then(async (known) => {
            if (subject && subject !== me.subject && !known.has(subject)) known = await members(site.id, subject);
            if (live) setNames(known);
        });
        return () => {
            live = false;
        };
    }, [site.id, subject, me.subject]);
    const text = actorLabel(a, me.subject, names);
    if (!a) return <>{text}</>;
    return (
        <span title={`Subject ${a.subject}`}>
            {text}
            {a.subject !== me.subject && dups.has(a.subject) && (
                <span className="mini" title="Another site member has the same display name; check the subject tag">
                    {" "}⚠ duplicate name
                </span>
            )}
        </span>
    );
}
// Val renders a value; only a genuinely absent value becomes an em dash (0 stays 0).
export function Val({ v }: { v: number | string | null | undefined }) {
    if (v === null || v === undefined)
        return (
            <span title="Not reported yet" aria-label="Not reported yet">
                —
            </span>
        );
    return <>{v}</>;
}
// Dec shows a decimal rounded for reading; the exact recorded value stays in the tooltip.
export function Dec({ v, unit, digits = 1 }: { v: string | null | undefined; unit?: string; digits?: number }) {
    if (v === null || v === undefined) return <Val v={null} />;
    return (
        <span title={`Exact: ${v}${unit ? ` ${unit}` : ""}`}>
            {roundDecimal(v, digits)}
            {unit ? ` ${unit}` : ""}
        </span>
    );
}
export function Time({ iso, tz }: { iso: string | null | undefined; tz?: string }) {
    if (!iso) return <Val v={null} />;
    return (
        <time dateTime={iso} title={formatIn(iso, tz)}>
            {formatTime(iso, tz)}
        </time>
    );
}

// Details holds full identifiers and revision counters out of the main view, copyable.
export function Details({ items }: { items: [string, string | number | null | undefined][] }) {
    return (
        <details className="ids">
            <summary>Details</summary>
            <MetadataList>
                {items.map(([k, v]) => (
                    <MetadataListItem key={k} label={k}>
                        <span className="identifier">{v ?? "—"}</span>
                        {typeof v === "string" && v.length > 20 && (
                            <Button label={`Copy ${k}`} variant="ghost" size="sm" onClick={() => navigator.clipboard?.writeText(v)}>
                                Copy
                            </Button>
                        )}
                    </MetadataListItem>
                ))}
            </MetadataList>
        </details>
    );
}

export function StationName({ id }: { id: string }) {
    const { list } = useStations();
    return <>{list?.find((s) => s.id === id)?.name ?? short(id)}</>;
}

// Refreshed shows the last load time in the same zone as the page's other times.
export function Refreshed({ at, tz, onRefresh }: { at: Date | null; tz?: string; onRefresh?: () => void }) {
    return (
        <Text as="p" size="sm" color="secondary" hasTabularNumbers aria-live="polite">
            Refreshed {at ? formatTime(at.toISOString(), tz, at) : "not yet"}
            {onRefresh && (
                <>
                    {" "}
                    <Button label="Refresh now" variant="ghost" size="sm" onClick={onRefresh} />
                </>
            )}
        </Text>
    );
}

export const short = (id: string) => id.slice(0, 8);

// Announce puts text into the live region around it, after that region is in the page. A region
// mounted with its text already inside may not be announced, so a screen keeps an empty
// role="status" element rendered at all times and Announce adds the text a moment after it
// changes (and clears it at once when it becomes empty). The text is visually hidden: the
// Skeleton beside it is the visual loading state.
export function Announce({ text }: { text: string }) {
    const [shown, setShown] = useState("");
    useEffect(() => {
        if (!text) return setShown("");
        const t = setTimeout(() => setShown(text), 100);
        return () => clearTimeout(t);
    }, [text]);
    return shown ? <VisuallyHidden>{shown}</VisuallyHidden> : null;
}

// lengthError checks a text field against a server limit. The server counts Unicode code points
// (Go utf8.RuneCountInString, as JSON Schema maxLength does), not UTF-16 units, so an emoji counts
// once. The text is never cut: truncating in onChange breaks IME composition and can split a
// surrogate pair. An over-limit value gets an error on the field, and the caller blocks submit.
export function lengthError(value: string, max: number): { type: "error"; message: string } | undefined {
    const n = [...value].length;
    return n > max ? { type: "error", message: `${n} characters; the limit is ${max}. Shorten it to submit.` } : undefined;
}

export type Station = { id: string; name: string; line_id: string; revision: number };
const stationCache = new Map<string, Promise<Station[]>>();
// loadStations follows next_cursor until absent and resolves only with every page. A failed page or
// a repeated cursor rejects, so a truncated list is never presented as complete (rule 06).
export async function loadStations(fetchPage: (cursor: string) => Promise<StationProfilePageV1>): Promise<Station[]> {
    const stations: Station[] = [];
    const seen = new Set<string>();
    let cursor = "";
    for (;;) {
        const page = await fetchPage(cursor);
        for (const w of page.workstations)
            stations.push({ id: w.workstation_id, name: w.display_name ?? w.workstation_id, line_id: w.line_id ?? "", revision: w.current_revision });
        if (!page.next_cursor) return stations;
        if (seen.has(page.next_cursor)) throw new Error("STATION_CURSOR_LOOP");
        seen.add(page.next_cursor);
        cursor = page.next_cursor;
    }
}
// useStations gets each site's current station names and line IDs from its list pages.
export function useStations() {
    const { site } = useSession();
    const [list, setList] = useState<Station[] | null>(null);
    const [error, setError] = useState<unknown>(null);
    useEffect(() => {
        let pending = stationCache.get(site.id);
        if (!pending) {
            pending = loadStations((cursor) =>
                request<StationProfilePageV1>(`/v1/sites/${site.id}/workstations?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`),
            );
            stationCache.set(site.id, pending);
            pending.catch(() => stationCache.delete(site.id));
        }
        pending.then(setList, setError);
    }, [site.id]);
    return { list, error };
}

// Display names. A line may live in master data or only in the knowledge graph (cases, some
// jobs); an order only in the knowledge graph. Each source is tried in turn; while loading, or
// when none can be read, a short ID stays visible instead (unknown is never blank or guessed).
type Named = { current_revision: number; revisions: readonly { revision: number; body: { display_name: string } }[] };
const masterName = (v: Named) => v.revisions.find((r) => r.revision === v.current_revision)?.body.display_name;
const entityName = (v: { display_name?: string }) => v.display_name;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const nameCache = new Map<string, Promise<string>>();
function lookup(sources: [string, (v: any) => string | undefined][]): Promise<string> {
    const key = sources.map(([p]) => p).join("|");
    let pending = nameCache.get(key);
    if (!pending) {
        pending = sources.reduce<Promise<string>>(
            (prior, [path, pick]) =>
                prior.catch((e) => {
                    // Only "not here" moves on to the next source; any other failure stays visible.
                    if (e instanceof ApiError && e.problem.status !== 404) throw e;
                    if (e instanceof AmbiguousError) throw e;
                    return request<unknown>(path).then((v) => {
                        const n = pick(v);
                        if (!n) throw new Error("NAME_UNAVAILABLE");
                        return n;
                    });
                }),
            Promise.reject(new Error("start")),
        );
        nameCache.set(key, pending);
        pending.catch(() => nameCache.delete(key));
    }
    return pending;
}
function useName(sources: [string, (v: any) => string | undefined][]) {
    const [name, setName] = useState<string | null>(null);
    const key = sources.map(([p]) => p).join("|");
    useEffect(() => {
        let live = true;
        setName(null);
        if (!sources.length) return;
        lookup(sources).then((n) => live && setName(n), () => live && setName(null));
        return () => {
            live = false;
        };
    }, [key]); // sources is derived from key
    return name;
}
// Name shows a line, order or item by its best readable display name. With prefixed, "Line" or
// "Order" is put in front unless the name already starts with it.
export function Name({ id, kind, prefixed }: { id: string; kind: "line" | "order"; prefixed?: boolean }) {
    const { site } = useSession();
    const base = `/v1/sites/${site.id}`;
    const word = kind === "line" ? "Line" : "Order";
    const sources: [string, (v: any) => string | undefined][] = !uuid.test(id)
        ? []
        : kind === "line"
          ? [[`${base}/lines/${id}`, masterName], [`${base}/entities/${id}`, entityName]]
          : [[`${base}/entities/${id}`, entityName]];
    const name = useName(sources);
    // A non-UUID order reference is the customer's own text; show it as it is.
    const text = name ?? (uuid.test(id) ? `${word} ${short(id)}` : id);
    const shown = prefixed && !new RegExp(`^${word}\\b`, "i").test(text) ? `${word} ${text}` : text;
    return <span title={`${word} ${id}`}>{shown}</span>;
}
export const LineName = ({ id }: { id: string }) => <Name id={id} kind="line" />;
export function ItemName({ id, fallback }: { id: string; fallback: string }) {
    const { site } = useSession();
    const name = useName([[`/v1/sites/${site.id}/items/${encodeURIComponent(id)}`, masterName]]);
    return <span title={`Item ${fallback}`}>{name ?? fallback}</span>;
}
