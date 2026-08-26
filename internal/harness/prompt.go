package harness

import _ "embed"

// DefaultSystemPrompt is the system prompt the `default` harness configuration
// sends, and it is a harness value rather than a detail of the loop.
//
// ADR-0007 decision 6 puts the prompt inside [Config.Hash] by digest, so the
// prose below is part of what an A/B arm varies: every number this project
// reports is relative to it, and editing it opens a new arm. That is why it
// lives here and not wherever the loop happens to assemble a request — a prompt
// written as a side effect of building the loop is an unversioned change to the
// thing being measured.
//
// # Why a file rather than a string constant
//
// The prose *is* the product of this card, so it is reviewed as prose: a diff
// over prompt_default.md shows what changed to a reader who is judging wording,
// and the text can use backticks, which a Go raw string literal cannot. It is
// still compiled in — go:embed, not a file read at run time — which is what
// ADR-0007 decision 5 requires: a harness configuration is a value in the
// binary, never something the user can edit underneath a recorded result.
//
// # What holds the prose to the code
//
// The prompt is still the only *description* of the tools the model gets: there
// is no tool catalogue on the wire, because provider.Request carries no tool
// definitions and KAN-844 owns that shape. What has changed is that the argument
// *names* are no longer the prompt's own invention. They live on the `json` tags
// of internal/engine/catalogue.go's argument structs, which engine.schemaOf
// turns into the parse.Schema the repair loop decodes a call against and quotes
// back when it rejects one. So the prompt is prose over a contract that exists,
// and prompt_test.go holds it to that contract rather than to a hand-written
// list: every tool in ToolSet has a section, in ToolSet's order; every json name
// on that tool's argument struct appears in its section as a JSON key; and every
// rejection reason internal/tools can refuse an edit with has a documented
// recovery. Something added on one side and not the other fails the suite.
//
// # What it claims about the harness, and what it does not
//
// Forced verification is built (internal/verify, wired in
// internal/engine/loop.go), so the prompt states what it costs the model: the
// project's own check runs after any reply that changed the tree, its output
// comes back only when it fails, and a prose reply while it is failing ends the
// session as a failure rather than as a success. That is the loop's actual
// behaviour and the model can act on it.
//
// The post-edit syntax gate (internal/syntax) is built too and the prompt says
// nothing about it, deliberately: its result is journaled and is never shown to
// the model, so an instruction about it would be an instruction about something
// the model cannot observe. If the gate ever feeds back, that changes.
//
//go:embed prompt_default.md
var DefaultSystemPrompt string

// NaiveSystemPrompt is [NaiveV1ConfigName]'s system prompt (KAN-1013).
//
// It is a required consequence of that configuration's other two changes,
// not an independent fourth one: the default prompt's "## Verification"
// section describes forced-verification behaviour that is simply false once
// [Verification.Forced] is off, and a prompt still claiming multi-route
// tolerance the harness no longer honours (once [Config.ParseRoutes] is
// narrowed to native calls only) would be the same problem in the other
// direction. Every tool still gets a `### <tool>` section in [Config.ToolSet]
// order with every argument shown as a JSON key — prompt_test.go holds every
// registered configuration to that contract, this one included — but the
// recovery-per-reason essay, the verification-cost paragraph and most of the
// "Working" section's elaboration are cut. That cut is the actual "thinner
// system prompt" lever KAN-1012 decided on: the same contract, far less
// guidance prose.
//
//go:embed prompt_naive.md
var NaiveSystemPrompt string

