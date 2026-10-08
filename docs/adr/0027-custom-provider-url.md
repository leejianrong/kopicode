# ADR-0027: A custom provider URL, for local models and proxies

*Proposed.* Amends [ADR-0007](0007-model-selection-and-harness-config-shape.md) decisions 1 to 4 and [ADR-0024](0024-user-level-config.md). It does not touch [ADR-0005](0005-benchmark-and-ab-methodology.md): a custom endpoint can never anchor a benchmark number.

## Context

Live testing, above all through cuttlefish-crew, costs real money, and kopicode can only talk to OpenRouter. `provider.WithBaseURL` and `engine.Options.ProviderBaseURL` exist, but only tests and the bench recorder set them, so no person can point kopicode at a local Ollama, llama.cpp or vLLM server, or at a proxy (a Codex-backed one, KAN-1974, for example).

Three things assume OpenRouter and would go wrong if the URL were simply made settable:

- **The credential.** `OPENROUTER_API_KEY` goes in the `Authorization` header of every request. Sent to another host, it is a leak.
- **The pin.** Every request carries `provider.order`, `allow_fallbacks: false` and `quantizations`, and the client refuses a request without them (`ErrUnpinned`). Those are OpenRouter routing fields; a local server has nothing to route.
- **The registry.** An unknown model id is refused at startup (ADR-0007 decision 4). A local model is never in it.

And one thing a URL setting must not become: a way for a repository to decide where your source code and prompts are sent.

## Decision

1. **`--provider-url URL` and a `provider_url` key**, in the user config or the flag only. A repository's `.kopicode/config.toml` that sets it is refused with the user-file-only message ADR-0024 uses for `default_mode`. It is accepted by the REPL, `run --print`, `serve` and `mcp` (a process-level flag on the last two, applying to every session they open). `kopibench` does not offer it. No environment variable names the URL (ADR-0007 decision 3).
2. **The key never crosses hosts.** For any URL other than OpenRouter's, the credential is `KOPICODE_PROVIDER_API_KEY` and `OPENROUTER_API_KEY` is not read or sent. When the host is loopback (`localhost`, `127.0.0.1`, `::1`) the key is optional and no `Authorization` header is sent. The new variable is in the redaction list and is covered by the same no-leak test as the old one. A URL with embedded credentials is refused; `http` is accepted for loopback only, `https` otherwise.
3. **Any model id is accepted on a custom URL**, passed through verbatim. The harness configuration is the one `--harness` names, else the default model's. The resolved configuration is renamed `custom:<name>` and that name is in the hash preimage, so a custom endpoint can never pool with a registered arm, the same device as `declared:` in ADR-0010. The context window is unknown, so it is absent and `/context` and the context-pressure fields say so (ADR-0021: never estimated). So is cost, unless the server reports one.
4. **No pin is sent.** On a custom URL the `provider` routing object is omitted from the request body. The run is unpinned, the journal records that and the endpoint host (never a path, query or credential), and anything that reads a journal for a benchmark refuses it, as ADR-0005 requires.
5. **A feature name for clients.** `provider_url.flag` is added to `features` in `cmd/kopicode/capabilities.go`, as `consent_timeout.flag` was, so cuttlefish can ask `kopicode version --json` before passing the flag. No wire method changes.
6. **Codex OAuth (KAN-1974) is not decided here.** It would be a further credential source behind this same seam. Reading OpenAI's terms on third-party use comes first.

## Rejected

- **Setting the URL in a repository's file**: a cloned repository could send your code to its own server.
- **An environment variable for the URL**: ADR-0007 decision 3, ambient and invisible in a diff or a job log.
- **Registry rows for local models**: there are too many, they change weekly, and a row asserts a pin and a measured arm that do not exist.
- **Reusing `OPENROUTER_API_KEY` for a custom host**: the leak above.
- **A second client implementation**: the OpenAI-compatible streaming shape is the one the existing client already reads.

## Consequences

- `harness.Resolve` gains a custom-endpoint path that skips `Lookup`, builds the `custom:` configuration and sets an empty pin. `engine.Options.ProviderBaseURL` becomes reachable and `requireProviderCredential` learns the second variable.
- `provider.Client` gains a way to omit the routing object and, for loopback, the `Authorization` header.
- A model that is not tuned for will fail in ways the harness was not measured against. The documentation says that a custom endpoint is a development tool, not an arm.
- cuttlefish can run its live tests against a local or free endpoint for plumbing, and keep real arms for what needs one.
