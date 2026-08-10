package parse_test

import (
	"slices"

	"github.com/leejianrong/kopicode/internal/parse"
)

// testTools is the tool catalogue the repair corpus is scored against. It
// stands in for internal/tools, which is being built in parallel (KAN-780) and
// which will implement parse.Tools for real.
//
// It is a fake rather than a mock: it answers lookups and lists names, and has
// no expectations about being called. The seam is one interface with two
// methods, so there is nothing else for it to do.
type testTools struct {
	schemas []parse.Schema
}

func (t testTools) Schema(name string) (parse.Schema, bool) {
	for _, s := range t.schemas {
		if s.Name == name {
			return s, true
		}
	}
	return parse.Schema{}, false
}

func (t testTools) Names() []string {
	out := make([]string, 0, len(t.schemas))
	for _, s := range t.schemas {
		out = append(out, s.Name)
	}
	// Reversed rather than sorted, so a message that renders names in a stable
	// order is doing the sorting itself rather than inheriting the registry's.
	slices.Reverse(out)
	return out
}

// tools is the fixture catalogue: enough shapes to reach every semantic
// classification, and no more.
var tools = testTools{schemas: []parse.Schema{
	{
		Name: "read_file",
		Params: []parse.Param{
			{Name: "path", Type: parse.TypeString, Required: true},
			{Name: "limit", Type: parse.TypeInteger},
		},
	},
	{
		Name: "write_file",
		Params: []parse.Param{
			{Name: "path", Type: parse.TypeString, Required: true},
			{Name: "content", Type: parse.TypeString, Required: true},
		},
	},
	{
		Name: "grep",
		Params: []parse.Param{
			{Name: "pattern", Type: parse.TypeString, Required: true},
			// Enum without a declared type: the closed set already says the
			// value is one of these strings, and a tool should not have to say
			// it twice.
			{Name: "mode", Enum: []string{"fixed", "regex"}},
		},
	},
	{Name: "list_dir"},
}}

// repairCase is one entry of the repair corpus: a reply, and the outcome the
// loop must produce for it on the first step.
//
// It is data, exactly like the extraction corpus, so that KAN-777's recordings
// of real malformed calls from the live model append to this slice rather than
// growing new test functions. If a recording contradicts a hand-authored case,
// the recording wins and the invented case goes.
type repairCase struct {
	// name is the model behaviour, not the code path.
	name string
	// why records what this case stands for, so a reader can tell an invented
	// case from an observed one.
	why string
	msg parse.Message

	// wantEvent is the journal event this reply must map to.
	wantEvent parse.Event
	// wantKind is the classification, on the failing events.
	wantKind parse.Kind

	// says are fragments the repair message must contain: the thing that was
	// wrong, or the thing that was expected.
	says []string
	// omits are fragments it must not contain — the guard against handing the
	// model the whole catalogue back.
	omits []string
}

