import { JobOrdersPage } from "./job-orders";
import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
// @ts-expect-error CSS is handled by the frontend bundler.
import "./app.css";
import { Theme } from "@astryxdesign/core/theme";
import { neutralTheme } from "@astryxdesign/theme-neutral/built";
import { LinkProvider } from "@astryxdesign/core/Link";
import { AppShell } from "@astryxdesign/core/AppShell";
import { TopNav, TopNavHeading } from "@astryxdesign/core/TopNav";
import { SideNav, SideNavItem } from "@astryxdesign/core/SideNav";
import { Breadcrumbs, BreadcrumbItem } from "@astryxdesign/core/Breadcrumbs";
import { Badge } from "@astryxdesign/core/Badge";
import { Button } from "@astryxdesign/core/Button";
import { Banner } from "@astryxdesign/core/Banner";
import { Kbd } from "@astryxdesign/core/Kbd";
import { HStack } from "@astryxdesign/core/HStack";
import { Text } from "@astryxdesign/core/Text";
import { useMediaQuery } from "@astryxdesign/core/hooks";
import { CalendarClock, LayoutDashboard, MonitorCog, Settings2, ShieldCheck } from "lucide-react";
import { ApiError, configureSession, formatInterval, Intents, loadShiftPlans, match, request, type Me, type Site } from "./api";
import type { ShiftPlanPageV1 } from "../../contracts/typescript/oee";
import { Link, NavigateContext, Problem, RouterAnchor, SessionContext, StationName, canSupervise, useOnline, useSession, useStations, type Session } from "./ui";
import { ApprovalsPage, Today } from "./today";
import { MyStation } from "./station";
import { WorkstationList, WorkstationPage } from "./workstation";
import { ShiftList, ShiftNew, ShiftPage } from "./shifts";
import { MasterDataPage } from "./master-data";
const ProfileBuilder = React.lazy(() => import('./profile-builder').then(m => ({default:m.ProfileBuilder})));
const ProfileList = React.lazy(() => import('./profile-builder').then(m => ({default:m.ProfileList})));
const Plant = React.lazy(() => import('./plant'));
const SourceMappingPage = React.lazy(() => import('./source-mapping').then(m => ({default:m.SourceMappingPage})));
import { RoutingEditor, RoutingList } from "./routing-editor";
import { ValueStreamChecklist } from "./checklist";
const DocumentUpload = React.lazy(() => import('./documents').then(m => ({default:m.DocumentUpload})));
const ModelHosting = React.lazy(() => import('./model-hosting').then(m => ({default:m.ModelHosting})));
import { ProcessCommissioning } from "./commissioning";
import { ReviewQueue } from "./review-queue";
import { ProcessGraph } from "./process-graph";
import { InterviewList, InterviewPage } from "./interviews";
import { CaseList, CasePage } from "./cases";
import { TaskList, TaskPage } from "./tasks";

type Receipt = { id: string; site_id: string; status: "accepted" | "completed" | "rejected"; message: string };
const RETURN_KEY = "supervisor.return_to";

// Top-level tabs. Plan and Setup group the existing pages; every page keeps its URL.
const PLAN_PAGES: [string, string][] = [
    ["shifts", "Shifts"],
    ["job-orders", "Job orders"],
    ["tasks", "Tasks"],
];
const SETUP_PAGES: [string, string, string][] = [
    ["workstations", "Workstations", "Stations, their IDs and who may record"],
    ["profiles", "Profiles", "Station forms"],
    ["sources", "Sources", "Machine data feeds and their mappings"],
    ["master-data", "Master data", "Lines, items and calendars"],
    ["routings", "Routings", "The steps each product takes"],
    ["processes", "Processes", "Register areas, processes and their value-stream checklists (owners)"],
    ["interviews", "Interviews", "Ask your team how the line really runs"],
    ["documents", "Documents", "Upload SOPs, routing sheets and plans as evidence"],
    ["review", "Facts to review", "Approve or reject candidate facts against their passages"],
    ["onboarding", "Onboarding checklist", "How much of each value stream is understood"],
    ["process", "Approved process", "What reviewers approved, from suppliers to dispatch"],
    ["cases", "Cases", "Every shortfall case, open and closed"],
    ["model-hosting", "Model hosting", "Whether this site may use a hosted model (owners decide)"],
    ["connection", "Connection check", "Verify this site's connection"],
];
const tabOf = (section: string, role: string) =>
    PLAN_PAGES.some(([s]) => s === section)
        ? "plan"
        : section === "workstations" && role === "operator"
          ? "station"
          : SETUP_PAGES.some(([s]) => s === section) || section === "setup"
            ? "setup"
            : section;
