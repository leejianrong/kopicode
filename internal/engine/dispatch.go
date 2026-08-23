package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/tools"
)

// toolOutcome is what one tool call produced, in the terms the journal needs.
//
// err is the tool's **own** failure and is not fatal: a missing file, a refused
// path, a cancelled command. It becomes journal.ToolResult.ErrorKind and an
// observation the model sees, and the loop carries on. A fatal error — the
// journal refusing a write, a tool result that cannot be placed in the
// conversation — travels as the second return value of the functions below and
// ends the exchange.
type toolOutcome struct {
	output   string
	err      error
	exitCode *int
}

// toolEntry is one dispatchable tool: how to decode its arguments, what
// permission question it raises, and how to run it.
type toolEntry struct {
	name string
	// op is the granularity policy reasons about. It is not the tool name:
	// read_file and grep are both a read.
	op permission.Operation
	// schema is derived from the argument struct; see catalogue.go.
	schema parse.Schema
	// mutates says this tool can change the working tree, which is what earns
	// the turn a TurnSnapshot. run_shell counts: a shell goes where it likes,
	// and a snapshot taken only after an edit would miss everything a command
	// wrote.
	mutates bool

	newArgs func() any
	action  func(args any, root string, act *permission.Action)
	run     func(ctx context.Context, e *Engine, turn int, callID string, args any) (toolOutcome, error)
}

// entryOf builds a [toolEntry] for a tool whose arguments are A, so the type
// assertions from the `any`-typed dispatch table live in exactly one place.
//
// desc is the tool's own one-clause description, threaded straight to
// [schemaOf] — see catalogue.go's doc comment on why a tool-level description
// is a parameter here rather than a struct field or a second lookup table.
func entryOf[A any](
	name string,
	desc string,
	op permission.Operation,
	mutates bool,
	action func(args *A, root string, act *permission.Action),
	run func(ctx context.Context, e *Engine, turn int, callID string, args *A) (toolOutcome, error),
) toolEntry {
	var zero A
	return toolEntry{
		name:    name,
		op:      op,
		schema:  schemaOf(name, desc, zero),
		mutates: mutates,
		newArgs: func() any { return new(A) },
		action: func(args any, root string, act *permission.Action) {
			if action != nil {
				action(args.(*A), root, act)
			}
		},
		run: func(ctx context.Context, e *Engine, turn int, callID string, args any) (toolOutcome, error) {
			return run(ctx, e, turn, callID, args.(*A))
		},
	}
}

