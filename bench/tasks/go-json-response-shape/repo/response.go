// Package apiresp defines the JSON envelope the service returns.
package apiresp

import "encoding/json"

// Response is the envelope every endpoint returns. The client is written
// against snake_case field names.
type Response struct {
	RequestID string `json:"requestId"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	Items     []Item `json:"items,omitempty"`
}

// Item is one record in a successful response.
type Item struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Encode renders r as JSON with no trailing newline.
func Encode(r Response) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
