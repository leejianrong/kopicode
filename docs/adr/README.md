# Decisions of record

Each ADR records a decision and why. If a document contradicts an ADR, the ADR wins.
Eighteen exist. Two reverse earlier plans that still appear in older project notes, and
five are amendments layered on top of earlier ones (one of which amends two prior ADRs at
once). Read the relevant one before proposing a change to what it settles.

| ADR | Decision |
|---|---|
| [0001](0001-go-implementation-language.md) | **Go**, not Python or TypeScript. Distribution, compile-time safety, process control. |
| [0002](0002-no-durable-runtime-own-journal.md) | **No durable-execution runtime.** kopicode owns a typed event log; **git** versions the tree via shadow refs. |
| [0003](0003-single-repo-internal-engine.md) | **One repo.** The engine is `internal/`, not a library. |
| [0004](0004-line-oriented-repl.md) | **Line-oriented REPL.** No alt-screen, no TUI framework. |
| [0005](0005-benchmark-and-ab-methodology.md) | **Paired McNemar**, pinned providers, mock/replay provider, early stopping, task pruning. No add-on catalogue yet. |
| [0006](0006-hash-anchored-edits-and-failure-attribution.md) | **Hash-anchored edits** — anchor drift is a hard rejection. Three-bucket failure attribution. Post-edit syntax gate. No AST editing. |
| [0007](0007-model-selection-and-harness-config-shape.md) | **One binary, every supported model.** Harness config is a named in-binary value resolved from the model id. A bench **arm** is (model × harness config × provider pin). |
| [0008](0008-shell-isolation-accepted-risk.md) *(Proposed)* | **Model-authored shell isolation is an accepted risk, not a sandbox.** Trust model: one developer, their own machine. Revisit when that stops being true — see 0011. |
| [0009](0009-ask-tool-contract.md) *(Proposed)* | **The `ask` tool** is a sibling mechanism to consent, not an extension of `internal/permission` — free-text question/answer, never a `Verdict`. |
| [0010](0010-declarative-harness-configs-and-self-tuning.md) | **Declarative harness configs + `kopitune`.** Amends 0007: a second, *declared* config class (TOML, base + overrides) alongside the built-in registry, for models with no hand-tuned entry. Local-only — never anchors a published benchmark number. |
| [0011](0011-unattended-invocation-policy-gate.md) | **A policy gate for unattended invocation.** Amends 0008: a new opt-in `permission.Policy` (declared allowlist) for a caller like cuttlefish that spawns kopicode with no human present. Real containment is the caller's job, not kopicode's — kopicode gains no sandbox dependency from this. |
| [0012](0012-context-compaction-strategy.md) | **Context compaction.** Decision 1 (a smaller verification-truthfulness fix) **Accepted**; decision 2 (a supersession-based compaction strategy) **Rejected** on review. |
| [0013](0013-agent-controlled-resident-session-surface.md) | **`kopicode serve`, a resident session surface over stdio.** NDJSON JSON-RPC 2.0, N concurrent `engine.Open` sessions in one process, credentials via env only, reusing ADR-0011's policy flags and adding an opt-in `--ask-policy-file` (a sibling to consent, per ADR-0009). No engine-boundary change. EPIC-131. |
| [0014](0014-skills-mechanism.md) *(Proposed)* | **A skills mechanism — a documented directory, no new tool.** Reusable task instructions under `.agents/skills/<name>/SKILL.md` (a multi-vendor convention), discovered and read with the existing `read_file`/`list_dir`/`grep`; one system-prompt sentence points the model there. No engine loader, no dispatch or catalogue change. EPIC-132. |
| [0015](0015-mcp-server-front-end.md) *(Accepted)* | **An MCP server front end.** A fourth `cmd/kopicode` subcommand (`mcp`), reusing `serve`'s session core over a new `cmd/kopicode/session` package, so any MCP-capable agent orchestrator can drive kopicode with zero bespoke client code. Additive — `serve`'s wire is unchanged. |
| [0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md) *(Accepted)* | **Live remote consent.** Amends 0009/0011: a new `RemoteConsenter` bubbles permission decisions live over the wire instead of a pre-declared allowlist, plus an explicit `consent_mode` a caller must declare (`remote_interactive` vs. `unattended_policy` with a required containment acknowledgment). Does not loosen ADR-0011's exact-match allowlist. |
| [0017](0017-auto-consent-mode.md) *(Accepted)* | **An `auto` consent mode.** Amends 0016: a third `consent_mode` where the harness answers itself — shell inside `root` is allowed, a fixed never-allow list (`sudo`, `rm` outside `root`, forced `git push`, download piped into a shell, redirection outside `root`) is denied with a reason. Tokenized and fail-closed, not substring-matched; callers may add entries, never remove one. Journalled as `source: "auto"`. Not a sandbox. |
| [0018](0018-tokenized-allowlist.md) *(Accepted)* | **A tokenized allowlist.** Amends 0011: `allow_commands` in the policy file — command prefixes matched on whole tokens, a line permitted only if *every* command in it matches, ADR-0017's never-allow rules still on top. Answers #157 without the character-prefix matching 0008/0011/0016 rejected. |

Satay's natural consumer in this suite is **cuttlefish** (unattended, triggered,
credential-holding, not started), not kopicode. Do not reintroduce it here.