// toolEntries is the dispatch table, in a slice so the catalogue's name list
// has a defined order before it is sorted.
var toolEntries = []toolEntry{
	entryOf(tools.ToolReadFile,
		"Read a text file from the repository, returning each line with an anchor you can later edit by.",
		permission.OperationRead, false,
		func(a *readFileArgs, _ string, act *permission.Action) { act.Path = a.Path },
		func(ctx context.Context, e *Engine, _ int, _ string, a *readFileArgs) (toolOutcome, error) {
			out, err := e.cfg.Tools.ReadFile(ctx, tools.ReadRequest{Path: a.Path, Offset: a.Offset, Limit: a.Limit})
			return toolOutcome{output: modelText(out, err), err: err}, nil
		}),

	entryOf(tools.ToolListDir,
		"List a directory's entries, optionally recursively and filtered by a glob pattern.",
		permission.OperationRead, false,
		func(a *listDirArgs, _ string, act *permission.Action) { act.Path = a.Path },
		func(ctx context.Context, e *Engine, _ int, _ string, a *listDirArgs) (toolOutcome, error) {
			out, err := e.cfg.Tools.ListDir(ctx, tools.ListRequest{Path: a.Path, Recursive: a.Recursive, Pattern: a.Pattern})
			return toolOutcome{output: modelText(out, err), err: err}, nil
		}),

	entryOf(tools.ToolGrep,
		"Search text files for a regular expression, returning matching lines with their file and line number.",
		permission.OperationRead, false,
		func(a *grepArgs, _ string, act *permission.Action) { act.Path = a.Path },
		func(ctx context.Context, e *Engine, _ int, _ string, a *grepArgs) (toolOutcome, error) {
			out, err := e.cfg.Tools.Grep(ctx, tools.GrepRequest{
				Pattern: a.Pattern, Path: a.Path, Include: a.Include, IgnoreCase: a.IgnoreCase,
			})
			return toolOutcome{output: modelText(out, err), err: err}, nil
		}),

	entryOf(tools.ToolWriteFile,
		"Create a file or replace its entire contents, creating parent directories as needed.",
		permission.OperationWrite, true,
		func(a *writeFileArgs, _ string, act *permission.Action) { act.Path = a.Path },
		func(ctx context.Context, e *Engine, _ int, _ string, a *writeFileArgs) (toolOutcome, error) {
			res, err := e.cfg.Tools.WriteFile(ctx, tools.WriteRequest{Path: a.Path, Content: a.Content})
			return toolOutcome{output: modelText(res.Output, err), err: err}, nil
		}),

	entryOf(tools.ToolEditFile,
		"Replace one anchored region of a file with new text, using anchors printed by a prior read_file call.",
		permission.OperationWrite, true,
		func(a *editFileArgs, _ string, act *permission.Action) { act.Path = a.Path },
		func(ctx context.Context, e *Engine, turn int, callID string, a *editFileArgs) (toolOutcome, error) {
			res, err := e.cfg.Tools.EditFile(ctx, tools.EditRequest{
				Path: a.Path, AnchorStart: a.AnchorStart, AnchorEnd: a.AnchorEnd, NewText: a.NewText,
			})
			if jerr := e.journalEdit(ctx, turn, callID, res); jerr != nil {
				return toolOutcome{}, jerr
			}
			return toolOutcome{output: modelText(res.Output, err), err: err}, nil
		}),

	entryOf(tools.ToolEditFileFuzzy,
		"Replace text matched by similarity when no anchor is available; refuses an ambiguous or too-weak match. Prefer edit_file.",
		permission.OperationWrite, true,
		func(a *editFileFuzzyArgs, _ string, act *permission.Action) { act.Path = a.Path },
		func(ctx context.Context, e *Engine, turn int, callID string, a *editFileFuzzyArgs) (toolOutcome, error) {
			res, err := e.cfg.Tools.EditFileFuzzy(ctx, tools.FuzzyEditRequest{
				Path: a.Path, Before: a.Before, After: a.After,
			})
			if jerr := e.journalEdit(ctx, turn, callID, res); jerr != nil {
				return toolOutcome{}, jerr
			}
			return toolOutcome{output: modelText(res.Output, err), err: err}, nil
		}),

	// delete_file is classified permission.OperationWrite, matching write_file:
	// a delete inside the repository root is the agent's own job (permission
	// gate's fixed rule "inside it, writing is the agent's job"), and one
	// outside it always asks — see internal/permission/gate.go's doc comment.
	// mutates is true because a delete changes the tree and earns the turn a
	// TurnSnapshot, exactly like write_file and edit_file.
	entryOf(tools.ToolDeleteFile,
		"Delete a file.",
		permission.OperationWrite, true,
		func(a *deleteFileArgs, _ string, act *permission.Action) { act.Path = a.Path },
		func(ctx context.Context, e *Engine, _ int, _ string, a *deleteFileArgs) (toolOutcome, error) {
			res, err := e.cfg.Tools.DeleteFile(ctx, tools.DeleteRequest{Path: a.Path})
			return toolOutcome{output: modelText(res.Output, err), err: err}, nil
		}),

	entryOf(tools.ToolRunShell,
		"Run a command line through the platform shell and return its combined output and exit status.",
		permission.OperationShell, true,
		func(a *runShellArgs, root string, act *permission.Action) {
			// The argv comes from internal/tools so the thing consented to and
			// the thing executed cannot disagree.
			act.Command = tools.ShellArgv(a.Command)
			act.Dir = a.Dir
			if act.Dir == "" {
				// An empty Dir means the repository root to run_shell, and
				// means "the process's own working directory" to a policy
				// judging containment — which is global state and exactly how
				// a bench task escapes its worktree. Naming the root is the
				// engine deciding the policy question, which is its job.
				act.Dir = root
			}
		},
		func(ctx context.Context, e *Engine, _ int, _ string, a *runShellArgs) (toolOutcome, error) {
			res, err := e.cfg.Tools.RunShell(ctx, tools.ShellRequest{
				Command: a.Command,
				Dir:     a.Dir,
				Timeout: time.Duration(a.TimeoutSeconds) * time.Second,
			})
			code := res.ExitCode
			return toolOutcome{output: modelText(res.Output, err), err: err, exitCode: &code}, nil
		}),

	// ask is docs/adr/0009-ask-tool-contract.md's tool. permission.OperationRead
	// and a nil action closure, per decision 2: it changes nothing on the tree,
	// so it earns no TurnSnapshot, needs no path resolution and raises no
	// permission.Action field — Gate.classify's "reads never ask" rule lets it
	// through with zero PermissionRequested/PermissionDecided events, which is
	// correct rather than a bypass, because there was never a permission
	// question here for the gate to skip. The actual pause-and-collect-an-answer
	// step happens entirely inside runAsk, which calls e.cfg.Ask directly and
	// never through internal/permission.
	entryOf(tools.ToolAsk,
		"Ask the human a clarifying question when the repository cannot answer it, and receive their "+
			"reply as this call's result. Use sparingly — read and grep first; most ambiguity is "+
			"resolved by looking, not by asking.",
		permission.OperationRead, false,
		nil,
		func(ctx context.Context, e *Engine, turn int, callID string, a *askArgs) (toolOutcome, error) {
			return e.runAsk(ctx, turn, callID, a)
		}),
}

