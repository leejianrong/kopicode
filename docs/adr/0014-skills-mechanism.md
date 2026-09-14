# ADR-0014: A skills mechanism — a documented directory, no new tool

- **Status:** Proposed
- **Date:** 2026-08-29
- **Deciders:** Jian (leejianrong2@gmail.com)

Drafted by an agent during the same 2026-08-29 planning session as
[ADR-0013](0013-agent-controlled-resident-session-surface.md), after Jian reopened a
question [KAN-1021](../SLICE-1.md)'s brainstorm had deferred: "I do want a
skills/plugin mechanism." Like ADR-0008, ADR-0009 and ADR-0013, this is a
recommendation for Jian to accept, amend or reject, and Status stays **Proposed**
until he does.

## Context

**KAN-1021 already picked a shape, conditionally, and named the trigger for revisiting
it.** Its brainstorm comment considered three shapes for a skills mechanism, safest
first: (a) zero new tools — files under a documented, git-tracked directory, with one
sentence in the system prompt telling the model to look there; (b) a model-driven
`read_skill`/`list_skills` tool pair, mirroring `read_file`; (c) an auto-injected
catalogue at session start, ruled out as more machinery than either buys. Its
recommendation: "start with (a), escalate to (b) only once a project actually
accumulates enough skill fragments that 'the model has to know skills exist without
reading a directory first' is an observed problem, not a hypothetical one." Nothing in
this ADR changes that recommendation — it exists to settle what shape (a) actually
is, which KAN-1021 deliberately left unspecified pending a real go-ahead.

**"Not carding this now: AGENTS.md loading plausibly delivers most of the value for
free" turned out to be half right, and it's worth saying which half.** AGENTS.md
loading has since shipped (epic 130, KAN-1024/1025) — but reading its actual
implementation shows it solves a *different* problem than a skills library does, not
the same one by another name. `internal/engine/instructions.go`'s
`loadProjectInstructions` reads a repository's single AGENTS.md, in full, and
unconditionally injects it as a wrapped message before the first real turn — right for
a small, always-relevant project brief, wrong for a library of many task-specific
skill files where injecting all of them regardless of relevance is exactly the
unbounded-context-growth mistake [ADR-0012](0012-context-compaction-strategy.md)
spent an entire document establishing evidence against. A skills mechanism needs the
model to discover *and selectively read only what's relevant*, not have everything
handed to it — which is precisely shape (a)'s system-prompt-pointer design, not
AGENTS.md's auto-inject design. The two mechanisms solve genuinely different problems
and this ADR does not try to unify them.

**Directory convention: researched, not guessed, because kopicode already made this
exact call once and should stay consistent with its own precedent.** kopicode chose to
read AGENTS.md — an open, multi-vendor standard — over inventing a kopicode-specific
project-instructions file. The same question exists for skills, and the landscape has
a real answer as of this writing: **`.agents/skills/<name>/SKILL.md`**, with YAML
frontmatter (`name`, `description`), is the convention Codex CLI deliberately adopted
specifically *because* it is not vendor-specific (as opposed to `.claude/skills/`,
which Claude Code originated and which other tools — Cline's discovery chain, for one
— fall back to for compatibility rather than treat as the standard). Cursor and Gemini
CLI also read `.agents/skills/`. opencode's own skills discovery checks it as one of
several equivalent paths. There is no equivalent of agents.md's own spec covering
skills specifically — no single body publishes a "skills.md" standard the way it does
for project instructions — but `.agents/skills/*/SKILL.md` is the de facto convergence
point among the tools that have looked at this problem, in the same way `AGENTS.md`
was the de facto convergence point before it had a formal spec site. Picking it means
a skill someone already wrote for Claude Code, Codex CLI, Cursor or Gemini CLI works
under kopicode completely unmodified, and vice versa — the identical interop win
AGENTS.md already delivers, for the identical reason.

## Decision

**1. Shape (a), exactly as KAN-1021 scoped it: no new tool, no new dispatch entry, no
catalogue change.** `read_file`, `list_dir` and `grep` already do everything a skills
mechanism needs mechanically — discovering what exists under a directory, reading a
file's content, searching across many. Nothing about `internal/engine/dispatch.go` or
`internal/engine/catalogue.go` changes because of this ADR.

**2. Directory convention: `.agents/skills/<name>/SKILL.md`, frontmatter `name` and
`description`, no cascading beyond that one level.** Matches the shape every tool
listed in Context reads, so an existing skill authored for another agent works here
unmodified. **Only this one path is checked** — not also `.claude/skills/` as a
fallback the way Cline's discovery chain does. Considered and left out for the same
reason AGENTS.md's own first cut stayed root-level-only with "no nested
per-directory cascading — real complexity with no evidence of need yet": a second
fallback path is easy to add later with evidence a real project needs it, and free to
skip now with none.

**3. One sentence added to the default system prompt, pointing at the convention —
nothing about frontmatter parsing, nothing about a catalogue.** Illustrative wording,
not final (the exact phrasing is a prompt-writing decision, the same register ADR-0009
left "the exact wording ... not settled here" for the ask refusal text):

> This repository may define packaged, reusable task instructions under
> `.agents/skills/<name>/SKILL.md` (each with a `name` and `description`). If one looks
> relevant to the current task, read it before improvising an approach from scratch.

This is a **generic, repo-agnostic** sentence describing a fixed, documented
convention — the same category of system-prompt content as every existing tool
description, not repository-specific content. It is categorically different from
AGENTS.md's own content, which ADR/epic 130 deliberately keeps **out** of
`Config.SystemPrompt` because that content varies per repository and the system
prompt must stay identical for every repository a given arm runs against (ADR-0007
decision 6). A fixed sentence describing where to look is not that: every repository
running the same harness configuration gets the identical sentence, hashed the same
way, whether or not that repository happens to have a `.agents/skills/` directory —
exactly like the tool catalogue documents `read_file` identically regardless of
whether a given repository has any files worth reading with it.

