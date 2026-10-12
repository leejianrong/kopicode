# The Codex backend, checked with one login (KOP-196)

Date: 2026-10-12. Follows `2026-10-codex-oauth-spike.md` and settles the claim ADR-0035 rested on. Method: Codex CLI's open-source code for what to call, then three small live calls with the owner's own Codex login (a Plus plan), the token passed from `auth.json` to `curl` over a pipe and never printed or stored. About 60 tokens were spent in all. Nothing here contains the token, the account id or a response id.

**Result: the claim holds. The backend speaks the Responses API only.** ADR-0035's design stands, with the corrections in §5.

## 1. What the open-source client says (primary source)

From `openai/codex`, `codex-rs/model-provider-info/src/lib.rs` and `model-provider/src/bearer_auth_provider.rs`:

- Base URL `https://chatgpt.com/backend-api/codex`.
- `wire_api = "responses"` is the only wire. The code carries an error for the old value: ``"wire_api = \"chat\"` is no longer supported``.
- Auth: `Authorization: Bearer <access_token>` and `ChatGPT-Account-ID: <account_id>` (an extra `X-OpenAI-Fedramp` for FedRAMP accounts).
- Model metadata comes from `GET {base}/models?client_version=<codex version>`, not the public `/v1/models`.

## 2. What the live calls showed

| call | result |
|---|---|
| `GET /models?client_version=0.161.0` | 200, `application/json`, about 600 KB. `{"models":[…]}` with `slug`, `context_window`, `visibility` (`list` or `hide`), `supported_reasoning_levels`, `supports_parallel_tool_calls`, `base_instructions` and many more fields. Slugs seen: `gpt-6.1-sol` (context 500000), `gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna` (272000), and hidden ones. |
| `POST /responses`, stream, text only | 200, an SSE stream. Events: `response.created`, `response.in_progress`, `response.output_item.added`, `response.content_part.added`, `response.output_text.delta`, `response.output_text.done`, `response.content_part.done`, `response.output_item.done`, `response.completed`. |
| `POST /responses`, stream, one forced function tool | 200. Events: the same frame plus `response.function_call_arguments.delta` (several) and `response.function_call_arguments.done`. The finished item is `{type:"function_call", name, call_id, arguments:"<json string>", status:"completed"}`. |
| `POST /chat/completions` | **403** with a Cloudflare HTML page. Chat completions is not served here. |

The request that worked, text only: `{"model", "instructions", "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":…}]}], "stream":true, "store":false}`. Tools are `{"type":"function","name","description","parameters","strict"}` with `tool_choice` and `parallel_tool_calls` as named. `instructions` (the system prompt) is a top-level field, not a message. A plain `curl` user agent was accepted.

## 3. Things the wire does that differ from chat completions

- **`response.completed` carries `output: []`.** The assembled items arrive earlier as `response.output_item.done`; a client must collect them there. The final event holds status, model and usage only.
- **Usage** is `input_tokens`, `output_tokens`, `total_tokens`, `input_tokens_details.cached_tokens` and `cache_write_tokens`, `output_tokens_details.reasoning_tokens`, and a per-message `attribution` block. It maps onto `provider.Usage` (`Prompt`, `Completion`, `Total`, `CacheRead`, `CacheWrite`).
- **Tool arguments** are a JSON string on the `function_call` item, delivered as deltas then a `.done`. Tool results go back as input items of another type (not checked here; the client card must record one more live exchange for that).
- **No price.** A subscription has no per-token cost. The response carries plan limits instead (§4), so cost stays absent (ADR-0021).

## 4. Rate limits arrive as headers on every response

`x-codex-plan-type` (`plus`), `x-codex-active-limit`, `x-codex-primary-used-percent` with `-window-minutes` (300) and `-reset-after-seconds`, the same for `secondary` (10080 minutes, a week), and `x-codex-credits-has-credits` / `-balance` / `-unlimited`. These are the quota, and the figures a `/usage`-style display should show. What a refused request looks like when the quota is spent was **not** triggered and is still unknown; the client card needs it recorded before it handles one. A `x-codex-turn-state` header (opaque) also came back; whether it must be echoed on the next request is not known.

## 5. Corrections to ADR-0035

- Decision 1 and the closing consequence: the Responses API is confirmed, so the CodexClient is needed and KOP-194 does not shrink.
- Decision 5: the account id goes in a `ChatGPT-Account-ID` header, not an assumed name.
- Decision 6: the model list is available from `/models` on the backend, so `--provider codex` can list real model slugs and context windows instead of passing any id blindly. Passing an unlisted id should still be allowed, with a note.
- Decision 8 is done by this document.
- Unchanged: unpinned, `codex:` in the hash, REPL and `run --print` only, no benchmark use.

## 6. Still unknown, for the client card

1. The refused-request shape when the quota is spent or the token expired mid-session (status code and body).
2. The format of a tool *result* sent back as input, and of a multi-turn history (`previous_response_id` against full replay with `store:false`; the working call used `store:false`).
3. Whether `x-codex-turn-state` has to be echoed.
4. Reasoning controls (`reasoning.effort`) and how reasoning summaries stream.
5. Whether `session_id` / `originator` headers matter for routing or abuse checks. The calls here sent neither and worked.

## 7. Follow-up (KOP-197, same day): tool results, history, sampling, reasoning

Five more small calls on the same login, recorded under `internal/provider/testdata/codex/`. They settle items 2 and 4 of §6.

| probe | result |
|---|---|
| history `[user message, function_call, function_call_output]` with the same `call_id`, `store:false` | 200. The model answered from the output ("a.txt contains: PINEAPPLE"). The call is echoed back as `{"type":"function_call","call_id","name","arguments":"<json string>"}` and the result is `{"type":"function_call_output","call_id","output":"<string>"}`. |
| history `[user, assistant message (output_text), user]` | 200, and the model used the earlier turn. Assistant history is `{"type":"message","role":"assistant","content":[{"type":"output_text","text"}]}`. |
| `temperature: 0.2` | **400** `{"detail":"Unsupported parameter: temperature"}`. |
| `previous_response_id` with `store:false` | **400** `{"detail":"Unsupported parameter: previous_response_id"}`. |
| `reasoning: {"effort":"low","summary":"auto"}` | 200. A `reasoning` output item (with an opaque `encrypted_content`, scrubbed in the recording) precedes the message; the summary streams as `response.reasoning_summary_text.delta` (with `response.reasoning_summary_part.added/done` and `response.reasoning_summary_text.done`). |

What follows for the client:

- History is always replayed in full, and no sampling parameter is sent. The client never sends `temperature`, `top_p`, `max_output_tokens` or `previous_response_id`.
- A backend refusal of a parameter is a plain 400 whose body is `{"detail": "..."}`, not an SSE error. That is the first failure shape recorded for KOP-199; the quota and expiry shapes are still unknown.
- Reasoning items are not replayed in the history. Whether dropping them degrades a long session is not measured; the live probes were one turn long.
- Still unknown: quota-spent and expired-token responses, whether `x-codex-turn-state` has to be echoed, and `session_id` / `originator` headers (none were sent and nothing failed).
