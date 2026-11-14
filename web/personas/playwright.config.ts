import { defineConfig } from "@playwright/test";
const baseURL = process.env.SUPERVISOR_PUBLIC_ORIGIN;
if (baseURL !== "http://127.0.0.1:62376") throw new Error("Facility personas require their isolated API at 127.0.0.1:62376");
const root = process.env.FACILITY_ARTIFACTS;
if (!root?.startsWith("/var/tmp/") || root.split("/").includes("..")) throw new Error("BLOCKED: FACILITY_ARTIFACTS must be set to a path under /var/tmp (scripts/supervisor/facility.sh sets it)");
export default defineConfig({
 testDir: ".", testMatch: "*.spec.ts", timeout: 240_000, expect: { timeout: 15_000 }, workers: 1,
 outputDir: `${root}/browser`, reporter: [["list"], ["json", { outputFile: `${root}/playwright-report.json` }]],
 use: { baseURL, headless: true, channel: "chromium", screenshot: "only-on-failure", trace: "off", video: "off" },
});
