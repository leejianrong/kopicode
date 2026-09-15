# Aider-polyglot integration scoping: manifest format + corpus loader fit + licensing

- **Status:** Scoping only. No code change, no corpus change, no runner. Design
  input for a later implementation card.
- **Card:** KAN-1037 (epic 133).
- **Builds on:**
  [`standardized-benchmark-scoping.md`](standardized-benchmark-scoping.md) §2, which
  recommended a stratified Go+Python subset of Aider's polyglot benchmark as the next
  actionable benchmark card and named the one caveat this document exists to resolve:
  *"Aider's own `benchmark.py` harness is written to drive Aider specifically… Using
  the 225 exercises for kopicode means treating them as raw task data… not running
  Aider's own harness code. That's `internal/corpus`-shaped integration work (a
  manifest per exercise, possibly a sibling loader), not `internal/bench`-isolation
  work."* This document turns that one paragraph into a concrete design.
- **Grounding:** every structural claim below was read out of a real checkout of
  [`Aider-AI/polyglot-benchmark`](https://github.com/Aider-AI/polyglot-benchmark) and
  of kopicode's own `internal/corpus` package, not inferred. Exact counts: 225
  exercises total (cpp 26, **go 39**, java 47, javascript 49, **python 34**, rust 30),
  so the Go+Python pool is **73 exercises**.

## Recommendation up front

1. **No task-schema change is needed to *represent* an Aider exercise.** kopicode's
   existing `task.json` shape (id, title, statement, language, requires, oracle,
   traits, max_turns, notes) maps onto an Exercism exercise field-for-field for the
   Go and Python tracks, and each exercise's `.meta/config.json` already declares
   exactly the file roles kopicode needs to build one. A manifest can be *generated*
   from `config.json` + `.docs/instructions.md` rather than hand-authored.
2. **The corpus loader splits cleanly into two layers, and only the lower one is
   reusable as-is.** `internal/corpus`'s per-task parsing/validation
   (`readTask`/`validateTask`), its content `Digest`, and its `knownLanguages`/
   `knownRequirements` tables are correct for *any* corpus and should be reused
   verbatim. Its *corpus-level composition policy* (`MinTasks`, `minRequiresRead`,
   `minMultiFile`) is baked as package constants tuned for kopicode's own slice-1
   corpus, and an external corpus with a different composition will fail them. So the
   fit is: **reuse the task/digest layer; give the second source its own corpus-level
   policy** — a thin sibling loader, or a small refactor that makes the floors
   injectable. Recommend the refactor (below), because a second copy of the walk +
   digest + listing cross-check is exactly the drift `internal/corpus`'s own doc
   comment warns against.
3. **Two integration risks live outside the manifest and must be designed for, not
   discovered later:** *test-file integrity* (the oracle must grade against the
   pristine test, not the agent's possibly-edited copy) and *oracle choice* (grade
   Python with stdlib `unittest`, not pytest, to avoid a new requirement). Both are
   `internal/bench`/oracle concerns, both are handled by the same bidirectional
   fails-before/passes-after check the existing corpus already runs.
4. **Licensing is clear and permissive for Go+Python:** Exercism content is MIT
   (© Exercism + per-exercise contributors), Aider's harness code is Apache-2.0, and
   kopicode vendors *content, not harness*. Retain the notices; do not vendor Aider's
   `benchmark.py`. Details in §5.

## 1. What one Aider exercise actually is

Every exercise is one directory under `<lang>/exercises/practice/<slug>/`. Read from
the real repo, a Go exercise (`go/.../wordy`) and a Python exercise
(`python/.../affine-cipher`) have this shape:

```
wordy/                          affine-cipher/
  .docs/instructions.md           .docs/instructions.md
  .meta/config.json               .docs/instructions.append.md
  .meta/example.go                .meta/config.json
  .meta/gen.go                     .meta/example.py
  .meta/tests.toml                 .meta/template.j2
  cases_test.go                    .meta/tests.toml
  go.mod                           affine_cipher.py
  wordy.go   <- stub               affine_cipher_test.py
  wordy_test.go  <- tests
```

The `.meta/config.json` is the key: it names the role of every file.

```json
"files": {
  "solution":    ["wordy.go"],          // the stub the agent edits
  "test":        ["wordy_test.go"],     // the hidden test suite = the oracle
  "example":     [".meta/example.go"],  // reference solution
  "editor":      ["cases_test.go"],     // extra read-only context
  "invalidator": ["go.mod"]
}
```

An Exercism exercise is *defined by its tests* — the stub is a `panic("Please
implement …")` placeholder and the spec lives in the test file plus
`.docs/instructions.md`. That is the same "read the tree, make its tests pass" shape
`bench/tasks/` already has; it is not a SWE-bench-style unfamiliar-repo issue.

## 2. (a) The per-exercise manifest format

**No new schema.** Map each `config.json` + `instructions.md` onto the existing
`task.json` fields:

| `task.json` field | Source in the Aider exercise |
|---|---|
| `schema_version` | constant `1` |
| `id` | the exercise slug, prefixed to avoid collisions, e.g. `aider-go-wordy` |
| `title` | `config.json` `.blurb` |
| `statement` | `.docs/instructions.md` (+ `.append.md`), plus a fixed trailer naming the run command — e.g. *"Implement the stub so the tests pass. `go test ./...`."* |
| `language` | `go` / `python` (the track name; already in `knownLanguages`) |
| `requires` | `["go"]` / `["python3"]` (already in `knownRequirements`) |
| `oracle.argv` | `["go","test","./..."]` / `["python3","-m","unittest"]` |
| `oracle.env` | Go: `GOFLAGS=-mod=readonly`, `GOPROXY=off` (as the existing Go tasks set); Python: `PYTHONDONTWRITEBYTECODE=1` |
| `oracle.timeout_seconds` | a fixed cap (e.g. 120), ≤ the loader's 300 ceiling |
| `traits` | `requires_read` for all (the stub cannot be written blind; the spec is in the tree) — see the `multi_file` caveat in §3 |
| `max_turns` | a fixed budget ≤ 20 (ADR-0005 §6) |
| `notes` | provenance: upstream slug, track, `config.json` authors/contributors, source_url |

**`repo/` contents** = everything the agent should see: the `solution` stub(s), the
`test` file(s), the `editor` files, and `go.mod` — i.e. the exercise directory minus
`.meta/` and `.docs/` (whose instruction text becomes the statement). The tests are
*in* `repo/` on purpose: that is the Exercism contract and it is what makes the task
solvable without a network.

**`example.{go,py}` → the `_solutions/` overlay.** `internal/corpus`'s `SolutionDir`
convention puts a reference fix at `<corpus>/../_solutions/<id>/`, applied as an
overlay (each file replaces the same path under `repo/`). `config.json`'s `example`
file drops straight into that slot: overlay it and the oracle must pass (passes-after);
run the bare stub and it must fail (fails-before). This is not new machinery — it is
the exact check `internal/corpus/oracle_integration_test.go` already applies to every
kopicode task, and it doubles as the **admission filter**: any exercise that does not
fail-before-and-pass-after under kopicode's chosen oracle is excluded from the subset.

**Generation, not authoring.** Because every field above is mechanical, the manifest
for a subset should be produced by a small generator that reads `config.json` +
`instructions.md` and writes `task.json` + `repo/` + `_solutions/<id>/`. That keeps
the vendored subset honest (it *is* the upstream exercise) and cheap to regenerate if
the stratified selection changes. The generator is a tool, not part of the loader;
the frozen output is what the loader validates and the digest pins.

## 3. (b) Does `internal/corpus` fit, or does it need a sibling loader?

`internal/corpus.Load(dir)` already takes a directory, so a *second frozen corpus
tree* (e.g. `bench/aider-go-python/` with its own `corpus.json`, its own digest) is
loadable by the same entry point with zero changes — **for the parts that are
corpus-agnostic.** Those parts are correct for any corpus and should be reused as-is:

- **`readTask` / `validateTask`** — schema check, id/dir match, argv-not-shell-string
  (rejects shell metacharacters), `oracle.Argv[0]` ∈ `requires`, timeout bounds,
  non-empty `repo/`. All of this is exactly the discipline an external task also wants.
- **`Digest`** — a content hash over every non-`.md` file except `corpus.json`,
  length-prefixed and sorted, platform-stable. A vendored subset gets its own digest
  and therefore the same "frozen experiment series" guarantee ADR-0005 requires, at no
  cost.
- **`knownLanguages` (`go`, `python`) and `knownRequirements` (`go`, `python3`)** —
  already exactly the Go+Python subset's needs. **No change for this subset.** (The
  other four tracks would each need a `knownLanguages`/`knownRequirements` entry and a
  toolchain the bench machine is guaranteed to have — which is precisely why the
  scoping doc stratified to Go+Python, kopicode's two guaranteed toolchains. Out of
  scope here.)

