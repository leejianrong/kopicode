//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// sockClient is a client of `serve --listen`: it reads every line the server
// sends into a list the test polls, as serveHarness does for stdio.
type sockClient struct {
	t    *testing.T
	conn net.Conn
	gone chan struct{} // closed when the server ended the connection

	mu  sync.Mutex
	all []map[string]any
}

func dialSocket(t *testing.T, path string) *sockClient {
	t.Helper()
	var conn net.Conn
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if conn, err = net.Dial("unix", path); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("dialing %s: %v", path, err)
	}
	c := &sockClient{t: t, conn: conn, gone: make(chan struct{})}
	go func() {
		defer close(c.gone)
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				c.mu.Lock()
				c.all = append(c.all, m)
				c.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return c
}

func (c *sockClient) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("writing to the socket: %v", err)
	}
}

func (c *sockClient) await(what string, match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		c.mu.Lock()
		for _, m := range c.all {
			if match(m) {
				c.mu.Unlock()
				return m
			}
		}
		c.mu.Unlock()
	}
	c.t.Fatalf("timed out waiting for %s", what)
	return nil
}

func (c *sockClient) awaitResponse(id int) map[string]any {
	c.t.Helper()
	return c.await("a response", func(m map[string]any) bool { return m["method"] == nil && numEq(m["id"], id) })
}

func (c *sockClient) awaitRequest(method string) map[string]any {
	c.t.Helper()
	return c.await(method, func(m map[string]any) bool { return m["method"] == method && m["id"] != nil })
}

func (c *sockClient) sawResponse(id int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.all {
		if m["method"] == nil && numEq(m["id"], id) {
			return true
		}
	}
	return false
}

func (c *sockClient) awaitGone() {
	c.t.Helper()
	select {
	case <-c.gone:
	case <-time.After(10 * time.Second):
		c.t.Fatal("the server did not end this connection")
	}
}

// listening is a running `serve --listen`.
type listening struct {
	path   string
	cancel context.CancelFunc
	done   chan struct{} // closed once serveListen has returned, with code set
	code   int
	stderr *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// shortDir is a temp directory with a path short enough for a unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startListening(t *testing.T, base engine.Options, path string) *listening {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &listening{path: path, cancel: cancel, done: make(chan struct{}), stderr: &syncBuffer{}}
	go func() {
		defer close(l.done)
		l.code = serveListen(ctx, path, l.stderr, base, remoteConsentTimeout)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-l.done:
		case <-time.After(10 * time.Second):
		}
	})
	return l
}

func (l *listening) stop(t *testing.T) int {
	t.Helper()
	l.cancel()
	select {
	case <-l.done:
		return l.code
	case <-time.After(10 * time.Second):
		t.Fatal("serve --listen did not exit after it was told to stop")
		return -1
	}
}

func sessionRowsOf(t *testing.T, c *sockClient, id int) map[string]map[string]any {
	t.Helper()
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": methodServerSessions})
	res, _ := c.awaitResponse(id)["result"].(map[string]any)
	rows := map[string]map[string]any{}
	list, _ := res["sessions"].([]any)
	for _, r := range list {
		row, _ := r.(map[string]any)
		rows[row["session"].(string)] = row
	}
	return rows
}

// TestListenSessionsOutliveTheirClient: a client starts a session and goes away;
// the next client finds it open, asks what happened, and carries on with it.
func TestListenSessionsOutliveTheirClient(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("first"), sseBody("second"))
	path := filepath.Join(shortDir(t), "s.sock")
	startListening(t, engine.Options{ProviderBaseURL: srv.URL}, path)

	a := dialSocket(t, path)
	a.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "hello")})
	if r := a.awaitResponse(1); r["error"] != nil {
		t.Fatalf("session.start: %v", r["error"])
	}
	_ = a.conn.Close()

	b := dialSocket(t, path)
	row := sessionRowsOf(t, b, 1)["s1"]
	if row == nil || row["state"] != "idle" || row["last_stop"] != "completed" {
		t.Fatalf("after the first client left, s1 is %v", row)
	}
	b.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": methodSessionEvents, "params": map[string]any{"session": "s1"}})
	res, _ := b.awaitResponse(2)["result"].(map[string]any)
	if n := len(seqsOf(t, res["events"])); n < 4 {
		t.Errorf("the replay holds %d events, want the first client's whole turn", n)
	}
	b.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": methodSessionSubmit,
		"params": map[string]any{"session": "s1", "prompt": "again"}})
	if r := b.awaitResponse(3); r["error"] != nil {
		t.Fatalf("session.submit from the second client: %v", r["error"])
	}
}

// TestListenANewConnectionReplacesTheOldOne: only the latest client is served,
// and the one before it is disconnected.
func TestListenANewConnectionReplacesTheOldOne(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	startListening(t, engine.Options{}, path)

	a := dialSocket(t, path)
	a.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodServerHello})
	a.awaitResponse(1)

	b := dialSocket(t, path)
	a.awaitGone()
	b.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodServerHello})
	res, _ := b.awaitResponse(1)["result"].(map[string]any)
	features, _ := res["features"].([]any)
	found := false
	for _, f := range features {
		found = found || f == "serve.listen"
	}
	if !found {
		t.Errorf("server.hello over the socket lacks serve.listen: %v", features)
	}
}

