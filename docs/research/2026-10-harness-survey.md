# Harness survey, October 2026: memory, token savings, opinions, auth, cheap testing

Research behind [ADR-0026](../adr/0026-handoff.md) and the cards filed from it. Done on
2026-10-08 by web search and fetch, not by running other tools. **Status of the claims:** each
is marked *verified* (a primary page read), *snippet* (seen only in a search result) or
*unverified* (background knowledge, not checked). Nothing here has been measured on
kopicode's own benchmark. Where this file and the code disagree, the code is right.

## What kopicode already has and lacks, as of v0.4.0

Has: parse-and-repair, hash-anchored fail-closed edits, post-edit syntax gate, forced
verification, consent gate with `auto`, skills, `ask`, shadow-ref snapshots, resume/fork,
bounded `read_file` with an offset to fetch the rest, `/context`, usage and cost reporting,
per-session limits, user-level config and `AGENTS.md`.

Lacks: hooks, subagents, plan mode, a todo tool, mid-conversation reminders, a general stall
detector (only ADR-0012 decision 1's identical-denial collapse), an `AGENTS.md` hierarchy
(it reads the repo's and the user's, nothing in between), any prompt-cache control
(`cache_read` is recorded in usage but nothing sets `cache_control` or a `session_id`), a
user-facing provider base URL (`WithBaseURL` is an internal option), and any provider but
OpenRouter.

## 1. Knowledge and memory across harnesses

| Harness | Within a session | Across sessions |
|---|---|---|
| Claude Code | `/compact`; subagents return a distilled summary | `CLAUDE.md` hierarchy with `@` imports (4 hops) and `.claude/rules/`; auto memory is a `MEMORY.md` index (first 200 lines or 25 KB) plus topic files read on demand; reads `AGENTS.md` only when no `CLAUDE.md` exists (verified, code.claude.com/docs/en/memory) |
| Codex CLI | compaction (unverified) | `AGENTS.md` chain built once per session: global, then root to cwd, one file per directory, nearer last, capped at 32 KiB (verified, learn.chatgpt.com/docs/agent-configuration/agents-md) |
| Aider | repo map, a graph-ranked symbol map with a 1k-token default budget | `CONVENTIONS.md` loaded read-only; chat transcript kept in `.aider.chat.history.md` (verified) |
| Cline | `/newtask` opens a clean window carrying plan, work done, relevant files and next steps; auto compact | Memory Bank: `projectbrief`, `activeContext`, `progress` and others, read at session start, updated at the end (verified, docs.cline.bot) |
| OpenHands | condensers; `LLMSummarizingCondenser` by default | not covered |
| opencode | not covered | `AGENTS.md` walked up from cwd, a global one, `CLAUDE.md` fallback, an `instructions` array of globs and URLs (verified, opencode.ai/docs/rules) |
| Pi | auto-compaction near the window limit, `/compact`; the session JSONL tree is append-only and only the model's view changes (snippet) | the session tree |
| Cursor | not covered | `.cursor/rules/*.mdc` with four activation types, plus nested `AGENTS.md` (verified) |
| Amp | compaction dropped for `/handoff`: state a goal, Amp drafts a prompt and file list, you edit it, a new thread starts (snippet) | handoff |
| Gemini CLI | not covered | `GEMINI.md` global, ancestors and subdirectories with `@file` imports; `/memory show\|refresh\|add`; `context.fileName` may name `AGENTS.md` (verified) |
| Letta/MemGPT | core memory blocks pinned in the prompt, edited by the agent through tools | all messages persisted and retrievable (verified) |

Not covered at all: Goose (docs 404), Aider's history summarisation.

Patterns that recur: a plain-markdown instruction file, hierarchical, nearest wins, and
`AGENTS.md` is the name everyone is converging on; a hard size cap on what auto-loads (Codex
32 KiB, Claude auto memory 200 lines); an index file plus topic files read on demand; a
progress file read first and updated last; the record stays append-only while the model's view
is derived (Pi), which is kopicode's journal rule.

### Compaction against reset: what the evidence says

- Against long contexts: Chroma's context-rot study (18 models) found reliability falls with
  input length and a focused prompt beat the full one. Anthropic names the same effect.
- Against LLM summarisation specifically: JetBrains, arXiv 2508.21433, found that hiding old
  tool outputs and keeping the reasoning halved cost and matched or slightly beat summarising,
  with Qwen3-Coder 480B going from 53.8% to 54.8% on SWE-bench Verified. One scaffold and one
  benchmark, and the window size needed retuning on OpenHands.
- Amp's reason for dropping compaction (a summary encourages long, meandering threads) is an
  opinion with no data behind it that was seen.
- **There is no controlled study of handover-and-reset against compaction, and none on weak
  open-weight models.** Reset is well motivated and not proven better. kopicode's rig is the
  place to find out.

What a handover carries (Cline, Amp, Anthropic's long-running-agent post): goal and plan with
done and remaining, decisions, open problems, file paths not contents, the exact next step, how
to run and verify. Anthropic's feature list is JSON with a single editable `passes` field,
because models damage JSON less than markdown; the failure modes it reports are declaring
victory early and marking things done without verifying.

## 2. Saving input and output tokens

Provider caching on OpenRouter (verified against openrouter.ai/docs/guides/best-practices/prompt-caching
and api-docs.deepseek.com/guides/kv_cache):

| Provider | Switch | Cache read | Notes |
|---|---|---|---|
| DeepSeek | automatic | 0.1x input | write costs normal input; best effort; only the input prefix; a request hits only if it fully matches a stored unit; an entry takes seconds to build |
| Qwen (Alibaba) | explicit `cache_control: {type: ephemeral}`, select models | 0.1x | write 1.25x, 5 minute TTL |
| GLM (Z.ai) | automatic | about 0.2x | write free |
| Moonshot | automatic | 0.25x | write free |
| MiniMax | not documented there | unknown | unverified |

OpenRouter sends follow-up requests to the same provider only when cache reads are cheaper than
normal input, expires the sticky session after 10 minutes idle, and a manual `provider.order`
overrides it. A `session_id` (body field or `x-session-id`, up to 256 characters) pins routing
before any hit has been seen. Usage reports `prompt_cache_hit_tokens` and
`prompt_cache_miss_tokens` on DeepSeek.

| Technique | Saves | Caveat for open-weight models on OpenRouter |
|---|---|---|
| Append-only history, stable prefix (system prompt, then tools in a fixed order, then history) | input, 90% of the hit portion on DeepSeek | no timestamps or git status early; JSON key order must be deterministic; log hit counters |
| `cache_control` breakpoints | input | Qwen and Anthropic only; gate per model |
| Observation masking (stub old tool results in the wire view) | input, about half of total cost in the JetBrains paper | edits the prefix, so each batch costs a cache write; mask rarely and in large batches; keep the journal whole; the stub must point at the blob |
| Cap what the model sees of large outputs, full output in blobs | input | needs a decision on "never truncate": the rule protects the record, and the record stays whole |
| Skip repeat reads of an unchanged file | input | content hash per path, invalidated on edit; gain unmeasured |
| Search before read, line-range reads | input | a nudge in tool descriptions |
| Anchor edits instead of whole-file writes | output | kopicode already has this |
| Cap reasoning effort and `max_tokens`; drop old reasoning from replayed history | output | per-model support varies |
| Lazy tool loading | input | third-party figures only (27k to 9.7k on the first request, 46% on the task, from Claude Code with MCP tools); kopicode's tool list is small, and changing it invalidates the cache |
| Subagents for isolation | input in the main thread | total tokens usually rise; a quality win, not a cost win; unverified |
| Model routing per task | cost | switching models mid-session loses the cache |

Aider's repo map is about 1k tokens by default (verified); the article gives no benchmark that
it helps.

## 3. Opinions that harnesses bake in

Evidence, strongest first:

- **SWE-agent ACI ablations** (arXiv 2405.15793, GPT-4 Turbo, SWE-bench Lite, 18.0% baseline;
  verified): no lint-on-edit 15.0, no edit command 10.3; a 100-line file window 18.0, 30 lines
  14.3, the full file 12.7; summarised search 18.0, iterative 12.0, none 15.7; last 5
  observations 18.0 against full history 15.0. Search over 50 hits is withheld and the agent is
  told to narrow it; an empty output returns "ran successfully, no output". The last two were
  not ablated. These are GPT-4 numbers, not open-weight ones.
- **LangChain Deep Agents** (verified, vendor blog, one benchmark): harness changes alone moved
  Terminal Bench 2.0 from 52.8 to 66.5 with the model fixed. The pieces were build-then-verify
  prompting, a pre-completion checklist that intercepts exit, loop detection ("reconsider your
  approach" after N edits to one file), time-budget warnings and a reasoning-effort schedule.
  Only the effort schedule was isolated (xhigh throughout 53.9, high 63.6, high-low-high 66.5).
- **Claude Code best practices** (verified, code.claude.com/docs/en/best-practices): the whole
  guide rests on context filling and performance degrading; "give Claude a way to verify its
  work" is first; hooks are deterministic where `CLAUDE.md` is advisory; subagents keep
  exploration out of the main context; a fresh-context reviewer avoids bias toward code it just
  wrote, and over-reports; plan mode, skipped for a one-sentence diff; keep `CLAUDE.md` short;
  after two failed corrections, clear and restart.
- **Aider**: forcing code into JSON lowered scores for essentially every model tested
  (aider.chat/2024/08/14/code-in-json.html, verified); the best edit format differs per model
  (leaderboard, verified).
- **Hashline** (oh-my-pi): a line number plus a short content hash as the anchor, the closest
  published relative of ADR-0006. The claimed jump (6.7% to 68.3% edit success for one model) is
  *unverified*, from a secondhand summary; the original was not reached.
- **mini-swe-agent**: bash only, linear history, over 74% on SWE-bench Verified, but that is a
  frontier model, so it says nothing about weak ones (snippet).
- **Counter-evidence** (REAP, arXiv 2604.01527, snippet): developer-written context files helped
  a weaker harness (+6.4) and gave nothing, or slightly hurt, on a stronger one. Gains do not
  stack.
- Claude Code's todo tool and `<system-reminder>` injections: no authoritative source and no
  ablation found. Capture raw API traffic through a logging proxy if the exact text matters.

Traps for weak models: subagents (lossy summaries, delegation errors, more cost); full plan
mode with approval UI (plans verbosely, then deviates), so prefer a todo list plus read-only
gating; a tree-sitter repo map (no proof, and CGo); JSON-wrapped code arguments; long
instruction files and many tool definitions; the reasoning-effort schedule unless the provider
exposes effort levels.

## 4. Codex OAuth: what is known

The maintainer wants to pursue authenticating kopicode against a ChatGPT/Codex subscription,
as other terminal harnesses do.

- **Hermes Agent** (Nous Research): authenticates through ChatGPT device-code OAuth, stores
  credentials in `~/.hermes/auth.json`, and can import `~/.codex/auth.json`. Its docs say which
  plan tiers qualify, and how usage counts against Codex limits, is not documented (verified,
  hermes-agent.nousresearch.com/docs/integrations/providers). Open issues: `/usage` ignores the
  quota (NousResearch/hermes-agent #15167), a request for the documented "Sign in with ChatGPT"
  flow (#128880), and a slower-than-portal report (#134798). Community docs mention a
  `hermes proxy` exposing OAuth-backed providers as an OpenAI-compatible endpoint
  (unverified).
- **opencode** has a community plugin (`opencode-openai-codex-auth`) that says it is for
  personal development with your own Plus or Pro subscription. **Pi** has a built-in
  `openai-codex` provider that a community package overrides (snippet). **oh-my-pi** was not
  checked: that it supports Codex OAuth out of the box is the maintainer's report, not something
  verified here.
- **Terms**: OpenAI's own text on third-party use of Codex OAuth was not retrieved. Sources
  disagree. One says OpenAI sanctions the flow for Codex tooling, one says it is plausibly
  against the terms, one quotes "coding-assistant harnesses and light AI assists, not general
  inference" (unconfirmed). One report says a rollout lets third-party requests draw on the
  plan's included Codex usage with per-app weekly caps. Anthropic, by contrast, has cut off
  third-party harnesses from Claude subscriptions. Read OpenAI's current terms before building.
- **Fit**: a Codex login gives GPT models, not the open-weight models the benchmark pins. It is
  for development and for exercising cuttlefish plumbing. It must not feed a published number,
  which ADR-0005 requires to come from a pinned provider and quantization.

## 5. Cheaper testing

1. Stop spending tokens on plumbing: cuttlefish's `KopicodeBackend` tests have a fake serve,
   kopicode has replay fixtures and `make bench-smoke` at zero tokens. Keep live runs for what
   needs a real model.
2. Expose a base URL, so kopicode can talk to a local OpenAI-compatible server (Ollama,
   llama.cpp, vLLM) or a proxy.
3. OpenRouter `:free` models: 20 requests a minute; 50 a day under $10 of lifetime purchases, 1,000
   a day after (third-party blogs quoting OpenRouter's limits page, read 2026-09-11; the roster
   rotates and one guide says `qwen/qwen3-coder:free` is going away).
4. Cheap paid models with caching working: `deepseek/deepseek-v3.2` is about $0.04 for a small
   fix today.
5. Cap every live run: `--max-turns`, `--token-budget`, and a lower
   `CUTTLEFISH_SESSION_TOKEN_BUDGET` in tests.

## 6. Cross-repo note

cuttlefish-crew's README still says "kopicode reports no cost yet, so a dollar limit cannot stop
it". That stopped being true with #189 and v0.4.0 (`usage.cost`).

## Sources

openrouter.ai/docs/guides/best-practices/prompt-caching · api-docs.deepseek.com/guides/kv_cache ·
platform.claude.com/docs/en/build-with-claude/context-editing · arxiv.org/abs/2508.21433 ·
arxiv.org/abs/2405.15793 · code.claude.com/docs/en/memory · code.claude.com/docs/en/best-practices ·
learn.chatgpt.com/docs/agent-configuration/agents-md · aider.chat/docs/repomap.html ·
aider.chat/docs/usage/conventions.html · aider.chat/2024/08/14/code-in-json.html ·
docs.cline.bot/features/slash-commands/new-task · docs.cline.bot/prompting/cline-memory-bank ·
opencode.ai/docs/rules · cursor.com/docs/context/rules · ampcode.com/news/handoff ·
google-gemini.github.io/gemini-cli/docs/cli/gemini-md.html · docs.letta.com/guides/agents/memory ·
trychroma.com/research/context-rot · anthropic.com/engineering/effective-context-engineering-for-ai-agents ·
anthropic.com/engineering/effective-harnesses-for-long-running-agents ·
langchain.com/blog/improving-deep-agents-with-harness-engineering ·
hermes-agent.nousresearch.com/docs/integrations/providers · github.com/NousResearch/hermes-agent/issues/15167 ·
github.com/jxstanford/opencode-openai-codex-auth · github.com/SWE-agent/mini-swe-agent
