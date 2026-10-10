package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// TestServeShutdownEndsAStdioProcess: over stdio server.shutdown ends the process
// even though stdin is still open, after answering.
func TestServeShutdownEndsAStdioProcess(t *testing.T) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- serveWith(t.Context(), pr, &out, &errb, engine.Options{}, remoteConsentTimeout) }()

	if _, err := io.WriteString(pw, `{"jsonrpc":"2.0","id":1,"method":"server.shutdown"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != exitSuccess {
			t.Errorf("exit = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after server.shutdown with stdin still open")
	}
	if got := out.String(); !strings.Contains(got, `"shutdown":true`) || !strings.Contains(got, `"sessions_closed":0`) {
		t.Errorf("answer = %q", got)
	}
}
