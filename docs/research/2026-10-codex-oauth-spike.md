# Codex OAuth as a provider: spike finding (KOP-177)

Follows `2026-10-harness-survey.md` §4. Date: 2026-10-12. The question: can kopicode sign in with a ChatGPT subscription the way Codex CLI does, and what would it cost to build?

**Verdict: conditional go, development and testing only, behind an explicit opt-in. Do not build until the owner accepts the terms risk below, and not before a provider abstraction exists.** It is a bigger change than ADR-0027's custom URL, because the wire protocol is probably different.

## 1. OpenAI's terms on third-party use: no primary statement found

- OpenAI's Codex auth page ([learn.chatgpt.com/docs/auth](https://learn.chatgpt.com/docs/auth), the redirect target of developers.openai.com/codex/auth) describes sign-in for Codex itself. It does not say whether other tools may use the same login. It says API keys remain the recommended default for automation.
- In the openai/codex discussion [#8338](https://github.com/openai/codex/discussions/8338) a maintainer said on 2025-12-19 that the Apache-licensed CLI may be forked, and linked the Terms of Use without addressing "Sign in with ChatGPT". On 2026-02-09 the same maintainer wrote that they are "not a lawyer", called the terms and code license "quite permissive", said OSS projects such as OpenCode are doing similar things, and suggested legal advice before selling a product. Later comments asking for official guidance had no staff reply on the page.
- The OpenCode community plugin says it is for personal use with your own Plus or Pro subscription, not for commercial services, resale or multi-user apps.

So third-party use is tolerated in practice and not authorized in writing. That is a risk for cuttlefish (unattended, multi-user) and acceptable only for a person's own interactive use. I did not read the Terms of Use text itself, which the card asked for first: the pages I could fetch do not contain it. A human should read it before any code lands.

## 2. The flow, as documented

- **Browser sign-in:** a localhost callback (default `localhost:1455`) returns an OAuth token to the CLI.
- **Device code:** `codex login --device-auth`, marked beta, and the person must enable it in their ChatGPT security settings or workspace permissions. This is the flow that fits a terminal harness.
- **Storage:** `~/.codex/auth.json` under `CODEX_HOME`, or the OS credential store; setting `cli_auth_credentials_store` is `file`, `keyring`, `auto` or `ephemeral`.
- **Refresh:** Codex refreshes tokens automatically before they expire.
- **Limits:** the page gives no usage limits. Usage draws on the plan's included credits, subject to workspace settings.

Not verified: the exact OAuth client id, endpoints and scopes. Third-party projects (a Pi extension, Pydantic AI, a Clojure provider) implement it against `auth.openai.com`; I did not read their code. Do not copy a client id from a third party without the owner's say.

## 3. Wire protocol: the likely hard part

kopicode speaks one wire, OpenAI-style `POST /chat/completions` with `stream=true` (`internal/provider/client.go`, `completionsPath`). A search result titled "codex oauth responses provider" (an Elixir project's ADR; the page itself could not be fetched, so this is unconfirmed) suggests the subscription backend is reached through the Responses API, not chat completions. If so, this is a second request and stream shape, a second tool-call format and a second recorded-fixture family, and the "one wire" assumption in the provider package stops holding. ADR-0027's `--provider-url` cannot cover it.

## 4. What it would take, if go

1. An ADR for a provider abstraction beyond OpenRouter: an interface at `internal/engine.Provider` already exists; the work is a second implementation, its tool-call extraction, its usage and cost accounting (no per-token price on a subscription), and registry entries for GPT models.
2. Credentials: read `~/.codex/auth.json` read-only first (import, like Hermes), mode checked 0600, never copied into the journal, a blob, a log or a fixture. The recorder's header allowlist (AGENTS.md) must cover the bearer token. Own device-code login and refresh only after that.
3. Surface rules: user file only, like `provider_url` (ADR-0024, ADR-0027): a repository must not be able to select it; `serve` and `mcp` get it only by process flag. A feature name in `capabilities.go`.
4. Never a benchmark arm: ADR-0005 needs a pinned provider and quantization, which a subscription route cannot give. Runs on it are named in the harness hash the way `custom:` is.

Estimate if accepted: an ADR (3), then a read-only credential import with chat-completions-compatible models only if the backend allows it (5), then a Responses-API provider (8, split before pulling). I would not start before the terms are read.

## 5. oh-my-pi claim

Not verified. Searches found Pi's `openai-codex` provider and a third-party device-login extension for Pi; nothing mentioned oh-my-pi, so the claim that it supports Codex OAuth out of the box is struck from the record until someone checks its repository.

## Sources

[learn.chatgpt.com/docs/auth](https://learn.chatgpt.com/docs/auth) · [openai/codex discussion 8338](https://github.com/openai/codex/discussions/8338) · [opencode-openai-codex-auth notice](https://numman-ali.github.io/opencode-openai-codex-auth/) · [pi-openai-codex-device-login README](https://cdn.jsdelivr.net/gh/MorseWayne/pi-openai-codex-device-login@main/README.md)
