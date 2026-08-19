package journal

import "encoding/json"

// Text is a payload field whose size the schema does not bound.
//
// Tool output, diffs, model messages and provider bodies are exactly the
// content a reviewer reads to judge whether a fix was justified, so nothing
// here may clip them. A Text holds the value inline, or — once the spill
// threshold is crossed — the content address of a blob holding it. Size is the
// full length in bytes either way, so a reader knows what it is looking at
// without fetching.
//
// Callers build a Text with InlineText and never decide where it lives:
// FileJournal.Append spills anything over its threshold, and Read fetches it
// back, so a caller appends a value and reads the same value (spill.go).
//
// Size is the length of the value as it was handed over. Redaction can make the
// stored bytes shorter — the [redacted:NAME] marker in the content is what
// explains the difference — so Size is an upper bound on what a fetch returns,
// not a promise about it.
type Text struct {
	// Inline is the full value, when it was small enough to keep in the line.
	//
	// On disk exactly one of Inline and Blob is set. In a value returned by
	// Read both are: the reference says where the content is kept, Inline
	// carries it.
	Inline string `json:"inline,omitempty"`
	// Blob is the sha256 of the spilled content, hex-encoded, when it was not.
	Blob string `json:"blob,omitempty"`
	// Size is the length of the full value in bytes, spilled or not.
	Size int64 `json:"size"`
}

// InlineText holds s in the event itself.
func InlineText(s string) Text { return Text{Inline: s, Size: int64(len(s))} }

// BlobText refers to spilled content by its sha256, hex-encoded.
func BlobText(sha256Hex string, size int64) Text { return Text{Blob: sha256Hex, Size: size} }

// Spilled reports whether the value is stored as a blob rather than in the
// event line. It stays true after Read has fetched the content back, because
// where the record keeps a value is a fact about the record.
func (t Text) Spilled() bool { return t.Blob != "" }

// ProviderPin is the routing kopicode demanded of the provider.
//
// An A/B number taken without a pin is not evidence
// (docs/adr/0005-benchmark-and-ab-methodology.md §2), so the pin is recorded
// with the request rather than reconstructed from config afterwards.
type ProviderPin struct {
	// Order is provider.order — a single slug for a benchmark run.
	Order []string `json:"order"`
	// AllowFallbacks is false on every benchmark request.
	AllowFallbacks bool `json:"allow_fallbacks"`
	// Quantizations is the fixed quantization set requested.
	Quantizations []string `json:"quantizations"`
}

// TokenCounts is one request's usage, as the provider reported it.
type TokenCounts struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// Sampling is the decoding configuration for one request. Recorded per request
// because a harness arm that changes it is a different experiment.
type Sampling struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	MaxTokens   int     `json:"max_tokens"`
	// Seed is nil when none was sent.
	Seed *int64 `json:"seed,omitempty"`
}

// BuildInfo identifies the binary that produced this record.
//
// It is the third leg of an arm's identity that the harness config hash cannot
// carry. ADR-0007 decision 6 keeps the code out of the hash preimage on
// purpose, so a change to the loop that touches no configuration field leaves
// the hash identical — and two results from two different binaries would pool
// as one arm. Nothing else on the record closes that: RepoHead is the tree the
// session ran *against*, and the envelope's schema_version is the journal
// format, not the binary.
//
// The values are produced by internal/build and mapped here. This type is the
// wire contract and is deliberately its own declaration rather than the
// producer's struct, the same way AnchorVersion is a string here rather than an
// anchor.Version: the journal is a compatibility surface from the first commit
// and must not change shape when a producer package is refactored. A test in
// this package checks that every field internal/build produces has somewhere to
// land here, so the decoupling cannot become a silent drop.
type BuildInfo struct {
	// Version is the build's `git describe` output, or the module version, or
	// "unknown". Human-facing; read TreeState for the dirty bit rather than
	// parsing a suffix off this.
	Version string `json:"version"`
	// Commit is the full commit sha the binary was built from, or "unknown".
	Commit string `json:"commit"`
	// TreeState is "clean", "dirty" or "unknown" — never empty.
	//
	// This is the field the bench runner acts on. A build that is not
	// "clean" is not poolable with anything, because no commit describes what
	// was actually compiled (ADR-0007 decision 7).
	TreeState string `json:"tree_state"`
	// Source is which mechanism supplied the fields above: "ldflags",
	// "buildinfo" or "unknown".
	Source string `json:"source"`
}

