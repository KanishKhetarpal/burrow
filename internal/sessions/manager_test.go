package sessions

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"burrow/internal/audit"
	"burrow/internal/config"
	"burrow/internal/pty"
)

// fakeProc is a scriptable stand-in for a ConPTY process, so the manager's
// lifecycle can be tested without launching Claude Code.
type fakeProc struct {
	mu       sync.Mutex
	out      chan []byte
	written  strings.Builder
	resizes  []pty.Size
	pid      int
	exitCode int
	killed   bool
	closed   bool
	exited   chan struct{}
	exitOnce sync.Once
}

func newFakeProc(pid int) *fakeProc {
	return &fakeProc{out: make(chan []byte, 64), pid: pid, exited: make(chan struct{})}
}

// emit queues output as if the child had printed it.
func (f *fakeProc) emit(s string) { f.out <- []byte(s) }

// finish makes the process exit with the given code.
func (f *fakeProc) finish(code int) {
	f.exitOnce.Do(func() {
		f.mu.Lock()
		f.exitCode = code
		f.mu.Unlock()
		close(f.exited)
		close(f.out)
	})
}

func (f *fakeProc) Read(p []byte) (int, error) {
	b, ok := <-f.out
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, b)
	return n, nil
}

func (f *fakeProc) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written.Write(p)
	return len(p), nil
}

func (f *fakeProc) Resize(s pty.Size) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, s)
	return nil
}

func (f *fakeProc) PID() int { return f.pid }

func (f *fakeProc) Wait() (int, error) {
	<-f.exited
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exitCode, nil
}

func (f *fakeProc) Kill() error {
	f.mu.Lock()
	f.killed = true
	f.mu.Unlock()
	f.finish(1)
	return nil
}

func (f *fakeProc) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeProc) input() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.String()
}

func (f *fakeProc) wasKilled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed
}

// mgrFixture builds a manager whose starter hands out fake processes and
// records the pty.Config it was asked for.
type mgrFixture struct {
	m     *Manager
	cfg   *config.Config
	procs []*fakeProc
	calls []pty.Config
	mu    sync.Mutex
	log   []audit.Entry
	logMu sync.Mutex
}

func newMgrFixture(t *testing.T) *mgrFixture {
	t.Helper()
	dir := t.TempDir()
	f := &mgrFixture{
		cfg: &config.Config{
			Projects: []config.Project{{Name: "demo", Path: dir}, {Name: "scratch", Path: dir}},
			Claude: config.Claude{
				Binary:                "claude",
				DefaultMode:           "chat",
				DefaultPermissionMode: "default",
				ServerSpawn:           "same-dir",
				ServerCapacity:        8,
				MaxSessions:           3,
				RestrictedToken:       true,
				ScrollbackBytes:       64 << 10,
				ReadyTimeoutSeconds:   30,
			},
		},
	}
	f.m = NewManager(
		func() *config.Config { return f.cfg },
		func(e audit.Entry) {
			f.logMu.Lock()
			f.log = append(f.log, e)
			f.logMu.Unlock()
		},
		func(c pty.Config) (pty.Process, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, c)
			p := newFakeProc(1000 + len(f.procs))
			f.procs = append(f.procs, p)
			return p, nil
		},
	)
	t.Cleanup(func() {
		for _, p := range f.procList() {
			p.finish(0)
		}
	})
	return f
}

func (f *mgrFixture) procList() []*fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*fakeProc, len(f.procs))
	copy(out, f.procs)
	return out
}

func (f *mgrFixture) lastCall() pty.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return pty.Config{}
	}
	return f.calls[len(f.calls)-1]
}

func (f *mgrFixture) actions() []audit.Action {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	var out []audit.Action
	for _, e := range f.log {
		out = append(out, e.Action)
	}
	return out
}