// Landing by role: an operator starts at their station, everyone else at Today.
export const landing = (s: Site) => `/sites/${s.id}/${s.role === "operator" ? "station" : "today"}`;

function SubNav({ tab, pages, current }: { tab: string; pages: [string, string, ...string[]][]; current: string }) {
    const { site } = useSession();
    return (
        <HStack as="nav" aria-label={`${tab} pages`} gap={1} wrap="wrap" className="subnav">
            {pages.map(([s, label]) => (
                <Button
                    key={s}
                    label={label}
                    size="sm"
                    variant={s === current ? "secondary" : "ghost"}
                    href={`/sites/${site.id}/${s}`}
                    aria-current={s === current ? "page" : undefined}
                />
            ))}
        </HStack>
    );
}

function SetupIndex() {
    const { site } = useSession();
    return (
        <>
            <h1>Setup</h1>
            <p className="meta">How the plant is set up. You only need this when something on the floor changes.</p>
            <nav aria-label="Setup pages">
                <ul className="setup">
                    {SETUP_PAGES.map(([s, label, text]) => (
                        <li key={s}>
                            <Link to={`/sites/${site.id}/${s}`}>{label}</Link>
                            <span>{text}</span>
                        </li>
                    ))}
                </ul>
            </nav>
        </>
    );
}


