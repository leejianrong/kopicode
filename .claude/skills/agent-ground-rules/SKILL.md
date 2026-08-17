---
name: agent-ground-rules
description: Ground rules for any agent working in the kopicode repo, to be read before running commands or writing tests. Covers the git worktree isolation trap (a worktree shares .git/config, the object store and the ref store with its parent, so a stray git command from inside one corrupts the real repository), how to write git fixture tests that cannot escape, how every subprocess must name its directory and build its own environment, what to verify before reporting a card done — including which worktrees are the bench runner's and must not be swept — and why the worktree pool manager is a lifecycle tool and not a safety boundary. Use when you are a sub-agent starting a kopicode card, when a card touches git, subprocesses, or the filesystem, or when auditing whether a test suite can reach outside its temp directory.
---

# Ground rules for agents in kopicode

Read this before you run anything. It exists because an agent already broke the
repository once, and the way it broke was not obvious from the outside.

Most of what follows is about git. That is not because git is the only thing worth
being careful with, but because it is the one place where the isolation you have been
promised is thinner than it looks.

## The trap: your worktree is not a separate repository

You are working in a git worktree. It has its own directory, its own checked-out
files, and its own branch, so it feels like a private copy of the project. It isn't.

A worktree shares almost everything with the repository it came from:

| Shared with the parent | Private to your worktree |
| --- | --- |
| `.git/config` | `HEAD` |
| the object store | the index |
| `refs/heads/*`, `refs/tags/*`, `refs/remotes/*` | `refs/bisect/*` |
| `refs/stash` | `ORIG_HEAD`, `MERGE_HEAD` |
| hooks, `info/exclude`, `packed-refs` | |

Your `.git` is not a directory. It is a one-line file containing a pointer:

```
gitdir: /path/to/real/repo/.git/worktrees/<name>
```

So "run git only against your own worktree, never cd into the parent" protects HEAD
and the index, and nothing else on that list. A branch you create lands in the
parent's refs. A stash you create lands in the parent's stash. A config value you set
rewrites the parent's config.

Three commands reproduce the whole incident:

```bash
git worktree add ../wt -b feature
git -C ../wt config core.bare true      # from inside the worktree
git status                              # in the parent
fatal: this operation must be run in a work tree
```

Something in that family is what happened here. A test suite reached the real
repository and left `core.bare = true` in its `.git/config`, at which point every git
command in the main checkout failed. The same run left a `task` branch, a `refs/tags/v1`
authored by `fixture@example.invalid`, a worktree registered under `/tmp`, and a stash
that would have deleted `CLAUDE.md`, the `Makefile`, the `LICENSE` and every doc if
anyone had popped it. The next section has the precise mechanisms, both reproduced.

The card that did this was KAN-788, whose stated purpose is "never touch the user's
git state". The promise was in the prose while the test harness broke it. Assume the
same gap exists in your card until you have checked.

## Writing a git test that cannot escape

Two rules, and the second one is the one people get wrong: **every git subprocess names
its target directory, and builds its own environment.** `Dir` alone is not enough.

Start with `Dir`, because without it nothing else matters. `exec.Command("git", "init")`
with no `Dir` set runs in the test process's working directory, which is the package
directory, which is inside the real repository.

```go
// Wrong. Inherits the test process's directory.
cmd := exec.Command("git", "init")

// Necessary, but still not sufficient. See below.
cmd := exec.Command("git", "init")
cmd.Dir = dir
```

**`GIT_DIR` beats `Dir`.** This is the part that bites, because the command looks
correctly targeted and is not. If `GIT_DIR` is set in the inherited environment, git
ignores where the process is running and operates on whatever `GIT_DIR` points at:

```bash
$ cd /tmp/other-repo
$ GIT_DIR=/real/repo/.git git rev-parse --absolute-git-dir
/real/repo/.git
```

Combine that with a subcommand that defaults to "here" when given no path argument and
you get the failure that started this. `git init --bare` with no path re-initialises
whatever `GIT_DIR` names, and re-initialising an existing repository as bare sets
`core.bare = true` on it:

```bash
$ GIT_DIR=/real/repo/.git git init --bare
Reinitialized existing Git repository in /real/repo/.git/
$ git -C /real/repo config --get core.bare
true
```

