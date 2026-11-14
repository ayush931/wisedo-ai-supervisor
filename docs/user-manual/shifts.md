# Shifts: planning, running and handing over

This page is for plant supervisors and operators. It explains how a shift is planned, how it is
started and ended, what operators can record, and what the end-of-shift summary and handover are.

## Who does what

| Role | Can do |
|---|---|
| Supervisor | Plan a shift, approve another supervisor's plan, start and end a shift |
| Owner | Plan a shift, start and end a shift (an owner does not approve plans) |
| Operator | See whether the shift is running, and record work at the stations they are authorised for |
| Viewer | See plans, shift state, summaries and handovers |

Starting, ending and approving ask you to have signed in recently. If you signed in a while ago,
the screen asks you to sign in again first.

## 1. Planning a shift

A shift plan says which job order runs at which workstation, for one line and one shift
(for example 06:00 to 14:00).

1. **Get a suggestion (optional).** Choose the job orders you want to run and ask for a
   suggestion. The optimiser proposes which station takes which job and shows the expected
   timing, and lists any job it could not place with the reason. The suggestion is advice
   only; nothing is saved until you save the plan.
2. **Edit.** Change the assignments and quantities as you see fit, then save. A saved plan
   starts as a draft. Every later change is saved as a new version of the plan; earlier
   versions stay in the history.
3. **Approval by a different supervisor.** Another supervisor reviews the exact version you
   saved and approves it. Whoever created or changed any version of the plan cannot approve
   it. Only an approved plan can be started.

## 2. Starting the shift

A supervisor or owner presses **Start shift** on the approved plan. The system records the
time and who started it.

- You can start a shift **up to 30 minutes before** its planned start, so the team can
  prepare. Earlier than that, the system refuses and tells you it is too early.
- You **cannot start a shift after its planned end time.** Plan a new shift instead.
- A shift starts once. If someone else already started it, the page shows it as running.

## 3. Recording during the shift

Operators record at their station: acknowledging the job, starting, pausing, quantities, cycle
times, quality checks, material problems, notes and completion.

- **Recording opens only once the shift has been started.** Before that the station screen
  says the shift has not started, and entries are refused.
- While the shift is running, everything works as before, including corrections of earlier
  entries.

## 4. Re-planning while the shift is running

You can still change the plan during a running shift: save a new version and have another
supervisor approve it. The shift keeps running; new entries follow the newly approved version,
and the summary notes which entries were made under the older one.

After the shift has ended, a new version can still be saved as a draft, but it can no longer
be approved for that shift. Plan the next shift instead (the handover helps with that).

## 5. Ending the shift

Any supervisor or owner can press **End shift**, including the person who started it. You
are asked which shift comes next (line, start, end and time zone); the handover is drafted
for that shift.

Ending the shift does three things at once:

1. It records the end time and who ended it.
2. It produces the **final shift summary** as of that moment.
3. It drafts the **next-shift handover** from that summary.

If you end the shift **early**, the summary describes the shift as ended at the actual end
time, not the planned one, and it is not marked "in progress".

After the end, operators see that the shift has ended, and new entries for times after the end
are refused.

## 6. The summary and the handover

- **The summary** compares what was planned with what was recorded: quantities, cycle times,
  downtime, quality holds and material problems, with a readiness label (for example
  "limited" when data is missing or stale) and the reasons behind it.
- **The handover** lists, for each planned job, what is left to do, open blockers, holds and
  material problems, and proposes a plan for the next shift with the remaining quantities. It
  is a **draft**: it changes no plan by itself. The next team reviews it. If they adopt it,
  that creates a new draft plan for the next shift, which still needs approval by a supervisor
  who did not adopt it.

## 7. Late records

Sometimes an entry about something that happened during the shift is made after the shift has
ended (for example a note typed in a few minutes later).

- An entry for a time **before** the shift ended is accepted. It does **not** change the final
  summary. Instead the system adds a **revised summary** next to it, and the shift page shows
  that the summary was revised after the end. Nothing is overwritten.
- An entry for a time **after** the shift ended is refused.
- The handover drafted at the end was based on the original summary. Once a revised summary
  exists, that handover can no longer be adopted. **Generate the handover again** from the
  latest summary, then review and adopt that one.

## Upgrading

After this release, operators can record only on a shift that has been started. **Plans that
were approved before the upgrade are not started automatically.** If a shift is running when
you upgrade, a supervisor or owner needs to open the plan and press **Start shift** (this is
allowed in the middle of a shift, as long as its planned end has not passed) before operators
can record again.
