package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// A load test of one serve process holding many sessions at once (ADR-0030, KOP-182):
// what N concurrent sessions cost in memory, goroutines and file descriptors, whether
// their turns really overlap, and how long a request that must never queue behind a
// turn (server.sessions) takes while every session is mid-turn. The provider is a
// local mock that waits loadTurnDelay before it answers, so no token is spent and the
// turns overlap measurably. The numbers are written up in docs/serve-load.md.
//
// It runs the stdio transport, the same code a socket serves, so it needs no unix.
// It is skipped under -short, and by default runs 10 sessions; KOPICODE_LOAD_SESSIONS
// (a comma list, for example 20,100,200) states the sizes to measure and logs the
// report with -v.

const loadTurnDelay = 150 * time.Millisecond

// loadClient drives serve over pipes and timestamps every message as it arrives, so a
// latency is measured at the client and includes the server's own write path.
type loadClient struct {
	inW  *io.PipeWriter
	exit chan int

	mu      sync.Mutex
	nextID  int
	replies map[int]loadReply
	firstEv map[string]time.Time // session -> first session.event after its mark
	marked  map[string]bool
	events  int
	decoded chan struct{}
	writeMu sync.Mutex
}

type loadReply struct {
	at    time.Time
	isErr bool
	raw   map[string]any
}

func startLoadServe(t *testing.T, base engine.Options) *loadClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &loadClient{
		inW: inW, exit: make(chan int, 1), decoded: make(chan struct{}),
		replies: map[int]loadReply{}, firstEv: map[string]time.Time{}, marked: map[string]bool{},
	}
	go func() {
		defer close(c.decoded)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
		for sc.Scan() {
			now := time.Now()
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				continue
			}
			c.mu.Lock()
			switch {
			case m["method"] == methodSessionEvent:
				c.events++
				params, _ := m["params"].(map[string]any)
				if id, _ := params["session"].(string); c.marked[id] {
					if _, seen := c.firstEv[id]; !seen {
						c.firstEv[id] = now
					}
				}
			case m["method"] == nil && m["id"] != nil:
				if f, ok := m["id"].(float64); ok {
					c.replies[int(f)] = loadReply{at: now, isErr: m["error"] != nil, raw: m}
				}
			}
			c.mu.Unlock()
		}
	}()
	go func() {
		c.exit <- serveWith(t.Context(), inR, outW, io.Discard, base, remoteConsentTimeout)
		_ = outW.Close()
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return c
}

// send writes one request and returns its id and the time it was written.
func (c *loadClient) send(method string, params any) (int, time.Time) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	at := time.Now()
	_, _ = c.inW.Write(append(b, '\n'))
	return id, at
}

func (c *loadClient) mark(session string) {
	c.mu.Lock()
	c.marked[session] = true
	delete(c.firstEv, session)
	c.mu.Unlock()
}

// await returns the reply to id, failing the test if it does not come.
func (c *loadClient) await(t *testing.T, id int) loadReply {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		c.mu.Lock()
		r, ok := c.replies[id]
		c.mu.Unlock()
		if ok {
			return r
		}
	}
	t.Fatalf("no reply to request %d", id)
	return loadReply{}
}

// loadSnapshot is the process's resources at a moment, after a GC so the heap is
// what is live rather than what is waiting to be collected.
type loadSnapshot struct {
	heap       uint64
	goroutines int
	fds        int // -1 where /proc is not there
}

func snapshot() loadSnapshot {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fds := -1
	if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
		fds = len(ents)
	}
	return loadSnapshot{heap: ms.HeapAlloc, goroutines: runtime.NumGoroutine(), fds: fds}
}

func quantile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*q))]
}

func loadSizes(t *testing.T) []int {
	t.Helper()
	raw := os.Getenv("KOPICODE_LOAD_SESSIONS")
	if raw == "" {
		return []int{10}
	}
	var sizes []int
	for _, f := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			t.Fatalf("KOPICODE_LOAD_SESSIONS %q: %q is not a positive number", raw, f)
		}
		sizes = append(sizes, n)
	}
	return sizes
}

func TestServeLoadManyConcurrentSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("a load test; skipped under -short")
	}
	for _, n := range loadSizes(t) {
		t.Run(fmt.Sprintf("%d_sessions", n), func(t *testing.T) { loadServe(t, n) })
	}
}