// toolByName indexes the table. A map is fine for lookup; nothing iterates it.
var toolByName = func() map[string]toolEntry {
	m := make(map[string]toolEntry, len(toolEntries))
	for _, entry := range toolEntries {
		if _, dup := m[entry.name]; dup {
			panic("engine: duplicate tool entry " + entry.name)
		}
		m[entry.name] = entry
	}
	return m
}()

// dispatch runs every call one reply asked for, in the order the model wrote
// them, and reports whether any of them could have changed the tree.
//
// Nothing is dispatched until all of it parses — that decision lives in the
// repair loop — so by the time this runs the whole reply is dispatchable. Each
// call is journaled before it runs and answered afterwards, and the conversation
// is checked to have an answer for every native call before the next request
// goes out.
func (e *Engine) dispatch(ctx context.Context, turn int, ext parse.Extraction) (bool, error) {
	route := ext.Route().String()
	mutated := false

	for i, call := range ext.Calls() {
		// A native call keeps the id the provider issued, because the tool
		// result has to echo it back. A text-route call has none — the model
		// wrote it as prose — so the engine mints one for its own records and
		// the result travels as a user turn.
		id := call.ID
		if id == "" {
			id = callID(turn, i+1)
		}

		if _, err := e.append(ctx, turn, journal.ToolCallRequested{
			CallID: id,
			Raw:    journal.InlineText(call.Raw),
		}); err != nil {
			return mutated, err
		}
		if _, err := e.append(ctx, turn, journal.ToolCallParsed{
			CallID: id,
			Tool:   call.Name,
			Args:   call.Arguments,
			Route:  route,
			// The wire string, not a typed value: internal/parse and
			// internal/journal share a vocabulary and not a dependency edge.
			// Recording it is how a normalisation the harness performed on the
			// model's behalf stays attributable.
			ArgEncoding: call.ArgEncoding.String(),
		}); err != nil {
			return mutated, err
		}

		output, mut, err := e.runTool(ctx, turn, id, call)
		if err != nil {
			return mutated, err
		}
		mutated = mutated || mut

		if err := e.asm.AppendToolResult(call.ID, output); err != nil {
			return mutated, fmt.Errorf("engine: placing the result of %s (call %q): %w", call.Name, id, err)
		}
	}

	if left := e.asm.Unanswered(); len(left) > 0 {
		return mutated, fmt.Errorf(
			"engine: turn %d dispatched every call and left %d unanswered: %v", turn, len(left), left)
	}
	return mutated, nil
}