That is the exact state the checkout was found in. (`git init --bare <path>` with an
explicit path is safe: the argument wins. The danger is the argumentless form.)

So build the environment you mean rather than filtering the one you were handed.
`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` and the `GIT_CONFIG*` variables are all
inherited, and any one of them will redirect a command whose `Dir` is perfectly correct.

One honest note on scope. Which command carried `GIT_DIR` into that run was never
pinned down conclusively, and the pre-push hook was suspected but does not export
`GIT_DIR` on git 2.34.1, which is what this machine runs. Do not spend time on the
archaeology. Both mechanisms above are real, both were reproduced, and the defences
below cover the whole class rather than one path through it.

**Isolate the user's global config.** A fixture that reads `~/.gitconfig` picks up
whatever the developer has set, and a test that passes for you because of your
`commit.gpgsign` is not a test. Point `HOME` at the temp directory, and set
`GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` at `/dev/null`.

**Give the fixture an identity.** `commit-tree` and `git commit` both fail without
`user.name` and `user.email`. Set them on the fixture repository rather than relying
on the machine having them, because CI has them and a contributor's laptop may not.

**Assert the isolation before you use it.** Check that the resolved git directory sits
under `t.TempDir()` before any command runs. It is about five lines and it converts a
silent repository corruption into a failed test:

```go
func assertIsolated(t *testing.T, dir string) {
    t.Helper()
    out, err := gitIn(dir, "rev-parse", "--absolute-git-dir")
    if err != nil {
        t.Fatalf("resolving git dir for %s: %v", dir, err)
    }
    if !strings.HasPrefix(out, t.TempDir()) {
        t.Fatalf("fixture git dir %s is outside the test temp dir; "+
            "a git command with no Dir set will corrupt the real repository", out)
    }
}
```

**A linked worktree in a test is created from the fixture.** If your card exercises
the `.git`-as-a-file case, and several do, `git worktree add` still has to run against
the fixture repository. Otherwise you register a worktree against the real one, which
is one of the things that went wrong here.

**Fingerprint the ambient repository in `TestMain`.** Record `core.bare`, the stash
list, the refs your fixtures author and any `/tmp` worktrees before the package runs,
compare after, and fail loudly on a difference. Pick the fields so a leak trips it but a
colleague committing in another window does not.

`internal/repo` is the worked example for all of this, and it is worth reading before
you write a git test. It also carries two guards that are structural rather than
advisory: the isolated path verifies `GIT_INDEX_FILE` is set before every snapshot, and
the read-only path refuses any index-writing subcommand, `status` included, because
`status` rewrites the index while reporting.

**These two rules are now enforced, not just documented — and they are not about git.**
`internal/arch/subprocess_test.go` parses the whole tree and fails on any subprocess that
does not assign both `Dir` and `Env`. It fails closed: a `Cmd` handed to a helper, or one
it cannot follow, counts as a violation.

Git is where the incident happened, so the failure message still names `GIT_DIR` and
`core.bare` when the program is git. The rule is the same for everything else, because
an inherited environment changes what any subprocess does without changing anything
visible in the command: `GOFLAGS` and `GOOS` change what `go build` produces,
`PYTHONPATH` and `VIRTUAL_ENV` change what `python` imports, `NODE_OPTIONS` changes what
`node` runs, and `PATH` decides which binary runs at all. The syntax gate spawns all
three. The git half was not relaxed to make room for the rest (KAN-837).

**A `Cmd` is not always a call** (KAN-853). The guard used to find subprocesses by
looking for `exec.Command` and `exec.CommandContext`, and os/exec documents a second way:
"It is also possible to construct a Cmd directly". `&exec.Cmd{Path: gitPath, Args: …}`
spawns exactly what the call spawns and inherits exactly what it inherits when `Env` is
left alone, and for a while it was the one shape that walked past every rule in the file.
It is not exotic — it is what you reach for when you want to set `SysProcAttr` or `Path`
yourself, which is what `internal/procgroup` and `internal/tools` are about. So a
composite literal of `exec.Cmd` is a site like any other, and a field written *inside* the
literal satisfies its rule exactly as an assignment to the bound variable does: both are
visible, and insisting on the assignment form would be a style rule wearing a safety
rule's failure message.