// TestListenResendsPendingRequestsToTheNewClient: a turn waiting on a consent
// answer survives its client being replaced; the new client is asked under the
// same id, and answering lets the turn finish.
func TestListenResendsPendingRequestsToTheNewClient(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t,
		sseToolCall("call-1", "run_shell", `{"command":"echo hi"}`),
		sseBody("done"),
	)
	path := filepath.Join(shortDir(t), "s.sock")
	startListening(t, engine.Options{ProviderBaseURL: srv.URL}, path)

	a := dialSocket(t, path)
	a.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": map[string]any{"session": "s1", "dir": t.TempDir(), "prompt": "run it",
			"consent_mode": consentModeRemoteInteractive}})
	first := a.awaitRequest(methodConsentRequest)

	b := dialSocket(t, path)
	a.awaitGone()
	again := b.awaitRequest(methodConsentRequest)
	if again["id"] != first["id"] || !strings.Contains(toJSON(again["params"]), "echo hi") {
		t.Fatalf("the new client was sent %v, want the pending %v", again, first)
	}
	if row := sessionRowsOf(t, b, 5)["s1"]; row["state"] != "awaiting_consent" || row["pending_request"] != first["id"] {
		t.Errorf("while the new client is asked: %v", row)
	}

	b.send(map[string]any{"jsonrpc": "2.0", "id": again["id"], "result": map[string]any{"answer": "allow"}})
	// Each ask needs its own id: a response is found by id, so a reused one
	// would keep returning the first answer.
	id := 6
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); id++ {
		if sessionRowsOf(t, b, id)["s1"]["state"] == "idle" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	row := sessionRowsOf(t, b, id+1)["s1"]
	if row["state"] != "idle" || row["last_stop"] != "completed" {
		t.Fatalf("after the new client answered: %v", row)
	}
	// The start's response belonged to the first client, which is gone: the
	// second must not receive an answer to an id it never used.
	if b.sawResponse(1) {
		t.Error("the replaced client's response reached its successor")
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestListenSocketIsOwnerOnly(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	startListening(t, engine.Options{}, path)
	dialSocket(t, path)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket mode = %v, want a socket with 0600", fi.Mode())
	}
}

func TestListenRefusesAPathALiveProcessHolds(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	startListening(t, engine.Options{}, path)
	live := dialSocket(t, path)

	var stderr strings.Builder
	if code := serveListen(context.Background(), path, &stderr, engine.Options{}, remoteConsentTimeout); code != exitUsage {
		t.Errorf("second serve on a held path exited %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "holds this socket") {
		t.Errorf("stderr = %q", stderr.String())
	}
	// The refusal did not touch the live process's client.
	live.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodServerHello})
	live.awaitResponse(1)
}

func TestListenReplacesAStaleSocketButNotAFile(t *testing.T) {
	dir := shortDir(t)

	// A crash leaves the socket file behind, with nothing holding the lock.
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	l := startListening(t, engine.Options{}, stale)
	c := dialSocket(t, stale)
	c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodServerHello})
	c.awaitResponse(1)
	if code := l.stop(t); code != exitSuccess {
		t.Errorf("exit = %d", code)
	}

	// A regular file at the path is somebody's data: refuse, and leave it.
	file := filepath.Join(dir, "data")
	if err := os.WriteFile(file, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if code := serveListen(context.Background(), file, &stderr, engine.Options{}, remoteConsentTimeout); code != exitUsage {
		t.Errorf("serve over a regular file exited %d, want %d", code, exitUsage)
	}
	if got, _ := os.ReadFile(file); string(got) != "keep me" {
		t.Errorf("the file was overwritten: %q", got)
	}
}

// TestListenStoppingClosesSessionsAndRemovesTheSocket: the way a supervisor ends
// the process is a signal, and the sessions are closed properly: each
// session_ended is written.
func TestListenStoppingClosesSessionsAndRemovesTheSocket(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	srv := scriptedProvider(t, sseBody("done"))
	path := filepath.Join(shortDir(t), "s.sock")
	l := startListening(t, engine.Options{ProviderBaseURL: srv.URL}, path)

	c := dialSocket(t, path)
	c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodSessionStart,
		"params": startPayload("s1", t.TempDir(), "hi")})
	record, _ := c.awaitResponse(1)["result"].(map[string]any)["record"].(string)

	if code := l.stop(t); code != exitSuccess {
		t.Errorf("exit = %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket is still there (%v)", err)
	}
	raw, err := os.ReadFile(filepath.Join(record, "events.jsonl"))
	if err != nil || !strings.Contains(string(raw), `"SessionEnded"`) {
		t.Errorf("the session was not closed with a session_ended (%v)", err)
	}
	c.awaitGone()
	if strings.Contains(l.stderr.String(), "reading the serve input") {
		t.Errorf("a client closed by the shutdown was reported as an error: %s", l.stderr.String())
	}
}

var _ io.Writer = (*syncBuffer)(nil)
