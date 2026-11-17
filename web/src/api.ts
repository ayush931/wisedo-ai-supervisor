// Shared API client. Pure module (no React, no CSS) so tests can import it directly.
// Tenant is never sent: it comes from the server session. Site comes from the URL path.
import type { MeV1, ProblemDetailsV1, ShiftPlanPageV1 } from "../../contracts/typescript/oee";

export type Me = MeV1;
export type Site = MeV1["sites"][number];

// actorLabel names a person: "you" only for the signed-in subject; otherwise the site
// directory's display name (WIS-290) plus a 4-hex subject tag, since a name is IdP profile data
// and may be user-editable. A missing name, or one that mimics a UI label ("you", "unknown",
// "member …", a " · 1a2b" tag), falls back to the subject prefix. Unknown stays visibly unknown.
const reservedName = /^(you|unknown)(\s|$)|^member\s/i;
// A separator dot (or a lookalike) next to whitespace is the shape of the subject tag itself;
// a name carrying one could pose as another member ("Approver · 9f3c" + " · 1a2b"). The server
// refuses such names at login; this covers names stored before that rule.
const tagLike = /\s[\u00B7\u2022\u2219\u22C5\u2027\u30FB\uFF65\u2E31\u16EB]|[\u00B7\u2022\u2219\u22C5\u2027\u30FB\uFF65\u2E31\u16EB]\s/;
export function actorLabel(a: { subject: string } | null | undefined, me: string | undefined, names: ReadonlyMap<string, string>) {
    if (!a) return "unknown";
    if (a.subject === me) return "you";
    const name = names.get(a.subject)?.trim();
    return name && !reservedName.test(name.normalize("NFKC")) && !tagLike.test(name.normalize("NFKC")) ? `${name} · ${a.subject.slice(0, 4)}` : `member ${a.subject.slice(0, 8)}`;
}

// duplicateNames returns the subjects whose display name (NFKC, trimmed, case-insensitive) is shared
// with another member of the directory.
export function duplicateNames(names: ReadonlyMap<string, string>): Set<string> {
    const bySubject = new Map<string, string[]>();
    for (const [subject, name] of names) {
        const k = name.normalize("NFKC").trim().toLowerCase();
        bySubject.set(k, [...(bySubject.get(k) ?? []), subject]);
    }
    return new Set([...bySubject.values()].filter((s) => s.length > 1).flat());
}

// loadDirectory reads every page of the site member directory into subject -> display name.
// It is all or nothing: a failed page, or more pages than the bound, returns an empty map so
// every actor shows its subject prefix, never a partial directory on which duplicate-name
// markers would silently be missing (rule 06: unknown stays unknown).
export const maxDirectoryPages = 50;
export async function loadDirectory(
    fetchPage: (cursor: string) => Promise<{ members: readonly { subject: string; display_name?: string | null }[]; next_cursor?: string | null }>,
): Promise<Map<string, string>> {
    const names = new Map<string, string>();
    try {
        let cursor = "";
        for (let i = 0; i < maxDirectoryPages; i++) {
            const page = await fetchPage(cursor);
            for (const m of page.members) if (m.display_name) names.set(m.subject, m.display_name);
            if (!page.next_cursor) return names;
            cursor = page.next_cursor;
        }
    } catch {
        // fall through: unknown
    }
    return new Map<string, string>();
}

export class ApiError extends Error {
    constructor(readonly problem: ProblemDetailsV1) {
        super(problem.code);
    }
}

// Thrown when the request may or may not have reached the server (connection lost).
export class AmbiguousError extends Error {
    constructor() {
        super("NETWORK_OUTCOME_UNKNOWN");
    }
}

export type Outcome =
    | "conflict" // 409/412: reload current revision, ask the user to review
    | "signed_out" // 401: sign out to reauthentication, never replay
    | "reauth" // 428 REAUTHENTICATION_REQUIRED: confirm identity, then resubmit by hand
    | "denied" // 403
    | "not_found" // 404 (non-disclosing)
    | "retry" // 429/503/5xx: retry status, no success
    | "invalid"; // 4xx contract errors: show code and path

export function classify(p: Pick<ProblemDetailsV1, "status" | "code">): Outcome {
    if (p.status === 401) return "signed_out";
    if (p.status === 428 && p.code === "REAUTHENTICATION_REQUIRED") return "reauth";
    if (p.status === 409 || p.status === 412) return "conflict";
    if (p.status === 403) return "denied";
    if (p.status === 404) return "not_found";
    if (p.status === 429 || p.status >= 500) return "retry";
    return "invalid";
}