// waitFor polls until cond holds or the deadline passes. The manager does its
// work on goroutines, so assertions need to wait rather than sleep a fixed
// amount.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStartChatSession(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo", Name: "demo-1200", Actor: "device:iPhone"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Status != StatusStarting {
		t.Errorf("Status = %q, want %q", s.Status, StatusStarting)
	}
	if s.Mode != ModeChat {
		t.Errorf("Mode = %q, want chat by default", s.Mode)
	}
	if !strings.HasPrefix(s.ID, "s_") {
		t.Errorf("ID = %q, want an s_ prefix", s.ID)
	}

	call := f.lastCall()
	if call.Path != "claude" {
		t.Errorf("binary = %q, want claude", call.Path)
	}
	if !call.Restricted {
		t.Error("the session was started without a restricted token")
	}
	want := []string{"--remote-control", "demo-1200"}
	if strings.Join(call.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", call.Args, want)
	}
	if call.Dir != f.cfg.Projects[0].Path {
		t.Errorf("cwd = %q, want the project path", call.Dir)
	}
}

func TestStartPassesNonDefaultPermissionMode(t *testing.T) {
	f := newMgrFixture(t)
	if _, err := f.m.Start(StartOptions{ProjectID: "p_demo", PermissionMode: "plan"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.lastCall().Args, " "), "--permission-mode plan") {
		t.Errorf("args = %v, want --permission-mode plan", f.lastCall().Args)
	}

	// The default mode is left off, so Claude's own default applies.
	if _, err := f.m.Start(StartOptions{ProjectID: "p_demo", PermissionMode: "default"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.lastCall().Args, " "), "--permission-mode") {
		t.Errorf("args = %v, want no --permission-mode for the default", f.lastCall().Args)
	}
}

// Off by default: only the owner's burrow.yaml can allow it.
func TestStartRefusesBypassPermissionsUnlessEnabled(t *testing.T) {
	f := newMgrFixture(t)
	_, err := f.m.Start(StartOptions{ProjectID: "p_demo", PermissionMode: "bypassPermissions"})
	if !errors.Is(err, config.ErrBypassDisabled) {
		t.Fatalf("err = %v, want ErrBypassDisabled", err)
	}
}

func TestStartServerModeArgs(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo", Name: "srv", Mode: ModeServer})
	if err != nil {
		t.Fatal(err)
	}
	if s.Spawn != SpawnSameDir {
		t.Errorf("Spawn = %q, want the configured same-dir", s.Spawn)
	}
	got := strings.Join(f.lastCall().Args, " ")
	for _, want := range []string{"remote-control", "--name srv", "--spawn same-dir", "--capacity 8"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q is missing %q", got, want)
		}
	}
}

func TestStartRejectsUnknownProject(t *testing.T) {
	f := newMgrFixture(t)
	if _, err := f.m.Start(StartOptions{ProjectID: "p_nope"}); !errors.Is(err, ErrBadProject) {
		t.Fatalf("Start with an unknown project = %v, want ErrBadProject", err)
	}
}

func TestStartEnforcesMaxSessions(t *testing.T) {
	f := newMgrFixture(t)
	for i := 0; i < f.cfg.Claude.MaxSessions; i++ {
		if _, err := f.m.Start(StartOptions{ProjectID: "p_demo"}); err != nil {
			t.Fatalf("Start %d: %v", i+1, err)
		}
	}
	_, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if !errors.Is(err, ErrMaxSessions) {
		t.Fatalf("Start past the cap = %v, want ErrMaxSessions", err)
	}

	// Ending one frees a slot.
	f.procList()[0].finish(0)
	waitFor(t, "a slot to free up", func() bool { return f.m.RunningCount() == f.cfg.Claude.MaxSessions-1 })
	if _, err := f.m.Start(StartOptions{ProjectID: "p_demo"}); err != nil {
		t.Fatalf("Start after one ended: %v", err)
	}
}

func TestDefaultSessionName(t *testing.T) {
	now := time.Date(2026, 10, 25, 23, 14, 0, 0, time.UTC)
	if got := defaultSessionName("demo-app", now); got != "demo-app-2314" {
		t.Errorf("defaultSessionName = %q, want demo-app-2314", got)
	}
}

func TestRemoteURLDetectionMarksReady(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	f.procList()[0].emit("Remote Control is on\r\n  https://claude.ai/code/s/XYZ789\r\n")

	waitFor(t, "the session to become ready", func() bool {
		got, _ := f.m.Get(s.ID)
		return got.Status == StatusReady
	})
	got, _ := f.m.Get(s.ID)
	if got.RemoteURL != "https://claude.ai/code/s/XYZ789" {
		t.Errorf("RemoteURL = %q, want the detected URL", got.RemoteURL)
	}
}

