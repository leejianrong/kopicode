package parse_test

import "github.com/leejianrong/kopicode/internal/parse"

// The extraction corpus.
//
// Every case is one thing a model does, named for the behaviour rather than for
// the code path, so that a failure reads as "the model does X and we stopped
// handling it" rather than as a line number.
//
// # This corpus is hand-authored, and that is a known weakness
//
// Nobody has yet seen `qwen/qwen3-coder-next` emit a malformed tool call
// through this harness, because collecting real ones needs the live provider
// client (KAN-777). These cases are therefore *plausible* weak-model output,
// drawn from the failure shapes the format makes available — single quotes, a
// trailing comma, an unescaped newline inside a string, an unlabelled fence, a
// fence that never closes, arguments double-encoded as a string. They are not
// evidence about this model.
//
// KAN-777 replaces that. When real recordings land they are appended to this
// slice as data — a name, a Message, and what should come out — and no new test
// function is written to hold them. If a recording contradicts a hand-authored
// case here, the recording wins and the invented case goes.
var corpus = []extractCase{
	// ---------------------------------------------------------------
	// Route (a): native OpenAI-style tool_calls.
	// ---------------------------------------------------------------
	{
		name:  "native/arguments as object",
		why:   "several providers send arguments as a bare object despite the OpenAI spec",
		msg:   parse.Message{ToolCalls: []parse.NativeCall{{ID: "call_1", Name: "read_file", Arguments: raw(`{"path": "main.go"}`)}}},
		route: parse.RouteNative,
		calls: []wantCall{{id: "call_1", name: "read_file", args: `{"path":"main.go"}`, encoding: parse.ArgsObject}},
	},
	{
		name:  "native/arguments as JSON string",
		why:   "the OpenAI wire format specifies a string holding JSON; this is the spec-correct shape",
		msg:   parse.Message{ToolCalls: []parse.NativeCall{{ID: "call_1", Name: "read_file", Arguments: raw(`"{\"path\": \"main.go\"}"`)}}},
		route: parse.RouteNative,
		calls: []wantCall{{id: "call_1", name: "read_file", args: `{"path":"main.go"}`, encoding: parse.ArgsJSONString}},
	},
	{
		name: "native/two calls in one message",
		why:  "a model batching a read and a grep in one turn",
		msg: parse.Message{ToolCalls: []parse.NativeCall{
			{ID: "call_1", Name: "read_file", Arguments: raw(`{"path":"a.go"}`)},
			{ID: "call_2", Name: "grep", Arguments: raw(`{"pattern":"func main"}`)},
		}},
		route: parse.RouteNative,
		calls: []wantCall{
			{id: "call_1", name: "read_file", args: `{"path":"a.go"}`},
			{id: "call_2", name: "grep", args: `{"pattern":"func main"}`},
		},
	},
	{
		name:  "native/no arguments at all",
		why:   "a zero-argument tool; absent arguments mean none, not a failure",
		msg:   parse.Message{ToolCalls: []parse.NativeCall{{ID: "call_1", Name: "list_dir"}}},
		route: parse.RouteNative,
		calls: []wantCall{{id: "call_1", name: "list_dir", args: `{}`}},
	},
	{
		name:  "native/prose alongside the call",
		why:   "content and tool_calls both populated; the native channel still wins",
		msg:   parse.Message{Content: "I'll read the file first.", ToolCalls: []parse.NativeCall{{ID: "c", Name: "read_file", Arguments: raw(`{"path":"a.go"}`)}}},
		route: parse.RouteNative,
		calls: []wantCall{{id: "c", name: "read_file", args: `{"path":"a.go"}`}},
	},
	{
		name: "native/missing function name",
		why:  "a truncated or malformed native call must not fall through to the text routes",
		msg: parse.Message{
			Content:   "```tool\n{\"tool\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```",
			ToolCalls: []parse.NativeCall{{ID: "call_1", Arguments: raw(`{"path":"a.go"}`)}},
		},
		kind: parse.KindMissingName,
	},
	{
		name:  "native/arguments string is not JSON",
		why:   "a model emitting prose where the arguments payload belongs",
		msg:   parse.Message{ToolCalls: []parse.NativeCall{{ID: "call_1", Name: "read_file", Arguments: raw(`"the file main.go"`)}}},
		kind:  parse.KindInvalidArguments,
		route: parse.RouteNative,
	},

	// ---------------------------------------------------------------
	// Route (b): a fenced ```tool JSON block.
	// ---------------------------------------------------------------
	{
		name:  "fenced/plain tool fence",
		why:   "the shape the system prompt asks for",
		msg:   parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{{name: "read_file", args: `{"path":"main.go"}`, rawHas: "```tool"}},
	},
	{
		name:  "fenced/prose wrapped around the block",
		why:   "weak models narrate before and after the call",
		msg:   parse.Message{Content: "Sure! Let me look at that file.\n\n```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}\n```\n\nI'll report back once I've read it."},
		route: parse.RouteFencedJSON,
		calls: []wantCall{{name: "read_file", args: `{"path":"main.go"}`}},
	},
	{
		name:  "fenced/label case and alias",
		why:   "TOOL_CALL and tool_call show up as often as tool",
		msg:   parse.Message{Content: "```TOOL_CALL\n{\"name\": \"grep\", \"arguments\": {\"pattern\": \"TODO\"}}\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{{name: "grep", args: `{"pattern":"TODO"}`}},
	},
	{
		name:  "fenced/two fences in one message",
		why:   "a model batching two calls without knowing about arrays",
		msg:   parse.Message{Content: "First:\n```tool\n{\"tool\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```\nThen:\n```tool\n{\"tool\":\"read_file\",\"arguments\":{\"path\":\"b.go\"}}\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{
			{name: "read_file", args: `{"path":"a.go"}`},
			{name: "read_file", args: `{"path":"b.go"}`},
		},
	},
	{
		name:  "fenced/array of calls in one fence",
		why:   "a model copying the native tool_calls array shape into text",
		msg:   parse.Message{Content: "```tool\n[{\"tool\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}},{\"tool\":\"grep\",\"arguments\":{\"pattern\":\"x\"}}]\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{
			{name: "read_file", args: `{"path":"a.go"}`},
			{name: "grep", args: `{"pattern":"x"}`},
		},
	},
	{
		name:  "fenced/two objects back to back",
		why:   "JSON-lines rather than an array; unambiguous, so it is read",
		msg:   parse.Message{Content: "```tool\n{\"tool\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n{\"tool\":\"read_file\",\"arguments\":{\"path\":\"b.go\"}}\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{
			{name: "read_file", args: `{"path":"a.go"}`},
			{name: "read_file", args: `{"path":"b.go"}`},
		},
	},
	{
		name:  "fenced/OpenAI envelope written into text",
		why:   "a model that learned the wire format from documentation writes it verbatim",
		msg:   parse.Message{Content: "```tool\n{\"id\":\"call_9\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{{id: "call_9", name: "read_file", args: `{"path":"a.go"}`, encoding: parse.ArgsJSONString}},
	},
	{
		name:  "fenced/arguments key alias",
		why:   "args and parameters are as likely as arguments",
		msg:   parse.Message{Content: "```tool\n{\"tool\":\"grep\",\"parameters\":{\"pattern\":\"x\"}}\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{{name: "grep", args: `{"pattern":"x"}`}},
	},
	{
		name:  "fenced/null arguments",
		why:   "a zero-argument tool, spelled explicitly",
		msg:   parse.Message{Content: "```tool\n{\"tool\":\"list_dir\",\"arguments\":null}\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{{name: "list_dir", args: `{}`}},
	},
	{
		name: "fenced/single quotes",
		why:  "the single most common malformation from a weak model",
		msg:  parse.Message{Content: "```tool\n{'tool': 'read_file', 'arguments': {'path': 'main.go'}}\n```"},
		kind: parse.KindInvalidJSON,
	},
	{
		name: "fenced/trailing comma",
		why:  "JavaScript habits leaking into JSON",
		msg:  parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\",},}\n```"},
		kind: parse.KindInvalidJSON,
	},
	{
		name: "fenced/unescaped newline inside a string",
		why:  "a model writing file content into arguments without escaping it",
		msg:  parse.Message{Content: "```tool\n{\"tool\": \"write_file\", \"arguments\": {\"content\": \"line one\nline two\"}}\n```"},
		kind: parse.KindInvalidJSON,
	},
	{
		name: "fenced/fence never closed",
		why:  "a truncated stream; the tail of a truncated call must never be run",
		msg:  parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}"},
		kind: parse.KindUnclosedFence,
	},
	{
		name: "fenced/unclosed fence beats a valid later block",
		why:  "a broken route fails loudly rather than falling through to something else the model wrote",
		msg:  parse.Message{Content: "```tool\n{\"tool\": \"read_file\"\n<tool_call>{\"name\":\"grep\",\"arguments\":{\"pattern\":\"x\"}}</tool_call>"},
		kind: parse.KindUnclosedFence,
	},
	{
		name: "fenced/empty block",
		why:  "a model opening the envelope and saying nothing",
		msg:  parse.Message{Content: "```tool\n```"},
		kind: parse.KindEmptyBlock,
	},
	{
		name: "fenced/no tool name",
		why:  "arguments without a tool cannot be dispatched anywhere",
		msg:  parse.Message{Content: "```tool\n{\"arguments\": {\"path\": \"main.go\"}}\n```"},
		kind: parse.KindMissingName,
	},
	{
		name: "fenced/two names disagreeing",
		why:  "picking the more plausible of two tool names is the silent misapplication this package refuses",
		msg:  parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"name\": \"write_file\", \"arguments\": {\"path\": \"a.go\"}}\n```"},
		kind: parse.KindAmbiguousCall,
	},
	{
		name: "fenced/two argument objects disagreeing",
		why:  "same refusal, on the arguments side",
		msg:  parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}, \"args\": {\"path\": \"b.go\"}}\n```"},
		kind: parse.KindAmbiguousCall,
	},
	{
		name:  "fenced/same arguments under two aliases",
		why:   "agreement is not ambiguity",
		msg:   parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}, \"args\": {\"path\":\"a.go\"}}\n```"},
		route: parse.RouteFencedJSON,
		calls: []wantCall{{name: "read_file", args: `{"path":"a.go"}`}},
	},
	{
		name: "fenced/arguments as an array",
		why:  "positional arguments; the tool surface is keyword-only, so this is a hard failure",
		msg:  parse.Message{Content: "```tool\n{\"tool\": \"read_file\", \"arguments\": [\"main.go\"]}\n```"},
		kind: parse.KindInvalidArguments,
	},

	// ---------------------------------------------------------------
	// Route (c): an XML-tagged block.
	// ---------------------------------------------------------------
	{
		name:  "xml/plain tool_call tag",
		why:   "the Hermes/Qwen convention the target model was trained on",
		msg:   parse.Message{Content: "<tool_call>\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}\n</tool_call>"},
		route: parse.RouteXMLTag,
		calls: []wantCall{{name: "read_file", args: `{"path":"main.go"}`, rawHas: "<tool_call>"}},
	},
	{
		name:  "xml/prose around the tag",
		why:   "same narration habit as the fenced route",
		msg:   parse.Message{Content: "Let me check.\n<tool_call>{\"name\": \"grep\", \"arguments\": {\"pattern\": \"TODO\"}}</tool_call>\nDone."},
		route: parse.RouteXMLTag,
		calls: []wantCall{{name: "grep", args: `{"pattern":"TODO"}`}},
	},
	{
		name:  "xml/two tags in one message",
		why:   "the Qwen convention for batching is one tag per call",
		msg:   parse.Message{Content: "<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}</tool_call>\n<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"b.go\"}}</tool_call>"},
		route: parse.RouteXMLTag,
		calls: []wantCall{
			{name: "read_file", args: `{"path":"a.go"}`},
			{name: "read_file", args: `{"path":"b.go"}`},
		},
	},
	{
		name:  "xml/arguments double-encoded",
		why:   "the same string-versus-object confusion, inside a tag",
		msg:   parse.Message{Content: "<tool_call>{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}</tool_call>"},
		route: parse.RouteXMLTag,
		calls: []wantCall{{name: "read_file", args: `{"path":"a.go"}`, encoding: parse.ArgsJSONString}},
	},
	{
		name: "xml/tag never closed",
		why:  "truncation again; the same refusal as an unclosed fence",
		msg:  parse.Message{Content: "<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}"},
		kind: parse.KindUnclosedTag,
	},
	{
		name: "xml/single quotes inside the tag",
		why:  "malformation is route-independent",
		msg:  parse.Message{Content: "<tool_call>{'name':'read_file'}</tool_call>"},
		kind: parse.KindInvalidJSON,
	},

	// ---------------------------------------------------------------
	// Nothing extracted, with the reason kept.
	// ---------------------------------------------------------------
	{
		name:   "none/plain prose",
		why:    "the ordinary case: the model answered rather than called",
		msg:    parse.Message{Content: "The bug is in main.go: the loop never terminates."},
		kind:   parse.KindNoCall,
		noCall: true,
	},
	{
		name:   "none/empty message",
		why:    "an empty completion is a no-call, not a crash",
		msg:    parse.Message{},
		kind:   parse.KindNoCall,
		noCall: true,
	},
	{
		name:   "none/unlabelled fence holding a call",
		why:    "the model got the content right and the envelope wrong; repair can say exactly that",
		msg:    parse.Message{Content: "```\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}\n```"},
		kind:   parse.KindUnlabelledFence,
		noCall: true,
	},
	{
		name:   "none/json-labelled fence holding a call",
		why:    "```json is not a tool fence — a model illustrating a payload in prose writes exactly that",
		msg:    parse.Message{Content: "Here's what I'd send:\n```json\n{\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}\n```"},
		kind:   parse.KindUnlabelledFence,
		noCall: true,
	},
	{
		name:   "none/bare call in prose",
		why:    "no fence at all, which is what a model does when the format instruction falls out of context",
		msg:    parse.Message{Content: "I'll call {\"tool\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}} next."},
		kind:   parse.KindUnfencedCall,
		noCall: true,
	},
	{
		name:   "none/prose about tool calls",
		why:    "talking about the format is not using it",
		msg:    parse.Message{Content: "You can call a tool by writing a ```tool fence — but I don't need one here."},
		kind:   parse.KindNoCall,
		noCall: true,
	},
}
