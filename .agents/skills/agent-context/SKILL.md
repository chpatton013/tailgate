---
name: agent-context
description: Load before creating, editing, or relocating any agent-facing instruction, rule, prompt, skill, or config file in this repo — decides whether it belongs in the shared .agents/ layout or in a harness-native directory, and how to add support for a new harness safely. Not for general repo work.
---

# Agent context: where it lives and why

This repo (and any project instantiated from it via the `bootstrap` skill) is
worked on through multiple AI coding harnesses — Claude Code, Cursor,
OpenCode, Pi, possibly others later. This skill is the policy for keeping
their configuration from diverging into duplicated, drifting copies.

## The core rule

**Agent context** — instructions, house rules, workflow docs, skills,
prompts: anything meaningful to state once and share across every harness —
goes in `AGENTS.md` and `.agents/skills/`. Examples: a project's coding
conventions, a step-by-step procedure like this one, a standing rule about
commit message style, a reusable prompt template.

**Agent tooling config** — settings with no cross-harness equivalent by
construction, because they configure one specific tool's runtime rather than
stating anything an agent should know — stays in its native harness
directory. Examples: Claude Code's permission grants and hooks
(`.claude/settings.json`), an MCP server registration, editor keybindings, a
harness-specific `.ignore` file, telemetry opt-out flags. Don't force these
into `.agents/` — there's nothing to share, and doing so would just break the
tool that reads them from its expected location.

If you're unsure which bucket something falls into, ask: would a *different*
harness ever want to read this same content? If yes, it's context and belongs
in the shared layout. If the question doesn't even make sense — because the
setting only means something to one specific tool — it's tooling config and
stays put.

## Where things go, concretely

- `AGENTS.md` at repo root — always-loaded standing rules, kept short. This
  is the primer, not the full policy for any given topic.
- `.agents/skills/<name>/SKILL.md` — on-demand procedures and reference
  material, loaded when relevant rather than always.
- `.agents/workspace/` — working state (task queue, memory, plans). This is
  not "context" in this policy's sense (it's data the agent produces and
  consumes, not instructions), so it isn't covered further here.

## Harness-native entry-point files stay shims

Some harnesses only discover context files under a fixed, harness-specific
filename. Claude Code, for instance, reads `CLAUDE.md`. When a harness
requires this, that file must contain *only* a minimal import/redirect —
this repo's `CLAUDE.md` is the single line `@AGENTS.md` — and never original
content. If you're wiring up a new harness that needs its own entry-point
filename, create a shim of the same shape. Never duplicate `AGENTS.md`'s
content into it.

## Before wiring in a new harness — verify, don't assume by analogy

Harnesses that look similar on the surface often aren't. Real examples found
the hard way in this repo's history:

- OpenCode's subagent files have no `tools:` field at all — it uses a
  `permission:` object instead, so a Claude-style `tools:` field silently
  does nothing there.
- Cursor's command files have no YAML frontmatter parsing at all — a
  `description:` block just renders as literal prompt text to the model.
- Pi has no `commands/` directory — it's an open, unimplemented feature
  request, not a documented-but-untried path.
- OpenCode reads skills from `.claude/skills/` directly, rather than having
  its own `.opencode/skills/`.

None of these were discoverable by assuming one harness's docs generalized to
a sibling harness. Before adding a new harness, or a new shared-context
mechanism (context files, skills, agents, commands) to an existing one,
follow this procedure:

1. **Find that harness's current official documentation** for the specific
   mechanism in question. Don't rely on memory or on what a similar-sounding
   harness does — the ecosystem moves fast and details change.
2. **Confirm the exact directory path it reads**, including pluralization
   and nesting (e.g. `.opencode/agents/` vs `.opencode/agent/`). Don't assume
   it matches a sibling harness's convention.
3. **Confirm whether the harness has its own frontmatter/field schema** that
   would silently ignore or misinterpret a field meant for a different
   harness (see the OpenCode `tools:`/`permission:` example above).
4. **Only after confirming 1-3**, add the harness's directory as a symlink
   into the shared `.agents/` layout, following the existing
   `.claude`/`.cursor`/`.pi` pattern. If the harness's format turns out to be
   genuinely incompatible with sharing (a schema that can't coexist with the
   others'), keep it as a separate, non-shared file instead of forcing a
   broken symlink — a working divergent file beats a symlink that silently
   misbehaves.