function App() {
    const [path, setPath] = useState(location.pathname);
    const [me, setMe] = useState<Me | null>(null);
    const [loaded, setLoaded] = useState(false);
    const [sessionError, setSessionError] = useState<unknown>(null);
    const [signedOutNotice, setSignedOutNotice] = useState("");
    const [logoutError, setLogoutError] = useState<unknown>(null);
    const online = useOnline();
    // Same tier start as Astryx's xl width breakpoint: a 1180 px tablet gets the bottom bar.
    const wide = useMediaQuery("(min-width: 1280px)", true);
    // Quick jump and the role badge stay in the top bar down to tablet width, as before.
    const roomy = useMediaQuery("(min-width: 821px)", true);
    const navigate = useCallback((to: string) => {
        if (to !== location.pathname) history.pushState(null, "", to);
        setPath(to);
        window.scrollTo(0, 0);
    }, []);
    useEffect(() => {
        const pop = () => setPath(location.pathname);
        addEventListener("popstate", pop);
        return () => removeEventListener("popstate", pop);
    }, []);
    useEffect(() => {
        request<Me>("/v1/me")
            .then((value) => {
                configureSession(value.csrf_token, () => {
                    setMe(null);
                    setSignedOutNotice("Your session ended. Sign in again; nothing was resubmitted.");
                    navigate("/login");
                });
                setMe(value);
                let back: string | null = null;
                try {
                    back = sessionStorage.getItem(RETURN_KEY);
                    sessionStorage.removeItem(RETURN_KEY);
                } catch {}
                if (back && location.pathname === "/") navigate(back);
            })
            .catch((e) => {
                if (!(e instanceof ApiError && e.problem.status === 401)) setSessionError(e);
            })
            .finally(() => setLoaded(true));
    }, [navigate]);
    // The OIDC callback returns to "/"; send a signed-in user to their site or sites.
    useEffect(() => {
        if (!loaded) return;
        // One site: go straight to its role landing; several: choose on /sites.
        if (me && (path === "/" || path === "/login")) navigate(me.sites.length === 1 ? landing(me.sites[0]) : "/sites");
        if (!me && path !== "/login" && !sessionError) navigate("/login");
    }, [loaded, me, path, navigate, sessionError]);

    async function reauthenticate() {
        try {
            const result = await request<{ authorization_url: string }>("/auth/reauthenticate", "POST");
            try {
                sessionStorage.setItem(RETURN_KEY, location.pathname);
            } catch {}
            window.location.assign(result.authorization_url);
        } catch (e) {
            setSessionError(e);
        }
    }
    // WIS-713: the app shows a person as signed out only once the server confirmed it. A failed
    // sign out (CSRF 403, 503, lost connection) leaves the session valid, so it stays visible.
    async function logout() {
        setLogoutError(null);
        let result: { end_session_url?: string } | undefined;
        try {
            result = await request<{ end_session_url?: string } | undefined>("/auth/logout", "POST");
        } catch (e) {
            // 401: the server has no session any more; request() already shows the signed-out page.
            if (!(e instanceof ApiError && e.problem.status === 401)) setLogoutError(e);
            return;
        }
        // End the identity provider's session too (it asks the person to confirm). The page is left
        // as is until the browser navigates, so it never flashes a signed-out page first.
        if (result?.end_session_url) {
            window.location.assign(result.end_session_url);
            return;
        }
        setMe(null);
        navigate("/login");
    }

    let body: React.ReactNode;
    let tab = "";
    let params: Record<string, string> | null;
    const siteParams = match("/sites/:siteId", path.split("/").slice(0, 3).join("/"));
    if (!loaded) body = <p role="status">Checking your session…</p>;
    else if (!me)
        body = (
            <section className="landing" aria-labelledby="landing-title">
                <div className="landing-hero">
                    <p className="landing-eyebrow">Shift supervision workspace</p>
                    <h1 id="landing-title">Plan the shift. Check the evidence. Approve the next step.</h1>
                    <p className="landing-lede">
                        One workspace for supervisors and operators. Recovery proposals cite their evidence, and a
                        different person must approve one before a task is created.
                    </p>
                    {signedOutNotice && <p role="status">{signedOutNotice}</p>}
                    <Problem error={sessionError} />
                    <a className="button landing-cta" href="/auth/login">
                        Sign in
                    </a>
                </div>
                <ul className="landing-tiles">
                    <li>
                        <strong>Shifts</strong>
                        <span>Plans, suggestions and handovers</span>
                    </li>
                    <li>
                        <strong>Evidence</strong>
                        <span>Cited sources and approved snapshots</span>
                    </li>
                    <li>
                        <strong>Approvals</strong>
                        <span>A second person approves each proposal</span>
                    </li>
                </ul>
            </section>
        );
    else if (path === "/sites" || path === "/")
        body = (
            <section>
                <h1>Your sites</h1>
                {!me.sites.length ? (
                    <p>No site access has been provisioned. Contact your site administrator.</p>
                ) : (
                    <ul className="cards">
                        {me.sites.map((s) => (
                            <li key={s.id}>
                                <Link to={landing(s)}>{s.name}</Link>
                                <span className="meta"> · your role: {s.role}</span>
                            </li>
                        ))}
                    </ul>
                )}
            </section>
        );
    else if (siteParams) {
        const site = me.sites.find((s) => s.id === siteParams.siteId);
        if (!site)
            body = (
                <section>
                    <h1>Not found</h1>
                    <p>This site does not exist or is not available to you.</p>
                    <Link to="/sites">Back to your sites</Link>
                </section>
            );
        else {
            const session: Session = { me, site, navigate, reauthenticate, online };
            const base = `/sites/${site.id}`;
            const crumbs: { label: React.ReactNode; to?: string }[] = [{ label: site.name, to: landing(site) }];
            let page: React.ReactNode;
            let section = "";
            if (path === base && site.role === "operator") {
                section = "station";
                page = <MyStation />;
            } else if (match(`${base}/today`, path) || match(`${base}/overview`, path) || path === base) {
                section = "today";
                page = <Today />;
            } else if (match(`${base}/plant`, path)) {
                section = "plant";
                crumbs.push({ label: "Plant view" });
                page = <Plant key={site.id} />;
            } else if (match(`${base}/approvals`, path)) {
                section = "approvals";
                crumbs.push({ label: "Approvals" });
                page = <ApprovalsPage />;
            } else if (match(`${base}/setup`, path)) {
                section = "setup";
                crumbs.push({ label: "Setup" });
                page = <SetupIndex />;
            } else if (match(`${base}/station`, path)) {
                section = "station";
                crumbs.push({ label: "My station" });
                page = <MyStation />;
            } else if ((params = match(`${base}/workstations`, path))) {
                section = "workstations";
                crumbs.push({ label: "Workstations" });
                page = <WorkstationList />;
            } else if ((params = match(`${base}/workstations/:stationId`, path))) {
                section = "workstations";
                crumbs.push({ label: "Workstations", to: `${base}/workstations` }, { label: <StationName id={params.stationId} /> });
                page = <WorkstationPage key={params.stationId} stationId={params.stationId} />;
            } else if ((params = match(`${base}/profiles`, path))) {
                section = "profiles";
                crumbs.push({ label: "Profiles" });
                page = <ProfileList />;
            } else if ((params = match(`${base}/profiles/:stationId`, path))) {
                section = "profiles";
                crumbs.push({ label: "Profiles", to: `${base}/profiles` }, { label: <StationName id={params.stationId} /> });
                page = <ProfileBuilder key={params.stationId} stationId={params.stationId} />;
            } else if ((params = match(`${base}/sources`, path))) {
                section = "sources";
                crumbs.push({ label: "Source mappings" });
                page = <SourceMappingPage />;
            } else if ((params = match(`${base}/sources/:sourceId`, path))) {
                section = "sources";
                crumbs.push({ label: "Source mappings", to: `${base}/sources` }, { label: "Source" });
                page = <SourceMappingPage key={params.sourceId} sourceId={params.sourceId} />;
            } else if ((params = match(`${base}/master-data`, path))) {
                section = "master-data";
                crumbs.push({ label: "Master data" });
                page = <MasterDataPage />;
            } else if ((params = match(`${base}/routings`, path))) {
                section = "routings";
                crumbs.push({ label: "Routings" });
                page = <RoutingList />;
            } else if ((params = match(`${base}/routings/new`, path))) {
                section = "routings";
                crumbs.push({ label: "Routings", to: `${base}/routings` }, { label: "New routing" });
                page = <RoutingEditor />;
            } else if ((params = match(`${base}/routings/:routingId`, path))) {
                section = "routings";
                crumbs.push({ label: "Routings", to: `${base}/routings` }, { label: "Routing" });
                page = <RoutingEditor key={params.routingId} id={params.routingId} />;
            } else if ((params = match(`${base}/job-orders`, path))) {
 section = "job-orders"; crumbs.push({label:"Job orders"}); page=<JobOrdersPage key={site.id} />;
 } else if ((params = match(`${base}/shifts`, path))) {
                section = "shifts";
                crumbs.push({ label: "Shifts" });
                page = <ShiftList />;
            } else if ((params = match(`${base}/shifts/new`, path))) {
                section = "shifts";
                crumbs.push({ label: "Shifts", to: `${base}/shifts` }, { label: "New plan" });
                page = <ShiftNew />;
            } else if ((params = match(`${base}/shifts/:shiftId`, path))) {
                section = "shifts";
                crumbs.push({ label: "Shifts", to: `${base}/shifts` }, { label: "Shift" });
                page = <ShiftPage key={params.shiftId} shiftId={params.shiftId} />;
            } else if ((params = match(`${base}/process`, path))) {
                section = "process";
                crumbs.push({ label: "Approved process" });
                page = <ProcessGraph key={site.id} />;
            } else if ((params = match(`${base}/onboarding`, path))) {
                section = "onboarding";
                crumbs.push({ label: "Onboarding checklist" });
                page = <ValueStreamChecklist key={site.id} />;
            } else if ((params = match(`${base}/processes`, path))) {
                section = "processes";
                crumbs.push({ label: "Processes" });
                page = <ProcessCommissioning key={site.id} />;
            } else if ((params = match(`${base}/model-hosting`, path))) {
                section = "model-hosting";
                crumbs.push({ label: "Model hosting" });
                page = <ModelHosting key={site.id} />;
            } else if ((params = match(`${base}/documents`, path))) {
                section = "documents";
                crumbs.push({ label: "Documents" });
                page = <DocumentUpload key={site.id} />;
            } else if ((params = match(`${base}/review`, path))) {
                section = "review";
                crumbs.push({ label: "Facts to review" });
                page = <ReviewQueue key={site.id} />;
            } else if ((params = match(`${base}/interviews`, path))) {
                section = "interviews";
                crumbs.push({ label: "Interviews" });
                page = <InterviewList />;
            } else if ((params = match(`${base}/interviews/:sessionId`, path))) {
                section = "interviews";
                crumbs.push({ label: "Interviews", to: `${base}/interviews` }, { label: "Interview" });
                page = <InterviewPage key={params.sessionId} sessionId={params.sessionId} />;
            } else if ((params = match(`${base}/tasks`, path))) {
                section = "tasks";
                crumbs.push({ label: "Tasks" });
                page = <TaskList key={site.id} />;
            } else if ((params = match(`${base}/tasks/:taskId`, path))) {
                section = "tasks";
                crumbs.push({ label: "Tasks", to: `${base}/tasks` }, { label: "Task" });
                page = <TaskPage key={params.taskId} taskId={params.taskId} />;
            } else if ((params = match(`${base}/cases`, path))) {
                section = "cases";
                crumbs.push({ label: "Cases" });
                page = <CaseList key={site.id} />;
            } else if ((params = match(`${base}/cases/:caseId`, path))) {
                section = "cases";
                crumbs.push({ label: "Cases", to: `${base}/cases` }, { label: "Case" });
                page = <CasePage key={params.caseId} caseId={params.caseId} />;
            } else if ((params = match(`${base}/connection`, path))) {
                section = "connection";
                crumbs.push({ label: "Connection check" });
                page = <ConnectionCheck />;
            } else page = <p>Page not found.</p>;
            tab = tabOf(section, site.role);
            const sub =
                tab === "plan" ? (
                    <SubNav tab="Plan" pages={PLAN_PAGES} current={section} />
                ) : tab === "setup" && section !== "setup" ? (
                    <SubNav tab="Setup" pages={SETUP_PAGES} current={section} />
                ) : null;
            body = (
                <SessionContext.Provider value={session}>
                    <div className="page">
                        <Breadcrumbs variant="supporting">
                            <BreadcrumbItem href="/sites" isCurrent={false}>
                                Sites
                            </BreadcrumbItem>
                            {crumbs.map((c, i) => (
                                <BreadcrumbItem key={i} href={c.to} isCurrent={!c.to}>
                                    {c.label}
                                </BreadcrumbItem>
                            ))}
                        </Breadcrumbs>
                        {sub}
                        <React.Suspense fallback={<p role="status">Loading page…</p>}>{page}</React.Suspense>
                    </div>
                    <Palette />
                </SessionContext.Provider>
            );
        }
    } else body = <p>Page not found.</p>;

    const currentSite = me && siteParams ? me.sites.find((s) => s.id === siteParams.siteId) : undefined;
    const siteNav = me && currentSite ? <SiteTabs site={currentSite} current={tab} rail={wide} /> : undefined;
    return (
        <Theme theme={neutralTheme}>
            <NavigateContext.Provider value={navigate}>
                <LinkProvider component={RouterAnchor}>
                    <AppShell
                        height="auto"
                        variant="section"
                        contentPadding={4}
                        mobileNav={false}
                        banner={
                            (!online || (me && logoutError !== null)) && (
                            <>
                                {!online && (
                                    <div role="status">
                                        <Banner status="warning" container="section" title="Offline. Changes are disabled until the connection returns." />
                                    </div>
                                )}
                                {me && logoutError !== null && (
                                    <div role="alert">
                                        <Banner
                                            status="error"
                                            container="section"
                                            title="Sign out failed. You are still signed in."
                                            description="Try Sign out again before you leave this computer."
                                        />
                                    </div>
                                )}
                            </>
                            )
                        }
                        topNav={
                            <TopNav
                                label="Account"
                                heading={<TopNavHeading heading="wiseDo" subheading="AI Supervisor" headingHref="/sites" />}
                                endContent={
                                    me && (
                                        <HStack gap={2} align="center">
                                            {roomy && (
                                                <Button
                                                    label="Jump to…"
                                                    variant="ghost"
                                                    size="sm"
                                                    onClick={() => dispatchEvent(new Event("open-palette"))}
                                                >
                                                    Jump to… <Kbd keys="mod+k" />
                                                </Button>
                                            )}
                                            <span title={`Signed in as ${me.subject}`}>
                                                <Text weight="semibold" maxLines={1}>
                                                    {me.display_name ?? `member ${me.subject.slice(0, 8)}`}
                                                </Text>
                                            </span>
                                            {currentSite && roomy && (
                                                <span title="Your role at this site">
                                                    <Badge label={<>{currentSite.role}</>} />
                                                </span>
                                            )}
                                            <Button label="Sign out" variant="ghost" size="sm" onClick={logout} />
                                        </HStack>
                                    )
                                }
                            />
                        }
                        sideNav={wide ? siteNav : undefined}
                    >
                        {body}
                        {!wide && siteNav}
                    </AppShell>
                </LinkProvider>
            </NavigateContext.Provider>
        </Theme>
    );
}