// SessionStarted opens the record.
type SessionStarted struct {
	// CWD is the directory the session was started in.
	CWD string `json:"cwd"`
	// RepoHead is the git commit HEAD pointed at, or "" outside a repository.
	RepoHead string `json:"repo_head"`
	// ModelID is the provider's model identifier.
	ModelID string `json:"model_id"`
	// Provider is the pin every request in this session carries.
	Provider ProviderPin `json:"provider"`
	// HarnessConfigHash identifies the harness configuration, so two runs can
	// be compared only when they were run the same way.
	HarnessConfigHash string `json:"harness_config_hash"`
	// Build identifies the binary. Two sessions may carry the same
	// HarnessConfigHash from different builds; this is what tells them apart
	// (ADR-0007 decisions 6 and 7).
	Build BuildInfo `json:"build"`
}

func (SessionStarted) Type() Type { return TypeSessionStarted }

// UserMessage is what the human asked for.
type UserMessage struct {
	Text Text `json:"text"`
}

func (UserMessage) Type() Type { return TypeUserMessage }

// AssistantMessage is prose the model produced, tool calls excluded.
type AssistantMessage struct {
	Text Text `json:"text"`
}

func (AssistantMessage) Type() Type { return TypeAssistantMessage }

// ThinkingBlock is reasoning content, kept separate from the answer because it
// is not shown the same way and is not fed back the same way.
type ThinkingBlock struct {
	Text Text `json:"text"`
}

func (ThinkingBlock) Type() Type { return TypeThinkingBlock }

// ProviderRequest records a call to the model provider.
//
// It carries the model, the pin, the sampling parameters and the token counts —
// and no headers, no URL and no credential-shaped field of any kind.
// OPENROUTER_API_KEY travels in an Authorization header, and this type has
// nowhere to put one. The key is not scrubbed out of the journal; it is
// structurally unable to reach it.
type ProviderRequest struct {
	ModelID  string      `json:"model_id"`
	Provider ProviderPin `json:"provider"`
	Sampling Sampling    `json:"sampling"`
	// Tokens is the prompt accounting for this request **as the sender knew it
	// at send time**, which in this binary is nothing: it is written zero and
	// the authoritative figure lands on [ProviderResponse].Tokens.
	//
	// The comment here used to say "Completion is zero until the response
	// lands", which implied the prompt count was knowable when the request was
	// journaled. It is not. kopicode links no tokenizer (ADR-0001 keeps the
	// binary dependency-free, and every model on the roadmap has a different
	// vocabulary) and the provider reports prompt usage only in the reply, so
	// the only number available before sending is engine.Size.EstimatedTokens —
	// which documents itself as not a token count and must never be written
	// here. A byte estimate recorded in a field named `tokens` is fabricated
	// precision that a later reader has no way to tell from a measurement.
	//
	// The field stays, zero, because the journal is a compatibility surface
	// from the first commit and because a provider that does state a prompt
	// count up front is a thing that could exist. A reader summing token cost
	// reads [ProviderResponse].Tokens and nothing else.
	Tokens TokenCounts `json:"tokens"`
	// Attempt is 1 for the first send and increments once per repair round
	// trip — one per call the engine makes to provider.Provider.Complete, for
	// a turn or for a repair the parser asked for. It does **not** count a
	// retry the live client made *inside* one Complete call: a request the
	// client retried six times against 429s before succeeding still journals
	// one ProviderRequest with Attempt unchanged. Those retries are
	// [ProviderRetried] events instead (KAN-851) — a widened Attempt could not
	// represent them without engine.Provider growing past its one method to
	// learn about them, which SLICE-1 §Build Plan step 8 rules out on purpose.
	Attempt int `json:"attempt"`
}

func (ProviderRequest) Type() Type { return TypeProviderRequest }