// runTool decodes, gates and runs one call, journaling everything it produced.
//
// The string it returns is what the model sees next, and it is never empty and
// never clipped. The bool says the tree may have changed. The error is fatal
// and only ever the harness's own: a tool that failed is an observation, not an
// exception.
func (e *Engine) runTool(ctx context.Context, turn int, callID string, call parse.ToolCall) (string, bool, error) {
	entry, ok := toolByName[call.Name]
	if !ok || !e.offered[call.Name] {
		// Two ways in, and both are refusals rather than surprises. The name
		// may be one no tool answers — unreachable with a catalogue matching
		// the dispatch table, which TestCatalogueCoversEveryTool holds. Or it
		// may be a tool this binary can run and *this arm does not present*,
		// which must not run either: the harness config hash records which
		// tools the model was given, and a dispatcher that ignored the tool set
		// would make that record decorative.
		detail := fmt.Sprintf("no tool named %q; this harness offers: %s",
			call.Name, strings.Join(e.cfg.Catalogue.Names(), ", "))
		if _, err := e.append(ctx, turn, journal.ToolCallFailed{
			CallID: callID,
			Tool:   call.Name,
			Reason: parse.KindUnknownTool.String(),
			Detail: journal.InlineText(detail),
		}); err != nil {
			return "", false, err
		}
		return detail, false, nil
	}

	args := entry.newArgs()
	if err := json.Unmarshal(call.Arguments, args); err != nil {
		detail := fmt.Sprintf("the arguments for %s did not decode: %v", call.Name, err)
		if jerr := e.journalToolResult(ctx, turn, callID, call.Name, detail, nil, tools.FaultTask); jerr != nil {
			return "", false, jerr
		}
		return detail, false, nil
	}

	act := permission.Action{ID: callID, Tool: call.Name, Operation: entry.op}
	entry.action(args, e.cfg.Tools.Root.Path(), &act)

	// The engine decides *that* consent is required and what the answer means.
	// How it is asked belongs to the policy the surface supplied, and nothing
	// about the prompt's presentation reaches this package or the record.
	outcome, perr := e.cfg.Permissions.Check(ctx, act)
	if outcome.Required {
		if err := e.journalPermission(ctx, turn, outcome); err != nil {
			return "", false, err
		}
	}
	if perr != nil {
		detail := perr.Error()
		if jerr := e.journalToolResult(ctx, turn, callID, call.Name, detail, nil, faultOf(perr)); jerr != nil {
			return "", false, jerr
		}
		return detail, false, nil
	}

	res, err := entry.run(ctx, e, turn, callID, args)
	if err != nil {
		return "", false, err
	}
	if jerr := e.journalToolResult(ctx, turn, callID, call.Name, res.output, res.exitCode, faultOf(res.err)); jerr != nil {
		return "", false, jerr
	}
	return res.output, entry.mutates, nil
}

// journalToolResult writes the outcome of one call. Output goes in whole:
// oversized payloads spill to a blob and stay complete, because clipping the
// diagnostic output that justified a fix is the specific failure this project
// is designed out of.
func (e *Engine) journalToolResult(
	ctx context.Context, turn int, callID, tool, output string, exitCode *int, fault tools.Fault,
) error {
	if fault == tools.FaultCancelled {
		// The single funnel every tool outcome passes through, which is why the
		// note lives here rather than at the call sites. A cancelled tool is the
		// loop's first sight of the cancellation: the checkpoint that ends the
		// exchange is one step further on and would record where the loop was
		// standing rather than what the user interrupted.
		e.noteCancelled(phaseToolCall)
	}
	_, err := e.append(ctx, turn, journal.ToolResult{
		CallID:    callID,
		Tool:      tool,
		Output:    journal.InlineText(output),
		ExitCode:  exitCode,
		ErrorKind: fault.String(),
	})
	return err
}

// journalPermission records that consent was required and how it was answered.
func (e *Engine) journalPermission(ctx context.Context, turn int, out permission.Outcome) error {
	if _, err := e.append(ctx, turn, journal.PermissionRequested{
		RequestID: out.Request.ID,
		Kind:      out.Request.Kind.String(),
		Tool:      out.Request.Action.Tool,
		Detail:    journal.InlineText(out.Request.Detail),
		Reason:    out.Request.Reason,
	}); err != nil {
		return err
	}
	_, err := e.append(ctx, turn, journal.PermissionDecided{
		RequestID: out.Request.ID,
		Decision:  out.Decision.Verdict.String(),
		Source:    out.Decision.Source.String(),
		Reason:    out.Decision.Reason,
	})
	return err
}

