# Reference solutions

One directory per task, holding whole files that overlay the task's `repo/`
tree: copy them over it and the oracle passes. An overlay rather than a patch,
so applying one needs no `patch` or `git apply` and behaves the same on every
platform.

They exist so the corpus can be re-verified rather than trusted.
`TestOracleFailsBeforeAndPassesAfter` in `internal/corpus` uses them to check
that every oracle fails on the starting tree and passes on the fixed one — the
property that decides whether a task measures anything at all.

Two things about the location, both deliberate:

- **Outside `bench/tasks/`.** The agent is pointed at `bench/tasks/<id>/repo`,
  and the answer must not be somewhere a run that stays inside its own root can
  read. Keeping it there is the runner's job; keeping it out of reach is this
  directory's.
- **The leading underscore.** The Go tool ignores directories whose names start
  with `_`, so these files are not compiled, vetted, formatted or linted as part
  of the module. They are still checked, by being compiled and run in a temp
  directory by the test above — which is the only check that matters for them.

They are not part of the corpus digest. A wrong solution makes the verification
test red; it does not change what any arm is measured against.