func TestPromptDetectionAndClearing(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	proc := f.procList()[0]
	proc.emit("Trust the files in this folder? [y/N] ")

	waitFor(t, "the prompt to surface", func() bool {
		got, _ := f.m.Get(s.ID)
		return got.Status == StatusNeedsInput && got.Prompt != nil
	})
	got, _ := f.m.Get(s.ID)
	if got.Prompt.Kind != PromptTrust {
		t.Errorf("Prompt.Kind = %q, want trust", got.Prompt.Kind)
	}

	// Answering clears it and reaches the child.
	if err := f.m.SendText(s.ID, "y\r", "device:iPhone"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	got, _ = f.m.Get(s.ID)
	if got.Prompt != nil {
		t.Errorf("Prompt = %+v after answering, want nil", got.Prompt)
	}
	if !strings.Contains(proc.input(), "y\r") {
		t.Errorf("the child received %q, want it to contain \"y\\r\"", proc.input())
	}
}

func TestSendKeyMapsNamedKeys(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.SendKey(s.ID, "ctrl_c", "device:iPhone"); err != nil {
		t.Fatalf("SendKey: %v", err)
	}
	if !strings.Contains(f.procList()[0].input(), "\x03") {
		t.Error("ctrl_c did not reach the child as 0x03")
	}
	if err := f.m.SendKey(s.ID, "f13", "device:iPhone"); err == nil {
		t.Error("SendKey accepted an unknown key name")
	}
}

func TestSendTextRefusedOnAStoppedSession(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	f.procList()[0].finish(0)
	waitFor(t, "the session to exit", func() bool {
		got, _ := f.m.Get(s.ID)
		return !got.Status.Running()
	})
	if err := f.m.SendText(s.ID, "hello\r", "device:iPhone"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("SendText to a stopped session = %v, want ErrNotRunning", err)
	}
}

func TestScrollbackAndSubscribe(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	proc := f.procList()[0]
	proc.emit("first chunk\r\n")
	waitFor(t, "scrollback to fill", func() bool {
		b, _ := f.m.Scrollback(s.ID)
		return strings.Contains(string(b), "first chunk")
	})

	out, events, cancel, err := f.m.Subscribe(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	proc.emit("second chunk\r\n")
	select {
	case b := <-out:
		if !strings.Contains(string(b), "second chunk") {
			t.Errorf("subscriber got %q, want the second chunk", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the subscriber received nothing")
	}

	// Exiting must reach subscribers as an event.
	proc.finish(0)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-events:
			if e.T == "exit" {
				if e.ExitCode == nil || *e.ExitCode != 0 {
					t.Errorf("exit event code = %v, want 0", e.ExitCode)
				}
				return
			}
		case <-deadline:
			t.Fatal("no exit event reached the subscriber")
		}
	}
}

func TestScrollbackIsCapped(t *testing.T) {
	f := newMgrFixture(t)
	f.cfg.Claude.ScrollbackBytes = 4096
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	proc := f.procList()[0]
	for i := 0; i < 20; i++ {
		proc.emit(strings.Repeat("x", 1000))
	}
	waitFor(t, "the cap to be reached", func() bool {
		b, _ := f.m.Scrollback(s.ID)
		return len(b) == 4096
	})
	b, _ := f.m.Scrollback(s.ID)
	if len(b) != 4096 {
		t.Errorf("scrollback length = %d, want it capped at 4096", len(b))
	}
}

func TestStopEscalatesAndMarksStopped(t *testing.T) {
	// Shorten the escalation so the test does not wait 15 seconds.
	origExit, origKill := stopExitDelay, stopKillDelay
	stopExitDelay, stopKillDelay = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { stopExitDelay, stopKillDelay = origExit, origKill })

	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	proc := f.procList()[0]

	if err := f.m.Stop(context.Background(), s.ID, "device:iPhone"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// A child that ignores everything must end up killed.
	if !proc.wasKilled() {
		t.Error("the child was not killed after it ignored /exit and Ctrl-C")
	}
	if in := proc.input(); !strings.Contains(in, "/exit") || !strings.Contains(in, "\x03") {
		t.Errorf("escalation sent %q, want /exit then Ctrl-C", in)
	}

	waitFor(t, "the session to be marked stopped", func() bool {
		got, _ := f.m.Get(s.ID)
		return got.Status == StatusStopped
	})
	got, _ := f.m.Get(s.ID)
	if got.EndedAt == nil {
		t.Error("EndedAt was not set")
	}
}

func TestStopReturnsEarlyWhenTheChildExits(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	proc := f.procList()[0]
	// Exit as soon as /exit arrives, as Claude Code would.
	go func() {
		for !strings.Contains(proc.input(), "/exit") {
			time.Sleep(5 * time.Millisecond)
		}
		proc.finish(0)
	}()

	start := time.Now()
	if err := f.m.Stop(context.Background(), s.ID, "device:iPhone"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Stop took %v, want it to return as soon as the child exited", took)
	}
	if proc.wasKilled() {
		t.Error("the child was killed although it exited on request")
	}
}

func TestFailedExitIsReportedAsFailed(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	f.procList()[0].finish(2)
	waitFor(t, "the failure to be recorded", func() bool {
		got, _ := f.m.Get(s.ID)
		return got.Status == StatusFailed
	})
	got, _ := f.m.Get(s.ID)
	if got.ExitCode == nil || *got.ExitCode != 2 {
		t.Errorf("ExitCode = %v, want 2", got.ExitCode)
	}
}

func TestResumeContinuesTheConversation(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo", Name: "demo-1200"})
	if err != nil {
		t.Fatal(err)
	}
	f.procList()[0].finish(0)
	waitFor(t, "the session to exit", func() bool {
		got, _ := f.m.Get(s.ID)
		return !got.Status.Running()
	})

	resumed, err := f.m.Resume(s.ID, "device:iPhone")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != StatusStarting {
		t.Errorf("Status = %q, want starting", resumed.Status)
	}
	if resumed.EndedAt != nil || resumed.ExitCode != nil {
		t.Error("Resume left the previous exit recorded")
	}
	got := strings.Join(f.lastCall().Args, " ")
	if !strings.Contains(got, "--continue") {
		t.Errorf("resume args = %q, want --continue", got)
	}
	if !strings.Contains(got, "--remote-control demo-1200") {
		t.Errorf("resume args = %q, want the same session name", got)
	}
}

func TestResumeRefusedWhileRunning(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Resume(s.ID, "device:iPhone"); !errors.Is(err, ErrStillRuns) {
		t.Fatalf("Resume while running = %v, want ErrStillRuns", err)
	}
}

func TestDeleteOnlyWhenStopped(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Delete(s.ID); !errors.Is(err, ErrStillRuns) {
		t.Fatalf("Delete while running = %v, want ErrStillRuns", err)
	}
	f.procList()[0].finish(0)
	waitFor(t, "the session to exit", func() bool {
		got, _ := f.m.Get(s.ID)
		return !got.Status.Running()
	})
	if err := f.m.Delete(s.ID); err != nil {
		t.Fatalf("Delete after exit: %v", err)
	}
	if _, err := f.m.Get(s.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestKillAllStopsEverything(t *testing.T) {
	f := newMgrFixture(t)
	for i := 0; i < 3; i++ {
		if _, err := f.m.Start(StartOptions{ProjectID: "p_demo"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.m.KillAll("device:iPhone"); n != 3 {
		t.Errorf("KillAll killed %d, want 3", n)
	}
	waitFor(t, "every session to stop", func() bool { return f.m.RunningCount() == 0 })
	for i, p := range f.procList() {
		if !p.wasKilled() {
			t.Errorf("process %d was not killed", i)
		}
	}
}

func TestKeepAwakeCallbackTracksRunningCount(t *testing.T) {
	f := newMgrFixture(t)
	var mu sync.Mutex
	var seen []int
	f.m.OnRunningCountChange(func(n int) {
		mu.Lock()
		seen = append(seen, n)
		mu.Unlock()
	})

	if _, err := f.m.Start(StartOptions{ProjectID: "p_demo"}); err != nil {
		t.Fatal(err)
	}
	f.procList()[0].finish(0)

	waitFor(t, "the count to return to zero", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) >= 2 && seen[len(seen)-1] == 0
	})
	mu.Lock()
	defer mu.Unlock()
	if seen[0] != 1 {
		t.Errorf("first reported count = %d, want 1", seen[0])
	}
}

func TestListOrdersRunningFirst(t *testing.T) {
	f := newMgrFixture(t)
	first, _ := f.m.Start(StartOptions{ProjectID: "p_demo", Name: "a"})
	time.Sleep(2 * time.Millisecond)
	if _, err := f.m.Start(StartOptions{ProjectID: "p_demo", Name: "b"}); err != nil {
		t.Fatal(err)
	}

	// Stop the older one; it must sort below the running one.
	f.procList()[0].finish(0)
	waitFor(t, "the first session to exit", func() bool {
		got, _ := f.m.Get(first.ID)
		return !got.Status.Running()
	})

	list := f.m.List()
	if len(list) != 2 {
		t.Fatalf("List returned %d sessions, want 2", len(list))
	}
	if !list[0].Status.Running() {
		t.Errorf("List put a stopped session first: %+v", list[0])
	}
	if list[1].ID != first.ID {
		t.Errorf("List[1] = %q, want the stopped session %q", list[1].ID, first.ID)
	}
}

func TestResizeReachesTheConsole(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Resize(s.ID, 100, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	p := f.procList()[0]
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.resizes) != 1 || p.resizes[0].Cols != 100 || p.resizes[0].Rows != 40 {
		t.Errorf("resizes = %+v, want one 100x40", p.resizes)
	}
}

func TestAuditTrailCoversTheLifecycle(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo", Actor: "device:iPhone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.SendText(s.ID, "y\r", "device:iPhone"); err != nil {
		t.Fatal(err)
	}
	f.procList()[0].finish(0)
	waitFor(t, "the session to exit", func() bool {
		got, _ := f.m.Get(s.ID)
		return !got.Status.Running()
	})

	got := f.actions()
	for _, want := range []audit.Action{audit.ActionSessionStart, audit.ActionSessionInput} {
		found := false
		for _, a := range got {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("audit actions %v are missing %q", got, want)
		}
	}
}

func TestSendTextDoesNotLogTheText(t *testing.T) {
	f := newMgrFixture(t)
	s, err := f.m.Start(StartOptions{ProjectID: "p_demo"})
	if err != nil {
		t.Fatal(err)
	}
	secret := "my-api-key-should-not-be-logged"
	if err := f.m.SendText(s.ID, secret+"\r", "device:iPhone"); err != nil {
		t.Fatal(err)
	}
	f.logMu.Lock()
	defer f.logMu.Unlock()
	for _, e := range f.log {
		if strings.Contains(e.Target, secret) {
			t.Fatalf("the audit entry recorded the typed text: %+v", e)
		}
		for _, v := range e.Meta {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				t.Fatalf("the audit meta recorded the typed text: %+v", e)
			}
		}
	}
}

func TestProjectID(t *testing.T) {
	if got := ProjectID(config.Project{Name: "DEMO-APP"}); got != "p_demo-app" {
		t.Errorf("ProjectID = %q, want p_demo-app", got)
	}
}

// A launcher's CLAUDE_CONFIG_DIR picks the Claude login, and must survive a
// resume, or a resumed session would silently switch accounts.
func TestLauncherSetsClaudeConfigDirAndKeepsItOnResume(t *testing.T) {
	f := newMgrFixture(t)
	dir := t.TempDir()
	f.cfg.Launchers = []config.Launcher{{Name: "Personal work", Project: "demo", ClaudeConfigDir: dir}}

	s, err := f.m.Start(StartOptions{Launcher: "Personal work"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := f.lastCall().Env["CLAUDE_CONFIG_DIR"]; got != dir {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, dir)
	}
	if s.Launcher != "Personal work" {
		t.Fatalf("session launcher = %q", s.Launcher)
	}
	if got := sessionEnv(f.cfg, s)["CLAUDE_CONFIG_DIR"]; got != dir {
		t.Fatalf("resume env CLAUDE_CONFIG_DIR = %q, want %q", got, dir)
	}
}

// A launcher with its own path starts there without naming a project.
func TestLauncherPathStartsWithoutAProject(t *testing.T) {
	f := newMgrFixture(t)
	dir := t.TempDir()
	f.cfg.Launchers = []config.Launcher{{Name: "Start personal work", Path: dir}}

	s, err := f.m.Start(StartOptions{Launcher: "Start personal work"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.CWD != dir || f.lastCall().Dir != dir {
		t.Fatalf("cwd = %q / %q, want %q", s.CWD, f.lastCall().Dir, dir)
	}
}
