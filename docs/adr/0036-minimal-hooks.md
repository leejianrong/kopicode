# ADR-0036: Minimal hooks: a pre-tool deny, a post-tool command and a stop gate

*Accepted* on the owner's instruction to settle the design (KOP-180). *Implemented* in KOP-202 to KOP-206 (PRs #239 to #243), one card each; what the implementation settled that the decisions below left open is under [Implementation notes](#implementation-notes). Builds on [ADR-0024](0024-user-level-config.md) (what a repository may and may not set), [ADR-0017](0017-auto-consent-mode.md) (a list that can only be lengthened), [ADR-0033](0033-tool-output-cap.md) and the forced verification in `internal/verify`.

## Context

Claude Code and Pi let a person declare commands the harness runs at fixed points: before a tool call, after one, and when the model tries to stop. They are deterministic where an instruction file is advisory (`docs/research/2026-10-harness-survey.md`). kopicode already has two hard-coded cases of the same idea: the consent gate decides before a call, and forced verification runs the project's test command after a turn that wrote files and refuses a success report when it fails. What is missing is a way for a person to add their own, such as "never touch `migrations/`", "run the formatter after an edit" or "do not stop until `make lint` passes".

The risks are the reason to keep it small. A hook is a command run on the person's machine. A repository is a stranger's checkout, so anything it can declare, a stranger can run (ADR-0024's reason for keeping `default_mode` out of a repository's file). A hook that fails quietly would be a gate that is open exactly when it is broken.

## Decision

1. **Three hook points, one command each.**
   - `pre_tool`: runs before a tool call is dispatched. It can **deny** the call. It cannot allow, rewrite the arguments or skip the consent prompt.
   - `post_tool`: runs after a call that changed the tree (`write_file`, `edit_file`, `run_shell`). It cannot undo the call. Its output is added to what the model sees, as a short note after the result.
   - `stop`: runs when the model finishes without a tool call, after forced verification. A non-zero result **refuses the stop**: the output goes back to the model as feedback and the loop continues.