export const outcomeText: Record<Outcome, string> = {
    conflict:
        "This changed on the server. The current revision has been reloaded; review it before submitting again.",
    signed_out: "Your session ended. Sign in again; nothing was resubmitted.",
    reauth: "Confirm your identity to continue. Nothing was submitted.",
    denied: "You do not have permission for this action at this site.",
    not_found: "Not found, or not available to you.",
    retry: "The service is busy or unavailable. Nothing was confirmed; try again shortly.",
    invalid: "The server rejected this request. Review the highlighted fields.",
};

export async function parseProblem(response: Response): Promise<ProblemDetailsV1> {
    const fallback: ProblemDetailsV1 = {
        type: "about:blank",
        title: `HTTP_${response.status}`,
        status: response.status,
        code: `HTTP_${response.status}`,
        correlation_id: response.headers.get("X-Correlation-ID") ?? "",
    };
    if (!(response.headers.get("Content-Type") ?? "").includes("application/problem+json")) return fallback;
    try {
        const body = (await response.json()) as Partial<ProblemDetailsV1>;
        return typeof body.code === "string" && typeof body.status === "number" ? { ...fallback, ...body } : fallback;
    } catch {
        return fallback;
    }
}

// Session-scoped form drafts: keep typed input through re-authentication (428). Never store credentials.
const DRAFT_PREFIX = "supervisor.draft.";

export function loadDraft<T>(key: string): T | null {
    try {
        const item = sessionStorage.getItem(`${DRAFT_PREFIX}${key}`);
        return item ? (JSON.parse(item) as T) : null;
    } catch {
        return null;
    }
}

export function saveDraft<T>(key: string, data: T): void {
    try {
        sessionStorage.setItem(`${DRAFT_PREFIX}${key}`, JSON.stringify(data));
    } catch {}
}

export function clearDraft(key: string): void {
    try {
        sessionStorage.removeItem(`${DRAFT_PREFIX}${key}`);
    } catch {}
}

export function clearAllDrafts(): void {
    try {
        const toRemove: string[] = [];
        for (let i = 0; i < sessionStorage.length; i++) {
            const k = sessionStorage.key(i);
            if (k?.startsWith(DRAFT_PREFIX)) toRemove.push(k);
        }
        for (const k of toRemove) sessionStorage.removeItem(k);
    } catch {}
}

let csrf = "";
let onSignedOut: () => void = () => {};
export function configureSession(token: string, signedOut: () => void) {
    csrf = token;
    onSignedOut = signedOut;
}

export type Mutation = { key?: string; ifMatch?: number; body?: unknown };

// request performs one HTTP call. It never retries by itself; retrying an intent is the
// caller's decision and reuses the same Idempotency-Key (see Intents).
export async function request<T>(path: string, method = "GET", m: Mutation = {}): Promise<T> {
    const headers: Record<string, string> = {};
    if (method !== "GET") {
        headers["X-CSRF-Token"] = csrf;
        headers["Content-Type"] = "application/json";
        if (m.key) headers["Idempotency-Key"] = m.key;
        if (m.ifMatch !== undefined) headers["If-Match"] = `"${m.ifMatch}"`;
    }
    let response: Response;
    try {
        response = await fetch(path, {
            method,
            headers,
            credentials: "same-origin",
            cache: "no-store",
            body: m.body === undefined ? undefined : JSON.stringify(m.body),
        });
    } catch {
        throw new AmbiguousError();
    }
    if (!response.ok) {
        const problem = await parseProblem(response);
        if (problem.status === 401) onSignedOut();
        throw new ApiError(problem);
    }
    return (response.status === 204 ? undefined : await readBody(response)) as T;
}

// readBody reads a successful response's JSON. A body lost or cut off after a 2xx means the server
// may have committed: the outcome is unknown (AmbiguousError), so a caller keeps its intent and a
// retry replays the same Idempotency-Key instead of writing twice (WIS-625 post-merge review).
export async function readBody<T>(response: Response): Promise<T> {
    try {
        return (await response.json()) as T;
    } catch {
        throw new AmbiguousError();
    }
}

// Intents keep one Idempotency-Key per user intent. The same intent with the same body
// reuses its key on retry; a changed body is a new intent and gets a new key.
export class Intents {
    private pending = new Map<string, { body: string; key: string }>();
    constructor(private mint: () => string = () => crypto.randomUUID()) {}
    key(intent: string, body: unknown): string {
        const text = JSON.stringify(body);
        const current = this.pending.get(intent);
        if (current && current.body === text) return current.key;
        const next = { body: text, key: this.mint() };
        this.pending.set(intent, next);
        return next.key;
    }
    has(intent: string) {
        return this.pending.has(intent);
    }
    done(intent: string) {
        this.pending.delete(intent);
    }
}