What does **not** fit is the **corpus-level composition policy**, which is baked as
package constants tuned for kopicode's own slice-1 corpus:

- `MinTasks = 10` — fine; a 30–40 task subset clears it.
- `minRequiresRead = 2` — fine; every Aider exercise is `requires_read`.
- **`minMultiFile = 1` — this is the blocker.** It exists so kopicode's *own* corpus
  always exercises a multi-file fix (ADR-0006's anchored-edit story). Exercism Go/Python
  exercises are overwhelmingly single-solution-file (`config.json` `.files.solution`
  is usually one file), so a pure-Aider corpus can easily contain **zero** `multi_file`
  tasks and fail `validateCorpus` — even though it is a perfectly valid external
  corpus. The floor is a statement about *kopicode's corpus design goals*, not about
  corpus validity in general.

**Conclusion on (b):** the fit is real but not total. The task/digest/known-tables
layer is genuinely shared and should not be duplicated; the composition floors are
kopicode-corpus policy that an external source must be allowed to set for itself. Two
ways to honour both, recommendation first:

1. **Recommended — make the corpus-level floors injectable.** Extract the three
   composition constants into a small `CompositionPolicy` value that `Load` takes (or
   that a `LoadWithPolicy` takes, with today's constants as the default `Load` keeps
   using). kopicode's corpus passes today's floors; the Aider corpus passes its own
   (`minMultiFile = 0`, and whatever `MinTasks` the chosen subset size warrants).
   Everything else — walk, digest, listing cross-check, per-task validation — stays
   single-sourced. This is a ~small, well-contained change and keeps one loader.
2. **Fallback — a thin sibling loader** in a new package that calls the *exported*
   `readTask`-equivalent + `Digest` and applies its own corpus-level checks. Only
   choose this if the policy divergence grows beyond three constants; today it would
   duplicate the walk and the listing cross-check, which is the drift
   `internal/corpus`'s doc comment explicitly argues against.

Either way, **the per-task manifest and the digest-freeze need no new concepts** — the
only genuinely corpus-specific thing is *which composition a given corpus is allowed to
have*, and that is one small policy object, not a second schema.

## 4. Two risks that live outside the manifest

**Test-file integrity (must-fix).** The oracle runs the *agent-mutated* tree's tests.
Nothing structurally stops a model from editing the test file to pass trivially, and an
Aider exercise makes the test a large, obvious, named target (`wordy_test.go`). Aider's
own harness restores the test file from a pristine copy before grading; kopicode must
do the same: before running the oracle, restore `config.json`'s `files.test` (and the
`editor` cases file, for Go) from a pristine copy kept out of `repo/`. This is an
`internal/bench` behaviour, not a corpus-schema concern, but it must be scoped into the
implementation card. (Worth a separate note: this is arguably a latent gap in the
*existing* corpus runner too — kopicode's own tasks rely on the statement asking for a
source fix rather than on structural protection. Cheap to check while implementing
this.)

