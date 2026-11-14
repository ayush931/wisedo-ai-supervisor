import { defineConfig } from "@playwright/test";
export default defineConfig({
    testDir: "./tests",
    timeout: 60000,
    use: {
        baseURL:
            process.env.SUPERVISOR_PUBLIC_ORIGIN || "http://127.0.0.1:58176",
        headless: true,
        launchOptions: {
            executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE,
        },
    },
    reporter: "list",
    // Every spec shares the one seeded site, and a configuration-import report's ETag binds that
    // whole site's master data (by design: apply refuses a stale check). With parallel workers a
    // line written by another spec turned an import apply into 412 REVISION_CONFLICT, so the
    // import spec failed nondeterministically. Specs run one at a time. (Playwright project
    // dependencies would isolate only the import spec, but they ignore --grep/--grep-invert,
    // which the two-phase @recent-auth run relies on.)
    workers: 1,
});
