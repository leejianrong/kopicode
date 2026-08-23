package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/leejianrong/kopicode/internal/engine"
)

// `kopicode run --print` — the headless surface, docs/SLICE-1.md build step 14
// and affordance U2.
//
// # It is a projection, not a transcript
//
// Every line but the first is one [engine.Event], and an engine.Event is a
// journal payload's fields copied across at the moment the record accepted them
// — the append happens first and only what was recorded is announced. So this
// file accumulates nothing, holds no session state, and has no way to print
// something the journal does not hold. That is ADR-0002 decision 2 and the
// sibei-flow ADR-0013 lesson CLAUDE.md cites, and it is why the projection below
// is field-for-field rather than a second vocabulary: a translation table would
// be the place the two accounts drift.
//
// Deltas are the one thing on the engine's stream that is deliberately dropped.
// An [engine.Event] carrying [engine.EventDelta] has Seq zero and says so in its
// own doc — it is a fragment of a reply that is still arriving, not a thing the
// record holds. The REPL prints them because a human wants to watch; a
// journal-derived JSON stream must not, because the AssistantMessage that
// follows is the recorded text and emitting both would put the same words on the
// stream twice with only one of them in the record.
//
// # The schema
//
// Newline-delimited JSON, one object per line, `kind` on every line.
//
// **Line 1 is the stream header** and is the only line that is not an event:
//
//	{"kind":"stream","schema":1,"record":"/repo/.kopicode/sessions/<id>"}
//
// `schema` is [schemaVersion] and moves when the meaning of a field changes.
// `record` is where the journal for this session lives — the one thing a surface
// knows that the record cannot say about itself, and the same line the REPL
// prints as a notice.
//
// **Every other line is an event**, and its fields are [engine.Event]'s, which
// are in turn the journal payload field names:
//
//	{"kind":"session_started","seq":1,"detail":"<session id>","text":"<model id>","reason":"<harness hash>"}
//	{"kind":"user_message","seq":2,"turn":1,"text":"fix the failing test","size":20}
//	{"kind":"tool_call_parsed","seq":5,"turn":1,"tool":"write_file","detail":"{\"path\": ...}","reason":"native"}
//	{"kind":"turn_cancelled","seq":8,"turn":1,"text":"context canceled","reason":"provider_stream"}
//	{"kind":"session_ended","seq":9,"reason":"completed","exit_code":0}
//
// Zero fields are omitted, so a line carries only what its kind populates. Which
// fields a kind populates is [engine.Event]'s documentation and not repeated
// here — repeating it is how the two would come to disagree.
//
// **A `turn_cancelled` line is the one that says a turn was interrupted.** It
// carries a turn of 1 or more and `reason` is the phase — what was in flight
// when the loop first saw the cancelled context, from journal.TurnCancelled. A
// consumer must not read "the stream stopped" as a cancellation: a killed
// process and a truncated pipe stop the stream too, and this line is what tells
// the three apart. A cancellation that also ended the *session* is followed by
// `session_ended` with `"reason":"cancelled"` and turn 0; one the user typed
// through in the REPL is not.
//
// Three fields need stating because their zero value is meaningful and omission
// would therefore lie:
//
//   - `exit_code` is present only when the event has one. A missing one means
//     the event carries no exit code, never zero-and-therefore-fine.
//   - `ran` is present only on `syntax_gate`, where `false` is a real answer: a
//     gate that did not run must not read as a pass (ADR-0006 §4).
//   - `size` is the size the *record* holds, not the length of `text`. Content
//     over the journal's spill threshold lives in a blob, so a large event
//     arrives with an empty `text` and a real `size` — nothing is truncated
//     here, because nothing is carried here.
//
// **The exit code is on the stream.** The last line is `session_ended`, and its
// `exit_code` is the code the process returns; both come from the same
// [engine.Stop], so a consumer that reads the stream never has to guess what
// happened from a number. The exception is a failure before the record exists —
// an unknown model, a missing credential — where stdout is empty by design and
// the reason is on stderr.
//
// # Compatibility
//
// The schema is versioned from its first commit because a headless surface is a
// compatibility surface the moment anything reads it, and internal/bench is the
// obvious next reader. Adding a field is not a version change; changing what an
// existing one means is.

// schemaVersion is the version of the stream this binary writes. Bump it when
// the meaning of a field changes, not when one is added.
const schemaVersion = 1

// kindStream names the header line — the only line in the stream that is not an
// event. It is a value the journal has no type for, so it cannot collide with a
// kind [engine.EventKind] produces.
const kindStream = "stream"

