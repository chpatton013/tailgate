---
name: followup
description: Manage this repository's own followup queue at .agents/workspace/followup/ — add a new item, triage the inbox into the task list, or work the next queued tasks. Only for this repo's own workspace queue, not a general task-tracking tool.
---

# Followup queue

Backs onto two files:

- `.agents/workspace/followup/inbox.md` — a drop box for new items.
- `.agents/workspace/followup/tasks.md` — the curated, agent-owned task list.

Invoked as `/followup <verb>: <text>`. Read the invocation text and dispatch:

- Starts with `add:` (case-insensitive, optional surrounding whitespace) →
  **Add** phase, with the remaining text as input.
- Starts with `triage:` → **Triage** phase, with the remaining text as input.
- Starts with `next:` → **Next** phase, with the remaining text as input.
- Empty (bare `/followup`) → run **Triage** with no argument, then **Next**
  with no argument, in sequence.
- Non-empty but doesn't start with one of the three verbs → infer which phase
  was meant from the text and the surrounding conversation (e.g. "here's
  something to track" implies Add; "clean up the inbox" implies Triage; "keep
  going on the tasks" implies Next). State which phase you inferred and why
  before proceeding, so the user can correct you if you inferred wrong.

`inbox.md` is otherwise read-only for the agent outside the Add and Triage
phases below — copy items out via Triage, append via Add, and leave it alone
otherwise. Direct manual edits by the user to `inbox.md` remain equally valid
at any time; Add is a convenience path in through chat, not the only path in.

## Add

Append one bullet to `.agents/workspace/followup/inbox.md` under `## Items`:

- Auto-stamp today's date as a leading `[YYYY-MM-DD]`.
- If the input text starts with `!`, preserve it as the urgent marker
  immediately after the date stamp (matching the file's own format guide).
- Otherwise append the text as-is after the date stamp.

Report the bullet you added, verbatim, as confirmation.

## Triage

Process the contents of `.agents/workspace/followup/inbox.md` and incorporate
them into `.agents/workspace/followup/tasks.md`. This is a triage-only phase:
move + id + clear. Do NOT start doing the tasks themselves.

If given input text, treat it as freeform steering for how to group or
prioritize this pass — not a filter that excludes items. Every item under
`## Items` still gets moved.

Steps:

1. **Read `.agents/workspace/followup/inbox.md`.** For each item under
   `## Items`:
   - Add it to the `## Active` section of `.agents/workspace/followup/tasks.md`,
     preserving its sub-bullets/detail. Group clearly-related sub-items under
     one task.
   - Prefix each new task with a unique **task id** in the form
     `[<date>-<rand>]`:
     - `<date>` — if the inbox item begins with a leading `[YYYY-MM-DD]` date,
       use that date; otherwise use **today's** date (`YYYY-MM-DD`).
     - `<rand>` — a short random token (4 lowercase base36 chars, e.g. `k7q2`),
       unique among task ids already present in
       `.agents/workspace/followup/tasks.md`.
   - Suffix each new task with a readiness summary in the form
     `(**Complexity:** <complexity>. **Readiness:** <readiness>)`:
     - `<complexity>` — `Low`, `Medium`, or `High`, with an optional blurb
       about why it is complex. Only include a blurb if it would not
       otherwise be obvious.
     - `<readiness>` — `Ready`, `Blocked`, or `Deferred`, with an optional
       blurb about why it is blocked or deferred. Always include a blurb for
       `Blocked` and `Deferred`; never for `Ready`.
   - Example:

     ```
     - [ ] [2026-07-13-k7q2] Build the calibrated-feedback template (**Complexity:**
       Medium. **Readiness:** Blocked - waiting for user for template sections)
     ```

2. **Clear the inbox.** After every item has been moved into
   `.agents/workspace/followup/tasks.md`, remove all bullets under `## Items`
   in `.agents/workspace/followup/inbox.md`, leaving the file's header and
   format guide intact and the `## Items` section empty with a placeholder
   like `_None — cleared <YYYY-MM-DD>._`.

3. **Report** a short summary: the task ids created and their one-line
   titles, and confirm the inbox was cleared.

## Next

Execute the tasks under `## Active` in `.agents/workspace/followup/tasks.md`,
top to bottom:

- If given input text, treat it as a filter — resolve it against task ids,
  keywords, or descriptions in `## Active`, and work only the matching
  task(s). If it matches zero tasks, or matches more than one clearly
  unrelated task, stop and ask which task was meant rather than guessing.
- Read `AGENTS.md` conventions and use the relevant **skill** for each task
  (`.agents/skills/...`). Prefer to delegate tasks to subagents so you can
  remain an orchestrator that the user can continue to interact with while
  the tasks are implemented. Verify subagent output before reporting its
  completion to the user.
- As each task completes: check it off / move it to a `## Completed` section
  in `.agents/workspace/followup/tasks.md`, and **commit** the work in
  logical steps with descriptive messages. Never stage
  `.agents/workspace/followup/inbox.md` content beyond a Triage-phase clear.
- Keep going until the in-scope tasks are done **or** a task needs the user's
  input or a decision you can't reasonably default — then **STOP and ask**
  rather than guess.
- Give a short progress note as you finish each task, and a summary at the
  end (what's done, what's left, anything awaiting the user).
