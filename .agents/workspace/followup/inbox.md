# Inbox

A drop box for new tasks/notes without colliding with edits to `tasks.md`
(which lives alongside this file at `.agents/workspace/followup/tasks.md`).

**Contract:**

- **The user** appends new items here, any time, either by editing this file
  directly or via `/followup add: <text>`.
- **The agent** only reads it, except for `/followup add:` (appends one item)
  and `/followup triage:` (moves items into `.agents/workspace/followup/tasks.md`
  and clears them here) — the only sanctioned writes to this file.

**Format:** one item per bullet, optionally timestamped. Prefix with `!` for
anything urgent enough to interrupt current work.

```
- [YYYY-MM-DD] <task or note>
- [YYYY-MM-DD] ! <urgent task or note>
```

## Items

_None._
