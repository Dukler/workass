package scheduledmessage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type sent struct {
	entry Entry
	text  string
}

type recorder struct {
	mu   sync.Mutex
	sent []sent
	err  error
}

func (r *recorder) Send(_ context.Context, e Entry, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, sent{e, text})
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

type rig struct {
	t     *testing.T
	dir   string
	clock *fakeClock
	rec   *recorder
	s     *Scheduler
	probe Probes
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		t:     t,
		dir:   t.TempDir(),
		clock: &fakeClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)},
		rec:   &recorder{},
	}
	r.reload()
	return r
}

func (r *rig) reload() {
	r.t.Helper()
	s, err := New(Options{StateDir: r.dir, Now: r.clock.Now, Probes: r.probe, Send: r.rec.Send})
	if err != nil {
		r.t.Fatal(err)
	}
	r.s = s
}

func mustParse(t *testing.T, params map[string]any) Request {
	t.Helper()
	req, err := ParseRequest(params)
	if err != nil {
		t.Fatalf("ParseRequest(%v): %v", params, err)
	}
	return req
}

func after(op string, seconds float64) map[string]any {
	return map[string]any{"message": "ping", "operation_id": op, "after_seconds": seconds}
}

func TestAfterSecondsFiresExactlyOnceAtTheRightTime(t *testing.T) {
	r := newRig(t)
	e, err := r.s.Schedule("tab", "chat", mustParse(t, after("op1", 90)))
	if err != nil {
		t.Fatal(err)
	}
	if e.Condition.Kind != "at" || !e.Condition.At.Equal(r.clock.Now().Add(90*time.Second)) {
		t.Fatalf("condition = %+v", e.Condition)
	}
	if !strings.HasPrefix(e.ID, "sched-") || len(e.ID) != len("sched-")+16 {
		t.Fatalf("id = %q", e.ID)
	}
	ctx := context.Background()
	r.clock.Advance(89 * time.Second)
	r.s.Tick(ctx)
	if r.rec.count() != 0 {
		t.Fatal("fired early")
	}
	r.clock.Advance(time.Second)
	r.s.Tick(ctx)
	r.s.Tick(ctx)
	if r.rec.count() != 1 {
		t.Fatalf("sent %d times", r.rec.count())
	}
	got := r.rec.sent[0]
	if got.entry.ID != e.ID || got.text != Text(got.entry) || !strings.HasSuffix(got.text, "\n\nping") {
		t.Fatalf("sent %+v", got)
	}
	list := r.s.List("tab", "chat")
	if len(list) != 1 || list[0].Status != StatusFired || list[0].SettledAt == nil {
		t.Fatalf("list = %+v", list)
	}
}

