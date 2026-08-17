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
const promptBudget = 8 << 10