// journalEdit records what one edit did, and runs the syntax gate behind an
// edit that landed.
//
// The gate run follows the EditApplied immediately and with nothing between
// them, because that adjacency is the rule SLICE-1 §9 classifies on: a syntax
// failure directly after an edit means the edit produced code that does not
// parse, which is a harness signal rather than a model one.
func (e *Engine) journalEdit(ctx context.Context, turn int, callID string, res tools.EditResult) error {
	switch {
	case res.Rejection != nil:
		_, err := e.append(ctx, turn, journal.EditRejected{
			CallID:      callID,
			Path:        res.Path,
			Mode:        res.Mode,
			Reason:      string(res.Rejection.Reason),
			AnchorStart: res.AnchorStart,
			AnchorEnd:   res.AnchorEnd,
			Detail:      journal.InlineText(res.Rejection.Detail),
		})
		return err

	case res.Cancelled || res.StartLine == 0:
		// Nothing landed: the call was cancelled before the write, or never
		// got as far as resolving the file. There is no diff to record and
		// recording one would say a change happened.
		return nil

	default:
		if _, err := e.append(ctx, turn, journal.EditApplied{
			CallID:        callID,
			Path:          res.Path,
			Mode:          res.Mode,
			AnchorStart:   res.AnchorStart,
			AnchorEnd:     res.AnchorEnd,
			AnchorVersion: res.AnchorVersion,
			Diff:          journal.InlineText(res.Diff),
		}); err != nil {
			return err
		}
		return e.syntaxGate(ctx, turn, res.Path)
	}
}

// syntaxGate runs the post-edit language check and journals what it concluded.
//
// A gate that could not be run at all is fatal, not a skip. syntax.Gate reports
// every honest not-run — no checker for the language, the toolchain missing,
// the step timing out — as a Result with Ran false; an *error* from it means
// the path the engine handed over is not one the gate can talk about, which is
// the engine and the tool set disagreeing about their root.
func (e *Engine) syntaxGate(ctx context.Context, turn int, path string) error {
	res, err := e.cfg.Syntax.Check(ctx, path)
	if err != nil {
		return fmt.Errorf("engine: syntax gate on %q: %w", path, err)
	}
	_, jerr := e.append(ctx, turn, journal.SyntaxGateRun{
		Path:     res.Path,
		Checker:  res.Checker,
		Ran:      res.Ran(),
		ExitCode: res.ExitCode,
		Output:   journal.InlineText(res.Output),
	})
	return jerr
}

// faultOf classifies a failure for journal.ToolResult.ErrorKind.
//
// tools.FaultOf answers for anything internal/tools produced and already puts a
// cancellation ahead of everything else. What it cannot answer for is a refusal
// from the consent gate, which it would call FaultInternal — the harness broke
// — when in fact the harness worked exactly as designed and the answer was no.
// A denial booked as `harness` is noise against an acceptance criterion of
// zero, so it is classified here as a task-level refusal, which is the closest
// honest reading: the model was told what was wrong and may try something else.
//
// errAskRefused draws the identical line for ask (docs/adr/0009-ask-tool-contract.md
// decision 2): an Answerer that could not get an answer — no human present, the
// turn was cancelled — is not internal/permission's ErrDenied (ask is never
// routed through that package at all), but it is the same shape of "the harness
// worked exactly as designed" that ErrDenied already gets FaultTask for.
func faultOf(err error) tools.Fault {
	if err == nil {
		return tools.FaultNone
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return tools.FaultCancelled
	}
	if errors.Is(err, permission.ErrDenied) || errors.Is(err, errAskRefused) {
		return tools.FaultTask
	}
	return tools.FaultOf(err)
}

// modelText is what the model is shown for one call: the tool's own rendering,
// falling back to the failure when there is none.
//
// Neither is clipped. A tool that produced both — an edit that was refused
// renders the refusal and returns an error saying the same thing — is shown the
// rendering, which is the fuller of the two.
func modelText(rendered string, err error) string {
	switch {
	case rendered != "":
		return rendered
	case err != nil:
		return err.Error()
	default:
		return "the tool produced no output"
	}
}
