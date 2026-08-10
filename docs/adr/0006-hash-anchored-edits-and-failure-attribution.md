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

**7. The anchor format, settled.** Decision 1 called the format a versioned
model-facing contract and then left it unspecified. It is settled here, before any code
depends on it, because changing it later invalidates every recorded fixture.

The normative statement, implemented in `internal/anchor`:

```
anchor := first 8 hex characters of SHA-256(preimage)
preimage := len(Version) ":" Version  ⧺  field(i-1) ⧺ field(i) ⧺ field(i+1)
field(j) := "-1:"                    if j is outside the file
          := len(line[j]) ":" line[j] otherwise
Version  := "kopicode-anchor-v1"
```

Rendered by `read_file` as `<anchor> <line-number>| <content>`, right-aligning the
number to the file's width:

```
a3f21b09   1| package main
5c0e77d2   2|
9b2d4a10   3| import "fmt"
```

Lines are split on `\n` with one trailing `\r` dropped and a single trailing newline
treated as a terminator, so a file yields the same anchors on a CRLF checkout as on an
LF one and whether or not it ends in a newline. Otherwise a fixture recorded on one
platform would reject every edit on another.

**The dominant problem is duplicate lines, not the birthday bound.** Source code is
full of byte-identical lines — `}`, blanks, `\t\treturn nil` — and a hash of line
content alone gives all of them the same anchor at *any* hash length. Measured over the
Go standard library (6,433 files of at most 4,000 lines, 2.03M lines), **40.4% of all
lines share their content with another line in the same file, and in the median file it
is 33.5%.** An anchor that is ambiguous on a third of a file is not an addressing
scheme.

**One line of context either side is the fix, and the window stops there.** Hashing
`(previous, this, next)` drops ambiguity to 12.1% of lines and, more tellingly, to
**1.9% in the median file**. Wider windows keep helping — ±2 gives 5.4%, ±3 gives 2.8%
— but each doubles the *staleness blast radius*: after an edit, every anchor within the
window of the change is invalidated. At ±1 that is the edited lines plus one either
side, the tightest any window scheme allows.

That trade is decided by which failure is worse for a weak model. An ambiguity refusal
is legible — two lines visibly carry the same anchor, and the model picks a different
one. A staleness rejection on anchors the model read *moments ago*, because it edited
five lines away, is confusing and burns turns, which is exactly the retry loop
[risk 2](../SLICE-1.md#risks) is about. So the window is kept minimal and the residual
is handled by refusal.

**No window width removes the need for a refusal rule** — even ±3 leaves 2.6% of
non-trivial lines ambiguous, and only 81% of files are fully unambiguous. Since the rule
must exist regardless, extra width buys a smaller constant and no structural
simplification. **An anchor matching more than one line is refused, naming the candidate
line numbers**, consistent with decision 1's posture that ambiguity is refused rather
than guessed at.

**Eight hex characters, and the reason is not collisions.** Because the scheme fails
closed, a truncation collision is indistinguishable from a genuine duplicate and costs a
*refusal*, not a corruption — so length trades token cost against refusal rate, not
against correctness. The bar is that the collision term stay negligible against the ~12%
structural term. At 32 bits, a 2,000-line file expects 4.7 × 10⁻⁴ colliding pairs — five
orders of magnitude below it. The card's strawman of four hex characters fails not
merely on the birthday bound but because at 16 bits the collision term reaches ~30
expected pairs per 2,000-line file, the same order as the structural term, at which point
spurious refusals are common *and indistinguishable from real ones when debugging*.
Eight is then chosen over six for a reason that is not mathematical: it is the shape of a
short git SHA, which is a string models emit fluently, and that matters for
[risk 2](../SLICE-1.md#risks).

The cost is real and is stated rather than assumed, per [risk 3](../SLICE-1.md#risks).
Against Go source averaging 30.2 characters a line, line numbers alone add 16% to a
`read_file` body and the anchor adds a further **24.9%** on top. Whether eliminated retry
loops repay that is measured against a no-anchor arm, not asserted here.

**SHA-256, truncated — and honestly, not for its cryptographic strength.** This is not a
security boundary; nothing authenticates anchors, and a fabricated one earns a rejection
rather than access. It is chosen because (a) it is defined by a standard rather than by
an implementation, which matters for something recorded into fixtures and re-derived
later, and (b) every bit of its output is uniform, so truncation to any width is uniform
by construction. Cost is irrelevant at this scale: 1.3 ms for a 2,000-line file, against
a provider round trip measured in seconds.

**Versioning.** `anchor.Version` is mixed into the preimage, so a bump changes every
anchor even if the rule is otherwise unchanged — a stale fixture then fails loudly
instead of subtly. A bump obliges all of: re-recording every provider fixture (both the
rendered reads and the anchors the model wrote back change), updating the system prompt,
including `anchor.Version` in `SessionStarted`'s harness config hash, and — per
[ADR-0005](0005-benchmark-and-ab-methodology.md) — opening a new experiment series, since
results either side of a bump may not be pooled or paired.

### Alternatives rejected

- **Hash of line content alone.** Ambiguous on a third of the median file (measured
  above). Rejected on the numbers.
- **Content hash plus an occurrence ordinal for repeats** (`a3f21b09.2`). Rejected
  decisively, and not on frequency: the ordinal is positional, so inserting one `}` near
  the top of a file renumbers every later `}`. The stale anchor still *resolves* — to a
  different line. That is a **fail-open** path, and silently retargeting an edit is the
  precise defect this ADR exists to prevent. Candidates are ranked by how they fail
  before how often.
- **Mixing the line number into the hash.** Ruled out by
  [SLICE-1](../SLICE-1.md#test-plan)'s requirement that anchors be stable under unrelated
  edits: one inserted line at the top would stale every anchor below it at once.
- **A line number accepted as a tiebreaker among equal anchors.** Superficially
  attractive — the hash verifies, the number selects. But on a shifted file a repeated
  window still matches at the wrong number, so the pair validates and applies to the
  wrong region. Fail-open again. Line numbers are rendered for humans, stack traces and
  grep output, and `edit_file` ignores them.
- **Wider context windows (±2, ±3).** Rejected above: still need the refusal rule, and
  each doubling of the blast radius trades a legible failure for a confusing one.
- **`hash/maphash`.** Seeded randomly per process, so anchors would differ between runs
  and no fixture could ever replay. Named because it is the obvious "fast Go hash" to
  reach for.
- **`hash/crc32`.** Linear over GF(2), so collisions are structured and constructible,
  and truncating a linear function preserves that structure — worst exactly on the
  systematically repetitive source where ambiguity already bites.
- **`hash/fnv`.** Deterministic and adequate, but its avalanche is weakest in the
  low-order bits, which is where truncation lives, and the speed it buys is irrelevant
  against a network round trip.
- **Marking ambiguous anchors in `read_file` output.** Unnecessary: a repeated anchor is
  visibly repeated in the rendered output, and the refusal explains itself at the moment
  it matters. A marker would be a second thing to learn and a per-line token cost for a
  case that is ~2% of the median file.

## Consequences

- `read_file` output grows by one anchor per line, which costs input tokens on every
  read. Expected to be more than repaid by eliminated retry loops, but it is a real
  cost and should be measured, not assumed.
- Ambiguity refusal is a **new, visible failure mode**, and decision 7 accepts it
  deliberately: roughly 2% of lines in a median file cannot be addressed directly and the
  model must anchor on a neighbour. Anchored-mode refusal rate per model belongs in the
  journal next to fuzzy-fallback rate — it is the same kind of finding.
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