**4. The hash moves, for the first configuration lineage that already has a published,
externally-cited result attached to its current hash — say so loudly rather than let
it surface as a surprise.** `Config.SystemPrompt` is in `Config.Hash()`'s preimage
(ADR-0007 decision 6). `minimaxM2Config`, `naiveV1Config`, `naiveV2Config` and
`naiveToolsetConfig` are all built by copying a `base Config` — today, `defaultHarnessConfig`
— so this sentence's addition moves every one of their hashes together, the identical
mechanical consequence the `ask` tool's landing already established as expected, not a
regression (ADR-0009 decision 5/Consequences: "the hash moves once, and that is
expected"). What's different this time: `naiveV1Config`'s hash already anchors a
**real, published** result — the harness-axis paired A/B (KAN-1015) and its repeated
run for significance (KAN-1018). That published result remains exactly as valid as it
ever was, as a record of what *that* hash measured on the day it was measured;
reproducing it after this ADR ships requires checking out the commit before this
sentence landed, the same way any pinned experiment does when its arm's config
changes underneath it later. Nothing about this ADR invalidates KAN-1015/1018's
finding — it just means a fresh run today would be measuring a new hash, not the old
one, and should be labeled as such rather than pooled with the old result.
`PreimageVersion` does not move — this is a configuration value changing, not an
encoding change, the identical distinction ADR-0009 already drew for the same reason.

**5. No escalation to shape (b) in this ADR.** A `read_skill`/`list_skills` tool pair
remains exactly what KAN-1021 already said it should be: built only once a project
accumulates enough skill fragments that "the model has to know skills exist without
reading a directory first" is an observed problem. Nothing in this ADR pre-builds
toward it or reserves space for it beyond what's already true (a future tool pair
would be an ordinary new catalogue entry, no different in kind from any other future
tool).

## Alternatives rejected

- **Auto-injecting every skill's full content at session start, the way AGENTS.md is
  injected.** Rejected in Context: right for one small, always-relevant file, wrong
  for a library of many task-specific files where most are irrelevant to any given
  task — exactly the context-growth mistake ADR-0012 already built its evidence base
  against.
- **`.claude/skills/` as the primary or a fallback path.** Rejected: it is a single
  vendor's own convention that other tools read for compatibility, not the convention
  itself. Adopting it as primary would repeat, for skills, the exact choice this
  project already declined to make for project instructions when it picked AGENTS.md
  over a kopicode-specific or vendor-specific file.
- **A kopicode-specific path** (e.g. `.kopicode/skills/`). Rejected for the identical
  reason: `.kopicode/` already holds machine-internal state (sessions, blobs, the
  lock, config) that a human does not author by hand; skills, like AGENTS.md, are
  human-authored, git-tracked content, and belong at a path other tools can also read
  rather than one only kopicode understands.
- **A model-driven `read_skill`/`list_skills` tool pair now (shape (b)), on the theory
  that building it now saves a later migration.** Rejected: no project has yet
  accumulated enough skill fragments to know whether shape (a)'s "the model has to
  find the directory itself" cost is even real, and building ahead of that evidence is
  exactly the guess ADR-0010's self-tuning scope-down and ADR-0012's rejected
  compaction mechanism both already argued against for adjacent problems.
- **Gating the new system-prompt sentence to only some harness configurations**, to
  avoid moving `naiveV1Config`'s hash out from under its published result. Rejected:
  the project's own established norm (ADR-0009 decision 5) is that a real
  configuration-value change moves the hash and that is expected, loudly stated,
  never hidden — special-casing one configuration lineage to dodge that would be the
  first exception to a rule this project otherwise applies uniformly, for a cost
  (labeling a fresh harness-axis run under a new hash) far smaller than the
  inconsistency it would buy.

## Consequences

- No change to `internal/engine/dispatch.go`, `internal/engine/catalogue.go`, or the
  wire tool catalogue. The only code change is one new sentence in
  `internal/harness/prompt_default.md` (or wherever the default prompt template
  lives) and whatever `internal/harness/prompt_test.go` assertions need updating for
  the new sentence's presence and the 8 KiB budget (`TestSystemPromptFitsItsBudget`).
- `defaultHarnessConfig`'s hash moves, and with it `minimaxM2Config`'s,
  `naiveV1Config`'s, `naiveV2Config`'s and `naiveToolsetConfig`'s. The harness-axis A/B
  result (KAN-1015, KAN-1018) remains valid as a record of the pre-change hash; a
  fresh run after this lands measures a new hash and must not be pooled with the old
  one without saying so.
- No new journal event type, no new `Config` field. `.agents/skills/` is documented
  purely as a convention a repository may or may not have — kopicode records nothing
  about its presence or absence, the same way it records nothing about which files
  happen to exist in a repository generally.
- A future card (not scoped here) should add a short section to README or
  `docs/examples/` documenting the convention for a human author, and possibly a
  worked example skill file, mirroring how `.kopicode/config.toml` templates already
  exist under `docs/examples/`.
- If real dogfooding later surfaces the "the model doesn't know to look" problem shape
  (a) accepts as a known cost, KAN-1021's shape (b) is the named next step — this ADR
  does not need to be reopened to authorize that escalation, only to record it once it
  happens, the same way `AdvertiseNativeTools`/`ParseRoutes` already vary per
  configuration without a fresh ADR each time.