// runPrint is `kopicode run --print`.
//
// The order is ADR-0007 decision 4's: everything that can be refused from the
// command line alone is refused before the working directory is even looked at,
// so an invocation that will not run leaves no journal, no lock and no
// half-written record.
func runPrint(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kopicode run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	overrides := engine.BindSelectionFlags(fs)
	asJSON := fs.Bool("print", false, "emit the session record as JSON on stdout")
	debug := fs.Bool("debug", false, "engine diagnostics on stderr")
	// --resume makes sense here for the reason it does on the REPL: a script
	// picking up a `run --print` invocation that was interrupted — killed,
	// timed out, the process's own machine rebooted — wants to continue
	// that session's record rather than start a disconnected new one, and
	// KAN-941's `kopicode sessions --json` is what finds the id to pass.
	// It costs this surface nothing new to support: Options.Resume replays
	// history into the turn loop before the first new request is built, the
	// same plumbing session() already exercises for the REPL, and it does
	// not touch the working tree (see Options.Resume's own doc comment) —
	// exactly the property a headless surface with nobody watching needs.
	resume := fs.String("resume", "", "resume an existing session by id, instead of starting a new one")
	// --fork is refused outright rather than wired up. Forking wipes every
	// file directly under the working tree except .git and .kopicode
	// before restoring the source session's tree — see
	// internal/engine/fork.go's "Why the tree is restored automatically" —
	// and the REPL surface's own --fork handling (main.go's confirmFork)
	// exists because that needs an explicit, typed, human confirmation
	// before it happens. A headless invocation has no human to ask: KAN-885
	// already settled the general version of this question for a
	// model-authored tool call — leaving ConsentUnattended's answerer nil,
	// below, so engine.Open's own permission.NewUnattendedDeny refuses — by
	// refusing rather than approving with nobody watching, and a
	// human-authored --fork flag
	// is the same shape of question with a much bigger blast radius, so it
	// gets the same answer. This is a flag this binary parses and refuses,
	// not a flag it silently ignores: a caller who typed --fork here is
	// told why, in time to use the REPL instead, rather than reading in
	// silence that nothing happened.
	fork := fs.String("fork", "", "not supported here; forking wipes the working tree and needs a "+
		"human's typed confirmation, which this headless surface has nobody to ask for — use `kopicode "+
		"repl --fork` instead")
	// --policy-file is ADR-0011's opt-in: with nothing passed, this surface's
	// default is exactly what it was before this flag existed —
	// denyHeadless's refuse-everything policy, wired below through
	// engine.ConsentUnattended with no Options.Policy set. A caller that
	// passes this flag is an orchestrator like cuttlefish-agent stating, up
	// front and in a file it authored, the closed set of shell commands this
	// invocation may run and the root writes outside the repo root are
	// confined to — see internal/permission/allowlist_file.go's own doc
	// comment for the file's grammar. This flag exists only here: ADR-0011
	// decision 5 and ADR-0008 keep the REPL's single-trusted-developer,
	// always-interactive mode exclusive, and a human at a terminal has no use
	// for a policy file answering questions on their behalf.
	policyFile := fs.String("policy-file", "", "load a declared-allowlist policy file (ADR-0011) and let "+
		"it answer shell/write consent instead of refusing everything; unset means the existing default, "+
		"refuse everything")
	if err := fs.Parse(args); err != nil {
		// flag has already printed the error and the usage.
		return exitUsage
	}
	setupLogging(*debug, stderr)

	if *fork != "" {
		say(stderr, "kopicode: --fork is not supported on `run --print`: it would wipe the working "+
			"tree with no human present to confirm that. Use `kopicode repl --fork %s` instead.\n", *fork)
		return exitUsage
	}

	// --print is required rather than assumed. It is the only output this
	// command has today, and defaulting to it would spend the flag's name on
	// nothing — leaving a later second format no way to be asked for that does
	// not silently change what the existing spelling produces.
	if !*asJSON {
		say(stderr, "kopicode: `run` needs --print; it is the only headless output there is "+
			"(docs/SLICE-1.md build step 14)\n")
		return exitUsage
	}

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		say(stderr, "kopicode: `run --print` needs a task, for example: "+
			"kopicode run --print \"make the failing test pass\"\n")
		return exitUsage
	}

	dir, err := os.Getwd()
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		return exitHarness
	}

	// Before the journal is opened, before anything is created: an unknown model
	// is a configuration failure wearing a network failure's clothes (ADR-0007
	// decision 4).
	selection, err := engine.ResolveSelection(dir, *overrides)
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		if engine.IsSelectionUsageError(err) {
			return exitUsage
		}
		return exitHarness
	}

	opts := engine.Options{Dir: dir, Selection: selection}
	if *resume != "" {
		opts.SessionID = *resume
		opts.Resume = true
	}

	// Loaded here, alongside --model/--harness resolution above and for the
	// identical reason (ADR-0007 decision 4's ordering): a malformed
	// --policy-file is the caller's own mistake, refused before the working
	// directory is locked, the journal is opened, or anything is billed.
	if *policyFile != "" {
		pf, err := engine.LoadPolicyFile(*policyFile)
		if err != nil {
			say(stderr, "kopicode: %v\n", err)
			return exitUsage
		}
		opts.Policy = &pf
	}

	// context.Background() is built here rather than inside headless, so that
	// the boundary a test can substitute a cancellable context at is this
	// call and not a line buried inside the function it calls (KAN-904):
	// headless takes a context instead of manufacturing its own, the same
	// discipline main.go's session already holds for the REPL path.
	return headless(context.Background(), prompt, stdout, stderr, opts)
}

