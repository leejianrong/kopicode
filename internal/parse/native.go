package parse

import (
	"bytes"
	"encoding/json"
)

// extractNative reads route (a): the OpenAI-style tool_calls array the provider
// already destructured off the response.
//
// This route is tried first because it is the one that carries a provider
// call id and needs no scanning. An empty array is not a failure — it is how
// every text reply looks — but a populated array whose entries are broken is,
// because the model reached for its native channel and got it wrong, which is
// precisely the finding this package exists to surface.
func extractNative(native []NativeCall) ([]ToolCall, *Error) {
	if len(native) == 0 {
		return nil, nil
	}

	out := make([]ToolCall, 0, len(native))
	for i, nc := range native {
		call, err := nativeToolCall(i, nc)
		if err != nil {
			return nil, err
		}
		out = append(out, call)
	}
	return out, nil
}

func nativeToolCall(index int, nc NativeCall) (ToolCall, *Error) {
	raw, marshalErr := json.Marshal(struct {
		Index     int             `json:"index"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}{Index: index, ID: nc.ID, Name: nc.Name, Arguments: nc.Arguments})
	if marshalErr != nil {
		// Only reachable if Arguments holds invalid JSON, which is itself the
		// failure worth reporting rather than hiding behind a marshal error.
		raw = []byte(nc.Name)
	}

	if nc.Name == "" {
		return ToolCall{}, &Error{
			Kind:    KindMissingName,
			Route:   RouteNative,
			Detail:  "a native tool call carried no function name",
			Snippet: snippet(string(raw)),
		}
	}

	// The arguments value is reused wholesale so that native and text routes
	// agree on what "arguments" means, down to the double-encoded string the
	// OpenAI wire format specifies.
	fields := callFields{args: []json.RawMessage{nc.Arguments}}
	if len(bytes.TrimSpace(nc.Arguments)) == 0 {
		fields.args = nil
	}
	args, encoding, err := soleArguments(RouteNative, fields, raw)
	if err != nil {
		return ToolCall{}, err
	}

	return ToolCall{
		ID:          nc.ID,
		Name:        nc.Name,
		Arguments:   args,
		ArgEncoding: encoding,
		Raw:         string(raw),
	}, nil
}