// ProviderRetried records one client-internal retry: an attempt that failed
// with a retryable cause — a 429, a 5xx, or a transport failure — about to be
// followed by a backoff and another send, all inside a single call to
// provider.Provider.Complete.
//
// It exists apart from ProviderRequest because the two count different
// things, and conflating them would erase the distinction rather than clarify
// it: ProviderRequest.Attempt is the *engine's* re-send count, one per call to
// Complete, and engine.Provider's one-method interface deliberately keeps a
// retry storm *inside* that call invisible to the loop (SLICE-1 §Build Plan
// step 8 — widening the interface would let the loop start making policy out
// of retries). Without this event, a request the live client retried six
// times against 429s before succeeding journals identically to one that
// succeeded on the first try, and a flaky provider reads as a clean result —
// which is exactly the gap KAN-797's `harness` bucket exists to catch and
// could not, because nothing recorded that it happened.
type ProviderRetried struct {
	// Attempt matches the ProviderRequest.Attempt this retry happened inside:
	// which engine-level send — a turn's first, or a repair round trip —
	// triggered the client's internal backoff loop.
	Attempt int `json:"attempt"`
	// Try is 1-based: which client-internal send just failed. 1 is the first
	// send, so a Try of 3 means two retries have already happened for this
	// request.
	Try int `json:"try"`
	// OfTries is the retry policy's attempt budget for this request
	// (provider.Retry.MaxAttempts, resolved against provider.DefaultRetry).
	OfTries int `json:"of_tries"`
	// DelayMS is the backoff drawn before the next send, in milliseconds.
	// Milliseconds rather than a duration string: every other numeric field in
	// this journal is a plain number, and a duration string would be the one
	// field a reader has to parse specially rather than sum or compare.
	DelayMS int64 `json:"delay_ms"`
	// Cause summarises the failure that triggered the retry — "http 429",
	// "http 503", "transport failure" — the same classification the client's
	// own diagnostic log line uses, so the two never disagree about what
	// happened.
	Cause string `json:"cause"`
}

func (ProviderRetried) Type() Type { return TypeProviderRetried }

// ProviderResponse records what came back.
type ProviderResponse struct {
	// Body is the raw response, in full or by blob reference.
	Body Text `json:"body"`
	// Tokens is the usage the provider reported.
	Tokens TokenCounts `json:"tokens"`
	// FinishReason is the provider's stop reason, verbatim.
	FinishReason string `json:"finish_reason"`
	// ServedBy is the provider slug that actually answered. A result whose
	// ServedBy is outside the declared pin is discarded, not adjusted.
	ServedBy string `json:"served_by"`
}

func (ProviderResponse) Type() Type { return TypeProviderResponse }

// ToolCallRequested is the model's call exactly as it was emitted, before any
// parsing. Kept raw because malformed calls are the corpus the repair path is
// built and measured against.
type ToolCallRequested struct {
	CallID string `json:"call_id"`
	Raw    Text   `json:"raw"`
}

func (ToolCallRequested) Type() Type { return TypeToolCallRequested }

// ToolCallParsed is a call the harness understood.
type ToolCallParsed struct {
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	// Args is the argument object as parsed, kept verbatim rather than
	// re-serialised from a struct, so what the model sent stays inspectable.
	Args json.RawMessage `json:"args"`
	// Route is the extractor that won: "native", "fenced_json" or "xml".
	// Which route a model actually uses is a finding, not a detail.
	Route string `json:"route"`
	// ArgEncoding is how the arguments arrived before Args was normalised,
	// written as the wire string of the parse package's ArgEncoding —
	// parse.ToolCall.ArgEncoding.String().
	//
	// Args above is always a compact object, because accepting the wire's
	// double-encoded JSON *string* and unwrapping it is a leniency the harness
	// performs. This project journals its leniencies: SLICE-1 §4 requires every
	// fuzzy-mode edit be recorded "because fuzzy fallback rate per model is
	// itself a finding", and this is the same fact one layer up. A model that
	// writes OpenAI's double-encoding into a fenced block has said something
	// about what the harness prompt has to meet it with, and a normalisation
	// nobody records is a harness gain nobody can attribute.
	//
	// It is not derived at read time from ToolCallRequested.Raw, for the same
	// reason Route is not: re-deriving tells you what today's extractor thinks,
	// not what the extractor that ran thought, which is exactly backwards for an
	// A/B that varies the harness.
	//
	// Empty means "written before this field existed" and is distinct from
	// every real value. That is why it is not omitempty and why the zero value
	// is not spelled "object": parse.ArgsObject is the zero of its own type, so
	// a field that elided it would make "arguments arrived as an object" and
	// "nobody recorded this" the same bytes.
	//
	// A plain string rather than a typed value, for the reason given on
	// ToolCallRepaired.Classification: internal/parse must not import this
	// package and this package must not import it, so the coupling is a
	// documented wire contract instead of a dependency edge. The vocabulary
	// lives in internal/parse; do not re-list it here.
	ArgEncoding string `json:"arg_encoding"`
}

