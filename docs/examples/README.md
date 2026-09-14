# Configuration examples

Example `.kopicode/config.toml` files for different project types.

## Use one

```bash
cp docs/examples/go-project.toml .kopicode/config.toml
```

Edit the `model` field if you want a model other than the default, then run
`kopicode` in your project directory.

| File | Project type |
| --- | --- |
| `go-project.toml` | Go |
| `node-project.toml` | JavaScript/TypeScript |
| `python-project.toml` | Python |
| `multi-language.toml` | Multiple languages in one repo |

## What the file can hold

kopicode's reader is intentionally not a full TOML implementation — flat
top-level `key = "value"` lines and comments only, per
[ADR-0007](../adr/0007-model-selection-and-harness-config-shape.md) decision 2.
The keys it reads:

- `model` — a registered model id (see the main [README](../../README.md#3-pick-a-model)
  for the current list).
- `harness` — a registered harness config name, when overriding the one the
  model resolves to by default.
- `verify` — an argv array, e.g. `verify = ["go", "test", "./..."]`, that pins
  the forced-verification command instead of relying on discovery. It must be
  an array, never a shell string: kopicode records and replays argv, and there
  is no shell in the loop to re-split one.

Precedence, highest first: `--model`/`--harness` on the command line, this
file, then the built-in default.

## Skills: packaged, reusable task instructions

A repository can teach kopicode how it likes a recurring task done by dropping a
skill file at `.agents/skills/<name>/SKILL.md`
([ADR-0014](../adr/0014-skills-mechanism.md)). There is no new tool and nothing
to configure: the default system prompt tells the model to look there, and it
reads a relevant skill with `read_file` before improvising an approach.

`skills/go-table-test/SKILL.md` in this directory is a worked example. The shape
is a short YAML frontmatter block followed by Markdown instructions:

```markdown
---
name: go-table-test
description: Add or extend a table-driven test in this Go project. Use when asked to add test coverage for a function.
---

# Writing a table-driven test

This project tests with table-driven subtests and no assertion library...
```

- `name` — a short identifier, conventionally the directory name.
- `description` — one line saying *when* the skill applies. This is what the
  model reads to decide whether the skill is relevant, so write it as a trigger
  ("Use when...") rather than a title.
- The body is ordinary Markdown: the steps, conventions, and commands you want
  followed. Keep it focused on one task — a library of small, specific skills
  beats one that tries to cover everything.

The path is deliberately vendor-neutral: a skill authored for Claude Code, Codex
CLI, Cursor or Gemini CLI at `.agents/skills/<name>/SKILL.md` works under
kopicode unmodified, and vice versa — the same interop reason kopicode reads
`AGENTS.md` rather than a tool-specific instructions file.

To use the example as a starting point:

```bash
mkdir -p .agents/skills/go-table-test
cp docs/examples/skills/go-table-test/SKILL.md .agents/skills/go-table-test/
```

Skills are human-authored, git-tracked content, so they live at that path rather
than under `.kopicode/` (which holds machine-internal state kopicode writes).

## See also

- [Main README — Quickstart](../../README.md#quickstart)
- [Architecture Decision Records](../adr/)
- [PRD](../PRD.md)
