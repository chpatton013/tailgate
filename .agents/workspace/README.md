# Workspace

This directory serves as the Agent's workspace. Anything that doesn't belong as
part of the project featured in this repository, but is worth persisting for the
sake of agentic development, should live here.

## What belongs here

- `followup/tasks.md` — the agent-owned development task list
- `followup/inbox.md` — drop box for new tasks/notes, moved into `tasks.md` by
  the `followup` skill (`/followup add:`/`triage:`/`next:`). See both files'
  headers for the write-ownership split.
- `MEMORY.md` — durable facts about development in this repository worth
  remembering (conventions, decisions, etc). Keep it short; it's a
  quick-reference, not an archive.
- `plans/` — plan documents for larger development tasks
- Scratch drafts, intermediate analyses, and experimental results that inform
  some work but aren't the work itself.

## Persisting workspace content

Note that the git log can be made into a useful tool of past challenges and
decision making if we effectively capture intermediates and their associated
ideas in commits.

Prefer committing meaningful intermediates with a message explaining the idea
behind them, so the git log stays a useful record of our reasoning across time.