// ConnectionCheck is the foundation probe: authenticated, durable processing, no operations.
function ConnectionCheck() {
    const { site, online, reauthenticate } = useSession();
    const [message, setMessage] = useState("Foundation connectivity check");
    const [receipt, setReceipt] = useState<Receipt | null>(null);
    const [error, setError] = useState<unknown>(null);
    const [busy, setBusy] = useState(false);
    const intents = useMemo(() => new Intents(), []);
    const active = useRef(true);
    useEffect(() => () => void (active.current = false), []);
    useEffect(() => {
        if (!receipt || receipt.status !== "accepted") return;
        let attempts = 0;
        const timer = setInterval(async () => {
            if (document.hidden || !navigator.onLine) return;
            if (++attempts > 30) {
                clearInterval(timer);
                return;
            }
            try {
                const result = await request<Receipt>(`/v1/sites/${receipt.site_id}/foundation/probes/${receipt.id}`);
                if (active.current) setReceipt(result);
            } catch (e) {
                if (active.current) setError(e);
                clearInterval(timer);
            }
        }, 2000);
        return () => clearInterval(timer);
    }, [receipt?.id, receipt?.status]);
    async function submit(event: React.FormEvent) {
        event.preventDefault();
        setBusy(true);
        setError(null);
        const body = { message };
        try {
            setReceipt(
                await request<Receipt>(`/v1/sites/${site.id}/foundation/probes`, "POST", {
                    key: intents.key("probe", body),
                    body,
                }),
            );
        } catch (e) {
            setError(e);
        } finally {
            setBusy(false);
        }
    }
    return (
        <>
            <div className="titlebar">
                <h1>Connection check</h1>
                <button type="button" onClick={reauthenticate}>
                    Confirm identity
                </button>
            </div>
            <p>
                Verify this site's authenticated connection and durable processing. This check creates no operational
                task and controls no equipment.
            </p>
            <section>
                <form onSubmit={submit}>
                    <label htmlFor="message">Check description</label>
                    <input
                        id="message"
                        value={message}
                        maxLength={500}
                        required
                        onChange={(e) => setMessage(e.target.value)}
                        disabled={busy}
                    />
                    <button className="primary" disabled={busy || !online || !canSupervise(site.role)}>
                        {busy ? "Submitting…" : "Run connection check"}
                    </button>
                </form>
            </section>
            {receipt && (
                <section role="status">
                    <h2>
                        {receipt.status === "completed"
                            ? "Processing confirmed"
                            : receipt.status === "rejected"
                              ? "Processing denied"
                              : "Durably accepted · awaiting worker"}
                    </h2>
                    <p>{receipt.message}</p>
                    <p className="identifier">Receipt: {receipt.id}</p>
                    <p>Persisted status: {receipt.status}</p>
                </section>
            )}
            <Problem error={error} />
        </>
    );
}

