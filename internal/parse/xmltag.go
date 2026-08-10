package parse

import "strings"

// The XML route's delimiters. Lowercase and exact: this is the Hermes/Qwen
// convention the target model was trained on, and loosening the match to
// anything tag-shaped would start reading the model's prose about tool calls as
// tool calls.
//
// The <function=…><parameter=…> variant some Qwen builds emit is deliberately
// not handled here. It is a different grammar rather than a looser spelling of
// this one, and adding it on a guess — before KAN-777's recordings show which
// form the pinned model actually produces — would be building a parser for
// output nobody has seen.
const (
	xmlOpenTag  = "<tool_call>"
	xmlCloseTag = "</tool_call>"
)

// tagBlock is one <tool_call> block found in the reply text.
type tagBlock struct {
	content string
	raw     string
}

// extractXMLTag reads route (c): a <tool_call>…</tool_call> block wrapping JSON.
//
// As with the fenced route, an opened tag that never closes fails rather than
// being read from whatever followed it.
func extractXMLTag(text string) ([]ToolCall, *Error) {
	if !strings.Contains(text, xmlOpenTag) {
		return nil, nil
	}

	blocks, unclosed := scanTags(text)
	if unclosed != "" {
		return nil, &Error{
			Kind:    KindUnclosedTag,
			Route:   RouteXMLTag,
			Detail:  "a " + xmlOpenTag + " tag was opened and never closed",
			Snippet: snippet(unclosed),
		}
	}

	var out []ToolCall
	for _, b := range blocks {
		calls, err := decodeStream(RouteXMLTag, b.content, b.raw)
		if err != nil {
			return nil, err
		}
		out = append(out, calls...)
	}
	return out, nil
}

// scanTags returns every closed <tool_call> block, and the text following an
// opening tag that was never closed.
func scanTags(text string) (blocks []tagBlock, unclosed string) {
	rest := text
	for {
		open := strings.Index(rest, xmlOpenTag)
		if open < 0 {
			return blocks, ""
		}
		after := rest[open+len(xmlOpenTag):]

		end := strings.Index(after, xmlCloseTag)
		if end < 0 {
			return blocks, xmlOpenTag + after
		}

		content := after[:end]
		blocks = append(blocks, tagBlock{
			content: content,
			raw:     xmlOpenTag + content + xmlCloseTag,
		})
		rest = after[end+len(xmlCloseTag):]
	}
}
