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

// nativeRaw renders one native call as the JSON object that becomes its
// [ToolCall.Raw].
//
// This route is the only place in the package that *builds* Raw rather than
// slicing it out of the reply text, and it has to: the provider destructures
// the tool_calls array off the response, and on the streaming path the
// arguments arrive as fragments that internal/provider concatenates, so no
// contiguous run of provider bytes survives to hand through. Re-encoding is the
// honest second best, and then it must change nothing but the framing.
//
// Hence the encoder rather than json.Marshal. encoding/json rewrites `<`, `>`
// and `&` as <, > and & by default, which is valid JSON over
// different bytes — so a call whose arguments held a comparison, an HTML
// fragment or a shell `&&` reached the journal's ToolCallRequested not
// byte-identical to what the model sent, in the one field defined by being
// byte-identical (KAN-884). It is the same trap journal.Marshal avoids on the
// way out and provider.jsonString avoids on the way in; this package can reach
// neither, and must not — internal/parse does not import the journal.
func nativeRaw(index int, nc NativeCall) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Index     int             `json:"index"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}{Index: index, ID: nc.ID, Name: nc.Name, Arguments: nc.Arguments}); err != nil {
		return nil, err
	}
	// Encode terminates the value with a newline. Raw is a value, not a line.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func nativeToolCall(index int, nc NativeCall) (ToolCall, *Error) {
	raw, marshalErr := nativeRaw(index, nc)
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