// SiteTabs: top-level tabs. An operator lands on My station and can also open Today for approved process chat.
// A left rail at desktop width; below it (tablet and phone) the same nav is a bottom bar.
function SiteTabs({ site, current, rail }: { site: Site; current: string; rail: boolean }) {
    const base = `/sites/${site.id}`;
    const tabs: [string, string, string, React.ComponentType][] =
        site.role === "operator"
            ? [
                  ["station", "My station", `${base}/station`, MonitorCog],
                  ["today", "Today", `${base}/today`, LayoutDashboard],
                  ["plan", "Plan", `${base}/shifts`, CalendarClock],
                  ["setup", "Setup", `${base}/setup`, Settings2],
              ]
            : [
                  ["today", "Today", `${base}/today`, LayoutDashboard],
                  ["plant", "Plant view", `${base}/plant`, MonitorCog],
                  ["plan", "Plan", `${base}/shifts`, CalendarClock],
                  ["approvals", "Approvals", `${base}/approvals`, ShieldCheck],
                  ["setup", "Setup", `${base}/setup`, Settings2],
              ];
    if (rail)
        return (
            <SideNav aria-label="Site sections">
                {tabs.map(([key, label, to, icon]) => (
                    <SideNavItem key={key} label={label} href={to} icon={icon} isSelected={current === key} />
                ))}
            </SideNav>
        );
    return (
        <nav aria-label="Site sections" className="bottom-bar">
            {tabs.map(([key, label, to, Icon]) => (
                <Button
                    key={key}
                    label={label}
                    href={to}
                    variant={current === key ? "secondary" : "ghost"}
                    icon={<Icon aria-hidden="true" />}
                    aria-current={current === key ? "page" : undefined}
                />
            ))}
        </nav>
    );
}

