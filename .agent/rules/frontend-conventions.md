# Frontend Engineering Rules & Conventions

Applies to all web frontend work under `web/` (`web/src/`, `web/tests/`).

## 1. Scope Boundaries & Clean Code
- **Minimal, Focused Changes:** Edit only the code required to solve the target Linear ticket. Do not perform unrelated refactoring, formatting overhaul, or dependency additions.
- **Preserve Unrelated Work:** Never overwrite in-progress or unstaged changes from peer tickets (e.g., changes in `workstation.tsx`).
- **No Authority in UI:** The frontend is a view of server state; it is never the authorization authority. Permissions, revisions, and validation are strictly enforced by the Go API.

## 2. Directory & Component Structure
- `web/src/`: React 18 + TypeScript + Vite components and utilities.
  - `api.ts`: API client, HTTP methods, error taxonomy, `Intents`, and session state. Pure TypeScript (no React, no styling).
  - `ui.tsx`: Shared primitives, design tokens, contexts (`SessionContext`, `NavigateContext`), error banners (`Problem`), and standard form helpers.
  - Domain screens (`shifts.tsx`, `job-orders.tsx`, `workstation.tsx`, etc.): Feature-specific views.
- `web/tests/`: End-to-end integration tests using Playwright.

## 3. Forms, Dirty State & Focus Management
- **Explicit Dirty Tracking:** Any form or editor maintaining state must determine whether user input differs from the baseline (`isDirty`).
- **Never Truncate Input in `onChange`:**
  - Never use `.slice(0, max)` on input values. Slicing silently deletes user text, breaks IME composition, and splits Unicode surrogate pairs.
  - Always keep full typed input in state and validate using `lengthError(value, max)` from `web/src/ui.tsx`.
  - Block submit and display the character count / overage message.
- **Focus Etiquette:**
  - Never place `focus()` inside an inline ref callback (e.g., `ref={(el) => el?.focus()}`). This causes focus to be stolen on every keystroke/re-render.
  - Focus alerts or error summaries only once, specifically after form submit validation fails.

## 4. API Error & Conflict Handling
- **Problem Classification:** All API errors conform to RFC 7807 (`ProblemDetailsV1`) and are classified via `classify(problem)` in `api.ts`:
  - `401 Unauthorized` (`signed_out`): Session ended; clear local session, never silently replay mutations.
  - `428 Precondition Required` (`reauth` / `REAUTHENTICATION_REQUIRED`): Action requires step-up authentication. Preserve typed draft state before redirecting to the auth provider.
  - `409 Conflict` / `412 Precondition Failed` (`conflict`): Revision conflict. Retain typed user values, reload the newest server revision, display differences, and allow the user to reconcile or re-apply. Never wipe input with a forced "Cancel".
  - `429` / `5xx` (`retry`): Transient server issue; retain input and allow retry.
- **Mutations & Idempotency:** Mutations must pass `If-Match` revision headers and use `Intents` (`api.ts`) to provide a consistent `Idempotency-Key` across retries.

## 5. Security & Client Storage
- **Never Store Credentials:** Never place passwords, tokens, API keys, or IdP session cookies into `localStorage` or `sessionStorage`.
- **Session-Scoped Drafts Only:**
  - Only non-sensitive form input drafts may be saved into `sessionStorage` (e.g. to survive re-authentication redirects).
  - Draft keys must be prefixed and scoped by site and resource ID.
  - Drafts must be removed immediately upon successful submission or explicit user logout.