func TestAtBounds(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	for name, at := range map[string]time.Time{
		"past":   now.Add(-time.Minute),
		"now":    now,
		"beyond": now.Add(7*24*time.Hour + time.Second),
	} {
		req := mustParse(t, map[string]any{"message": "m", "operation_id": "op-" + name, "at": at.Format(time.RFC3339)})
		if _, err := r.s.Schedule("tab", "chat", req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	edge := mustParse(t, map[string]any{"message": "m", "operation_id": "edge", "at": now.Add(7 * 24 * time.Hour).Format(time.RFC3339)})
	if _, err := r.s.Schedule("tab", "chat", edge); err != nil {
		t.Fatalf("exactly 7 days: %v", err)
	}
}

func TestParseRequestValidation(t *testing.T) {
	ok := func(extra map[string]any) map[string]any {
		p := map[string]any{"message": "m", "operation_id": "op"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	abs := filepath.Join(t.TempDir(), "f")
	good := []map[string]any{
		ok(map[string]any{"at": "2026-10-16T18:30:00Z"}),
		ok(map[string]any{"after_seconds": 1.0}),
		ok(map[string]any{"after_seconds": 604800.0}),
		ok(map[string]any{"when_tcp_up": "127.0.0.1:8080"}),
		ok(map[string]any{"when_file_exists": abs, "expires_after_seconds": 60.0}),
		ok(map[string]any{"when_pid_exits": 4242.0, "expires_after_seconds": 604800.0}),
	}
	for _, p := range good {
		if _, err := ParseRequest(p); err != nil {
			t.Errorf("%v: %v", p, err)
		}
	}
	bad := map[string]map[string]any{
		"no condition":         ok(nil),
		"two conditions":       ok(map[string]any{"after_seconds": 5.0, "when_tcp_up": "h:1"}),
		"empty message":        {"message": "  ", "operation_id": "op", "after_seconds": 5.0},
		"huge message":         {"message": strings.Repeat("x", 32*1024+1), "operation_id": "op", "after_seconds": 5.0},
		"no operation":         {"message": "m", "after_seconds": 5.0},
		"long operation":       {"message": "m", "operation_id": strings.Repeat("o", 201), "after_seconds": 5.0},
		"after zero":           ok(map[string]any{"after_seconds": 0.0}),
		"after too big":        ok(map[string]any{"after_seconds": 604801.0}),
		"after fractional":     ok(map[string]any{"after_seconds": 1.5}),
		"after string":         ok(map[string]any{"after_seconds": "5"}),
		"bad at":               ok(map[string]any{"at": "tomorrow"}),
		"bad addr":             ok(map[string]any{"when_tcp_up": "localhost"}),
		"relative file":        ok(map[string]any{"when_file_exists": "rel/path"}),
		"pid 1":                ok(map[string]any{"when_pid_exits": 1.0}),
		"pid zero":             ok(map[string]any{"when_pid_exits": 0.0}),
		"expiry on after":      ok(map[string]any{"after_seconds": 5.0, "expires_after_seconds": 120.0}),
		"expiry on at":         ok(map[string]any{"at": "2026-10-16T18:30:00Z", "expires_after_seconds": 120.0}),
		"expiry too small":     ok(map[string]any{"when_pid_exits": 5.0, "expires_after_seconds": 59.0}),
		"expiry too big":       ok(map[string]any{"when_pid_exits": 5.0, "expires_after_seconds": 604801.0}),
		"message not a string": {"message": 5.0, "operation_id": "op", "after_seconds": 5.0},
	}
	for name, p := range bad {
		if _, err := ParseRequest(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	req := mustParse(t, ok(map[string]any{"when_pid_exits": 7.0}))
	if req.ExpiresAfter != 24*time.Hour {
		t.Errorf("default expiry = %v", req.ExpiresAfter)
	}
}

func TestOperationIDIdempotency(t *testing.T) {
	r := newRig(t)
	first, err := r.s.Schedule("tab", "chat", mustParse(t, after("op", 60)))
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(10 * time.Second)
	again, err := r.s.Schedule("tab", "chat", mustParse(t, after("op", 60)))
	if err != nil || again.ID != first.ID || !again.Condition.At.Equal(first.Condition.At) {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if _, err := r.s.Schedule("tab", "chat", mustParse(t, after("op", 61))); err == nil ||
		err.Error() != "operation_id was already used for a different scheduled message" {
		t.Fatalf("conflict err = %v", err)
	}
	other, err := r.s.Schedule("tab", "chat2", mustParse(t, after("op", 61)))
	if err != nil || other.ID == first.ID {
		t.Fatalf("same op id in another chat must be independent: %+v, %v", other, err)
	}
	if n := len(r.s.List("tab", "chat")); n != 1 {
		t.Fatalf("list = %d", n)
	}
}

func TestPendingLimit(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 50; i++ {
		if _, err := r.s.Schedule("tab", "chat", mustParse(t, after(fmt.Sprint("op", i), 60))); err != nil {
			t.Fatalf("#%d: %v", i, err)
		}
	}
	if _, err := r.s.Schedule("tab", "chat", mustParse(t, after("op-51", 60))); err == nil {
		t.Fatal("51st accepted")
	}
	if _, err := r.s.Schedule("tab", "other", mustParse(t, after("op-51", 60))); err != nil {
		t.Fatalf("limit must be per chat: %v", err)
	}
	// An idempotent retry of an existing one still works at the limit.
	if _, err := r.s.Schedule("tab", "chat", mustParse(t, after("op0", 60))); err != nil {
		t.Fatalf("retry at limit: %v", err)
	}
}

func TestCancel(t *testing.T) {
	r := newRig(t)
	e, _ := r.s.Schedule("tab", "chat", mustParse(t, after("op", 60)))
	if _, err := r.s.Cancel("tab", "other", e.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other chat: %v", err)
	}
	if _, err := r.s.Cancel("tab", "chat", "sched-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	c, err := r.s.Cancel("tab", "chat", e.ID)
	if err != nil || c.Status != StatusCancelled || c.SettledAt == nil {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	r.clock.Advance(2 * time.Minute)
	r.s.Tick(context.Background())
	if r.rec.count() != 0 {
		t.Fatal("cancelled scheduled message fired")
	}
	again, err := r.s.Cancel("tab", "chat", e.ID)
	if err != nil || again.Status != StatusCancelled {
		t.Fatalf("second cancel = %+v, %v", again, err)
	}
	// A fired scheduled message stays fired.
	f, _ := r.s.Schedule("tab", "chat", mustParse(t, after("op2", 1)))
	r.clock.Advance(time.Second)
	r.s.Tick(context.Background())
	got, err := r.s.Cancel("tab", "chat", f.ID)
	if err != nil || got.Status != StatusFired {
		t.Fatalf("cancel fired = %+v, %v", got, err)
	}
}

func TestCancelChat(t *testing.T) {
	r := newRig(t)
	a, _ := r.s.Schedule("tab", "chat", mustParse(t, after("a", 60)))
	b, _ := r.s.Schedule("tab", "chat", mustParse(t, after("b", 60)))
	keep, _ := r.s.Schedule("tab", "other", mustParse(t, after("c", 60)))
	r.s.CancelChat("tab", "chat", "chat closed")
	for _, id := range []string{a.ID, b.ID} {
		c, _ := r.s.Cancel("tab", "chat", id)
		if c.Status != StatusCancelled || c.Error != "chat closed" {
			t.Fatalf("%s = %+v", id, c)
		}
	}
	if got := r.s.List("tab", "other"); len(got) != 1 || got[0].ID != keep.ID || got[0].Status != StatusPending {
		t.Fatalf("other chat touched: %+v", got)
	}
}

func TestTCPProbeUsesRealListener(t *testing.T) {
	r := newRig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens yet
	req := mustParse(t, map[string]any{"message": "up", "operation_id": "tcp", "when_tcp_up": addr})
	if _, err := r.s.Schedule("tab", "chat", req); err != nil {
		t.Fatal(err)
	}
	r.s.Tick(context.Background())
	if r.rec.count() != 0 {
		t.Fatal("fired with nothing listening")
	}
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot rebind %s: %v", addr, err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	r.s.Tick(context.Background())
	if r.rec.count() != 1 || !strings.Contains(r.rec.sent[0].text, "when "+addr+" accepts connections") {
		t.Fatalf("sent = %+v", r.rec.sent)
	}
}

func TestFileProbe(t *testing.T) {
	r := newRig(t)
	path := filepath.Join(t.TempDir(), "ready")
	req := mustParse(t, map[string]any{"message": "file", "operation_id": "f", "when_file_exists": path})
	if _, err := r.s.Schedule("tab", "chat", req); err != nil {
		t.Fatal(err)
	}
	r.s.Tick(context.Background())
	if r.rec.count() != 0 {
		t.Fatal("fired before the file existed")
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.s.Tick(context.Background())
	if r.rec.count() != 1 || !strings.Contains(r.rec.sent[0].text, "when "+path+" exists") {
		t.Fatalf("sent = %+v", r.rec.sent)
	}
}

func TestPIDProbeFlips(t *testing.T) {
	r := newRig(t)
	alive := true
	r.probe = Probes{PIDAlive: func(pid int) bool {
		if pid != 4242 {
			t.Errorf("pid = %d", pid)
		}
		return alive
	}}
	r.reload()
	req := mustParse(t, map[string]any{"message": "done", "operation_id": "p", "when_pid_exits": 4242.0})
	if _, err := r.s.Schedule("tab", "chat", req); err != nil {
		t.Fatal(err)
	}
	r.s.Tick(context.Background())
	if r.rec.count() != 0 {
		t.Fatal("fired while the process lived")
	}
	alive = false
	r.s.Tick(context.Background())
	if r.rec.count() != 1 || !strings.Contains(r.rec.sent[0].text, "when pid 4242 exits") {
		t.Fatalf("sent = %+v", r.rec.sent)
	}
}

func TestDefaultPIDProbe(t *testing.T) {
	if !defaultPIDAlive(os.Getpid()) {
		t.Fatal("own process reported dead")
	}
}

func TestConditionalExpires(t *testing.T) {
	r := newRig(t)
	r.probe = Probes{PIDAlive: func(int) bool { return true }}
	r.reload()
	req := mustParse(t, map[string]any{"message": "m", "operation_id": "p", "when_pid_exits": 99.0, "expires_after_seconds": 120.0})
	e, err := r.s.Schedule("tab", "chat", req)
	if err != nil {
		t.Fatal(err)
	}
	if !e.ExpiresAt.Equal(r.clock.Now().Add(2 * time.Minute)) {
		t.Fatalf("expiresAt = %v", e.ExpiresAt)
	}
	r.clock.Advance(119 * time.Second)
	r.s.Tick(context.Background())
	if got := r.s.List("tab", "chat")[0]; got.Status != StatusPending {
		t.Fatalf("status = %s", got.Status)
	}
	r.clock.Advance(time.Second)
	r.s.Tick(context.Background())
	got := r.s.List("tab", "chat")[0]
	if got.Status != StatusExpired || got.SettledAt == nil || r.rec.count() != 0 {
		t.Fatalf("got = %+v sent = %d", got, r.rec.count())
	}
}

func TestSendErrorRetriesThenFails(t *testing.T) {
	r := newRig(t)
	r.rec.err = errors.New("engine busy; token=abc123secret")
	e, _ := r.s.Schedule("tab", "chat", mustParse(t, after("op", 1)))
	r.clock.Advance(time.Second)
	for i := 1; i <= 2; i++ {
		r.s.Tick(context.Background())
		got := r.s.List("tab", "chat")[0]
		if got.Status != StatusPending || got.Attempts != i {
			t.Fatalf("attempt %d: %+v", i, got)
		}
	}
	r.s.Tick(context.Background())
	got := r.s.List("tab", "chat")[0]
	if got.ID != e.ID || got.Status != StatusFailed || got.Attempts != 3 || got.SettledAt == nil {
		t.Fatalf("final = %+v", got)
	}
	if strings.Contains(got.Error, "abc123secret") || !strings.Contains(got.Error, "engine busy") {
		t.Fatalf("error not redacted: %q", got.Error)
	}
	r.s.Tick(context.Background())
	if r.s.List("tab", "chat")[0].Attempts != 3 {
		t.Fatal("kept retrying after failure")
	}
}

func TestRetrySucceedsAfterTransientError(t *testing.T) {
	r := newRig(t)
	r.rec.err = errors.New("busy")
	r.s.Schedule("tab", "chat", mustParse(t, after("op", 1)))
	r.clock.Advance(time.Second)
	r.s.Tick(context.Background())
	r.rec.err = nil
	r.s.Tick(context.Background())
	if got := r.s.List("tab", "chat")[0]; got.Status != StatusFired || r.rec.count() != 1 {
		t.Fatalf("got = %+v sent = %d", got, r.rec.count())
	}
}

func TestChatGoneFailsAtOnce(t *testing.T) {
	r := newRig(t)
	r.rec.err = fmt.Errorf("deliver: %w", ErrChatGone)
	r.s.Schedule("tab", "chat", mustParse(t, after("op", 1)))
	r.clock.Advance(time.Second)
	r.s.Tick(context.Background())
	got := r.s.List("tab", "chat")[0]
	if got.Status != StatusFailed || got.Error != "chat deleted" {
		t.Fatalf("got = %+v", got)
	}
}

func TestReloadKeepsPendingAndMarksPastDueLate(t *testing.T) {
	r := newRig(t)
	late, _ := r.s.Schedule("tab", "chat", mustParse(t, after("late", 60)))
	future, _ := r.s.Schedule("tab", "chat", mustParse(t, after("future", 3600)))
	path := filepath.Join(t.TempDir(), "never")
	cond, _ := r.s.Schedule("tab", "chat", mustParse(t, map[string]any{"message": "c", "operation_id": "cond", "when_file_exists": path}))

	r.clock.Advance(10 * time.Minute) // the daemon "was not running"
	r.reload()
	byID := map[string]Entry{}
	for _, e := range r.s.List("tab", "chat") {
		byID[e.ID] = e
	}
	if len(byID) != 3 {
		t.Fatalf("entries = %+v", byID)
	}
	if got := byID[late.ID]; !got.Late || got.Status != StatusPending {
		t.Fatalf("late = %+v", got)
	}
	if byID[future.ID].Late || byID[cond.ID].Late {
		t.Fatal("only a past-due at scheduled message is late")
	}
	text := Text(byID[late.ID])
	want := "(This was due at " + late.Condition.At.Format(time.RFC3339) + "; Workass was not running then, so it is delivered late.)"
	if !strings.Contains(text, want) || !strings.HasSuffix(text, "\n\nping") {
		t.Fatalf("text = %q", text)
	}
	if strings.Contains(Text(byID[future.ID]), "delivered late") {
		t.Fatal("on-time text carries the late note")
	}
	r.s.Tick(context.Background())
	if r.rec.count() != 1 || !r.rec.sent[0].entry.Late || r.rec.sent[0].text != text {
		t.Fatalf("sent = %+v", r.rec.sent)
	}
	// Late survives another reload (persisted) and the settled one stays fired.
	r.reload()
	if got := r.s.List("tab", "chat"); len(got) != 3 {
		t.Fatalf("after second reload: %+v", got)
	}
	info, err := os.Stat(filepath.Join(r.dir, stateFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v, %v", info, err)
	}
}

func TestTextFormats(t *testing.T) {
	at := time.Date(2026, 10, 16, 18, 30, 0, 0, time.UTC)
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{ID: "sched-1", Message: "m", Condition: Condition{Kind: "at", At: at}}, "[Workass scheduled message sched-1 · at 2026-10-16T18:30:00Z]\n\nm"},
		{Entry{ID: "sched-1", Message: "m", Condition: Condition{Kind: "tcp", Addr: "h:1"}}, "[Workass scheduled message sched-1 · when h:1 accepts connections]\n\nm"},
		{Entry{ID: "sched-1", Message: "m", Condition: Condition{Kind: "file", Path: "/x"}}, "[Workass scheduled message sched-1 · when /x exists]\n\nm"},
		{Entry{ID: "sched-1", Message: "m", Condition: Condition{Kind: "pid", PID: 9}}, "[Workass scheduled message sched-1 · when pid 9 exits]\n\nm"},
	}
	for _, c := range cases {
		if got := Text(c.e); got != c.want {
			t.Errorf("Text = %q, want %q", got, c.want)
		}
	}
}

func TestCorruptFileMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0).UTC()}
	s, err := New(Options{StateDir: dir, Now: clock.Now, Send: (&recorder{}).Send})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.List("tab", "chat"); len(got) != 0 {
		t.Fatalf("not empty: %+v", got)
	}
	aside := path + ".corrupt-1800000000"
	if data, err := os.ReadFile(aside); err != nil || string(data) != "{not json" {
		t.Fatalf("aside: %q, %v", data, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt file still in place: %v", err)
	}
	// The scheduler is usable afterwards.
	if _, err := s.Schedule("tab", "chat", mustParse(t, after("op", 60))); err != nil {
		t.Fatal(err)
	}
}

func TestMissingFileIsEmptyAndDirIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	s, err := New(Options{StateDir: dir, Send: (&recorder{}).Send})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List("a", "b")) != 0 {
		t.Fatal("not empty")
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir: %v, %v", info, err)
	}
}

func TestRetentionTrimming(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	old, err := r.s.Schedule("tab", "chat", mustParse(t, after("old", 1)))
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Second)
	r.s.Tick(ctx)
	keep, _ := r.s.Schedule("tab", "chat", mustParse(t, after("keep", 600000)))
	r.clock.Advance(7*24*time.Hour - time.Second)
	r.reload()
	if got := r.s.List("tab", "chat"); len(got) != 2 {
		t.Fatalf("within retention: %+v", got)
	}
	r.clock.Advance(2 * time.Second)
	r.s.Tick(ctx) // keep is due and fires; old is now past 7 days
	got := r.s.List("tab", "chat")
	if len(got) != 1 || got[0].ID != keep.ID || got[0].Status != StatusFired {
		t.Fatalf("after retention: %+v (old %s)", got, old.ID)
	}

	// Per-chat cap of 200 settled, oldest dropped first; other chats and
	// pending ones are untouched. Seeded straight into the state file: 205
	// fsynced Tick passes would only slow the suite.
	dir := t.TempDir()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	var seeded []Entry
	for i := 0; i < 205; i++ {
		at := now.Add(-time.Duration(205-i) * time.Minute)
		seeded = append(seeded, Entry{ID: fmt.Sprintf("sched-%016d", i), TabID: "tab", ChatID: "chat", OperationID: fmt.Sprint("op", i),
			Condition: Condition{Kind: "at", At: at}, CreatedAt: at, Status: StatusFired, SettledAt: &at})
	}
	other := now.Add(-time.Hour)
	seeded = append(seeded,
		Entry{ID: "sched-other", TabID: "tab", ChatID: "other", OperationID: "o", Condition: Condition{Kind: "at", At: other}, CreatedAt: other, Status: StatusFired, SettledAt: &other},
		Entry{ID: "sched-pending", TabID: "tab", ChatID: "chat", OperationID: "p", Condition: Condition{Kind: "tcp", Addr: "h:1"}, CreatedAt: other.Add(-5 * 24 * time.Hour), ExpiresAt: now.Add(time.Hour), Status: StatusPending})
	writeState(t, dir, seeded)
	clock := &fakeClock{t: now}
	s, err := New(Options{StateDir: dir, Now: clock.Now, Send: (&recorder{}).Send})
	if err != nil {
		t.Fatal(err)
	}
	list := s.List("tab", "chat")
	if len(list) != 201 || list[0].ID != "sched-pending" {
		t.Fatalf("kept %d, first %s", len(list), list[0].ID)
	}
	if list[1].ID != fmt.Sprintf("sched-%016d", 204) || list[200].ID != fmt.Sprintf("sched-%016d", 5) {
		t.Fatalf("wrong ones kept: newest %s oldest %s", list[1].ID, list[200].ID)
	}
	if got := s.List("tab", "other"); len(got) != 1 {
		t.Fatalf("other chat affected: %+v", got)
	}
}

func writeState(t *testing.T, dir string, entries []Entry) {
	t.Helper()
	data, err := json.Marshal(diskFile{Version: fileVersion, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSendNeverRunsConcurrentlyForOneScheduledMessage(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	s, err := New(Options{StateDir: t.TempDir(), Now: clock.Now, Send: func(context.Context, Entry, string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := s.Schedule("tab", "chat", mustParse(t, after("op", 1)))
	clock.Advance(time.Second)

	first := make(chan struct{})
	go func() { s.Tick(context.Background()); close(first) }()
	<-started
	s.Tick(context.Background()) // must skip the in-flight scheduled message
	if _, err := s.Cancel("tab", "chat", e.ID); err == nil {
		t.Error("cancel during delivery succeeded")
	}
	close(release)
	<-first
	s.Tick(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("Send ran %d times", calls)
	}
	if got := s.List("tab", "chat")[0]; got.Status != StatusFired {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestRunDeliversAndStops(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	delivered := make(chan struct{})
	var once sync.Once
	s, err := New(Options{StateDir: t.TempDir(), Now: clock.Now, Tick: time.Millisecond, Send: func(context.Context, Entry, string) error {
		once.Do(func() { close(delivered) })
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.Schedule("tab", "chat", mustParse(t, after("op", 1)))
	clock.Advance(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	<-delivered
	cancel()
	<-done
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"Authorization: Bearer abc.def": "Authorization: Bearer [redacted]",
		"api_key=sk-123 failed":         "api_key=[redacted] failed",
		"tokens exhausted":              "tokens exhausted",
		"plain failure":                 "plain failure",
	}
	for in, want := range cases {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	if got := redact(strings.Repeat("é", 400)); len([]rune(got)) != 300 {
		t.Errorf("truncated to %d runes", len([]rune(got)))
	}
}