func loadServe(t *testing.T, n int) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	body := sseBody("done")
	var inflight, peak int
	var pmu sync.Mutex
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pmu.Lock()
		inflight++
		peak = max(peak, inflight)
		pmu.Unlock()
		time.Sleep(loadTurnDelay)
		pmu.Lock()
		inflight--
		pmu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(provider.Close)

	before := snapshot()
	c := startLoadServe(t, engine.Options{ProviderBaseURL: provider.URL})

	// 1. Open n sessions at once. Each start runs a turn, so with the provider
	// waiting loadTurnDelay a process that ran them one at a time would take n times
	// as long as one.
	ids := make([]int, n)
	start := time.Now()
	for i := range n {
		ids[i], _ = c.send(methodSessionStart, startPayload(fmt.Sprintf("s%d", i), t.TempDir(), "hello"))
	}
	for i := range n {
		if r := c.await(t, ids[i]); r.isErr {
			t.Fatalf("session.start s%d: %v", i, r.raw["error"])
		}
	}
	openAll := time.Since(start)
	pmu.Lock()
	peakAtOpen := peak
	peak = 0
	pmu.Unlock()

	opened := snapshot()

	// 2. Every session starts a second turn at once. While they run, ask the table
	// and one session's usage, which must answer without waiting for a turn, and
	// time how long each session takes to say its first event.
	sent := make(map[string]time.Time, n)
	turnIDs := make([]int, n)
	for i := range n {
		sess := fmt.Sprintf("s%d", i)
		c.mark(sess)
		turnIDs[i], sent[sess] = c.send(methodSessionSubmit, map[string]any{"session": sess, "prompt": "again"})
	}
	turnsSentAt := time.Now()
	time.Sleep(loadTurnDelay / 3) // inside the turns
	var probes []time.Duration
	for range 5 {
		id, at := c.send(methodServerSessions, nil)
		r := c.await(t, id)
		probes = append(probes, r.at.Sub(at))
		time.Sleep(5 * time.Millisecond)
	}
	var busy int
	tableID, _ := c.send(methodServerSessions, nil)
	res, _ := c.await(t, tableID).raw["result"].(map[string]any)
	rows, _ := res["sessions"].([]any)
	for _, row := range rows {
		if m, _ := row.(map[string]any); m["state"] == "running" {
			busy++
		}
	}
	for i := range n {
		if r := c.await(t, turnIDs[i]); r.isErr {
			t.Fatalf("session.submit s%d: %v", i, r.raw["error"])
		}
	}
	allTurns := time.Since(turnsSentAt)

	c.mu.Lock()
	var evLat []time.Duration
	for sess, at := range c.firstEv {
		evLat = append(evLat, at.Sub(sent[sess]))
	}
	events := c.events
	c.mu.Unlock()
	if len(evLat) != n {
		t.Errorf("%d of %d sessions announced an event for their second turn", len(evLat), n)
	}
	pmu.Lock()
	peakAtTurns := peak
	pmu.Unlock()

	// 3. Close the process the way a client going away does and time it.
	closeAt := time.Now()
	_ = c.inW.Close()
	select {
	case code := <-c.exit:
		if code != exitSuccess {
			t.Errorf("exit = %d", code)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not exit after its input closed")
	}
	shutdown := time.Since(closeAt)
	<-c.decoded
	var after loadSnapshot
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		after = snapshot()
		if after.goroutines <= before.goroutines+8 || time.Now().After(deadline) {
			break
		}
	}

	perHeap := int64(opened.heap-before.heap) / int64(n)
	perG := float64(opened.goroutines-before.goroutines) / float64(n)
	perFD := float64(opened.fds-before.fds) / float64(n)
	t.Logf("sessions=%d\n"+
		"  open all (each runs a turn of %v): %v   peak concurrent provider requests %d\n"+
		"  second turn on all, to the last reply: %v   peak %d   running when asked: %d of %d\n"+
		"  heap: +%d KiB per open session (%d KiB total)   goroutines: +%.1f per session (%d total)   fds: +%.1f per session\n"+
		"  first event of a turn after submit: p50 %v  p95 %v  max %v   events seen %d\n"+
		"  server.sessions while all are mid-turn: p50 %v  max %v\n"+
		"  shutdown of all %d sessions: %v   goroutines after %d (before %d)   fds after %d (before %d)",
		n, loadTurnDelay, openAll.Round(time.Millisecond), peakAtOpen,
		allTurns.Round(time.Millisecond), peakAtTurns, busy, n,
		perHeap/1024, (opened.heap-before.heap)/1024, perG, opened.goroutines, perFD,
		quantile(evLat, .5).Round(time.Microsecond), quantile(evLat, .95).Round(time.Microsecond),
		quantile(evLat, 1).Round(time.Microsecond), events,
		quantile(probes, .5).Round(time.Microsecond), quantile(probes, 1).Round(time.Microsecond),
		n, shutdown.Round(time.Millisecond), after.goroutines, before.goroutines, after.fds, before.fds)

	// What a regression would break, with room for a slow machine:
	// Opening a session is serial today: session.start opens on the read loop (see
	// docs/serve-load.md), so the open phase is not asserted to overlap, only to
	// stay within a generous per-session bound.
	if per := openAll / time.Duration(n); per > time.Second {
		t.Errorf("opening %d sessions took %v, %v each", n, openAll, per)
	}
	// Turns on sessions already open do overlap: that is what one worker per session is for.
	if peakAtTurns < n*9/10 {
		t.Errorf("only %d of %d sessions' second turns were in the provider at once", peakAtTurns, n)
	}
	if busy < n/2 {
		t.Errorf("server.sessions saw only %d of %d sessions running while every one was mid-turn", busy, n)
	}
	// server.sessions never queues behind a turn: it answers in far less than one.
	if worst := quantile(probes, 1); worst > loadTurnDelay {
		t.Errorf("server.sessions took %v with every session mid-turn; a turn takes %v", worst, loadTurnDelay)
	}
	// Nothing is left behind.
	if after.goroutines > before.goroutines+8 {
		t.Errorf("goroutines after shutdown: %d, before: %d", after.goroutines, before.goroutines)
	}
	if before.fds >= 0 && after.fds > before.fds+8 {
		t.Errorf("open files after shutdown: %d, before: %d", after.fds, before.fds)
	}
}

// TestServeSessionsMayBePolledDuringATurn: server.sessions reads each session's turn
// count and usage from another goroutine while the turn runs. The race detector is
// the assertion (KOP-182 found Engine.Turns reading a counter the loop was writing).
func TestServeSessionsMayBePolledDuringATurn(t *testing.T) {
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	body := sseBody("done")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(provider.Close)

	c := startLoadServe(t, engine.Options{ProviderBaseURL: provider.URL})
	startID, _ := c.send(methodSessionStart, startPayload("s1", t.TempDir(), "hello"))
	for i := 0; i < 40; i++ {
		id, _ := c.send(methodServerSessions, nil)
		c.await(t, id)
		time.Sleep(3 * time.Millisecond)
	}
	if r := c.await(t, startID); r.isErr {
		t.Fatalf("session.start: %v", r.raw["error"])
	}
}