// Keep an idempotency key only while a write's outcome may be unknown. The key binds the body and
// If-Match: the same body against a newer revision (r2 after an ambiguous r1) is a new intent.
export async function mutate<T>(intents: Intents, intent: string, path: string, body: unknown, ifMatch?: number): Promise<T> {
    try {
        const out = await request<T>(path, "POST", { key: intents.key(intent, { body, ifMatch }), body, ifMatch });
        intents.done(intent);
        return out;
    } catch (e) {
        if (!(e instanceof AmbiguousError || (e instanceof ApiError && classify(e.problem) === "retry"))) intents.done(intent);
        throw e;
    }
}

// allPages follows next_cursor to the end; a repeated cursor or more than 50 pages rejects.
export async function allPages<T>(fetchPage: (cursor?: string) => Promise<{ items: readonly T[]; next_cursor?: string }>) {
    const items: T[] = [];
    const seen = new Set<string>();
    let cursor: string | undefined;
    for (let pageNumber = 0; pageNumber < 50; pageNumber++) {
        const page = await fetchPage(cursor);
        items.push(...page.items);
        if (!page.next_cursor) return items;
        if (seen.has(page.next_cursor)) throw new Error("List pagination returned a repeated cursor.");
        seen.add(page.next_cursor);
        cursor = page.next_cursor;
    }
    throw new Error("List pagination exceeded 50 pages.");
}