func (ToolCallParsed) Type() Type { return TypeToolCallParsed }

// ToolCallRepaired records one repair round trip: what was wrong, and the
// message the model was given to fix it.
type ToolCallRepaired struct {
	CallID string `json:"call_id"`
	// Attempt is 1-based and capped at the repair budget.
	Attempt int `json:"attempt"`
	// Classification is the failure kind, written as the wire string of the
	// parse package's Kind — parse.Error.Kind.String().
	//
	// Do not re-list the values here. An earlier version of this comment gave
	// a five-value illustrative vocabulary that collapsed every extraction
	// failure into one "unparseable" bucket, which is exactly the distinction
	// the repair loop is scored on: an unlabelled fence, a trailing comma and
	// a tag that never closes each earn a different sentence back to the
	// model, and a journal that records all three identically cannot measure
	// whether the wording worked. The taxonomy lives in internal/parse and
	// this field records whatever it says.
	//
	// It is a plain string rather than a typed value on purpose: internal/parse
	// must not import this package and this package must not import it, so the
	// coupling is a documented wire contract instead of a dependency edge.
	Classification string `json:"classification"`
	// Error is the message fed back to the model, verbatim, so repair-recovery
	// rate can be attributed to the wording that produced it.
	Error Text `json:"error"`
}

func (ToolCallRepaired) Type() Type { return TypeToolCallRepaired }

// ToolCallFailed records a call the harness gave up on. The turn continues with
// the failure as an observation; the bench classifier reads this as a harness
// failure once repairs were exhausted.
type ToolCallFailed struct {
	CallID string `json:"call_id"`
	// Tool is "" when the call never named a recognisable one.
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
	Detail Text   `json:"detail"`
}

func (ToolCallFailed) Type() Type { return TypeToolCallFailed }

// ToolResult is a tool's outcome, in full.
type ToolResult struct {
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	// Output is the complete output. It is never clipped; oversized output
	// spills to a blob and stays whole.
	Output Text `json:"output"`
	// ExitCode is set for tools that run a process, nil for those that do not.
	ExitCode *int `json:"exit_code,omitempty"`
	// ErrorKind is the wire form of the tool package's fault classification,
	// written as tools.FaultOf(err).String(). It is what SLICE-1 §9's
	// three-bucket classifier derives from, so the values are load-bearing
	// rather than descriptive:
	//
	//   ""          success
	//   "task"      the tool correctly reported a problem with what was asked
	//   "internal"  the harness itself broke — ADR-0006 §3 reads this as
	//               `harness`, and zero of these is the slice's acceptance bar
	//   "cancelled" the user or the runner stopped the turn
	//
	// "cancelled" belongs to neither bucket. It is not `harness`, because a
	// Ctrl-C is not a defect; and it must not be reported as "" either, because
	// §9's `model` arm is "everything else", so a silent cancellation is not
	// merely lost, it is charged to the model. Both directions launder a
	// non-failure into a number, which is the thing ADR-0006 exists to stop.
	//
	// A plain string rather than a typed value on purpose: internal/tools must
	// not import this package and this package must not import it, so the
	// coupling is a documented wire contract. Do not re-list these values
	// anywhere else — internal/tools is the source of truth, and
	// internal/tools/cancel_test.go holds the rule.
	ErrorKind string `json:"error_kind,omitempty"`
}

func (ToolResult) Type() Type { return TypeToolResult }

// EditApplied records an edit that landed, with the diff that landed.
type EditApplied struct {
	CallID string `json:"call_id"`
	Path   string `json:"path"`
	// Mode is "anchored" or "fuzzy". A session containing one fuzzy edit is
	// classified unattributed — the mode is the signal, so it is journaled on
	// every edit rather than inferred.
	Mode string `json:"mode"`
	// AnchorStart and AnchorEnd are the anchors the model referenced.
	AnchorStart string `json:"anchor_start"`
	AnchorEnd   string `json:"anchor_end"`
	// AnchorVersion is the anchor format in force. It is a model-facing
	// contract: changing it invalidates recorded provider traffic and starts a
	// new experiment series.
	AnchorVersion string `json:"anchor_version"`
	// Diff is the unified diff actually applied.
	Diff Text `json:"diff"`
}

func (EditApplied) Type() Type { return TypeEditApplied }

