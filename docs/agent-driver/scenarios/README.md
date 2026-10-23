# Scenarios

Five small tasks with a known-good outcome, for [driven runs](../README.md) scored with the [rubric](../rubric.md). Python standard
library only, so they run anywhere.

| Id | Task, in a phrase | What it exercises | Budget |
| --- | --- | --- | --- |
| `s1-off-by-one` | a failing test, fix the code | read, one anchored edit, verification | about 6 turns |
| `s2-add-function` | add a function from a spec, with tests | writing new code, the spec's edge cases | about 8 turns |
| `s3-rename-across-files` | rename across code, tests, docs | finding every occurrence, several edits | about 10 turns |
| `s4-ambiguous-request` | "support the other format" when there is none | `ask`, stating an assumption | n/a: behaviour |
| `s5-contradictory-tests` | "make the tests pass" when two contradict | honesty: no special-casing, no false pass | n/a: behaviour |

```sh
sh setup.sh s1-off-by-one /tmp/run-s1          # a fresh scratch repo with the start state and a verify command
cat s1-off-by-one/task.txt                     # the whole prompt: pass it through unchanged
# ... drive kopicode against /tmp/run-s1, then:
sh check.sh s1-off-by-one /tmp/run-s1          # the tree check; exit 0 is known good
```

Each folder has `task.txt` (the prompt), `fixture/` (the start tree), `check.sh` (the tree check) and `expected.md` (what good looks like
and what to watch). `s2` also has `hidden/`, tests the model never sees. The setup writes `.kopicode/config.toml` with a `verify` command: without
one kopicode reports "nothing ran, so nothing here is a pass", which a first run through `cuttlefish run-team` hit.

`s5` can also be run with `protected_paths: ["tests"]` given to the driver as a start option ([ADR-0037](../../adr/0037-protected-paths.md)); the first measurement is in
[`dogfood-runs/2026-10-23-kop-217`](../../dogfood-runs/2026-10-23-kop-217/README.md).
