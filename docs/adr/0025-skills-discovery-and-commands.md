# ADR-0025: Skills: discovery roots and slash commands

*Accepted.* Amends [ADR-0014](0014-skills-mechanism.md); it keeps ADR-0014's shape (a) and its system-prompt sentence, and adds what a person at the REPL needs.

## Context

ADR-0014 chose files under a documented directory and one sentence in the prompt, with no tool and no command. That works when a repository ships skills. It does not give a person their own skills across projects, a way to see what is available, or a way to run one deliberately, and the model's file tools cannot read anything outside the project, so a personal skill could never reach it.

## Decision

1. **Roots, in precedence order.** `<repo>/.agents/skills`, `<repo>/.kopicode/skills`, `<user config dir>/skills`, each path in the user config's `skills_paths` (ADR-0024), then `~/.claude/skills` and `~/.agents/skills` as read-only fallbacks, so a skill written for another agent works unmodified. The first root to hold a name wins, so a repository's skill shadows a personal one. `<repo>` is the nearest ancestor with a `.git`.
2. **Format.** `<name>/SKILL.md` with `name` and `description` in frontmatter, a flat subset of YAML (quoted or bare values, continuation lines, folded `>`). The name defaults to the directory's and must be lowercase letters, digits, `-` and `_`, because it is also a command. A skill with no description, a bad name, unclosed frontmatter or more than 256 KiB is **skipped with a warning**, shown by `/skills`, never silently dropped.
3. **`/skills`** lists them with their description and where each came from. **`/<name> [task]`** runs one: the body is inlined into the user turn under a `<skill>` tag, followed by the task. It is inlined because the model's tools cannot open a skill outside the project, and because it makes the journal's `UserMessage` exactly what the model saw. A built-in command (`/context`, `/exit`, `/mode`, `/quit`, `/skills`) wins over a skill of the same name, and `/skills` says so. A slash word that is neither goes to the model as typed, as before.
4. **Discovery is live.** It runs on each `/skills`, invocation and Tab, so a skill written mid-session is usable at once.
5. **Tab completes** a slash command or skill name, extending to the shared prefix and listing the candidates below the line when it cannot narrow them. This is a `Complete` hook on the line editor, bound to Tab only when set. A live dropdown as you type is not built; Tab covers the need, and a dropdown redraws the screen in ways this editor has avoided.
6. **A catalogue for the model, only of what it can read.** A new REPL session is told once, as a user-turn message, the name, description and path of each skill inside the session's root. Skills outside it are left out, since listing what cannot be opened invites a failed read; those are the user's, through `/<name>`. It is journalled as a `ProjectInstructionsLoaded` event with `scope: "skills"` and replayed on resume like the instructions files, so there is no new event type. It is never put in `Config.SystemPrompt`, so the hash and the benchmark arms do not move. It is capped at 100 entries and says so.
7. **`serve`, `mcp`, `run --print` and `kopibench` are unchanged.** They set no `Options.Skills`, so their sessions are told nothing new and read no user directory. A caller that wants the catalogue can set it when it has a reason to, by an additive option and a feature name.

## Rejected

- **A `read_skill` tool** (ADR-0014 shape (b)): still no evidence the model needs it. Inlining on `/<name>` reaches personal skills without one.
- **Injecting every skill's body at start**: ADR-0014's objection, unchanged.
- **Letting `skills_paths` come from a repository**: ADR-0024.
- **A live dropdown now**: see decision 5.

## Consequences

- `internal/skills` is a new leaf package, re-exported through `internal/engine` so front ends keep their import allowlist.
- The model, shown a catalogue, may read a skill's `SKILL.md` unprompted. That is the point.
- A skill is prompt text, so it is as trusted as the directory it came from. A personal directory is the user's own; a repository's skill is as trusted as its `AGENTS.md`, which kopicode already injects.