// NaiveVerifyOnlySystemPrompt is [NaiveV2ConfigName]'s system prompt (KAN-1019,
// KAN-1022).
//
// It is [DefaultSystemPrompt] with exactly one change, and that change is a
// required consequence of [NaiveV2ConfigName]'s single independent field
// ([Verification.Forced] off), not a second one: the default prompt's "##
// Verification" section, and the "Working" section's item 3 ("rather than to
// repeat the check that runs anyway"), both describe forced-verification
// behaviour that is simply false once that field is off. The replacement
// sentence for item 3 — "no check runs automatically... verify your own work"
// — is [NaiveSystemPrompt]'s own "Working" section's wording, reused rather
// than reworded, because it already says the true thing precisely and this
// prompt is not trying to say it differently.
//
// Every other section — Calling a tool, Anchors, every `### <tool>` section in
// [Config.ToolSet] order with every argument as a JSON key — is byte-identical
// to [DefaultSystemPrompt]: [Config.RepairBudget] and [Config.ParseRoutes] are
// unchanged from default in [naiveV2Config], so nothing here should read as if
// they were. prompt_test.go holds this configuration to the same tool/argument
// contract every other one is held to.
//
//go:embed prompt_naive_verify.md
var NaiveVerifyOnlySystemPrompt string

// NaiveToolsetSystemPrompt is [NaiveToolsetConfigName]'s system prompt
// (KAN-1020, KAN-1023).
//
// It is [DefaultSystemPrompt] with the `### edit_file` tool section removed —
// a required consequence of [NaiveToolsetConfigName]'s single independent
// field ([Config.ToolSet] dropping "edit_file") — plus two smaller fixes the
// removal forces: [DefaultSystemPrompt]'s `### write_file` section tells the
// model to "use `edit_file`" for a partial change, and its `### edit_file_fuzzy`
// section tells the model to "read it and use `edit_file`" once it can. Both
// point at a tool this configuration does not offer; left unchanged, either
// would send a call for a tool name the model was never given. Both now name
// `edit_file_fuzzy` instead, since it is the only edit tool this configuration
// has.
//
// What this prompt deliberately does NOT change, and why: the "## Anchors"
// section's rejection-reason list still names anchor_malformed, anchor_drift,
// ambiguous and anchor_order — all four are edit_file's own rejection
// reasons, from a tool this configuration does not offer — and the anchor
// version line is unchanged too. That is not an oversight: KAN-1020's design
// record found TestSystemPromptAnswersEveryRejectReason and
// TestSystemPromptStatesTheAnchorVersion require both, verbatim, in every
// registered configuration's prompt, unconditionally — unlike
// TestSystemPromptDocumentsEveryTool, neither test is scoped to what
// [Config.ToolSet] actually offers. Removing that prose would fail the suite
// without changing the test; keeping it is inert (nothing instructs the model
// to expect those rejections from a tool it is using) rather than misleading
// (nothing tells the model to call a tool that is not there, which is the
// distinction the write_file/edit_file_fuzzy fix above draws the other way).
// [Config.RepairBudget], [Config.ParseRoutes] and [Verification.Forced] are
// all default's values, unchanged — this configuration probes the tool-set
// axis alone.
//
//go:embed prompt_naive_toolset.md
var NaiveToolsetSystemPrompt string

// promptBudget is the largest the prompt may grow to, in bytes.
//
// Slice 1 assembles context as naive full history (docs/SLICE-1.md affordance
// E2), so the system prompt is re-sent on every turn of every task: at the
// 20-turn cap it is paid twenty times per corpus task and two hundred times per
// corpus run. The dollar cost of that is small; the cost that matters is the
// share of a cheap model's attention spent on instructions rather than on the
// repository.
//
// 8 KiB is roughly 2000 tokens, about a page and a half. The prompt as written
// is under it with less than a tenth to spare, which is the intended amount of
// room: the number is a deliberate ceiling and not a measurement, and its job is
// to make growth a decision somebody takes rather than one that accumulates. The
// next addition that does not fit is a paragraph to cut or a line in a pull
// request raising this, and either way somebody chose.
//
// It moved to 9 KiB on KAN-988. By then the 8 KiB margin had already eroded to
// five bytes — earlier additions (ask, the syntax-gate note, KAN-960's
// verification rewrite) had already spent the headroom this comment describes
// without anyone raising the ceiling — so delete_file's section, at under 350
// bytes, was the addition that could not fit rather than an unusually large
// one. Raising it restores roughly the original margin instead of trimming an
// unrelated section to make room for this one; the next addition that does not
// fit is still a paragraph to cut or a line in a pull request raising this
// again.
const promptBudget = 9 << 10