// loadShiftPlans reads every shift-plan page of a site (Today, the palette).
export function loadShiftPlans(site: string) {
    return allPages(async (cursor) => {
        const page = await request<ShiftPlanPageV1>(`/v1/sites/${site}/shift-plans?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
        return { items: page.plans, next_cursor: page.next_cursor };
    });
}

export const isConflict = (e: unknown) => e instanceof ApiError && classify(e.problem) === "conflict";

export function fieldOf(path?: string): { step?: number; field: string } | null {
    const step = /^steps\[(\d+)\]\.([a-z][a-z0-9_]*)$/.exec(path ?? "");
    if (step) return { step: Number(step[1]), field: step[2] };
    return ["display_name", "line_id", "item_id", "active", "steps"].includes(path ?? "") ? { field: path! } : null;
}

export function sameBody(a: unknown, b: unknown): boolean {
    const stable = (value: unknown): unknown => Array.isArray(value)
        ? value.map(stable)
        : value && typeof value === "object"
            ? Object.fromEntries(Object.keys(value).sort().map((key) => [key, stable((value as Record<string, unknown>)[key])]))
            : value;
    return JSON.stringify(stable(a)) === JSON.stringify(stable(b));
}

// roundDecimal rounds a canonical decimal string half-even to `digits` places for display only
// (rule 04): exact BigInt arithmetic, no floats. Anything that is not a plain decimal is returned
// unchanged so an unexpected value stays visible rather than being guessed.
// compareInstant orders RFC3339 instants exactly. Comparing the text is wrong when the
// fraction length varies ("…39.4Z" would sort after "…39.401Z"), and Date.parse alone
// drops digits past the millisecond, so those break the tie, padded to nanoseconds.
export function compareInstant(a: string, b: string): number {
    const sub = (s: string) => (s.match(/\.\d{3}(\d*)/)?.[1] ?? "").padEnd(6, "0");
    return Date.parse(a) - Date.parse(b) || sub(a).localeCompare(sub(b));
}

export function roundDecimal(value: string, digits = 1): string {
    const m = /^(-?)(\d+)(?:\.(\d+))?$/.exec(value);
    if (!m) return value;
    const [, sign, int, frac = ""] = m;
    if (frac.length <= digits) return value;
    const scaled = BigInt(int + frac.slice(0, digits));
    const rest = frac.slice(digits);
    const half = "5" + "0".repeat(rest.length - 1);
    const up = rest > half || (rest === half && scaled % 2n === 1n);
    const r = (scaled + (up ? 1n : 0n)).toString().padStart(digits + 1, "0");
    const out = digits ? `${r.slice(0, -digits)}.${r.slice(-digits)}` : r;
    return sign && /[1-9]/.test(out) ? `-${out}` : out;
}

// zonedISO turns a wall-clock "YYYY-MM-DDTHH:mm" in an IANA zone into RFC 3339 with the
// zone's offset at that instant (the backend checks the offset matches the zone).
export function zonedISO(local: string, timeZone: string): string {
    const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/.exec(local);
    if (!m) throw new RangeError("local time");
    const wall = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5]);
    const offsetAt = (t: number) => {
        const name = new Intl.DateTimeFormat("en-US", { timeZone, timeZoneName: "longOffset" })
            .formatToParts(new Date(t))
            .find((p) => p.type === "timeZoneName")!.value; // "GMT", "GMT+05:30"
        const o = /([+-])(\d{2}):(\d{2})/.exec(name);
        return o ? (o[1] === "-" ? -1 : 1) * (+o[2] * 60 + +o[3]) : 0;
    };
    // ponytail: two-pass offset guess; wall times inside a DST gap resolve to the later offset.
    let offset = offsetAt(wall);
    offset = offsetAt(wall - offset * 60000);
    const sign = offset < 0 ? "-" : "+";
    const abs = Math.abs(offset);
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${local}:00${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`;
}

// toLocalInput is the inverse of zonedISO: an instant as "YYYY-MM-DDTHH:mm" wall time in a zone.
export function toLocalInput(iso: string, timeZone: string): string {
    const parts = Object.fromEntries(
        new Intl.DateTimeFormat("en-GB", {
            timeZone,
            year: "numeric",
            month: "2-digit",
            day: "2-digit",
            hour: "2-digit",
            minute: "2-digit",
            hourCycle: "h23",
        })
            .formatToParts(new Date(iso))
            .map((p) => [p.type, p.value]),
    );
    return `${parts.year}-${parts.month}-${parts.day}T${parts.hour}:${parts.minute}`;
}

// zoneAbbr is a short zone name ("IST", "CET") or, when no locale has one, the GMT offset.
export function zoneAbbr(iso: string, timeZone?: string): string {
    let fallback = "";
    for (const locale of ["en-US", "en-IN", "en-GB", "en-AU"]) {
        const name =
            new Intl.DateTimeFormat(locale, { timeZone, timeZoneName: "short" })
                .formatToParts(new Date(iso))
                .find((p) => p.type === "timeZoneName")?.value ?? "";
        if (!/^(GMT|UTC)[+-]/.test(name)) return name;
        fallback ||= name;
    }
    return fallback;
}
const hm = (iso: string, timeZone?: string) =>
    new Intl.DateTimeFormat("en-GB", { timeZone, hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(new Date(iso));
const day = (iso: string, timeZone?: string) =>
    new Intl.DateTimeFormat("en-GB", { timeZone, weekday: "short", day: "numeric", month: "short" }).format(new Date(iso));

// formatInterval: "Tue 21 Oct · 06:00–14:00 IST" (end date added when it differs).
export function formatInterval(start: string, end: string, timeZone?: string): string {
    const endDay = day(end, timeZone) !== day(start, timeZone) ? `${day(end, timeZone)} ` : "";
    return `${day(start, timeZone)} · ${hm(start, timeZone)}–${endDay}${hm(end, timeZone)} ${zoneAbbr(start, timeZone)}`;
}

// formatTime: "10:59 IST" today, "Mon 20 Oct 10:59 IST" otherwise. now is injectable for tests.
export function formatTime(iso: string | null | undefined, timeZone?: string, now: Date = new Date()): string {
    if (!iso) return "unknown";
    const prefix = day(iso, timeZone) === day(now.toISOString(), timeZone) ? "" : `${day(iso, timeZone)} `;
    return `${prefix}${hm(iso, timeZone)} ${zoneAbbr(iso, timeZone)}`;
}

// formatIn renders an instant in a zone, keeping the zone visible.
export function formatIn(iso: string | null | undefined, timeZone?: string): string {
    if (!iso) return "unknown";
    // dateStyle/timeStyle cannot be combined with timeZoneName (TypeError), so spell the fields out.
    return new Intl.DateTimeFormat("en-GB", {
        timeZone,
        day: "numeric",
        month: "short",
        year: "numeric",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
        hourCycle: "h23",
        timeZoneName: "short",
    }).format(new Date(iso));
}

// Every route parameter is a resource id. Accepting only a canonical UUID (never decoded) keeps a
// crafted link such as /workstations/..%2F..%2Fv1%2F… from steering API calls made with the
// user's session and CSRF token to another path (client-side path traversal).
const routeID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// match returns the :params of pattern when path matches it exactly.
export function match(pattern: string, path: string): Record<string, string> | null {
    const a = pattern.split("/"),
        b = path.replace(/\/+$/, "").split("/");
    if (a.length !== b.length) return null;
    const out: Record<string, string> = {};
    for (let i = 0; i < a.length; i++) {
        if (a[i].startsWith(":")) {
            if (!routeID.test(b[i])) return null;
            out[a[i].slice(1)] = b[i];
        } else if (a[i] !== b[i]) return null;
    }
    return out;
}