// headless runs one exchange and writes the record to stdout as JSON.
//
// It takes the options rather than building them, for the reason [session] does:
// a test supplies a directory and a provider endpoint, and everything about
// *how* a session is assembled stays the engine's.
//
// ctx is the caller's — runPrint's context.Background() in production, a
// cancellable one in a test that drives a real cancellation end to end
// (KAN-904, mirroring internal/engine's TestAMidStreamCancellationIsOnTheRecordOnDisk
// for this surface). headless used to build its own context.Background()
// internally, which meant the --print side of cancellation could only be
// tested at the projection level; there was no seam to cancel a real run
// through.
//
// The exit code comes from the [engine.Stop] the exchange settled on, through
// engine.Stop.ExitCode and nothing else. There is no second table here — exit.go
// says why.
func headless(ctx context.Context, prompt string, stdout, stderr io.Writer, opts engine.Options) int {
	out := newEmitter(stdout)
	opts.Events = out.observe
	// Consent is left nil: nobody is at a terminal here (KAN-885), and
	// engine.Open's mustAskPolicy recognises "no Consenter, ConsentUnattended"
	// as exactly this surface's case, answering with
	// permission.NewUnattendedDeny — a Reason specific enough that the model
	// is told not to retry (KAN-953) — rather than the generic "no Consenter
	// was supplied" refusal that path exists for elsewhere. Every resulting
	// PermissionDecided is still attributed to the policy and not to a user
	// who was never asked.
	opts.ConsentMode = engine.ConsentUnattended
	opts.Ask = denyHeadlessAsk
	// Same reasoning as ConsentMode above, one layer over: nobody is present
	// to answer an ask call either, so every resulting journal.AskAnswered
	// must be attributed to the policy and not to a user who was never asked
	// (docs/adr/0009-ask-tool-contract.md decision 4).
	opts.AskMode = engine.AskUnattended

	sess, err := engine.Open(ctx, opts)
	if err != nil {
		// Nothing is normally on the stream at this point, and nothing must
		// reach stdout for a session that never opened. If the record did get as
		// far as accepting an event, though, dropping it would be the surface
		// hiding something the journal holds — so it is released behind a header
		// that names no record, because where the record is is exactly what this
		// path could not find out.
		out.orphaned()
		if errors.Is(err, engine.ErrNoAPIKey) {
			say(stderr, "kopicode: %s is not set, so there is no provider to talk to\n", engine.APIKeyEnv)
		} else {
			say(stderr, "kopicode: %v\n", err)
		}
		return exitHarness
	}
	out.open(sess.Path())

	res, runErr := sess.Run(ctx, prompt)
	stop := res.Stop
	if runErr != nil {
		// The stop already says what happened and the record already holds it.
		// This is the sentence, on stderr because --print owns stdout.
		say(stderr, "kopicode: %v\n", runErr)
	}

	// Close last and unconditionally: SessionEnded is the record's other
	// bookend, and a session that broke is the one whose ending most needs to be
	// on it. It is also the event carrying the exit code, so the stream is
	// incomplete until it lands.
	if cerr := sess.Close(ctx); cerr != nil {
		say(stderr, "kopicode: %v\n", cerr)
		if runErr == nil {
			stop = engine.StopHarnessError
		}
	}

	if out.err != nil {
		// A --print whose stream did not reach the reader has failed, whatever
		// the session did. Reporting the session's own success here would hand a
		// consumer half a record and a zero, which is the fail-open direction.
		say(stderr, "kopicode: %v\n", out.err)
		return exitHarness
	}
	return stop.ExitCode()
}

// denyHeadlessAsk answers every ask request by saying nobody is present.
//
// Same posture as denyHeadless, one layer over: there is nobody to ask, so
// the honest answer names that rather than inventing one, and it is not an
// error that ends the session (docs/adr/0009-ask-tool-contract.md decision
// 2 and 4) — the engine's own ask dispatch closure catches this error,
// journals a journal.AskAnswered with Refused true, and returns the message
// below as this call's ordinary tool result, worded so the model knows to
// proceed on its own judgement rather than retry the same call, matching the
// existing run_shell/consent refusal's "a refusal is an answer" line already
// in prompt_default.md.
func denyHeadlessAsk(context.Context, engine.AskRequest) (engine.AskAnswer, error) {
	return engine.AskAnswer{}, errors.New("no human is present to answer this question")
}

