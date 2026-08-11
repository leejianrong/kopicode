package apiresp

import "testing"

func TestEncodeSuccess(t *testing.T) {
	got, err := Encode(Response{
		RequestID: "req-1",
		Status:    "ok",
		Items:     []Item{{Name: "widget", Count: 2}},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := `{"request_id":"req-1","status":"ok","items":[{"name":"widget","count":2}]}`
	if got != want {
		t.Errorf("Encode =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEncodeFailure(t *testing.T) {
	got, err := Encode(Response{
		RequestID: "req-2",
		Status:    "error",
		Error:     "upstream timed out",
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := `{"request_id":"req-2","status":"error","error":"upstream timed out"}`
	if got != want {
		t.Errorf("Encode =\n  %s\nwant\n  %s", got, want)
	}
}
