# ADR-0018: A tokenized allowlist, `allow_commands`, for declared policy files

- **Status:** Proposed
- **Date:** 2026-10-06
- **Deciders:** Jian (leejianrong2@gmail.com)

Implements [issue #157](https://github.com/leejianrong/kopicode/issues/157). Amends
[ADR-0011](0011-unattended-invocation-policy-gate.md) decision 1; builds on the tokenizer
introduced for [ADR-0017](0017-auto-consent-mode.md).

## Context

ADR-0011's declared allowlist is exact-match on the argv `run_shell` produces, which is
always `["/bin/sh", "-c", "<the whole command line>"]`. A model never types the same line
twice: it adds a `cd /testbed &&`, a `2>&1 | head -100`, a flag. Issue #157 recorded three
phrasings of one `uv run pytest` refused in two sessions, and KAN-1404 had found the same
thing with `gh`. The reporter proposed prefix-matching the string after `sh -c`.

ADR-0008, ADR-0011 and ADR-0016 each rejected that, and the reason has not gone away:
`uv run pytest; rm -rf x` starts with `uv run pytest`. A prefix on *characters* cannot tell a
command from whatever follows it. What was missing was a way to look at the line the way a
shell does. ADR-0017 built one, to defend its never-allow list: a tokenizer that splits a
line into simple commands and fails closed on anything it cannot account for. That changes
what an allowlist can safely be.

## Decision

**1. A new optional key in the policy file, `allow_commands`**, beside the unchanged
exact-match `allow`:

```toml
root = "/abs/path/to/the/repository"
allow_commands = [["uv", "run", "pytest"], ["git", "status"], ["ls"], ["head"]]
```

At least one of the two keys is required. Both may be given. Exact-match entries keep their
meaning and are tried first.

**2. An entry is a command prefix, matched on whole tokens.** It is a command name and the
leading arguments that must follow it. `["uv","run","pytest"]` matches `uv run pytest -v` and
`/usr/bin/uv run pytest tests/`, and does not match `uv run pytestx`, `uv sync` or `uv`. The
command name is compared by basename unless the entry spells a path. A word computed at run
time (`$X`) can never stand in for a word of an entry.

**3. The line is permitted only if every command in it matches.** `run_shell`'s line is
tokenized and each simple command is checked: every segment of `;`, `&&`, `||`, `|`, `&` and
newline, every `$( )`, backtick and `<( )` substitution, every command in a subshell. So
`cd /repo && uv run pytest -v 2>&1 | head -100` is permitted by the list above and
`uv run pytest; rm -rf x`, `uv run pytest | tee f` and `ls $(make)` are not. This is the
property prefix-matching lacks, and it is the whole reason this is acceptable where that was
not.

**4. A few things are implicit, deliberately.** `cd` to a directory inside the declared `root`
needs no entry, because a line that changes directory first is the ordinary shape of a model's
command; a `cd` that leaves the root, is computed, or is bare is refused. Redirections that
write inside `root`, to `/dev/null`, or that merely duplicate a descriptor (`2>&1`) are fine;
one writing outside is refused. Shell keywords (`for`, `if`, `do`, …) and `VAR=value` prefixes
are skipped. Anything else must be listed, including the filters (`head`, `tail`, `grep`) a
model likes to pipe into.

**5. The ADR-0017 never-allow rules still apply on top.** `sudo`, `rm` outside the root, a
forced `git push`, a download piped into a shell, and a redirect outside the root are refused
even when an entry would otherwise permit the command. A declared entry can *narrow* what runs
and can never widen it past those rules, so `["git","push"]` cannot be used to force-push. A
caller that truly needs one of them writes an exact-match `allow` entry for that precise line.

**6. Entries that run other commands are refused at load time.** A wrapper (`env`, `xargs`,
`timeout`, `nice`, `find`, …), a shell (`sh`, `bash`, …), `eval`, `source`, `exec` or a
privilege escalator in an entry would match on its own words and leave the command it starts
unchecked. They are a startup usage error with a message to list the commands themselves. So
are empty entries and `cd`/`pushd`/`popd` (implicit, decision 4).

**7. It fails closed, as ADR-0017 does.** A line the tokenizer cannot account for (an
unterminated quote, a heredoc, a computed command name, excess nesting) is refused with the
reason. Only the `/bin/sh -c <line>` shape `run_shell` produces is analysed; any other argv
that is not an exact entry is refused. A shell command with no working directory is refused,
since relative paths could not be judged.

**8. Attribution is unchanged.** Decisions are `source: "policy"`: a caller declared the rule.

## What this is not

Like ADR-0017's list it judges the line the model wrote and not what the process then does:
an entry such as `["python"]` permits `python -c "…"`, `["make"]` runs whatever the Makefile
says, and `["git","log"]` permits `git log --output=<file>`. An entry is a statement that the
operator trusts that command with whatever arguments it is given; write the narrowest prefix
that serves, and use exact-match `allow` for a line that must not vary. Containment of the
running process remains the caller's job (ADR-0011 decision 4).

## Alternatives rejected

- **Prefix match on the string after `sh -c`**, as proposed in the issue. Defeated by `;`,
  `&&`, `|`, `$( )` and a newline. Rejected for the fourth time, for the original reason.
- **Docs only: tell callers to use `auto` or `remote_interactive`.** Those are the right choice
  for open-ended work, and the protocol doc says so, but they leave a caller that wants a
  *declared, closed* set (cuttlefish's imagined shape) with nothing that works.
- **Exact-argument matching inside an entry** (`["uv","run","pytest"]` permitting no extra
  flags). Reintroduces the problem: the model's `-v` and `tests/` are the variation.
- **Letting wrapper entries recurse** (`["timeout"]` permitting whatever follows if *that* is
  listed). Cleverer, and one more place to be wrong. Listing the real commands is clearer.
- **A built-in set of harmless filters** (`head`, `tail`, `wc`) allowed implicitly. A policy
  file should say everything the session may run; a hidden default is how one gets surprised.

## Consequences

- `internal/permission` gains `NewAllowlistCommands`; `NewAllowlist` is unchanged and delegates.
  `AllowlistFile` gains `AllowCommands`; `engine.Options.Policy` carries it with no new option.
- The `autoScan` that ADR-0017 introduced gains an enforcement mode, so one tokenizer and one
  set of never-allow rules serve both policies and cannot drift apart.
- `docs/kopicode-serve-protocol.md` and the policy-file doc comment describe the key.
- `cd` is tracked as a set of possible directories, so `cd sub && cd ..` is refused. That is
  conservative and rare, and the alternative (tracking subshell scope) is more machinery than
  the case earns.