// The repair corpus.
//
// # Hand-authored, and that is a known weakness
//
// Nobody has yet watched `qwen/qwen3-coder-next` fail a call through this
// harness; collecting real ones needs the live provider client (KAN-777). These
// are plausible weak-model failures drawn from the shapes the formats make
// available, plus every semantic failure a tool schema can express. They are
// not evidence about this model.
var repairCorpus = []repairCase{
	// ---------------------------------------------------------------
	// Extraction failures. The taxonomy is KAN-778's; the messages are this
	// card's.
	// ---------------------------------------------------------------
	{
		name:      "single quotes in a tool fence",
		why:       "the most common malformation from a weak model",
		msg:       parse.Message{Content: "```tool\n{'tool': 'read_file', 'arguments': {'path': 'main.go'}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindInvalidJSON,
		says:      []string{"did not parse as JSON", "double quotes"},
		omits:     []string{"write_file", "list_dir"},
	},
	{
		name:      "the fence was never labelled",
		why:       "content right, envelope wrong; one sentence fixes it",
		msg:       parse.Message{Content: "```\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindUnlabelledFence,
		says:      []string{"```tool"},
	},
	{
		name:      "the call was written loose in prose",
		why:       "what a model does when the format instruction falls out of context",
		msg:       parse.Message{Content: "I'll call {\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}} next."},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindUnfencedCall,
		says:      []string{"loose JSON", "```tool"},
	},
	{
		name:      "the fence was never closed",
		why:       "a truncated stream; the tail of a truncated call must never be run",
		msg:       parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindUnclosedFence,
		says:      []string{"never closed", "```"},
	},
	{
		name:      "the tag was never closed",
		why:       "truncation on the route the target model was trained on",
		msg:       parse.Message{Content: "<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindUnclosedTag,
		says:      []string{"</tool_call>"},
	},
	{
		name:      "the block was empty",
		why:       "a model opening the envelope and saying nothing",
		msg:       parse.Message{Content: "```tool\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindEmptyBlock,
		says:      []string{"envelope arrived empty"},
	},
	{
		name:      "the call named no tool",
		why:       "arguments without a tool cannot be dispatched anywhere",
		msg:       parse.Message{Content: "```tool\n{\"arguments\": {\"path\": \"main.go\"}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindMissingName,
		says:      []string{"named no tool", `"name"`},
	},
	{
		name:      "arguments arrived positional",
		why:       "the tool surface is keyword-only, so a list is a hard failure",
		msg:       parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"arguments\": [\"main.go\"]}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindInvalidArguments,
		says:      []string{"never positional"},
	},
	{
		name:      "two tool names disagreeing",
		why:       "picking the more plausible of two names is the misapplication this harness refuses",
		msg:       parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"name\": \"write_file\", \"arguments\": {\"path\": \"a.go\"}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindAmbiguousCall,
		says:      []string{"two different things", "read_file, write_file"},
	},

	// ---------------------------------------------------------------
	// Semantic failures. These need the tool catalogue, and they are the ones
	// a specific sentence recovers most often.
	// ---------------------------------------------------------------
	{
		name:      "a tool name one token short",
		why:       "read for read_file is the recoverable case this classification exists for",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"read\", \"arguments\": {\"path\": \"main.go\"}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindUnknownTool,
		says:      []string{"does not have", "Did you mean read_file?", "grep, list_dir, read_file, write_file"},
	},
	{
		name:      "a tool invented wholesale",
		why:       "no suggestion is better than a wrong one; the catalogue is still listed",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"summon_the_compiler\", \"arguments\": {}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindUnknownTool,
		says:      []string{"Tools available: grep, list_dir, read_file, write_file"},
		omits:     []string{"Did you mean"},
	},
	{
		name:      "a required argument left out",
		why:       "the model knew the tool and forgot one key",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"read_file\", \"arguments\": {}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindMissingArgument,
		says:      []string{`left out the required argument "path"`, `{"name": "read_file", "arguments": {"path": <string, required>, "limit": <integer, optional>}}`},
		omits:     []string{"write_file", "grep", "list_dir"},
	},
	{
		name:      "two required arguments left out",
		why:       "naming them one per round trip would spend the budget on arithmetic",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"write_file\", \"arguments\": {}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindMissingArgument,
		says:      []string{`required arguments "path", "content"`},
	},
	{
		name:      "a required argument sent as null",
		why:       "null is the model saying nothing is there, which is omission rather than a type error",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"read_file\", \"arguments\": {\"path\": null}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindMissingArgument,
		says:      []string{`left out the required argument "path"`},
	},
	{
		name:      "an argument of the wrong type",
		why:       "a number where a path belongs; the tool would have failed obscurely",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"read_file\", \"arguments\": {\"path\": 42}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindWrongArgumentType,
		says:      []string{"must be a JSON string", "a number was sent"},
		omits:     []string{"write_file", "grep"},
	},
	{
		name:      "a float where an integer belongs",
		why:       "JSON has one number type; integer-ness is the schema's claim, not the wire's",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\", \"limit\": 2.5}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindWrongArgumentType,
		says:      []string{"must be a JSON integer", "a number with a fractional part was sent"},
	},
	{
		name:      "a value outside a closed set",
		why:       "coercing to the nearest member is the same silent misapplication as guessing a tool name",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"grep\", \"arguments\": {\"pattern\": \"TODO\", \"mode\": \"glob\"}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindUnknownEnumValue,
		says:      []string{`does not accept the value "glob"`, `Accepted values for "mode": "fixed", "regex"`},
		omits:     []string{"read_file", "write_file"},
	},
	{
		name:      "an enum argument sent as a number",
		why:       "a closed set is a set of strings; anything else is a type failure, not an unknown value",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"grep\", \"arguments\": {\"pattern\": \"TODO\", \"mode\": 1}}\n```"},
		wantEvent: parse.EventToolCallRepaired,
		wantKind:  parse.KindWrongArgumentType,
		says:      []string{"drawn from a fixed set"},
	},

	// ---------------------------------------------------------------
	// The outcomes that owe no repair.
	// ---------------------------------------------------------------
	{
		name:      "a well-formed call",
		why:       "the ordinary case, and the denominator of the parse-success rate",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}\n```"},
		wantEvent: parse.EventToolCallParsed,
	},
	{
		name:      "a zero-argument tool",
		why:       "a tool with no parameters must not be rejected for having none",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"list_dir\", \"arguments\": {}}\n```"},
		wantEvent: parse.EventToolCallParsed,
	},
	{
		name:      "an optional argument omitted",
		why:       "optional means optional",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}\n```"},
		wantEvent: parse.EventToolCallParsed,
	},
	{
		name:      "an accepted enum value",
		why:       "the closed set accepts its own members",
		msg:       parse.Message{Content: "```tool\n{\"name\": \"grep\", \"arguments\": {\"pattern\": \"TODO\", \"mode\": \"regex\"}}\n```"},
		wantEvent: parse.EventToolCallParsed,
	},
	{
		name:      "plain prose",
		why:       "the model answered rather than called; ErrNoToolCall is benign and owes no repair",
		msg:       parse.Message{Content: "The bug is in main.go: the loop never terminates."},
		wantEvent: parse.EventNone,
	},
	{
		name:      "an empty completion",
		why:       "an empty reply is a no-call, not a malformed call",
		msg:       parse.Message{},
		wantEvent: parse.EventNone,
	},
}