Three shapes, and the middle one does not write `exec.Cmd` in front of the brace that
builds one at all, so know them by sight:

- `cmd := &exec.Cmd{…}` — bound to a variable, so a later `cmd.Dir = …` counts too.
- `[]*exec.Cmd{{Path: p}}`, `[]exec.Cmd{{…}}`, `map[string]*exec.Cmd{"build": {…}}` —
  inside a container of `Cmd`s the element type may be elided, so the inner brace builds
  a `Cmd` with nothing in front of it. Those are found through the container's element
  type, and they are bound by both rules.
- a literal used inline, handed straight to a helper, or written positionally. There is
  no later assignment for the guard to follow and — for the positional form — no field
  names to read, so **only what is in the literal counts**, and a positional literal
  counts as setting nothing at all.

**What it deliberately does not cover.** `var cmd exec.Cmd` is a declaration and not a
composite literal, and the guard says nothing about it; there is a `want: nil` fixture
case marking exactly that boundary. `new(exec.Cmd)` is neither a call nor a literal and
is outside the same way. This is stated rather than quietly left out because a guard
whose claimed scope is wider than its real scope is worse than one that admits its limit
— but neither shape is a loophole to use. Build a `Cmd` that way and nothing in the tree
is checking you, so set `Dir` and `Env` because you decided to.

The analysis is flow-insensitive on purpose. It asks whether the enclosing function
assigns `Dir` and `Env` at all, not whether it does so before every use: ordering is
visible in review, a missing assignment is not, and the false negative that costs a
repository is the missing one.

Genuine exceptions are waived in place and need a reason on the line:

```go
//kopicode:allow-nodir: the ambient repository is the subject here, so the package
// directory is the correct target. Read-only by construction.
```

The directives are separate (`allow-nodir`, `allow-noenv`, `allow-ambientenv`) so waiving
one does not quietly waive another, and a waiver with no reason does not waive at all. A
comment that starts like a waiver and is not one — the retired `allow-git-nodir`
spelling, or a typo — fails the suite rather than silently waiving nothing. Reach for a
waiver only when the inherited directory or environment genuinely is the right one, and
say why.

**Assigning `Env` is not enough either** (KAN-845). `cmd.Env = os.Environ()` passes the
assignment rule and inherits the whole environment, `GIT_DIR` included; `cmd.Env = nil`
is os/exec's spelling for the same thing. A third check reads the value, waived by
`//kopicode:allow-ambientenv: <reason>`. It decides only what it can see soundly —
`os.Environ()`, an `append` over one, `nil`, and a local variable in the same function
assigned one of those — and deliberately says nothing about a helper's return value.

That silence is the funnel, not a gap. The four forms above are the ones written *without*
deciding anything; every other shape means somebody wrote a named function, and the name
and its doc comment are the review surface. So the rule is not a list to memorise, it is
one sentence: **every package that spawns something owns a named environment builder, and
you start from that package's builder rather than from `os.Environ()`.** `internal/repo`,
`internal/syntax`, `internal/verify`, `internal/tools`, `internal/bench` and
`internal/corpus`'s test oracle each have one — several have more than one, because the
`go` steps and the `node` steps want different things layered on the same base. Grep the
package before you write a new one; if it already spawns a subprocess, the builder exists.

**Which shape yours takes is not a style choice.** The two shapes make opposite bets, and
the code argues it both ways on purpose:

- **Secrets take an allowlist.** The cost of missing one is unbounded — a credential in a
  child process is a credential in whatever that process prints — so you name what may
  pass and everything else is gone by default. `internal/bench`'s `goQueryEnv` and
  `oracleEnv` are built this way, `internal/corpus`'s `passThrough` is the same list for
  the same reason, and so is the fixture recorder's header scrub.
- **Toolchain variables take a denylist.** An allowlist is the wrong shape for a gate
  that must still find its own toolchain: `GOMODCACHE`, `GOCACHE`, `XDG_CACHE_HOME`,
  `SSL_CERT_FILE`, `TMPDIR` and a dozen distribution-specific variables all have to
  survive or the checker does not run at all, and a gate that cannot run reports "not
  run" on every file, which is honest and useless. So you strip the credential and strip
  the variables that change *what the tool concludes* — `GOFLAGS`, `GOOS`, `PYTHONPATH`,
  `NODE_OPTIONS`, the `GIT_*` overrides — and leave the rest alone. `internal/repo`,
  `internal/syntax`, `internal/verify` and `internal/bench`'s `gitEnv` are built this way.

