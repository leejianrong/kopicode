# KOP-240: what an ignored-files fingerprint costs

Measurement for the Proposed amendment to [ADR-0042](../../adr/0042-verify-only-a-changed-tree.md). Nothing here ran a model and nothing cost money.

`fp240.go.txt` is the program (a `.txt` so it is not part of any Go package; save as `main.go` in an empty module to build it). It runs `git ls-files -z --others --ignored --exclude-standard` with `GIT_OPTIONAL_LOCKS=0`, drops `.kopicode/`, `lstat`s each file and hashes the sorted `path size mtime_ns` lines. It writes nothing to the repository.

## Cost, on two real repositories with a large `node_modules`

Five runs each, one machine (WSL2), warm file cache except run 1.

| repository | ignored files | size | run 1 | runs 2 to 5 (total, ls-files + lstat) |
|---|---|---|---|---|
| `pre-fab` (node_modules) | 45,812 | 567 MB | 957 ms | 344 to 451 ms |
| `kaya` (frontend/node_modules) | 43,506 | 280 MB | 826 ms | 266 to 341 ms |
| this repository (`bin/`, `.kopicode/`, 79 files) | 79 | | 593 ms | 443 to 595 ms (`ls-files` dominates: it walks every untracked directory) |

For comparison, the check ADR-0042 already does, `git add -A` into a throwaway index plus `write-tree`, on the same two repositories with the index already built: 0.00 to 0.01 s (first run, building the index, 0.45 s). Measured with `GIT_OBJECT_DIRECTORY` pointing at a scratch directory so nothing was written to either repository.

So on a repository with about 45,000 ignored files the fingerprint costs roughly 30 to 40 times the tree check, and a third to a half of a second per mutating turn. The cost scales with the file count, not the bytes: it reads no file content. Cold-cache cost was not measured.

## Does a test run flip it?

The scenario repository from `docs/agent-driver/scenarios/setup.sh s1-off-by-one`, which excludes `__pycache__/` in `.git/info/exclude`:

| step | ignored files in the fingerprint |
|---|---|
| fresh repository | 0 |
| after one `python3 -m unittest discover -s tests` | 3 (`shop/__pycache__/*.pyc`, `tests/__pycache__/*.pyc`): the fingerprint changed |
| after a second run, source unchanged | identical |
| after the source file was edited, then a third run | one `.pyc` changed time |

So the first test run a shell makes on an unedited tree changes the fingerprint, though nothing a person would call a change happened.

Not measured: Go and Rust build caches (outside the repository by default), a monorepo with several hundred thousand ignored files, a network filesystem.
