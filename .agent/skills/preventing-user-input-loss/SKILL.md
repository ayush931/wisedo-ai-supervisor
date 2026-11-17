---
name: preventing-user-input-loss
description: Guide and patterns for preventing loss of user input in the React web application across re-authentication, route changes, window unload, discard actions, and 409 revision conflicts.
---

# Preventing User Input Loss

Covers the implementation patterns required by WIS-730 to ensure typed input is never dropped prematurely.

## 1. Re-authentication Round-Trip (HTTP 428)

When privileged operations require step-up authentication, the server returns HTTP 428 `REAUTHENTICATION_REQUIRED`. The user must confirm their identity without losing what they typed.

### Pattern:
1. **Save Draft on Step-up:**
   - When redirecting to `/auth/reauthenticate`, serialize current form values to `sessionStorage` under a scoped key (e.g. `supervisor.draft.<siteId>.<formKey>`).
   - Never store secrets or credentials.
2. **Rehydrate on Return:**
   - In `useEffect` or initial state hydration, check for an existing draft key.
   - If present, initialize form state from the draft.
3. **Clean Up:**
   - Delete the draft from `sessionStorage` on successful save.
   - Clear all drafts when signing out (`logout()`).

---

## 2. Unsaved Changes & Navigation Guards

Forms with unsaved changes must warn the user before navigation or tab closure drops their work.

### Pattern:
1. **Window Unload (`beforeunload`):**
   ```ts
   useEffect(() => {
       if (!isDirty) return;
       const onBeforeUnload = (e: BeforeUnloadEvent) => {
           e.preventDefault();
           e.returnValue = "";
       };
       window.addEventListener("beforeunload", onBeforeUnload);
       return () => window.removeEventListener("beforeunload", onBeforeUnload);
   }, [isDirty]);
   ```
2. **In-App Navigation:**
   - Intercept client-side route changes (`NavigateContext` / `Link` / `popstate`).
   - If any active form is dirty, prompt confirmation (`window.confirm("You have unsaved changes. Leave without saving?")`).
   - If canceled, halt navigation and preserve the current view and state.

---

## 3. Discard Confirmation

A single accidental click on a "Discard" button must not erase work.

### Pattern:
- If `isDirty` is true, clicking "Discard" must prompt confirmation before reverting state:
  ```ts
  function handleDiscard() {
      if (isDirty && !window.confirm("Discard all unsaved changes?")) {
          return;
      }
      resetForm();
  }
  ```

---

## 4. Conflict Handling (HTTP 409 / 412)

Concurrent edits from another supervisor must not blow away local edits.

### Pattern:
1. **Retain Local State:** Catch 409 / 412 in `save()` without resetting the form or calling `cancel()`.
2. **Fetch Current Revision:** Load the newest object revision in the background.
3. **Present Resolution:**
   - Display a banner explaining that the object was updated on the server.
   - Allow the user to compare changes or update the target revision and re-apply their edits.

---

## 5. Resilience in Polling & Background Refresh

In shop-floor views (e.g. `workstation.tsx`) with background polling:
- Background refresh failures (transient 403, 404, or network timeout) must not nullify the active record context (`current`).
- Retain the mounted composer and its typed state across refresh failures, surfacing errors in non-destructive status banners.