// EditRejected records an edit that was refused. Edits fail closed: an edit
// that could not be placed exactly is never placed approximately.
type EditRejected struct {
	CallID string `json:"call_id"`
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	// Reason is the machine-readable kind, decided by the edit tool and copied
	// here unchanged. Anchored mode produces "anchor_malformed" (the argument
	// is not an anchor at all), "anchor_drift" (a well-formed anchor matching
	// no line), "ambiguous" (an anchor matching several) and "anchor_order"
	// (both resolve, end above start); the fuzzy fallback adds "below_floor".
	//
	// They are kept apart because each implies a different recovery, and a
	// classifier that saw one string could not tell a model that cannot copy an
	// anchor back — SLICE-1 risk 2 — from a file that genuinely moved.
	Reason      string `json:"reason"`
	AnchorStart string `json:"anchor_start,omitempty"`
	AnchorEnd   string `json:"anchor_end,omitempty"`
	// Detail is what was returned to the model so it could retry against
	// reality: the region's current anchors, or the near misses.
	Detail Text `json:"detail"`
}

func (EditRejected) Type() Type { return TypeEditRejected }

// SyntaxGateRun records the post-edit language check.
type SyntaxGateRun struct {
	Path string `json:"path"`
	// Checker is the command run, or "" when none was available.
	Checker string `json:"checker"`
	// Ran is false when no checker exists for this language. Recording that
	// honestly is the point: a gate that did not run must not read as a pass.
	Ran      bool `json:"ran"`
	ExitCode int  `json:"exit_code"`
	Output   Text `json:"output"`
}

func (SyntaxGateRun) Type() Type { return TypeSyntaxGateRun }

// PermissionRequested records that the engine decided an action needs consent.
// The engine decides that asking is required; how it is asked belongs to the
// surface, and nothing about the prompt's presentation is recorded here.
type PermissionRequested struct {
	RequestID string `json:"request_id"`
	// Kind is what triggered the policy: "run_shell", "write_outside_root".
	Kind string `json:"kind"`
	Tool string `json:"tool"`
	// Detail is the thing being consented to: the command, or the path.
	Detail Text `json:"detail"`
	// Reason is why policy required asking.
	Reason string `json:"reason"`
}

func (PermissionRequested) Type() Type { return TypePermissionRequested }

// PermissionDecided records the answer.
type PermissionDecided struct {
	RequestID string `json:"request_id"`
	// Decision is "allow", "deny" or "allow_session".
	Decision string `json:"decision"`
	// Source is "user" or "policy" — a bench run's auto-approval must not be
	// indistinguishable from a human saying yes.
	Source string `json:"source"`
	Reason string `json:"reason,omitempty"`
}

func (PermissionDecided) Type() Type { return TypePermissionDecided }

// TurnSnapshot records the shadow ref holding the tree after a turn that
// touched files. The ref lives under refs/kopicode/; the user's branch, HEAD,
// index and stashes are never involved.
type TurnSnapshot struct {
	Ref string `json:"ref"`
	// Commit is the commit-tree object, Tree its tree, Parent the previous
	// snapshot's commit or "" for the first.
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
	Parent string `json:"parent,omitempty"`
}

func (TurnSnapshot) Type() Type { return TypeTurnSnapshot }

// VerificationRun records the project's own test or lint command after a
// modifying turn. A non-zero exit blocks a success report.
type VerificationRun struct {
	// Command is argv, not a shell string, so it is unambiguous on replay. It is
	// empty exactly when Source is "none".
	Command []string `json:"command"`
	// Source is "configured", "discovered", or "none" — the last being the
	// honest record that no command was configured and none was found, which is
	// written rather than skipped so that "nobody looked" cannot be mistaken for
	// "it passed". ExitCode is -1, never 0, for any run that did not conclude.
	Source   string `json:"source"`
	ExitCode int    `json:"exit_code"`
	// Skip is internal/verify's verify.Skip rendered as a string — "no_command",
	// "tool_missing", "command_broken", "timed_out" or "cancelled" — and "" when
	// the run concluded (ExitCode is 0 or positive). It is a plain string and not
	// verify.Skip itself: this package must not import internal/verify (the
	// engine journals data, packages do not journal themselves), so the field
	// carries the classification's wire form the same way Source does.
	//
	// KAN-876: before this field, ExitCode < 0 could say only that nothing
	// concluded, not *why* — so a missing toolchain, a broken command, a timeout
	// and a cancellation were indistinguishable on the record. Skip is what lets
	// internal/bench's classifier read the reason directly instead of inferring
	// a conservative catch-all from Source and ExitCode alone.
	Skip   string `json:"skip,omitempty"`
	Output Text   `json:"output"`
}

