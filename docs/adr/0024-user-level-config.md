# ADR-0024: User-level config and instructions

*Accepted.* Amends [ADR-0007](0007-model-selection-and-harness-config-shape.md) decisions 2 and 3.

## Context

A person who always wants one model, or auto mode, or an extra never-allow entry, had to repeat it as a flag or commit it into every repository. Claude Code has `~/.claude/settings.json` and a global instruction file; kopicode had neither.

Two earlier decisions constrain the answer. ADR-0007 decision 3 refused environment variables in the precedence chain because an ambient value is invisible in a shell history, a job log and a diff. And a repository's config is read from whatever checkout a person opens, so anything a repository can set, a stranger can set for them.

## Decision

1. **A user file**, `config.toml` in `$KOPICODE_HOME`, else `$XDG_CONFIG_HOME/kopicode`, else `~/.config/kopicode`. The variables only locate the directory, and what the file supplies is resolved and recorded in the journal like any other value, which is what decision 3 was protecting. Same flat-key reader as the repository's file.
2. **Precedence for `model`:** flag, then the repository's file, then the user file, then the built-in default. The repository outranks the user because it names the project's choice.
3. **Keys:**
   - `model`: either file.
   - `auto_never_allow`: either file, **additive**. The built-in list, the user's entries and the repository's all apply; a file can only lengthen it, as ADR-0017 requires.
   - `default_mode` (`"default"` or `"auto"`) and `skills_paths`: **user file only.** A repository setting `default_mode = "auto"` would turn off the prompts for everyone who clones it; `skills_paths` would let it point a session at any directory. Either key in a repository's file is a usage error that says where it belongs.
   - `harness`, `harness_config`, `verify`: repository only, refused in the user file.
   - `--mode` beats `default_mode`.
4. **Only the human-facing front end reads it.** The interactive REPL sets `Overrides.UserConfig`. `run --print`, `serve`, `mcp` and `kopibench` resolve from flags, the repository and the defaults alone, so one person's preferences cannot change what an orchestrator or a benchmark run. A repository's `auto_never_allow` applies everywhere, since it can only tighten.
5. **A user-level `AGENTS.md`** beside the config is given to a new REPL session before the repository's own, which is the more specific and so the more recent. It is journalled as a `ProjectInstructionsLoaded` event with `scope: "user"` (additive, omitted for the repository's, so old journals read as before), framed for the model as the user's own file, and replayed from the journal on resume rather than re-read, as ADR-0007 and KAN-1024 already require of the repository's. It is not in the harness hash.
6. **Errors name the file and line.** An unknown model from the user file names that file, not the repository's.

## Rejected

- **Dotted tables** (`[auto] never_allow`, `skills.paths`): the reader is flat by ADR-0007 decision 2. The keys are `auto_never_allow` and `skills_paths`.
- **JSON, as Claude Code does**: one config syntax per tool.
- **Letting `serve` read the user file**: it would make a cuttlefish session depend on the machine's owner's habits.

## Consequences

- `harness.Resolve` gains `Overrides.UserConfig` and `Selection.Settings` (not part of the hash). `engine.Options.UserInstructions` carries the path.
- `skills_paths` is parsed and validated now and consumed by the skills change that follows.
