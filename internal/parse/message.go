package parse

import (
	"errors"
	"slices"
	"strings"
)

// advice is the per-classification half of a repair message: one sentence
// naming what was wrong, and one naming what was expected instead.
//
// Two classifications that render the same sentence mean one of them is not
// classified, so these are held pairwise distinct by test. A [Kind] with no
// entry here is not repairable and fails the call immediately rather than
// spending a round trip on a message the model cannot act on.
type advice struct {
	// problem completes "Your tool call was not run because …".
	problem string
	// expected completes "Send it again with …" — the corrective action, in
	// the imperative, naming the one thing to change.
	expected string
}

// adviceByKind is the repair vocabulary.
//
// Note what is absent. [KindNoCall] has no entry: the model replied in prose
// and no repair is owed, which is the one benign outcome (see [ErrNoToolCall]).
// [KindUnspecified] has none either: it names a bug in this package, and asking
// the model to repair a harness bug is how a harness failure gets laundered
// into a model number (docs/adr/0006 §3).
var adviceByKind = map[Kind]advice{
	KindUnlabelledFence: {
		problem:  "it was inside a fenced block whose info string is not `tool`, so it was read as an illustration rather than as a call",
		expected: "the same JSON inside a block opened by exactly ```tool on its own line",
	},
	KindUnfencedCall: {
		problem:  "it was written as loose JSON in the middle of prose, with no block around it at all",
		expected: "the same JSON wrapped in a ```tool fence, on lines of its own",
	},
	KindUnclosedFence: {
		problem:  "the ```tool fence was opened and never closed, and running the tail of a truncated block is not something this harness will do",
		expected: "the whole call inside the fence, closed by ``` on its own line, and nothing after it",
	},
	KindUnclosedTag: {
		problem:  "the <tool_call> tag was opened and never closed, and running the tail of a truncated block is not something this harness will do",
		expected: "the whole call between <tool_call> and </tool_call>, and nothing after it",
	},
	KindEmptyBlock: {
		problem:  "the block was there but held nothing — the envelope arrived empty",
		expected: "exactly one JSON object inside the block, naming the tool and its arguments",
	},
	KindInvalidJSON: {
		problem:  "the block did not parse as JSON",
		expected: `valid JSON: double quotes around every key and string, no trailing commas, and newlines inside a string written as \n`,
	},
	KindMissingName: {
		problem:  "the call named no tool, so there was nothing to dispatch it to",
		expected: `the tool name as a JSON string under the key "name"`,
	},
	KindInvalidArguments: {
		problem:  `the "arguments" value was not a JSON object`,
		expected: "arguments as a JSON object keyed by argument name — never a list, never a bare string, never positional",
	},
	KindAmbiguousCall: {
		problem:  "the call said two different things at once, and guessing which one you meant is not something this harness will do",
		expected: "exactly one tool name and exactly one arguments object",
	},
	KindUnknownTool: {
		problem:  "it named a tool this harness does not have",
		expected: "one of the tool names listed below, spelled exactly",
	},
	KindMissingArgument: {
		problem:  "a required argument was left out",
		expected: "every required argument for that tool present in the arguments object",
	},
	KindWrongArgumentType: {
		problem:  "an argument arrived with the wrong JSON type",
		expected: "that argument sent as the type the tool declares",
	},
	KindUnknownEnumValue: {
		problem:  "an argument's value is outside the fixed set that tool accepts for it",
		expected: "one of the accepted values for that argument, spelled exactly",
	},
}

// repairable reports whether a classification is worth a round trip.
func repairable(k Kind) bool {
	_, ok := adviceByKind[k]
	return ok
}

// genericShape is the call envelope quoted when the failure never identified a
// tool, so there is no single tool whose shape could be quoted instead.
const genericShape = `{"name": "<tool name>", "arguments": {"<argument name>": <value>}}`

// repairMessage renders what goes back to the model.
//
// The contract, from docs/SLICE-1.md §3: what was wrong, what was expected, and
// the correct shape for that one tool. Not the whole schema again. Everything
// here is bounded by that — the offending snippet is left out entirely, because
// the model wrote it and still has it, and the tool catalogue appears only as a
// list of names on the one classification that is about a name.
func repairMessage(tools Tools, perr *Error, final bool) string {
	a, ok := adviceByKind[perr.Kind]
	if !ok {
		// Not repairable. The caller does not ask for a message in that case;
		// this branch exists so a Kind added without advice degrades to an
		// honest sentence instead of an empty one.
		return "Your tool call was not run: " + perr.Error()
	}

	var b strings.Builder
	b.WriteString("Your tool call was not run because ")
	b.WriteString(a.problem)
	b.WriteString(".\n")

	if detail := detailLine(perr); detail != "" {
		b.WriteString("Detail: ")
		b.WriteString(detail)
		b.WriteString("\n")
	}

	if final {
		b.WriteString("No repair attempts remain for this call, so no tool ran and none will. ")
		b.WriteString("Carry on with what you already know, or send a different call.\n")
	} else {
		b.WriteString("Send it again with ")
		b.WriteString(a.expected)
		b.WriteString(".\n")
	}

	if extra := kindExtra(tools, perr); extra != "" {
		b.WriteString(extra)
	}

	b.WriteString("Correct shape")
	if shape, named := toolShape(tools, perr.Tool); named {
		b.WriteString(" for ")
		b.WriteString(perr.Tool)
		b.WriteString(":\n")
		b.WriteString(shape)
	} else {
		b.WriteString(":\n")
		b.WriteString(genericShape)
	}
	b.WriteString("\n")

	return b.String()
}