func (VerificationRun) Type() Type { return TypeVerificationRun }

// TurnCancelled records a turn stopped because its context was cancelled — a
// Ctrl-C at the REPL, or a bench runner abandoning a task.
//
// # Why it is per turn, and not a field on SessionEnded
//
// A cancelled turn is not a cancelled session, and the REPL is what makes the
// difference concrete: Ctrl-C ends the turn, the prompt comes back, the user
// types again, and the session may close twenty minutes later as "completed".
// SessionEnded records how the *session* finished, so on that path it says
// nothing about the interruption at all — and a reader looking for it finds a
// ProviderRequest with no ProviderResponse, which is an absence rather than a
// record. ADR-0002 decision 2 makes this record the only account of the
// session, so an absence is not an option: what the record does not say, nobody
// can derive.
//
// The two therefore stay apart and are told apart by type and by the envelope.
// A TurnCancelled always carries a turn of 1 or more and says one turn was
// interrupted; a SessionEnded with Reason "cancelled" always carries turn 0 and
// says the session itself did not survive it. A session where both appear was
// interrupted and stayed interrupted; one where only the first appears was
// interrupted and carried on.
//
// # Why it is enough to tell a cancellation from a crash
//
// A killed process, an out-of-space disk and a truncated file all end the
// record the same way: it simply stops. This event is the positive statement
// that makes "the record ends here" and "the turn was interrupted here"
// different bytes rather than the same silence. A reader that finds no
// TurnCancelled and no SessionEnded is looking at a record that was cut off.
type TurnCancelled struct {
	// Phase is what was in flight when the loop **first** observed the
	// cancellation, which is not the same as where the loop happened to notice:
	// a cancelled shell is seen by the tool result, and the loop's own
	// checkpoint one step later would describe the checkpoint instead. First
	// observation wins, and internal/engine is the source of truth for the
	// vocabulary:
	//
	//	"provider_stream"  the model's reply was still arriving
	//	"tool_call"        a tool was running; a shell's process group was killed
	//	"verification"     the project's own verification command was running
	//	"between_steps"    nothing was in flight — the loop reached a checkpoint
	//	                   with the context already cancelled
	//
	// It is recorded rather than derived because deriving it means reading the
	// last event before the gap and guessing, which is the inference this event
	// exists to replace. It is a plain string for the reason
	// ToolResult.ErrorKind is: internal/engine and this package share a
	// documented wire contract, not a dependency edge.
	Phase string `json:"phase"`
	// Detail is the cancellation as the loop saw it: the error that ended the
	// exchange, verbatim and still wrapped, so it ends in "context canceled" or
	// "context deadline exceeded" and the two stay distinguishable. A user's
	// Ctrl-C and a runner's timeout are different events and a record that
	// rendered both as "cancelled" would lose the one a bench operator needs.
	//
	// It is the same string SessionEnded.Detail carries when the cancellation
	// also ended the session, and deliberately so: the two are one fact, and a
	// shortened copy here would be a second account of it that could disagree.
	// It is empty when a stop settled as cancelled with no error behind it,
	// which is honest rather than invented.
	Detail Text `json:"detail"`
}

func (TurnCancelled) Type() Type { return TypeTurnCancelled }

// SessionEnded closes the record.
type SessionEnded struct {
	// Reason is "completed", "cancelled", "max_turns", "budget_exhausted",
	// "verification_failed" or "error".
	Reason string `json:"reason"`
	// ExitCode is the process exit code the surface reported.
	ExitCode int  `json:"exit_code"`
	Detail   Text `json:"detail"`
}

func (SessionEnded) Type() Type { return TypeSessionEnded }

