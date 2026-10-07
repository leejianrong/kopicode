# ADR-0023: Auto mode in the REPL, and a longer never-allow list

*Accepted.* Amends [ADR-0017](0017-auto-consent-mode.md). Leaves ADR-0008, -0011 and -0018 as they were.

## Context

`auto` (ADR-0017) exists only for `serve` and `mcp`, where the mode is fixed when the session opens. A person at the REPL gets a prompt for every shell command, which is the friction the v0.3.0 feedback named: a long task that must be babysat. They also want to change their mind mid-session, in either direction. Using auto in the REPL exposes the list to a human's habits rather than an orchestrator's, and the first real use showed mistakes it did not cover: a bare `pip install` into the global Python is the one the user hit.

## Decision

1. **Two modes, no more: `default` and `auto`.** There is no plan or edit mode. `default` is today's behaviour. `auto` is ADR-0017's policy unchanged.
2. **Set at start with `--mode default|auto`, changed at any time with `/mode <name>`.** `/mode` alone says which mode is active. A switch applies from the next permission request; one in flight is unaffected. Entering auto prints a one-line statement of what it does and does not guarantee.
3. **The engine owns the switch.** `permission.Switch` holds the asking policy and the auto policy and consults one under a lock. `Options.Switchable` (with a `Consent`, and neither `Policy` nor `ConsentAuto`, nor `ReadOnly`) opens a session that can switch; `Session.SetAuto` and `Session.Auto` drive it. `Options.StartAuto` starts it in auto. A session not opened switchable cannot switch, and `SetAuto` says so. `ConsentMode` stays `ConsentInteractive`: what a person decided is stamped `user`, what auto decided is stamped `auto`, so the journal says who answered each request across a switch. No new event type: those stamps are the record.
4. **Session grants survive a switch.** "Always" grants a person gave are exact-match grants in the gate, considered answers to requests they saw; auto mode never creates one.
5. **A read-only session cannot be switchable**, for ADR-0019's reason: auto runs shell, and shell can write.
6. **The never-allow list grows**, for every auto session on every surface:
   - `pip`, `pip3` or `python -m pip` `install` outside a virtualenv, and `pip install --user` anywhere. A virtualenv is recognised by invoking it through a `bin` directory (`.venv/bin/pip`) or sourcing an `activate` earlier in the same line. Each shell call is a fresh shell, so activation cannot carry between calls.
   - `npm`, `pnpm`, `yarn` or `bun` installing globally (`-g`, `--global`, `yarn global`).
   - `git reset --hard`, `git clean -f`, `git checkout .` / `git checkout -- .` / `git restore .`.
   - `git commit` or `git push` with `--no-verify` (and `commit -n`).
   - `chmod -R 777` and its equivalents; a recursive `chmod`, `chown` or `chgrp` outside the root or on a computed target.
7. **Every one of these refusals names the alternative** (`uv add`, a project venv, `git stash`, "fix what the hook reports"). A bare refusal sends a model to the neighbouring spelling.
8. **Callers can still add entries, never remove one.** ADR-0017 decision 5 stands. The REPL accepts additions through `Options.AutoNeverAllow`, which is now valid with `Switchable`; the config file for it comes in a later change.

## Refinements after the v0.4.0 QA pass

The first cut matched only the plain spellings, and a QA agent found the obvious neighbours. The rules now also see: a global flag before the subcommand (`npm -g install`, `-gD`, `--global=true`, `--location global`), only for a subcommand that installs, so `npm test -- -g x` is not refused; `npx` and `corepack` as wrappers; a pip path counts as a virtualenv only when relative or inside the session root (`/usr/bin/pip` is the system's); `python -Im pip`; git long options by any prefix of three characters (`--har`, `--no-verif`); `git checkout/restore ./`, `git checkout -f`, `git switch --discard-changes`; and `chmod -R` modes that grant write to everyone in any spelling (`666`, `1777`, `o+w`, `a=rwx`). A `-m` message value is no longer read as an option.

Still outside the list, by design or by cost: `find -exec chmod`, `pipx`/`cargo`/`gem`/`go install`, `git stash drop`, and an `activate` that does not run (`false && . .venv/bin/activate; pip install`). The list is best-effort over a model's honest mistakes, not a defence against a model trying to get round it.

## Consequences

- **A behaviour change for existing `auto` callers**, cuttlefish among them: a session that ran `pip install` bare, `git commit --no-verify` or `git reset --hard` is now refused. Nothing on the wire changes, and the refusal text says what to do. The release notes say so.
- The list stays a tokenizer over the plain spellings. `python setup.py install`, `make install` or a script already in the tree can still do any of this; that is ADR-0017's "what this is not", unchanged.
- A flag anywhere in a git command that carries a computed argument after `reset`, `clean` and the like is refused as a possible `--hard` or `-f`. This costs the odd false positive and fails closed.
- `/mode` is a REPL command only. `serve` and `mcp` keep a mode fixed at `session.start`.
