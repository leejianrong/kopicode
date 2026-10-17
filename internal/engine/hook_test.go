//go:build unix

package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
	kh "github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/tools"
)

// ADR-0036, KOP-203: the pre_tool hook, the shared runner and the HookRan event.
// Driven the way the other engine tests are: a scripted provider, a temp tree, and
// assertions on journal events, the tree and what the model was sent.

// hookScript writes a shell script outside the working tree and returns the argv
// that runs it.
func hookScript(t *testing.T, body string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"/bin/sh", path}
}

func withHooks(h kh.Hooks) harnessOption {
	return func(c *engine.Config) { c.Selection.Config.Hooks = h }
}

func withHookTimeout(d time.Duration) harnessOption {
	return func(c *engine.Config) { c.HookTimeout = d }
}

func writeCall(id, path string) scriptedReply {
	return scriptedReply{calls: []wireCall{nativeCall(id, tools.ToolWriteFile,
		`{"path":`+strconv.Quote(path)+`,"content":"x\n"}`)}}
}

func shellCall(id, command string) scriptedReply {
	return scriptedReply{calls: []wireCall{nativeCall(id, tools.ToolRunShell, `{"command":`+strconv.Quote(command)+`}`)}}
}

// runHooked runs replies (plus a closing prose turn) under hooks and returns the
// harness and the requests the model was sent.
func runHooked(t *testing.T, hooks kh.Hooks, replies []scriptedReply, opts ...harnessOption) (*harness, *recordingProvider) {
	t.Helper()
	replies = append(replies, scriptedReply{text: "Done."})
	p := script(t, replies, oneAttemptPerTurn(len(replies)))
	spy := &recordingProvider{inner: p}
	opts = append([]harnessOption{withMaxTurns(len(replies) + 1), withProvider(spy), withHooks(hooks)}, opts...)
	h := newHarness(t, p, nil, opts...)
	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return h, spy
}

func exists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

