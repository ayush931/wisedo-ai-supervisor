import type { Locator, Page } from "@playwright/test";

// Astryx Button, NumberInput and the date/time inputs each render a visually hidden live region
// (role="status" or "alert") that belongs to the control and stays empty until it announces
// something. appRegions matches the application's own status or alert regions in a scope and
// excludes only those framework regions. It does not filter by text, so an absence check
// (toHaveCount(0)) still fails if the application renders an empty region there (WIS-707).
const framework = ":is(.astryx-button, .astryx-number-input, .astryx-date-input, .astryx-time-input, .astryx-date-time-input) [aria-live]";

export const appRegions = (scope: Page | Locator, role: "status" | "alert") => scope.getByRole(role).and(scope.locator(`:not(${framework})`));