**Oracle choice for Python (grade with stdlib `unittest`).** Aider grades with pytest;
kopicode should not add pytest to `knownRequirements`. Spot-checking the 34 Python
exercises, the test files import stdlib `unittest` (36 `unittest` imports across the
set; no `numpy`/`pytest` imports found), so `python3 -m unittest` grades them with the
standard library alone. Any exercise that relies on a pytest-only feature simply fails
the fails-before/passes-after admission filter (§2) under the stdlib oracle and is
dropped from the subset — the determinism discipline is self-enforcing, no new
dependency. Go exercises are cleaner still: **0 of 39** `go.mod` files declare an
external `require`, so `GOPROXY=off` grading is guaranteed to work.

## 5. (c) Licensing

- **Exercise content is Exercism's, under a permissive licence.** The polyglot repo's
  README states: *"All exercise content is copyright © Exercism… used in accordance
  with Exercism's open source licenses,"* and points at the upstream track repos
  (`exercism/go`, `exercism/python`, …). Those tracks are **MIT**. A per-exercise
  `LICENSE` (MIT, "Copyright (c) Exercism") ships alongside every JavaScript exercise
  in the polyglot repo and is the same licence the Go/Python track repos carry
  upstream. **Finding to note:** the polyglot repo does **not** ship a per-exercise
  `LICENSE` for the Go/Python trees (0 of 39 Go, 0 of 34 Python) and has **no
  root `LICENSE` file at all** — so a vendoring step must trace the licence to the
  upstream track repo rather than copy a file that isn't there.
- **Aider's harness is Apache-2.0** — but kopicode vendors *content, not harness*, and
  the whole point of KAN-1037 is that Aider's `benchmark.py` is not used. So Apache-2.0
  governs code kopicode will not import; it is not a licence obligation on the vendored
  tasks.
- **What vendoring must do:** for each vendored exercise, keep the attribution
  (`config.json`'s `authors`/`contributors`, the source URL) in the task's `notes`,
  and include the MIT notice (© Exercism) for the Go and Python tracks — pulled from
  the upstream track repo, since the polyglot repo omits it for those trees. MIT is
  compatible with kopicode's own licence and permits redistribution of a modified
  (manifest-wrapped) subset with the notice retained. No copyleft, no source-disclosure
  obligation.

## 6. Next steps (out of scope here, listed for the epic)

- An implementation card for the generator + a first stratified Go+Python subset
  (say 15–20 each; the scoping doc's suggested cut), producing a frozen
  `bench/aider-go-python/` corpus with its own `corpus.json`, digest and
  `_solutions/`.
- The small `internal/corpus` composition-policy change from §3 (recommendation 1).
- The `internal/bench` test-file-restore step from §4.
- **KAN-1038/1039 (running the subset against the pinned arm) remain out of scope and
  cost money** — not touched by any card above.
