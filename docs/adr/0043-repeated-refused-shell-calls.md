# ADR-0043: Tell the model how many shell calls were refused, once there are three

*Proposed* 2026-11-07, for the owner's decision. KOP-237. Nothing here is built. Builds on the unattended-mode refusal in `internal/permission/policy.go` ([ADR-0011](0011-unattended-invocation-policy-gate.md), [ADR-0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md)) and on [ADR-0039](0039-leftover-name-note.md) for how an opt-in harness change is shaped. It changes what the model is told, so it is a decision. It does not touch consent: nothing is approved that was refused, and the never-allow list is not read or changed.

## Context

In `run --print` with no policy file, and in any unattended mode, a `run_shell` call is refused. The model is told so at once, and the text already says not to retry: *"permission denied: run_shell: shell commands are never approved in unattended mode (no sandbox exists yet to contain them) — this will not be approved on retry; use the file tools instead"*. A caller pays for every refused call, and for every turn it takes the model to stop.

The worst case on record is `docs/dogfood-runs/2026-11-07-kop-233-verify-changed-only/flag-s5-contradictory-tests`: 15 refused calls, 6 distinct commands, and the session ended `max_turns` at turn 20.

### How often, over the committed journals

Counted from `PermissionRequested` (kind `run_shell`) paired with a `PermissionDecided` of `deny`, over the 281 journals under `docs/` that name a model:

| model | sessions | with 3 or more refused shell calls | of those, ended `max_turns` |
|---|---|---|---|
| qwen/qwen3-coder-next | 228 | 12 | 1 |
| deepseek/deepseek-v3.2 | 33 | 17 | 2 |
| deepseek/deepseek-v4.1-flash | 20 | 0 | 0 |

That is 29 of 281 sessions (10 percent), and 3 of the 29 ran out of turns. The count is **not a rate for ordinary use**. Every one of the 29 comes from one of four experiments, and one of them (`2026-11-07-kop-235-verify-changed-only-limits`, 17 of the 29) used a prompt written to make the model call a shell. The deepseek-v3.2 sessions from `2026-10-25-kop-222-deepseek` (7 of 10 with three or more) ran the five ordinary scenarios and are the closest to organic. I did not count by what the task asked for, and the scenarios are all "fix a failing test", which tempts a model to run the suite.

What the 29 look like, which changes what a fix can be:

- **deepseek-v3.2 refuses in a batch, not in a loop.** In the 9 sessions from `2026-10-25-kop-222-deepseek` and `2026-10-25-kop-226-leftover-note`, every refusal is in turn 1 (in one session, turns 1 to 3): the model sends many shell calls in one reply (3 to 30), gets every one refused at once, and moves on. Nothing is learnt from a second look, because there is no second turn of the same kind.
- **qwen3-coder-next refuses across turns.** Its refusals spread over most of the session (for example turns 1 to 14 of 16). Each refused call is a different command: in 25 of the 29 sessions no command repeats at all, and in 28 the distinct commands are at least half the refusals. A rule keyed on the *same command repeated* would catch almost none of these.
- The refusal text is the same in every case, and already says it will not be approved.

Not measured: what a refused call costs in tokens (the journals hold the calls, not a per-call price); whether a session that ended `completed` after refusals was worse than it would have been without them; whether any wording changes what a model does. One example (the s5 session) is a lead, not evidence.

## Decision (proposed)

1. **Count refused shell calls per session.** The engine keeps a count of `run_shell` calls the policy refused in the current exchange. Nothing is journaled for the count itself; the refusals are already `PermissionDecided` events.
2. **From the third refusal on, the refusal text ends with one sentence:** *"This is the Nth shell call refused in this session; none will be approved. Finish with the file tools and say what you could not run."* The first two refusals read exactly as today.
3. **Nothing else changes.** The tool list is not altered (a changing tool list breaks the prompt-cache prefix). No call is skipped, no stop is forced, no turn is added. The refusal remains a tool result with `error_kind: task`.
4. **Off by default.** `Config.RefusalCount` (bool, in the harness hash only when set), `--refusal-count`, a declared key `refusal_count` (0|1). It becomes default only on the measurement below.

### The measurement that would decide the default

Interleaved off/on pairs, 10 per arm, on the s5 scenario with `run --print` and no policy, qwen3-coder-next (the model of the 15-refusal session), then once on deepseek-v3.2 with the prompt used in KOP-235. Read, per arm: sessions with three or more refusals, mean turns after the third refusal, sessions ending `max_turns`, and whether the final tree differs. If the sentence moves none of those, the ADR is marked Rejected and nothing ships.

## Alternatives

- **Do nothing.** A real option. 3 sessions in 281 hit the cap, and the refusal already says what the sentence would. The counts above do not show harm outside s5.
- **End the session after N refusals.** Rejected for now: it turns a harness opinion into a stop, and `run --print` callers read stop reasons as the model's outcome.
- **Detect the same command repeated.** Rejected: in 25 of 29 sessions no command repeats; the pattern is many different commands.
- **Remove `run_shell` from the tool list after N refusals.** Rejected: the tool list never changes within a session, and a test pins it (cache prefix).
- **Tell the model up front, in the system prompt, that shell is unavailable.** The prompt is at its byte budget, and the model would be told in sessions where a policy file allows shell.

## Consequences

- If accepted, a small change in `internal/permission` or the dispatch path, one flag, one harness key, tests, and the measurement above in a dogfood note.
- A caller sees no new event type; an older reader ignores the flag.
