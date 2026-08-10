# ADR-0006: Hash-anchored edits, and failure attribution that cannot launder harness bugs

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Jian (leejianrong2@gmail.com)

Supersedes the "tolerant edit tool" design sketched in the README and the first draft
of [`SLICE-1.md`](../SLICE-1.md). Extends
[ADR-0005](0005-benchmark-and-ab-methodology.md) with the attribution rule its
acceptance criteria depend on.

## Context

The README's priority-2 mechanism was an edit tool that *tolerates imprecision* —
line ranges and fuzzy anchors instead of strict exact-match string replacement,
because weak models paraphrase the text they are trying to target and exact-match then
fails, burning a turn.

Slice-1 planning found a hole in that design and could not close it. A fuzzy match that
lands **above the similarity floor but in the wrong place** applies cleanly and returns
success. It emits no error signal, so the session runs to a clean stop, the oracle
fails, and the mechanical classifier in ADR-0005 records a `model` failure. **A harness
defect is laundered into a model-capability number.**

That is worse than an ordinary bug. kopicode's whole thesis is that harness quality
moves the number; if harness defects are silently counted as model incapacity, the
number stops measuring the thing the project exists to measure. Ambiguity refusal does
not help: it catches *multiple* matches above the floor, not a *single* wrong one.

Market research on 2026-08-11 found the problem already solved better elsewhere.
**oh-my-pi's "hashline"** format has the model *point at content-hash anchors* rather
than retype the lines it wants to change; when the file has drifted the anchors diverge
and the patch is **rejected before corruption**. Their README reports a 61% output-token
reduction on one model from eliminating retry loops — a vendor number, but the mechanism
stands on its own.

That reframes the problem correctly, and better than the original framing did. The
issue was never matching *tolerance*. It is that **the model should not have to
reproduce file content at all.** Anchors sidestep the whitespace and
string-not-found failure modes instead of tolerating them, and — decisively — they
**fail closed**.

## Decision

**1. Hash-anchored edits are the primary edit mechanism.**

- `read_file` returns each line with a short content-derived anchor.
- `edit_file` takes `anchor_start`, `anchor_end` and replacement text, and never
  requires the model to reproduce the existing content.
- If any referenced anchor no longer matches the file, the edit is **rejected** with the
  current anchors for that region. A drifted file can never be silently corrupted.
- Anchors are only obtainable from a read, so an edit into a region the model was never
  shown is structurally impossible.

**2. Fuzzy-anchor matching survives only as a fallback**, for when a model cannot
produce a usable anchor. It keeps the similarity floor, the ambiguity refusal, and the
near-miss reporting — and every fuzzy-mode use is journaled as such, because fuzzy
fallback rate is a per-model harness finding worth measuring.

**3. Failure attribution is three-bucket, and biased against us.** ADR-0005's
classifier gains `unattributed`, and any failing session that used the fuzzy fallback
lands there rather than in `model`. This does not detect a misapplication; it refuses to
launder one. It will absorb some genuine model failures and make the harness look worse
than it is — the correct direction to be wrong in for a project claiming its harness
adds points. The bucket shrinking over time is itself a quality signal.

**4. A post-edit syntax gate runs after every edit.** A language-native check
(`gofmt -e`, `go build`, `node --check`, `python -m py_compile`, `tsc --noEmit`) run
immediately after an edit. Failure directly after an edit is a hard `harness` signal in
the classifier. This converts the majority of remaining misapplications from silent into
loud, and it improves the product independently of the metric.

**5. Shell out, do not link.** The syntax gate invokes language-native binaries, the
same pattern already used for `git` and for diff rendering. This is a general rule for
language tooling in kopicode.

**6. No AST-structural editing, for now.** It would solve robust region location, which
1 already solves; what it adds beyond that is semantic operations like cross-file symbol
rename, which is LSP territory and an explicit non-goal. If it is ever wanted, the path
is stdlib `go/ast` for Go and an optional external `ast-grep` binary for everything
else — **not** tree-sitter, whose Go bindings are CGo. CGo does not break shipping
prebuilt binaries, but it does cost cross-compilation without a C toolchain, breaks
`go install` for users without a compiler, and inflates build times. Writing a
multi-language parser suite in pure Go is a multi-year project that moves the benchmark
number by approximately zero.

## Consequences

- `read_file` output grows by one anchor per line, which costs input tokens on every
  read. Expected to be more than repaid by eliminated retry loops, but it is a real
  cost and should be measured, not assumed.
- The anchor scheme is a **model-facing contract**. Changing its format changes every
  prompt and invalidates recorded provider traffic used by the mock provider. Version
  it, and treat a change as an experiment-series boundary.
- If the slice-1 model cannot reliably copy anchors back, the fuzzy fallback carries
  more traffic than intended and the `unattributed` bucket grows. That outcome is
  visible by construction, which is the point.
- The hole is **bounded and visible, not closed.** A syntactically valid edit into a
  region the model did read, referencing correct anchors, can still be semantically
  wrong. No mechanism here catches that; the `unattributed` bucket and a sampled review
  of applied diffs are what keep it honest.
- Credit where due: the mechanism is adopted from oh-my-pi (MIT). Kopicode's
  implementation is independent, but the design is not original to this project and the
  docs should not imply otherwise.