`internal/tools`'s `childEnv` is the one that fits neither, and it says so: `run_shell`
runs whatever the model wrote, so it needs the user's whole toolchain and an allowlist
would break real builds for no gain. It strips exactly one variable,
`OPENROUTER_API_KEY`, and its doc comment is explicit that this is not a sandbox. If your
builder is going to be that permissive, say why in the same place.

A single shared builder was considered and rejected: the two shapes cannot be collapsed
without a passthrough option, and a passthrough option is this hole with a name.

## What isolation does and does not give you

Worth being precise, because the gaps are where the accidents live.

**Write and Edit are confined to your worktree.** Those you can trust.

**Bash is not.** It runs as you, with your permissions, anywhere on the filesystem.
Nothing stops a command from writing to the parent checkout, to `$HOME`, or to `/tmp`.
Treat every path you pass to a subprocess as something you are responsible for.

**Reading outside your worktree is fine.** Checking the parent's state to verify you
did no harm is exactly the right use of it. Writing is what to avoid.

## Undoing a deliberate breakage

Every card that adds a guard has to break it on purpose, watch it fail, and put it back.
That last step is where agents have hurt themselves, and it is worth its own rules
because it is routine rather than exotic.

**Put it back with the inverse edit.** You broke it with Edit, so fix it with Edit. It is
the only method that touches exactly what you changed.

**Never `git checkout -- <paths>` to undo it.** That restores from the index, and if you
have staged nothing the index is empty, so it silently reverts the file all the way to
HEAD. On a card in progress that throws away your work, not just the breakage, and it
does it without a word. An agent lost most of two files this way.

**Do not reach for `git stash` either.** The stash is shared with the parent repository,
which is the whole subject of this page. A stash round-trip that goes well leaves no
trace; one that goes badly leaves an entry that would delete tracked files if anyone
popped it, which has already happened here once.

If you genuinely need a checkpoint before breaking something, commit it on your own
branch. You can always squash or amend afterwards, and a commit cannot be silently
reverted by a command that looks like it does something narrower.

## Before you report a card done

Run your gates, then check you left nothing behind. **Run these from inside your own
worktree, with no `-C`.** The sharing that makes this whole page necessary is what makes
the check easy: config, stashes, tags, branches and the worktree list all live in the
common directory, so reading them from your worktree reads the parent's.

```bash
git config --get core.bare   # must print false
git stash list               # must be empty
git tag -l                   # must be empty
git branch --list            # only main, feat/*, docs/*, worktree-agent-*
git worktree list            # nothing under /tmp
```

**`git worktree list` no longer means "anything unfamiliar is a leak".** `make
bench-smoke` runs the ten-task corpus and gives each task its own worktree under
`.kopicode/bench/worktrees`, so a clean run registers ten and reclaims ten, and a run that
is still going shows up here mid-flight. That directory is written by `internal/bench` and
by nothing else, which is the whole reason it is a fixed path rather than a per-run one.

**Do not sweep them, and this is the case where the reflex this page trains is wrong.**
A crashed or cancelled run leaves its checkouts behind *deliberately*, and so does
`--keep-worktrees`, which exists so you can open one and see what the agent actually did
to the tree. They are self-healing: `Worktrees.Reclaim` runs at the start of every run,
prunes the administrative entries whose directories have gone, removes what is left under
that directory, and reports both counts in the run's reclamation. Removing them by hand
destroys the post-mortem and saves nobody anything.

So read the worktree list as three categories, not one: entries under
`.kopicode/bench/worktrees` are the bench runner's and the next run reclaims them; entries
under `~/.treehouse` are leased agent checkouts and `treehouse status` says who holds
them; **anything under `/tmp` is a fixture that escaped**, which is the one this checklist
was written for and the one to report.

Two notes on why the rest is written this way. The obvious form, `git -C /path/to/parent ...`,
is refused by the harness even for reads, so a checklist built on it cannot be run.
And `git status` is deliberately absent: it is the one piece of state that is *not*
shared, so running it here tells you about your own worktree and nothing about the
parent's.