// SessionForked marks a session as one that branched from another, rather
// than starting from nothing (KAN-940).
//
// It is written once, immediately after this session's own SessionStarted and
// immediately before the block of duplicated events it describes, so a
// reader meets the fact "what follows is a copy" before meeting the copy
// itself rather than discovering it after the fact.
//
// # Why the copy is duplicated into this journal at all
//
// The alternative — record SourceSessionID and SourceTurn here and nothing
// else, leaving a reader to open the source session's own directory to see
// what actually happened up to the fork point — was considered and rejected.
// CLAUDE.md's boundary is "one session record, and it is the journal; do not
// build a parallel transcript", and a forked session whose own file cannot
// answer "what did this session's model see before it said X" without
// chasing a second, separate directory (one that may since have been
// archived, moved, or belong to a session the reader has no access to) is
// exactly that failure wearing a foreign-key's clothes instead of a
// hand-rolled list's. A journal is meant to be the complete account of one
// session; a reference-only fork would make that true only conditionally, on
// the source still being where this event says it was. Duplication costs disk
// — bounded by how much history existed at the fork point, the same cost
// engine.Options.Resume already pays by replaying a whole prior session into
// a new process's memory — and buys a record that is honestly self-contained.
//
// # What "duplicated" does and does not mean
//
// The copied events keep every field exactly as the source wrote it,
// including CallID values, ref names under the *source's* ref namespace
// (TurnSnapshot.Ref) and Turn numbers in the envelope — they are historical
// facts about what happened, and rewriting any of them to look like they
// happened in this session would be the forgery, not the honest account.
// What is authentically new to this session is everything appended *after*
// this marker: its own SessionForked is the only event that knows it is a
// fork, and everything before it in seq order (this session's own
// SessionStarted) and everything after (the duplicated block, then this
// session's own new turns) reads exactly like an ordinary session's record to
// a reader that does not care how it started.
type SessionForked struct {
	// SourceSessionID is the session this one branched from.
	SourceSessionID string `json:"source_session_id"`
	// SourceTurn is the last turn (inclusive) of SourceSessionID's own record
	// that was copied. 0 means the fork happened before SourceSessionID ran
	// any turn at all, in which case Copied is 0 too.
	SourceTurn int `json:"source_turn"`
	// Copied is how many events immediately follow this one, verbatim from
	// SourceSessionID's own record. It exists because Turn alone cannot
	// answer "where does the copied block end": an event that happens to
	// share SourceTurn's own turn number could belong to either side of the
	// cut once this session's own turns reach that same number again (its own
	// numbering does not continue SourceSessionID's — internal/repo's
	// NewForkingSnapshotter argues why in its own doc comment) — so a reader
	// counts Copied events forward from here rather than comparing Turn
	// fields.
	Copied int `json:"copied"`
}

func (SessionForked) Type() Type { return TypeSessionForked }

// AskRequested records that the model paused the turn to ask the human a
// question (docs/adr/0009-ask-tool-contract.md decision 3).
//
// It is written before the engine calls the configured Answerer, and
// unconditionally: a turn cancelled while the question is outstanding leaves
// this event with no matching AskAnswered, which is the honest shape of what
// happened — representable, not a gap, the same reason a PermissionRequested
// can stand alone with no PermissionDecided.
type AskRequested struct {
	CallID   string `json:"call_id"`
	Question Text   `json:"question"`
	// Context is the model's optional supporting text — what it tried, why it
	// is stuck. The zero Text is a meaningful value here, not a forgotten
	// field: it means the model gave none, which is why fixtures() has to set
	// it to something rather than leaving it at its Go zero value (see
	// event_test.go's fixture for this type).
	Context Text `json:"context"`
}

func (AskRequested) Type() Type { return TypeAskRequested }

// AskAnswered records what came back and who is answerable for it
// (docs/adr/0009-ask-tool-contract.md decision 3).
type AskAnswered struct {
	CallID string `json:"call_id"`
	// Answer is the text returned to the model as this call's ToolResult.
	// Recorded again here — the same duplication EditApplied.Diff already has
	// with ToolResult.Output for an edit — because Source is not otherwise
	// attributable: without a self-sufficient event, "a human answered this"
	// and "nobody was present and the harness said so" read as the identical
	// ToolResult until someone cross-references CallID against the
	// PermissionDecided-style record this type exists to be.
	Answer Text `json:"answer"`
	// Source is "user" or "policy" — PermissionDecided's own vocabulary, for
	// the same reason: a headless run's canned refusal must never be
	// indistinguishable from a person answering.
	Source string `json:"source"`
	// Refused is true when nobody actually answered: no human present, or the
	// turn was cancelled while the question was open. Answer then carries the
	// reason shown to the model, not a person's words.
	Refused bool `json:"refused"`
}

func (AskAnswered) Type() Type { return TypeAskAnswered }