// detailLine is the failure's own specifics: which two names disagreed, which
// argument was missing, where the JSON decoder gave up.
func detailLine(perr *Error) string {
	detail := perr.Detail
	if cause := errors.Unwrap(perr); cause != nil && !errors.Is(cause, ErrNoToolCall) {
		if detail == "" {
			return cause.Error()
		}
		return detail + ": " + cause.Error()
	}
	return detail
}

// kindExtra adds the one piece of context a classification needs beyond its
// advice, and nothing more.
func kindExtra(tools Tools, perr *Error) string {
	switch perr.Kind {
	case KindUnknownTool:
		return unknownToolExtra(tools, perr.Tool)
	case KindUnknownEnumValue:
		return enumExtra(tools, perr)
	case KindUnspecified, KindNoCall, KindUnlabelledFence, KindUnfencedCall,
		KindUnclosedFence, KindUnclosedTag, KindEmptyBlock, KindInvalidJSON,
		KindMissingName, KindInvalidArguments, KindAmbiguousCall,
		KindMissingArgument, KindWrongArgumentType:
		return ""
	default:
		return ""
	}
}

// unknownToolExtra lists the tool names — names only — and points at the
// nearest one when there is an obvious near miss, which is the common weak-model
// failure this classification exists for.
func unknownToolExtra(tools Tools, name string) string {
	if tools == nil {
		return ""
	}
	names := slices.Clone(tools.Names())
	if len(names) == 0 {
		return ""
	}
	slices.Sort(names)

	var b strings.Builder
	if near, ok := nearest(name, names); ok {
		b.WriteString("Did you mean ")
		b.WriteString(near)
		b.WriteString("?\n")
	}
	b.WriteString("Tools available: ")
	b.WriteString(strings.Join(names, ", "))
	b.WriteString("\n")
	return b.String()
}

// enumExtra names the accepted values for the one offending argument.
func enumExtra(tools Tools, perr *Error) string {
	if tools == nil {
		return ""
	}
	schema, ok := tools.Schema(perr.Tool)
	if !ok {
		return ""
	}
	for _, p := range schema.Params {
		if p.Name != perr.Argument || len(p.Enum) == 0 {
			continue
		}
		return "Accepted values for " + quote(p.Name) + ": " + strings.Join(quoteAll(p.Enum), ", ") + "\n"
	}
	return ""
}

// toolShape returns the one-line shape of the named tool, when the harness has
// one under that name.
func toolShape(tools Tools, name string) (string, bool) {
	if tools == nil || name == "" {
		return "", false
	}
	schema, ok := tools.Schema(name)
	if !ok {
		return "", false
	}
	return schema.shape(), true
}

// maxSuggestionDistance bounds how wrong a name may be and still earn a
// "did you mean". Two edits catches read/read_file's cousins — a dropped
// underscore, a plural, a transposition — without inventing a suggestion for a
// name the model made up wholesale.
const maxSuggestionDistance = 2

// nearest finds the closest available tool name to what the model wrote.
//
// It suggests; it never substitutes. Resolving a near miss silently is the same
// class of mistake as picking between two disagreeing tool names, and this
// package refuses that everywhere else.
func nearest(name string, candidates []string) (string, bool) {
	best, bestDist := "", maxSuggestionDistance+1
	for _, c := range candidates {
		d := editDistance(strings.ToLower(name), strings.ToLower(c))
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist <= maxSuggestionDistance {
		return best, true
	}

	// A prefix miss is usually a truncation — "read" for "read_file" — and is
	// worth suggesting however many characters short it fell.
	lower := strings.ToLower(name)
	if lower == "" {
		return "", false
	}
	for _, c := range candidates {
		if strings.HasPrefix(strings.ToLower(c), lower) {
			return c, true
		}
	}
	return "", false
}

// editDistance is Levenshtein over runes, with two rolling rows. Tool names are
// short and the candidate list is the harness's own, so the quadratic term is
// bounded by the catalogue rather than by anything the model sends.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}
