# Codex backend recordings

Two real streams from `POST https://chatgpt.com/backend-api/codex/responses`, recorded on 2026-10-12 with the owner's own Codex login (KOP-194). They are byte-for-byte what the backend sent, with two edits: every `safety_identifier` value (an identifier derived from the account) is replaced with `user-redacted`, and every `prompt_cache_key` (a random UUID that secret scanners read as a key) with `cache-key-redacted`. The request headers were never recorded, and the files were checked against the login's token and account id values before being kept.

- `text_turn.sse`: model `gpt-5.6-luna`, instructions "You are a connectivity test. Answer in one word.", user text "Reply with the single word: ok". The reply is `ok`.
- `tool_call.sse`: the same model with one function tool `read_file(path)` and `tool_choice: "required"`. The reply is a `function_call` item for `read_file` with arguments `{"path":"a.txt"}`.

These are OriginRecorded in spirit. They sit outside `internal/provider/fixture` because that container is built around chat-completions bodies; a `wire` field there and replay by it are part of KOP-198. To re-record: use the script described in `docs/research/2026-10-codex-backend-verification.md`, and scrub both fields again.
