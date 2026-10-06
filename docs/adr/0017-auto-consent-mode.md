# ADR-0017: An `auto` consent mode for serve/mcp sessions, with a hard never-allow list

- **Status:** Proposed
- **Date:** 2026-10-06
- **Deciders:** Jian (leejianrong2@gmail.com)

Implements [issue #164](https://github.com/leejianrong/kopicode/issues/164). Amends
[ADR-0016](0016-live-remote-consent-for-agent-orchestrated-sessions.md) (adds a third
`consent_mode` beside `remote_interactive` and `unattended_policy`) and leaves
[ADR-0008](0008-shell-isolation-accepted-risk.md) and
[ADR-0011](0011-unattended-invocation-policy-gate.md) as they were.

## Context

`serve` with `consent_mode: "remote_interactive"` sends every `run_shell` and every
out-of-root write to the client as a `consent.request`. That is right when someone answers
each one. Many developers run coding agents in an auto or bypass mode on their own
repository (Claude Code's auto mode, Codex's workspace-write), and for them the round trip
is pure friction: the client has to implement an allow-everything answerer of its own,
which is the version of this feature with no never-allow list at all.

ADR-0008, ADR-0011 and ADR-0016 each rejected loosening the exact-match allowlist into
prefix or structural matching, on the argument that it quietly becomes allow-everything.
This ADR does not reopen that. It adds an explicit mode instead of looser matching, and
the mode's one fixed rule is the opposite shape: allow by default *inside* a boundary,
refuse a closed list of things *no matter what*.

## Decision

**1. A third `consent_mode`, `"auto"`, declared in `session.start`.** Like the other two it
is explicit and never a default: omitting `consent_mode` is still a usage error, and the
zero `engine.ConsentMode` is still `ConsentInteractive`. No `containment_provided` is
required — see decision 6.

**2. The rule.** A `run_shell` whose working directory is inside the session root and that
matches nothing on the never-allow list is allowed without a request. A match is denied
with a reason naming the rule and saying it will not change on retry. Every write outside
the root is denied. No `consent.request` is ever sent.

**3. The built-in never-allow list is fixed in code** and cannot be removed or narrowed by
any caller, flag or file: privilege escalation (`sudo`, `doas`, `su`, `pkexec`,
`runuser`); `rm` of anything outside the root, of the root itself recursively, or
recursively of a computed target; a forced `git push` (`--force`, `-f`, `--force-with-lease`,
`--force-if-includes`, `--mirror`, `+refspec`); a download piped or substituted into a
shell; and a redirection that writes outside the root.

**4. Command lines are tokenized, never substring-matched, and the analysis fails
closed.** `run_shell` runs `/bin/sh -c <line>`, so the line is split into simple commands
(`;`, `&&`, `||`, `|`, `|&`, `&`, newline, subshells), quoting is removed, and each command
is checked. `$( )`, backticks and `<( )` are analysed as lines of their own. `sh -c '…'`
and `eval '…'` are followed (to depth 8). `env`, `nice`, `timeout`, `xargs`, `find -exec`
and similar wrappers are checked at every word offset rather than by guessing which word is
the real command. `cd` is tracked as a *set* of possible directories, so a `cd` in a
subshell cannot make a later `../..` look inside the root. A construct the tokenizer cannot
account for is denied rather than guessed: an unterminated quote, a heredoc or here-string,
a computed command name, `eval` or `sh -c` of a computed string, excess nesting or size.
This is the answer to the open question in the issue: an allowed first command must not
smuggle in a blocked one, and where that cannot be established the command is refused.

**5. Callers may add to the list, never remove from it.** `session.start` takes an optional
`never_allow` array of `"command [token ...]"` entries, matched by command basename plus
the remaining tokens in order among the arguments (a computed word matches any token).
Entries holding shell syntax, or beyond 64 entries / 200 bytes, are a usage error, as is
sending `never_allow` under any other mode, because a caller that sent a never-allow list is
relying on it.

**6. No separate "sandboxed" variant.** kopicode has no sandbox concept; ADR-0011 decision 4
makes containment the caller's job. A broader mode for a caller that acknowledges
containment would be a different trust posture and gets its own ADR if a real caller needs
it. Until then `auto` is one fixed rule, and a caller running it inside a container still
gets the never-allow list, which guards things a container does not (a forced push reaches
the network).

**7. A new attribution source, `"auto"`.** Every `PermissionDecided` the mode produces is
stamped `permission.SourceAuto`: not `user` (nobody was asked), not `policy` (no
caller-declared rule matched), not `remote` (no peer answered). It never answers
`allow_session`, so every allowed command is its own event on the record.

**8. The process-level `--policy-file` does not apply to an `auto` session.** It belongs to
`unattended_policy`. `serve` clears it for an `auto` session, and `engine.Open` refuses a
`Policy` or a `Consent` beside `ConsentAuto` rather than guessing between two answerers.

## What this is not

It judges the request the model wrote, not what the process then does. A command it allows
runs with the full authority of the user who started kopicode, and several things sit
outside what a tokenizer can see: `python -c`, `make`, a script already in the tree,
`find … -delete`, `cp`/`mv`/`tee` into another directory, `git -c alias.…`, and a download
saved to a file and executed by a second command. Closing those means executing the code,
which is a sandbox, which is out of scope here and in ADR-0008. The mode is for a developer
on their own machine and repository; the never-allow list removes the obvious catastrophes
and the cheap smuggling tricks, and the protocol documentation says so in the same words.

## Alternatives rejected

- **Substring or regex matching on the line.** Defeated by quoting, a wrapper, a
  substitution or an escape. Rejected for the reason the allowlist rejected prefix matching,
  from the other side: it looks like a guarantee and is not one.
- **A full POSIX shell parser.** More exact, but a dependency or a large in-tree parser
  against the near-zero-dependency rule, and its exactness is not needed: the tokenizer only
  has to over-approximate what runs and refuse the rest.
- **Letting the client do it** (answer every `consent.request` with allow). That is today's
  behaviour and has no never-allow list at all.
- **A caller-removable list, or a list in a file.** The point of a *hard* list is that no
  caller, however well meaning, can turn it off for a session whose model it has not read.
- **Making `auto` the default for `serve`.** ADR-0016's whole argument; unchanged.

## Consequences

- `internal/permission` gains `AutoPolicy` (`auto.go`), a tokenizer (`shellscan.go`) and
  `SourceAuto`. `internal/engine` gains `ConsentAuto`, `Options.AutoNeverAllow` and
  `ValidateAutoNeverAllow` (so `cmd/` need not import `internal/permission`).
- `docs/kopicode-serve-protocol.md` documents the mode, `never_allow`, the list and its
  limits.
- Nothing that branches on `Source` treats `"auto"` as a human answer. The bench classifier
  does not read it.
- `mcp` (ADR-0015), when it lands, reuses `serve`'s session core and so this mode with it.
