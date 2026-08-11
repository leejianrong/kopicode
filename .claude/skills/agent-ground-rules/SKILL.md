---
name: agent-ground-rules
description: Ground rules for any agent working in the kopicode repo, to be read before running commands or writing tests. Covers the git worktree isolation trap (a worktree shares .git/config, the object store and the ref store with its parent, so a stray git command from inside one corrupts the real repository), how to write git fixture tests that cannot escape, what to verify before reporting a card done, and why we do not use a worktree pool manager for this. Use when you are a sub-agent starting a kopicode card, when a card touches git, subprocesses, or the filesystem, or when auditing whether a test suite can reach outside its temp directory.
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

That is what happened here. A test helper ran a git command with no working directory
set, it inherited the test process's directory, which was inside the real repository,
and `core.bare = true` landed in the real `.git/config`. Every git command in the main
checkout failed until someone noticed. The same run also left a `task` branch and a
stash in the real ref store, and that stash would have deleted `CLAUDE.md`, the
`Makefile`, the `LICENSE` and every doc if anyone had popped it.

The card that did this was KAN-788, whose stated purpose is "never touch the user's
git state". The promise was in the prose while the test harness broke it. Assume the
same gap exists in your card until you have checked.

## Writing a git test that cannot escape

The rule is short: **every git subprocess names its target explicitly, every time.**

`exec.Command("git", "init")` with no `Dir` set runs in the test process's working
directory, which is the package directory, which is inside the real repository. There
is no version of this that is safe, and it fails silently rather than loudly, which is
what makes it dangerous.

```go
// Wrong. Inherits the test process's directory.
cmd := exec.Command("git", "init")

// Right. Says where it runs.
cmd := exec.Command("git", "init")
cmd.Dir = dir
```

Four more things, in the order they bite:

**Clear the git environment for fixture subprocesses.** `GIT_DIR`, `GIT_WORK_TREE`,
`GIT_INDEX_FILE` and the `GIT_CONFIG*` variables are all inherited, and an inherited
`GIT_DIR` sends a correctly-targeted command at the wrong repository anyway. Build the
environment you mean rather than filtering the one you were handed.

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
is the fourth thing that went wrong here.

## What isolation does and does not give you

Worth being precise, because the gaps are where the accidents live.

**Write and Edit are confined to your worktree.** Those you can trust.

**Bash is not.** It runs as you, with your permissions, anywhere on the filesystem.
Nothing stops a command from writing to the parent checkout, to `$HOME`, or to `/tmp`.
Treat every path you pass to a subprocess as something you are responsible for.

**Reading outside your worktree is fine.** Checking the parent's state to verify you
did no harm is exactly the right use of it. Writing is what to avoid.

## Before you report a card done

Run your gates, and then check that you left nothing behind. From your worktree,
against the parent checkout, read-only:

```bash
R=/home/jianlee/projects/abang-ai/kopicode
git -C $R config --get core.bare     # must print false
git -C $R stash list                 # must be empty
git -C $R branch --list              # only main, feat/*, docs/*, worktree-agent-*
git -C $R status --porcelain         # must be empty
git -C $R worktree list              # nothing under /tmp
```

If your suite dirties any of those, that is your bug and not an environment quirk.
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

## Why we do not use a worktree pool manager

`treehouse` is on this machine and it is a reasonable tool: it keeps a pool of
pre-warmed worktrees, leases them so a cleanup sweep cannot delete one that an agent
is still using, and its `prune` and `destroy` are dry runs by default. If we ever
drive agents outside the Claude Code harness, it is worth revisiting.

It would not have prevented any of this. Its worktrees are ordinary git worktrees,
which was checked rather than assumed:

```bash
$ cat $(treehouse get --lease)/.git
gitdir: /path/to/repo/.git/worktrees/repo

$ git -C <leased worktree> config core.bare true
$ git -C <parent repo> config --get core.bare
true
```

Same shared config, same failure. The problem was never how worktrees are handed out.
It was a test running a subprocess without saying where.
