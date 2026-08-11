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
	// Tokens is the prompt accounting for this request. Completion is zero
	// until the response lands.
	Tokens TokenCounts `json:"tokens"`
	// Attempt is 1 for the first send and increments per retry, so a retry
	// storm is visible rather than inferred.
	Attempt int `json:"attempt"`
}

func (ProviderRequest) Type() Type { return TypeProviderRequest }

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
	// Reason is "anchor_drift", "below_floor" or "ambiguous".
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
	// Command is argv, not a shell string, so it is unambiguous on replay.
	Command []string `json:"command"`
	// Source is "configured" or "discovered".
	Source   string `json:"source"`
	ExitCode int    `json:"exit_code"`
	Output   Text   `json:"output"`
}

func (VerificationRun) Type() Type { return TypeVerificationRun }

// SessionEnded closes the record.
type SessionEnded struct {
	// Reason is "completed", "cancelled", "max_turns", "budget_exhausted" or
	// "error".
	Reason string `json:"reason"`
	// ExitCode is the process exit code the surface reported.
	ExitCode int  `json:"exit_code"`
	Detail   Text `json:"detail"`
}

func (SessionEnded) Type() Type { return TypeSessionEnded }