The real defence is not a checklist anybody can forget. **Fingerprint the ambient
repository in `TestMain`** so a leak fails the suite instead of waiting to be noticed.
`internal/repo` does this, and `internal/arch/subprocess_test.go` enforces the rules
statically over the whole tree, so a subprocess missing `Dir` or `Env` now fails a
test rather than a repository.

If your suite dirties any of the above, that is your bug and not an environment quirk.
Say so in your report rather than cleaning up quietly, because the next card will hit
it too.

## The rest of the ground rules

These are in `CLAUDE.md` and this is only the index. Read the "Boundaries that must
not be crossed" section in full.

- **Never `git switch main`, never cd into the parent checkout.** Branch off
  `origin/main` in your own worktree.
- **A guard is unproven until it has been seen red.** If your card adds a safety check,
  break it on purpose, capture the failure, restore it, and put the output in the PR.
  A guard nobody has watched fail may be asserting nothing.
- **`-race` and `-count=1` are already in the make targets.** Go caches test results
  and a cached pass is indistinguishable from a real one, so never run a bare
  `go test`.
- **`make xbuild` when you touch anything platform-specific.** It vets every target
  platform including your test files, which is where process groups and path handling
  break first.
- **Shell out, never link.** No CGo.
- **The engine journals; packages return data.** Do not import `internal/journal` from
  a package that produces events. Reading its payload types to check your shapes line
  up is fine.
- **Never truncate tool output.** Oversized payloads spill to blobs. Clipping the
  diagnostic output that justifies a fix is the specific failure this project is
  designed to prevent.

## We use a worktree pool manager — for lifecycle, not for safety

This section used to say we did not. That has changed, and the reason it changed is
worth stating precisely, because the old argument was correct and is still correct.

**What has not changed: `treehouse` is not a safety mechanism.** Its worktrees are
ordinary git worktrees, which was checked rather than assumed:

```bash
$ cat $(treehouse get --lease)/.git
gitdir: /path/to/repo/.git/worktrees/repo

$ git -C <leased worktree> config core.bare true
$ git -C <parent repo> config --get core.bare
true
```

Same shared config, same failure. Everything on this page still applies inside a leased
worktree. A pool manager would not have prevented the incident that caused this
document, because the problem was never how worktrees are handed out — it was a test
suite whose git subprocesses could be redirected at the real repository. **Do not treat
a lease as isolation.** `Dir` *and* `Env` on every git subprocess, the static check in
`internal/arch/subprocess_test.go`, and `TestMain` fingerprinting remain the actual
defences.

**What has changed: the lifecycle problem turned out to be real, and it is a different
problem.** Running many agents against one repo produced, in a single session:

- eleven worktrees accumulating, because a sweep could not safely run while any agent
  was live, and there was nothing marking which were live;
- ten `gh pr merge --delete-branch` failures on branches still checked out elsewhere;
- a linter defect surfaced by a *deleted* sibling worktree, whose stale entries kept
  reappearing as findings against paths that no longer existed.

Leasing addresses all three, and the safe-by-default removal addresses the accident this
page's own advice was written to avoid ("never blanket-prune while agents are running").

```bash
treehouse get --lease --lease-holder agent-<id>   # prints the path; never handed out twice
treehouse status --json                            # what is live — read before cleaning
treehouse return <path>                            # release when done
treehouse prune                                    # dry run by default; --yes to act
treehouse destroy <path>                           # dry run by default; skips risky classes
```

A leased worktree is never handed out by a later `get` and never removed by `prune` until
returned, even with no process running in it. `prune` counts a worktree stale only when
it is unleased, idle, clean and already merged. `destroy` removes only the disposable set
unless you opt in per risk class (`--include-unlanded`, `--include-in-use`,
`--include-leased`), and refuses a global sweep outright.

**The gap to know about.** A worktree the Claude Code harness created for you
(`isolation: "worktree"`, under `.claude/worktrees/`) is *not* treehouse-managed:
`treehouse status` will not list it and `treehouse prune` will not reclaim it. If you are
in one, clean-up is manual, by path, only after nothing is running — and run
`golangci-lint cache clean` afterwards, because the lint cache is keyed on package
content rather than path and a removed sibling checkout leaves entries behind.