func TestAPassingPreToolHookLetsTheCallRun(t *testing.T) {
	record := filepath.Join(t.TempDir(), "stdin.json")
	hooks := kh.Hooks{PreTool: hookScript(t, `cat > `+record+`; echo checked`)}
	h, _ := runHooked(t, hooks, []scriptedReply{writeCall("c1", "out.txt")})

	if !exists(h.root, "out.txt") {
		t.Fatal("a hook that exited 0 stopped the call")
	}
	ran := sole[journal.HookRan](t, h.events())
	if ran.Point != "pre_tool" || ran.Outcome != "passed" || ran.ExitCode != 0 || ran.Tool != "write_file" || ran.CallID != "c1" {
		t.Errorf("HookRan = %+v", ran)
	}
	if ran.Stdout.Inline != "checked\n" || !slices.Equal(ran.Command, hooks.PreTool) {
		t.Errorf("HookRan output/command = %q %v", ran.Stdout.Inline, ran.Command)
	}

	// What the command was told: the point, the call and where it happened, and
	// the arguments as the model wrote them.
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Hook      string          `json:"hook"`
		SessionID string          `json:"session_id"`
		Turn      int             `json:"turn"`
		CallID    string          `json:"call_id"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("stdin was not JSON: %v\n%s", err, raw)
	}
	if in.Hook != "pre_tool" || in.SessionID != "session-under-test" || in.Turn != 1 || in.CallID != "c1" || in.Tool != "write_file" ||
		!strings.Contains(string(in.Arguments), `"out.txt"`) {
		t.Errorf("stdin = %s", raw)
	}
}

func TestExitTwoDeniesWithStderrAsTheReason(t *testing.T) {
	hooks := kh.Hooks{PreTool: hookScript(t, `echo "migrations/ is read only" >&2; exit 2`)}
	h, spy := runHooked(t, hooks, []scriptedReply{writeCall("c1", "migrations/001.sql")})

	if exists(h.root, "migrations/001.sql") {
		t.Fatal("a denied call ran")
	}
	evs := h.events()
	ran := sole[journal.HookRan](t, evs)
	if ran.Outcome != "denied" || ran.ExitCode != 2 || strings.TrimSpace(ran.Stderr.Inline) != "migrations/ is read only" {
		t.Errorf("HookRan = %+v", ran)
	}
	res := sole[journal.ToolResult](t, evs)
	if res.ErrorKind == "" || !strings.Contains(res.Output.Inline, "migrations/ is read only") {
		t.Errorf("ToolResult = %+v", res)
	}
	if msg := toolMessage(t, spy.requests[1], "c1"); !strings.Contains(msg, "pre_tool hook denied") || !strings.Contains(msg, "migrations/ is read only") {
		t.Errorf("the model was told %q", msg)
	}
}

func TestADeniedCallWithNoReasonSaysSo(t *testing.T) {
	hooks := kh.Hooks{PreTool: hookScript(t, `exit 2`)}
	_, spy := runHooked(t, hooks, []scriptedReply{writeCall("c1", "a.txt")})
	if msg := toolMessage(t, spy.requests[1], "c1"); !strings.Contains(msg, "gave no reason") {
		t.Errorf("the model was told %q", msg)
	}
}

func TestAHookThatCrashesDeniesTheCall(t *testing.T) {
	for name, body := range map[string]string{
		"exit 1":           `echo broke >&2; exit 1`,
		"exit 127":         `exit 127`,
		"killed by signal": `kill -9 $$`,
	} {
		t.Run(name, func(t *testing.T) {
			h, spy := runHooked(t, kh.Hooks{PreTool: hookScript(t, body)}, []scriptedReply{writeCall("c1", "a.txt")})
			if exists(h.root, "a.txt") {
				t.Fatal("a failing hook let the call run: a gate that is open when it is broken")
			}
			ran := sole[journal.HookRan](t, h.events())
			if ran.Outcome != "failed" {
				t.Errorf("outcome = %q, want failed", ran.Outcome)
			}
			if msg := toolMessage(t, spy.requests[1], "c1"); !strings.Contains(msg, "hook failed") || !strings.Contains(msg, "blocks the call") {
				t.Errorf("the model was told %q", msg)
			}
		})
	}
}

func TestAHookThatCannotStartDeniesTheCall(t *testing.T) {
	h, _ := runHooked(t, kh.Hooks{PreTool: []string{"/nonexistent/kopicode-hook"}}, []scriptedReply{writeCall("c1", "a.txt")})
	if exists(h.root, "a.txt") {
		t.Fatal("an unstartable hook let the call run")
	}
	ran := sole[journal.HookRan](t, h.events())
	if ran.Outcome != "failed" || ran.ExitCode != -1 || !strings.Contains(ran.Detail, "could not be started") {
		t.Errorf("HookRan = %+v", ran)
	}
}

func TestATimedOutHookDeniesTheCallAndItsProcessGroupIsGone(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// A grandchild in the hook's process group: killing only the shell would
	// leave it running.
	hooks := kh.Hooks{PreTool: hookScript(t, `sleep 300 & echo $! > `+pidFile+`; wait`)}
	t.Cleanup(func() { // whatever the assertions find, do not leave a sleeper behind
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	start := time.Now()
	h, _ := runHooked(t, hooks, []scriptedReply{writeCall("c1", "a.txt")}, withHookTimeout(300*time.Millisecond))
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("the exchange took %s: the timeout did not end the hook's process group", took)
	}

	if exists(h.root, "a.txt") {
		t.Fatal("a timed-out hook let the call run")
	}
	ran := sole[journal.HookRan](t, h.events())
	if ran.Outcome != "timed_out" || ran.ExitCode != -1 || !strings.Contains(ran.Detail, "timed out") {
		t.Errorf("HookRan = %+v", ran)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the hook never recorded its grandchild: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the hook's grandchild %d outlived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTheHookTimeoutIsBounded(t *testing.T) {
	// A configured timeout is honoured up to five minutes and no further, and
	// zero is the default. The bound is read off the journal's own detail.
	for _, tc := range []struct {
		set  time.Duration
		want string
	}{{0, "30s"}, {time.Hour, "5m0s"}} {
		clock := &stepHookClock{}
		clock.fireTimer = true
		h, _ := runHooked(t, kh.Hooks{PreTool: hookScript(t, `sleep 300 & wait`)},
			[]scriptedReply{writeCall("c1", "a.txt")}, withHookTimeout(tc.set), func(c *engine.Config) { c.HookClock = clock })
		ran := sole[journal.HookRan](t, h.events())
		if ran.Outcome != "timed_out" || !strings.Contains(ran.Detail, "timed out after "+tc.want) {
			t.Errorf("timeout %v: HookRan = %+v, want a bound of %s", tc.set, ran, tc.want)
		}
	}
}

// timedOutBound runs a pre_tool hook that never returns under hooks, with
// the clock firing the timeout at once, and returns the bound the journal says it
// timed out after.
func timedOutBound(t *testing.T, hooks kh.Hooks, opts ...harnessOption) string {
	t.Helper()
	clock := &stepHookClock{fireTimer: true}
	hooks.PreTool = hookScript(t, `sleep 300 & wait`)
	h, _ := runHooked(t, hooks, []scriptedReply{writeCall("c1", "a.txt")},
		append(opts, func(c *engine.Config) { c.HookClock = clock })...)
	ran := sole[journal.HookRan](t, h.events())
	_, bound, ok := strings.Cut(ran.Detail, "timed out after ")
	if ran.Outcome != "timed_out" || !ok {
		t.Fatalf("HookRan = %+v, want a timeout", ran)
	}
	bound, _, _ = strings.Cut(bound, " ")
	return strings.TrimRight(bound, ";,.")
}

// ADR-0036 decision 6, KOP-208: hook_timeout_seconds is honoured, 300 seconds is
// the ceiling, and Config.HookTimeout stays the seam a test overrides it with.
func TestAConfiguredHookTimeoutIsHonoured(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    string
	}{{1, "1s"}, {45, "45s"}, {300, "5m0s"}, {3600, "5m0s"}} {
		if got := timedOutBound(t, kh.Hooks{TimeoutSeconds: tc.seconds}); got != tc.want {
			t.Errorf("hook_timeout_seconds = %d: bound = %s, want %s", tc.seconds, got, tc.want)
		}
	}
	if got := timedOutBound(t, kh.Hooks{}); got != "30s" {
		t.Errorf("unset: bound = %s, want the 30s default", got)
	}
	if got := timedOutBound(t, kh.Hooks{TimeoutSeconds: 45}, withHookTimeout(7*time.Second)); got != "7s" {
		t.Errorf("Config.HookTimeout set as well: bound = %s, want the seam's 7s", got)
	}
}

func TestTheHookTimeoutCeilingIsTheConfigKeysCeiling(t *testing.T) {
	if got := time.Duration(kh.MaxHookTimeoutSeconds) * time.Second; got != engine.MaxHookTimeout {
		t.Errorf("harness allows %v and the engine caps at %v; they are one bound", got, engine.MaxHookTimeout)
	}
}

func TestAHookedSessionRecordsItsTimeoutOnSessionStarted(t *testing.T) {
	p := script(t, []scriptedReply{{text: "Done."}}, oneAttemptPerTurn(1))
	h := newHarness(t, p, nil, withHooks(kh.Hooks{Stop: []string{"guard"}, TimeoutSeconds: 90}))
	if got := sole[journal.SessionStarted](t, h.events()).Hooks; got == nil || got.TimeoutSeconds != 90 {
		t.Errorf("SessionStarted.Hooks = %+v, want a 90 second timeout", got)
	}
	h = newHarness(t, p, nil, withHooks(kh.Hooks{Stop: []string{"guard"}}))
	if got := sole[journal.SessionStarted](t, h.events()).Hooks; got == nil || got.TimeoutSeconds != 0 {
		t.Errorf("SessionStarted.Hooks = %+v, want no timeout recorded when unset", got)
	}
}

// stepHookClock is a clock a test drives: every Now is 250ms after the last, and
// NewTimer either never fires or fires at once for the first timer (the timeout).
type stepHookClock struct {
	n         int
	fireTimer bool
	timers    int
}

func (c *stepHookClock) Now() time.Time {
	c.n++
	return time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC).Add(time.Duration(c.n) * 250 * time.Millisecond)
}

func (c *stepHookClock) NewTimer(time.Duration) (<-chan time.Time, func()) {
	c.timers++
	ch := make(chan time.Time, 1)
	if c.fireTimer && c.timers == 1 {
		ch <- time.Time{}
	}
	return ch, func() {}
}

func TestHookDurationComesFromTheInjectedClock(t *testing.T) {
	h, _ := runHooked(t, kh.Hooks{PreTool: hookScript(t, `exit 0`)}, []scriptedReply{writeCall("c1", "a.txt")},
		func(c *engine.Config) { c.HookClock = &stepHookClock{} })
	if ran := sole[journal.HookRan](t, h.events()); ran.DurationMS != 250 {
		t.Errorf("DurationMS = %d, want 250", ran.DurationMS)
	}
}

// Stricter of the two (ADR-0036 decision 4): a hook that passes is not an allow.
func TestAPassingHookDoesNotOverrideAConsentDenial(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	hooks := kh.Hooks{PreTool: hookScript(t, `echo x >> `+marker)}
	p := script(t, []scriptedReply{shellCall("c1", "touch made.txt"), {text: "Done."}}, oneAttemptPerTurn(2))
	h := newHarness(t, p, nil, withMaxTurns(3), withHooks(hooks))
	h.policy.verdict = permission.VerdictDeny
	h.policy.reason = "not today"
	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if exists(h.root, "made.txt") {
		t.Fatal("a passing hook let a consent-denied command run")
	}
	if len(h.policy.requests) != 1 {
		t.Errorf("the consent gate was asked %d times, want 1: a hook pass is not an allow", len(h.policy.requests))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the hook did not run")
	}
}

func TestAHookDenialStopsACallConsentWouldAllow(t *testing.T) {
	hooks := kh.Hooks{PreTool: hookScript(t, `echo "no shell today" >&2; exit 2`)}
	p := script(t, []scriptedReply{shellCall("c1", "touch made.txt"), {text: "Done."}}, oneAttemptPerTurn(2))
	h := newHarness(t, p, nil, withMaxTurns(3), withHooks(hooks))
	h.policy.verdict = permission.VerdictAllow
	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if exists(h.root, "made.txt") {
		t.Fatal("a hook denial did not stop a call consent would allow")
	}
	if len(h.policy.requests) != 0 {
		t.Errorf("the person was asked to approve a call a hook had already refused: %d request(s)", len(h.policy.requests))
	}
}

func TestHookToolsScopesThePreToolHook(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	hooks := kh.Hooks{PreTool: hookScript(t, `echo x >> `+marker), Tools: []string{"write_file"}}
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("c1", tools.ToolListDir, `{"path":"."}`)}},
		writeCall("c2", "a.txt"),
	}
	h, _ := runHooked(t, hooks, replies)
	ran := payloadsOf[journal.HookRan](t, h.events())
	if len(ran) != 1 || ran[0].Tool != "write_file" {
		t.Errorf("hook runs = %+v, want one, for write_file only", ran)
	}
}

func TestWithNoHooksNothingRunsAndTheRecordIsAsBefore(t *testing.T) {
	h, _ := runHooked(t, kh.Hooks{}, []scriptedReply{writeCall("c1", "a.txt")})
	evs := h.events()
	if got := payloadsOf[journal.HookRan](t, evs); len(got) != 0 {
		t.Errorf("HookRan events with no hooks: %+v", got)
	}
	if sole[journal.SessionStarted](t, evs).Hooks != nil {
		t.Error("SessionStarted records hooks for a session with none")
	}
	raw, err := os.ReadFile(filepath.Join(h.root, ".kopicode", "sessions", "session-under-test.jsonl"))
	if err == nil && strings.Contains(string(raw), `"hooks"`) {
		t.Error("the unhooked journal carries a hooks field")
	}
}

func TestAHookedSessionRecordsItsHooksOnSessionStarted(t *testing.T) {
	hooks := kh.Hooks{
		PreTool: []string{"guard"}, Stop: []string{"make", "lint"}, Tools: []string{"write_file"},
	}
	p := script(t, []scriptedReply{{text: "Done."}}, oneAttemptPerTurn(1))
	h := newHarness(t, p, nil, withHooks(hooks))
	got := sole[journal.SessionStarted](t, h.events()).Hooks
	if got == nil || !slices.Equal(got.PreTool, hooks.PreTool) || !slices.Equal(got.Stop, hooks.Stop) ||
		!slices.Equal(got.Tools, hooks.Tools) || len(got.PostTool) != 0 {
		t.Errorf("SessionStarted.Hooks = %+v", got)
	}
}

func TestAHookRunsInTheTreeWithNeitherTheKeyNorAGitOverride(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test-secret")
	t.Setenv("KOPICODE_PROVIDER_API_KEY", "sk-custom-secret")
	t.Setenv("GIT_DIR", "/nonexistent/.git")
	t.Setenv("GIT_WORK_TREE", "/nonexistent")
	t.Setenv("GOFLAGS", "-mod=vendor")
	dump := filepath.Join(t.TempDir(), "env")
	hooks := kh.Hooks{PreTool: hookScript(t, `env > `+dump+`; pwd -P >> `+dump)}
	h, _ := runHooked(t, hooks, []scriptedReply{writeCall("c1", "a.txt")})

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, banned := range []string{"OPENROUTER_API_KEY", "KOPICODE_PROVIDER_API_KEY", "sk-or-test-secret", "GIT_DIR", "GIT_WORK_TREE", "GOFLAGS"} {
		if strings.Contains(out, banned) {
			t.Errorf("the hook inherited %s", banned)
		}
	}
	if !strings.Contains(out, "PATH=") {
		t.Error("the hook has no PATH, so it cannot find its own toolchain")
	}
	root, err := filepath.EvalSymlinks(h.root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), root) {
		t.Errorf("the hook ran in %q, want the tree root %q", out[strings.LastIndex(out, "\n")-len(root):], root)
	}
}

func TestAHookRelativeToTheTreeRootIsFoundThere(t *testing.T) {
	// A command named relative to the root resolves against the directory the
	// hook runs in, not the process's.
	files := map[string]string{"scripts/guard": "#!/bin/sh\nexit 2\n"}
	p := script(t, []scriptedReply{writeCall("c1", "a.txt"), {text: "Done."}}, oneAttemptPerTurn(2))
	h := newHarness(t, p, files, withMaxTurns(3), withHooks(kh.Hooks{PreTool: []string{"./scripts/guard"}}))
	if err := os.Chmod(filepath.Join(h.root, "scripts", "guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if ran := sole[journal.HookRan](t, h.events()); ran.Outcome != "denied" {
		t.Errorf("HookRan = %+v", ran)
	}
}

func TestHookOutputIsRecordedWholeAndCappedForTheModel(t *testing.T) {
	// 100 KiB on stderr: past the journal's spill threshold and past the cap the
	// model's view is held to.
	hooks := kh.Hooks{PreTool: hookScript(t, `i=0; while [ $i -lt 2000 ]; do echo "line $i of the hook's complaint, padded out to be long enough"; i=$((i+1)); done >&2; exit 2`)}
	h, spy := runHooked(t, hooks, []scriptedReply{writeCall("c1", "a.txt")},
		func(c *engine.Config) { c.Selection.Config.OutputCap = 4096 })

	ran := sole[journal.HookRan](t, h.events())
	if ran.Stderr.Size < 100*1024 {
		t.Fatalf("test setup: stderr is %d bytes, want at least 100 KiB", ran.Stderr.Size)
	}
	if !strings.Contains(ran.Stderr.Inline, "line 0 ") || !strings.Contains(ran.Stderr.Inline, "line 1999 ") ||
		int64(len(ran.Stderr.Inline)) != ran.Stderr.Size {
		t.Errorf("the record does not hold the whole output: inline %d bytes of %d", len(ran.Stderr.Inline), ran.Stderr.Size)
	}
	msg := toolMessage(t, spy.requests[1], "c1")
	if len(msg) > 4096+512 || !strings.Contains(msg, "omitted") {
		t.Errorf("the model was shown %d bytes, want it capped near 4096 with a note", len(msg))
	}
}

func TestAnInterruptedHookIsNotAPassAndIsNotLeftRunning(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	hooks := kh.Hooks{PreTool: hookScript(t, `sleep 300 & echo $! > `+pidFile+`; wait`)}
	p := script(t, []scriptedReply{writeCall("c1", "a.txt"), {text: "Done."}}, oneAttemptPerTurn(2))
	h := newHarness(t, p, nil, withMaxTurns(3), withHooks(hooks))

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for !exists(filepath.Dir(pidFile), "pid") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_, _ = h.eng.Run(ctx, "go")

	if exists(h.root, "a.txt") {
		t.Fatal("a cancelled hook let the call run")
	}
	ran := sole[journal.HookRan](t, h.events())
	if ran.Outcome != "failed" || !strings.Contains(ran.Detail, "cancelled") {
		t.Errorf("HookRan = %+v", ran)
	}
}

// Replay and resume read events; they never run a hook (ADR-0036 decision 8).
func TestResumeNeverRerunsAHook(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	sel := resumeSelection(t)
	sel.Config.Hooks = kh.Hooks{PreTool: hookScript(t, `echo x >> `+marker)}
	dir := t.TempDir()
	ctx := context.Background()

	first := &fakeReplyProvider{bodies: [][]byte{callBody(t, writeCall("c1", "a.txt").calls[0]), proseBody(t, "Done.")}}
	s1, err := engine.Open(ctx, engine.Options{Dir: dir, Selection: sel, SessionID: "hook-resume", Provider: first, Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Run(ctx, "write it"); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, marker); got != 1 {
		t.Fatalf("the hook ran %d times in the first session, want 1", got)
	}

	second := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "Still done.")}}
	s2, err := engine.Open(ctx, engine.Options{Dir: dir, Selection: sel, SessionID: "hook-resume", Resume: true, Provider: second, Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(ctx) }()
	if _, err := s2.Run(ctx, "anything else?"); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, marker); got != 1 {
		t.Errorf("resuming reran the hook: %d runs, want 1", got)
	}
}

// callBody is a one-shot reply body carrying one native tool call.
func callBody(t *testing.T, call wireCall) []byte {
	t.Helper()
	body := wireBody{
		ID: "gen-1", Object: "chat.completion", Model: testModelID, Provider: testServedBy,
		Choices: []wireChoice{{Message: wireMessage{Role: "assistant", ToolCalls: []wireCall{call}}, FinishReason: "tool_calls"}},
		Usage:   wireUsage{Prompt: 5, Completion: 3, Total: 8},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func lines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

// hook_tools accepts every tool the engine can dispatch, so a tool added later
// cannot be one a person is unable to gate (KOP-202's strict validation).
func TestHookToolsAcceptsEveryToolTheEngineCanRun(t *testing.T) {
	for _, name := range engine.ToolNamesForTest() {
		home := t.TempDir()
		t.Setenv("KOPICODE_HOME", home)
		if err := os.WriteFile(filepath.Join(home, "config.toml"),
			[]byte("pre_tool_hook = [\"true\"]\nhook_tools = [\""+name+"\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := kh.Resolve(t.TempDir(), kh.Overrides{UserConfig: true}); err != nil {
			t.Errorf("hook_tools = [%q] is refused: %v", name, err)
		}
	}
}