// header is the stream's first line. See the schema notes above.
type header struct {
	Kind   string `json:"kind"`
	Schema int    `json:"schema"`
	Record string `json:"record,omitempty"`
}

// record is one event as a line of JSON.
//
// The fields are [engine.Event]'s, one for one, in its order. That is the whole
// design: engine.Event's own fields are the journal payload field names, so this
// projection is checkable by eye against both, and there is no per-kind
// translation to fall out of date with a payload nobody remembered to update.
//
// Ran and ExitCode are pointers because their zero values are meaningful and
// `omitempty` would erase the difference between "false" and "not applicable".
type record struct {
	Kind     string   `json:"kind"`
	Seq      uint64   `json:"seq,omitempty"`
	Turn     int      `json:"turn,omitempty"`
	Text     string   `json:"text,omitempty"`
	Tool     string   `json:"tool,omitempty"`
	Path     string   `json:"path,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	Ref      string   `json:"ref,omitempty"`
	Command  []string `json:"command,omitempty"`
	Decision string   `json:"decision,omitempty"`
	Source   string   `json:"source,omitempty"`
	Checker  string   `json:"checker,omitempty"`
	Ran      *bool    `json:"ran,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Size     int64    `json:"size,omitempty"`
}

// emitter writes the JSON stream.
//
// It buffers until [emitter.open] is called, which is the whole of its state and
// exists for one reason: SessionStarted is appended inside engine.Open, so the
// first event arrives before the caller knows where the record lives. Holding it
// for the length of that call is what lets the header carry the record's path on
// line 1 instead of announcing it three lines later.
type emitter struct {
	enc     *json.Encoder
	opened  bool
	pending []record

	// err is the first write failure, kept rather than returned because the
	// observer this type installs cannot report one. See [headless].
	err error
}

func newEmitter(w io.Writer) *emitter {
	enc := json.NewEncoder(w)
	// Off, for the reason journal.Marshal exists: the default rewrites <, > and
	// & as \uXXXX, which is valid JSON and different bytes. A diff or a line of
	// tool output would then read differently on this stream than in the record
	// it was derived from, over a difference nobody chose.
	enc.SetEscapeHTML(false)
	return &emitter{enc: enc}
}

// observe is the [engine.Observer] the session announces its record through.
func (e *emitter) observe(ev engine.Event) {
	if ev.Kind == engine.EventDelta {
		// Not in the record, and this stream is the record. See the note above.
		return
	}
	r := recordOf(ev)
	if !e.opened {
		e.pending = append(e.pending, r)
		return
	}
	e.write(r)
}

// open writes the header and releases whatever the observer buffered.
func (e *emitter) open(path string) {
	e.write(header{Kind: kindStream, Schema: schemaVersion, Record: path})
	e.opened = true
	for _, r := range e.pending {
		e.write(r)
	}
	e.pending = nil
}

// orphaned releases events recorded by a session that never finished opening,
// and writes nothing at all when there are none — which is the ordinary case,
// and the one that keeps stdout empty for a session that never started.
func (e *emitter) orphaned() {
	if len(e.pending) == 0 {
		e.opened = true
		return
	}
	e.open("")
}

func (e *emitter) write(v any) {
	if e.err != nil {
		// Everything after the first failure is a line the reader will not see
		// in one piece anyway, and a second error would only rename the first.
		return
	}
	if err := e.enc.Encode(v); err != nil {
		e.err = fmt.Errorf("kopicode: writing the JSON stream: %w", err)
	}
}

// recordOf projects one event onto its line.
//
// [engine.EventKind.String] is total — an unrecognised kind renders as
// `event_kind(N)` and [engine.EventUnspecified] as `unspecified` — so a kind this
// build does not know about still reaches the stream rather than being dropped.
// That is the same promise the journal makes for a payload type it cannot name,
// carried one layer further out.
func recordOf(ev engine.Event) record {
	r := record{
		Kind:     ev.Kind.String(),
		Seq:      ev.Seq,
		Turn:     ev.Turn,
		Text:     ev.Text,
		Tool:     ev.Tool,
		Path:     ev.Path,
		Detail:   ev.Detail,
		Reason:   ev.Reason,
		Mode:     ev.Mode,
		Ref:      ev.Ref,
		Command:  ev.Command,
		Decision: ev.Decision,
		Source:   ev.Source,
		Checker:  ev.Checker,
		Size:     ev.Size,
	}
	if ev.HasExitCode {
		code := ev.ExitCode
		r.ExitCode = &code
	}
	if ev.Kind == engine.EventSyntaxGate {
		// The one kind where false is an answer rather than an absence.
		ran := ev.Ran
		r.Ran = &ran
	}
	return r
}