// Palette: Ctrl/Cmd+K quick jump to any workstation or shift plan of this site.
function Palette() {
    const { site, navigate } = useSession();
    const dialog = useRef<HTMLDialogElement>(null);
    const [query, setQuery] = useState("");
    const [active, setActive] = useState(0);
    const [plans, setPlans] = useState<ShiftPlanPageV1["plans"]>([]);
    const [plansError, setPlansError] = useState<unknown>(null);
    const [plansLoading, setPlansLoading] = useState(false);
    const stations = useStations();
    useEffect(() => {
        let active = true;
        let load = 0;
        const open = () => {
            const currentLoad = ++load;
            setQuery("");
            setActive(0);
            setPlans([]);
            setPlansError(null);
            setPlansLoading(true);
            loadShiftPlans(site.id).then(
                (page) => {
                    if (active && currentLoad === load) setPlans(page);
                },
                (reason) => {
                    if (active && currentLoad === load) setPlansError(reason);
                },
            ).finally(() => {
                if (active && currentLoad === load) setPlansLoading(false);
            });
            if (!dialog.current?.open) dialog.current?.showModal();
        };
        const key = (e: KeyboardEvent) => {
            if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
                e.preventDefault();
                open();
            }
        };
        addEventListener("keydown", key);
        addEventListener("open-palette", open);
        return () => {
            active = false;
            removeEventListener("keydown", key);
            removeEventListener("open-palette", open);
        };
    }, [site.id]);
    const items = [
        ...(stations.list ?? []).map((w) => ({
            label: `Workstation ${w.name}`,
            to: `/sites/${site.id}/workstations/${w.id}`,
        })),
        ...plans.map((p) => ({
            label: `Shift · ${formatInterval(p.starts_at, p.ends_at)}`,
            to: `/sites/${site.id}/shifts/${p.id}`,
        })),
    ].filter((i) => i.label.toLowerCase().includes(query.toLowerCase()));
    const visibleItems = items.slice(0, 30);
    const go = (to: string) => {
        dialog.current?.close();
        navigate(to);
    };
    return (
        <dialog ref={dialog} className="palette" aria-label="Jump to">
            <input
                aria-label="Search workstations and shifts"
                role="combobox"
                aria-expanded="true"
                aria-controls="palette-list"
                aria-activedescendant={items[active] ? `palette-${active}` : undefined}
                autoFocus
                value={query}
                placeholder="Search workstations and shifts"
                onChange={(e) => {
                    setQuery(e.target.value);
                    setActive(0);
                }}
                onKeyDown={(e) => {
                    if (e.key === "ArrowDown") setActive((a) => Math.min(a + 1, items.length - 1));
                    else if (e.key === "ArrowUp") setActive((a) => Math.max(a - 1, 0));
                    else if (e.key === "Enter" && items[active]) go(items[active].to);
                    else return;
                    e.preventDefault();
                }}
            />
            <Problem error={plansError} />
            <ul id="palette-list" role="listbox">
                {visibleItems.map((i, n) => (
                    <li
                        key={i.to}
                        id={`palette-${n}`}
                        role="option"
                        aria-selected={n === active}
                        onClick={() => go(i.to)}
                    >
                        {i.label}
                    </li>
                ))}
                {items.length > visibleItems.length && <li className="meta">Showing {visibleItems.length} of {items.length} matches; refine your search.</li>}
                {plansLoading && <li className="meta" role="status">Loading shift plans…</li>}
                {!items.length && !plansLoading && !plansError && <li className="meta">No matches</li>}
            </ul>
        </dialog>
    );
}

createRoot(document.getElementById("root")!).render(<App />);