2. **Declared in the user's `config.toml` only.** Three flat keys, each an argv array, never a shell string (the reason `verify` is one, `internal/harness/file.go`): `pre_tool_hook`, `post_tool_hook`, `stop_hook`. One more, `hook_tools`, an array of tool names the two tool hooks apply to, defaulting to every tool. A repository's file that sets any of them is a usage error that says they belong in the user file, the same treatment `default_mode` gets. So a repository **cannot declare, change or loosen a hook**, and a user's hooks cannot be switched off from a checkout. The repository's own gate remains its `verify` key, which is what a repository can already say.
3. **Only the human-facing front end reads them,** as with every other user key (ADR-0024 decision 4). `run --print`, `serve`, `mcp` and `kopibench` run no user hooks, so one person's habits cannot change what an orchestrator or a benchmark run does. A later decision may let `serve` take hooks from its caller; this one does not.
4. **A hook can only tighten.** A `pre_tool` result is combined with the consent gate by taking the stricter: the gate still runs, a hook allowing nothing is not an allow, and a deny from either stops the call. The never-allow list and `read_only` are untouched. This is ADR-0017's rule, that a list a person can add to is never one they can shorten, applied to a new source.
5. **Fail closed.** For `pre_tool`, anything other than a clean pass is a deny: a non-zero exit, a timeout, a command that cannot start, output that is not what the contract says. For `stop`, a failure or a timeout refuses the stop. A refused stop is bounded: after three refusals in a row the session ends with a distinct stop reason (`stop_gate_failed`, a non-zero exit code) and never reports success. `post_tool` cannot undo, so its failure is not a block; it is recorded and shown to the model as a failure, never swallowed.
6. **The contract with the command.** The event goes in as JSON on stdin: the hook point, the tool name, the call's arguments, the session id and the turn. It never includes the provider key or any credential. Exit `0` passes; exit `2` denies with stderr as the reason, which is shown to the model and to the person; any other non-zero exit is a hook error and is treated as a failure by decision 5. A default timeout of 30 seconds, configurable up to five minutes, after which the process group is killed.
7. **The subprocess rule holds.** Every hook command names the directory it runs in (the session's working tree root) and builds its own environment from the package's allowlist builder, never `os.Environ()`: no provider key, and no `GIT_DIR` or other variable that would redirect git (AGENTS.md, "Every subprocess names the directory it runs in *and* builds its own environment"). It runs in its own process group through `internal/procgroup`, so a timeout kills what it started. `internal/arch/subprocess_test.go` covers the new call site with no waiver.
8. **Everything is journalled, whole.** A new event, `HookRan` (additive; an old reader keeps it as an unknown type), records the hook point, the argv, the exit code, the duration, the outcome (`passed`, `denied`, `failed`, `timed_out`) and the command's output in full, spilling to a blob past 64 KiB. Nothing is truncated in the record; what the model sees of a long output is capped by ADR-0033's rule. Replay and resume read the events and **never run a hook again**.
9. **In the harness hash only when set.** A session with no hooks has the hash it has today, so no recorded fixture or benchmark number moves. A session with hooks records them in `SessionStarted`, so a run is never compared against one with a different gate.
10. **Out of scope here, each its own decision:** hooks that rewrite a tool's input or output, a prompt-submit or session-start hook, regex or glob matchers beyond the tool name, hooks shipped by a skill or a repository, and `serve` taking hooks from its caller.

## Implementation notes

Settled while building it, none of which changes a decision above:

- **Order against consent.** `pre_tool` runs *before* the consent gate, so a person is never asked to approve a call a hook has already refused. A pass is still not an allow: the gate runs next, and the stricter of the two stands (decision 4).
- **`post_tool` covers every tool that changes the tree**, the engine's own `mutates` set (`write_file`, `edit_file`, `edit_file_fuzzy`, `delete_file`, `run_shell`), which is decision 1's rule ("a call that changed the tree"). Decision 1's parenthetical named three of them. It is skipped for a call that never ran (consent or `pre_tool` denial) and for one that failed. Exit `2` has nothing to deny there and is recorded as `failed`. The note goes after the result and after the output cap, and is capped on its own.
- **`stop` runs at the clean prose stop, after the verification check.** A tree verification rejected still ends as `verification_failed` and the hook is not consulted. The refusal count is per exchange, and a pass resets it. `stop_gate_failed` is exit 1, charged to the model like `verification_failed`. Any non-pass refuses, exit `2` included.
- **The environment allowlist** (`hookPassEnv` in `internal/engine/hook.go`) is `PATH`, the home and user variables, locale, temp directories, the Go toolchain variables, XDG directories, TLS certificate paths and what Windows needs to start a process. A hook that needs another variable sets it in its own command.
- **`hook_tools` is validated** against the dispatch table: a name that is not a tool is a usage error, since a misspelling would leave that tool ungated.
- **The record.** `HookRan` carries stdout and stderr separately, both whole. `SessionStarted.hooks` is present only for a hooked session. Resume rebuilds a `post_tool` note and a refused stop from `HookRan` with the same functions that built them live, and never runs a hook.
- **Not on any wire.** No feature name and no wire rows were added: `features` describes the `serve` wire, and hooks are not on it (decision 3). The serve, `run --print` and mcp documents say so.
- **The timeout** (decision 6, "configurable up to five minutes") is a fifth user-file key, `hook_timeout_seconds`, an integer from 1 to 300 (KOP-208, approved by the owner): user file only and refused in a repository's, like the other four; a value outside the range is a usage error, never clamped. It is in the harness hash and in `SessionStarted.hooks` only when set, and does nothing without a hook command. `Config.HookTimeout` stays as the engine's test seam and wins over the key when set.

## Rejected

- **Hooks in the repository's file, as Claude Code allows in a project's settings.** A checked-in command run on every contributor's machine is the supply-chain shape ADR-0024 refused for `default_mode`. A repository states its test command already.
- **Allow from a hook.** A hook that could grant would let a script written once override a prompt the person never saw. Only tightening is safe to automate.
- **A shell string.** It would need a splitter that is not the shell that runs it, and the two disagree on the first path with a space.
- **Ignoring a hook that errors.** That is the gate that is open when it is broken.
- **Letting a hook modify the arguments.** The journal records the call the model made; a silent rewrite means the record and the tree disagree. It could return later with its own event and its own ADR.
- **Running hooks under `serve` and `mcp` now.** An orchestrator chooses its own policy through consent modes (ADR-0016, ADR-0017); a person's local hooks would change a fleet's behaviour by which machine it landed on.

## Measurement

No gain is claimed. The check is a regression one, for the implementation cards: mock-provider tests for allow, deny, exit 2 with a reason, a crash, a timeout (and that its process group is gone), a refused stop that is bounded at three, replay that does not rerun a hook, a repository file that sets a hook key being refused, and a session with no hooks hashing exactly as before. `make bench-smoke` must stay green with no hash change.

## Consequences

- Implementation split into five cards: the config keys and their repository refusal; the `pre_tool` hook with its stricter-of combination and the `HookRan` event; `post_tool`; the `stop` gate and `stop_gate_failed`; the README and the wire documents.
- `internal/engine` gains one small package-level runner for hook commands, so the three points share one subprocess path and one test of the rule in decision 7.
- The README has its section as of KOP-206.
