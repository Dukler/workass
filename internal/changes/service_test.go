package changes

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGit stands in for git: it records every call and answers from behave.
type fakeGit struct {
	mu      sync.Mutex
	calls   []gitCall
	running int
	peak    int
	behave  func(ctx context.Context, dir string, args []string) ([]byte, error)
}

type gitCall struct {
	dir  string
	args []string
	at   time.Time
}

func (f *fakeGit) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, gitCall{dir: dir, args: args, at: time.Now()})
	f.running++
	f.peak = max(f.peak, f.running)
	behave := f.behave
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	if behave != nil {
		return behave(ctx, dir, args)
	}
	return cleanStatus(), nil
}

func (f *fakeGit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// statusStarts are the times each repo's status command started.
func (f *fakeGit) statusStarts(dir string) []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []time.Time
	for _, call := range f.calls {
		if call.dir == dir && len(call.args) > 0 && call.args[0] == "status" {
			out = append(out, call.at)
		}
	}
	return out
}

func cleanStatus() []byte {
	return []byte("# branch.oid abc\x00# branch.head main\x00")
}

// makeRepos creates directories with a .git entry, which is all the root
// finder looks for, and returns their canonical paths.
func makeRepos(t *testing.T, names ...string) []string {
	t.Helper()
	base := t.TempDir()
	var roots []string
	for _, name := range names {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		canonical, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, canonical)
	}
	return roots
}

func chatsIn(roots ...string) Source {
	return func(time.Time) []ChatActivity {
		var out []ChatActivity
		for index, root := range roots {
			out = append(out, ChatActivity{ChatID: "chat-" + filepath.Base(root), TabID: "tab", CWD: root, LastActivityAt: int64(1000 + index)})
		}
		return out
	}
}

type published struct {
	mu     sync.Mutex
	states []State
}

func (p *published) add(state State) {
	p.mu.Lock()
	p.states = append(p.states, state)
	p.mu.Unlock()
}

func (p *published) last() (State, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.states) == 0 {
		return State{}, false
	}
	return p.states[len(p.states)-1], true
}

func fastOptions(git *fakeGit, source Source, sink *published) Options {
	return Options{
		Runner: git.run, Source: source, Publish: sink.add,
		CallTimeout: 400 * time.Millisecond, EditDebounce: 150 * time.Millisecond,
		PollInterval: 200 * time.Millisecond, PollMax: 800 * time.Millisecond,
		SlowThreshold: 60 * time.Millisecond, PublishDelay: 5 * time.Millisecond,
	}
}

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNoWatcherMeansNoGit(t *testing.T) {
	roots := makeRepos(t, "alpha", "beta")
	git := &fakeGit{}
	sink := &published{}
	service := New(fastOptions(git, chatsIn(roots...), sink))

	// Every trigger, with nobody watching: an edit, a focus, and time for
	// several polls to have come due.
	service.NoteEdit([]string{filepath.Join(roots[0], "a.go")})
	service.Focus()
	if _, err := service.Diff(context.Background(), roots[0], "a.go"); err != ErrNotWatching {
		t.Fatalf("diff without a watcher: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	if got := git.count(); got != 0 {
		t.Fatalf("git ran %d times with no watcher", got)
	}

	id, _ := service.Watch("", nil)
	waitFor(t, "the open check", time.Second, func() bool { return git.count() >= 2 })
	service.Unwatch(id)
	time.Sleep(50 * time.Millisecond) // anything already started finishes
	after := git.count()
	service.NoteEdit([]string{filepath.Join(roots[0], "a.go")})
	service.Focus()
	time.Sleep(700 * time.Millisecond)
	if got := git.count(); got != after {
		t.Fatalf("git ran %d more times after the last watcher left", got-after)
	}
	if service.Watched() {
		t.Fatal("still watched")
	}
}

func TestWatcherEndsWithItsConnectionAndExpires(t *testing.T) {
	roots := makeRepos(t, "alpha")
	git := &fakeGit{}
	options := fastOptions(git, chatsIn(roots...), &published{})
	options.WatchTTL = 300 * time.Millisecond
	service := New(options)

	done := make(chan struct{})
	service.Watch("", done)
	if !service.Watched() {
		t.Fatal("not watched")
	}
	close(done)
	waitFor(t, "the closed connection to end the watch", time.Second, func() bool { return !service.Watched() })

	// No renewal: the lease runs out on its own.
	id, _ := service.Watch("", nil)
	time.Sleep(150 * time.Millisecond)
	if renewed, _ := service.Watch(id, nil); renewed != id {
		t.Fatalf("renewal made a new watcher %q", renewed)
	}
	time.Sleep(200 * time.Millisecond)
	if !service.Watched() {
		t.Fatal("a renewed watch expired early")
	}
	waitFor(t, "the lease to expire", 2*time.Second, func() bool { return !service.Watched() })
}

func TestEditTriggerIsDebouncedPerRepo(t *testing.T) {
	roots := makeRepos(t, "alpha")
	git := &fakeGit{}
	options := fastOptions(git, chatsIn(roots...), &published{})
	options.PollInterval, options.PollMax = time.Hour, time.Hour // only edits trigger
	service := New(options)
	id, _ := service.Watch("", nil)
	defer service.Unwatch(id)
	waitFor(t, "the open check", time.Second, func() bool { return len(git.statusStarts(roots[0])) == 1 })

	// A burst of edits for 600 ms: checks start at most once per debounce.
	file := filepath.Join(roots[0], "pkg", "a.go")
	stop := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(stop) {
		service.NoteEdit([]string{file})
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	starts := git.statusStarts(roots[0])
	if len(starts) < 3 || len(starts) > 6 {
		t.Fatalf("%d checks for a 600 ms burst with a 150 ms debounce", len(starts))
	}
	for index := 1; index < len(starts); index++ {
		if gap := starts[index].Sub(starts[index-1]); gap < 140*time.Millisecond {
			t.Fatalf("checks %d and %d only %v apart", index-1, index, gap)
		}
	}
}

func TestSlowRepoBacksOffAndFastRepoKeepsItsPace(t *testing.T) {
	roots := makeRepos(t, "fast", "slow")
	git := &fakeGit{behave: func(ctx context.Context, dir string, args []string) ([]byte, error) {
		if strings.HasSuffix(dir, "slow") {
			select {
			case <-time.After(90 * time.Millisecond): // over the 60 ms threshold
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return cleanStatus(), nil
	}}
	service := New(fastOptions(git, chatsIn(roots...), &published{}))
	id, _ := service.Watch("", nil)
	time.Sleep(2600 * time.Millisecond)
	service.Unwatch(id)

	gaps := func(starts []time.Time) []time.Duration {
		var out []time.Duration
		for index := 1; index < len(starts); index++ {
			out = append(out, starts[index].Sub(starts[index-1]))
		}
		return out
	}
	fast := gaps(git.statusStarts(roots[0]))
	slow := gaps(git.statusStarts(roots[1]))
	if len(fast) < 8 {
		t.Fatalf("fast repo polled only %d times: %v", len(fast), fast)
	}
	for _, gap := range fast {
		if gap > 350*time.Millisecond {
			t.Fatalf("fast repo waited %v between polls: %v", gap, fast)
		}
	}
	// 200 ms base: the slow repo waits 400, 800, then stays at the 800 cap.
	if len(slow) < 2 || len(slow) > 4 {
		t.Fatalf("slow repo gaps %v", slow)
	}
	if slow[0] < 380*time.Millisecond || slow[1] < 760*time.Millisecond {
		t.Fatalf("slow repo did not back off: %v", slow)
	}
	for _, gap := range slow {
		if gap > 1100*time.Millisecond {
			t.Fatalf("slow repo backed off past the cap: %v", slow)
		}
	}
}

func TestAtMostTwoGitProcessesAtOnce(t *testing.T) {
	roots := makeRepos(t, "a", "b", "c", "d", "e", "f")
	release := make(chan struct{})
	git := &fakeGit{behave: func(ctx context.Context, dir string, args []string) ([]byte, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return cleanStatus(), nil
	}}
	options := fastOptions(git, chatsIn(roots...), &published{})
	options.CallTimeout = 5 * time.Second
	service := New(options)
	id, _ := service.Watch("", nil)
	defer service.Unwatch(id)
	time.Sleep(150 * time.Millisecond)
	git.mu.Lock()
	running, started := git.running, len(git.calls)
	git.mu.Unlock()
	if running != 2 || started != 2 {
		t.Fatalf("%d git processes running, %d started, while all six repos wait", running, started)
	}
	close(release)
	waitFor(t, "every repo checked", 2*time.Second, func() bool {
		for _, root := range roots {
			if len(git.statusStarts(root)) == 0 {
				return false
			}
		}
		return true
	})
	git.mu.Lock()
	peak := git.peak
	git.mu.Unlock()
	if peak > 2 {
		t.Fatalf("peak of %d git processes", peak)
	}
}

func TestTimeoutOfOneRepoDoesNotBlockTheOthers(t *testing.T) {
	roots := makeRepos(t, "hangs", "one", "two", "three")
	git := &fakeGit{behave: func(ctx context.Context, dir string, args []string) ([]byte, error) {
		if strings.HasSuffix(dir, "hangs") {
			<-ctx.Done() // never answers on its own
			return nil, ctx.Err()
		}
		return []byte("# branch.oid abc\x00# branch.head main\x001 .M N... 100644 100644 100644 a a f.txt\x00"), nil
	}}
	sink := &published{}
	options := fastOptions(git, chatsIn(roots...), sink)
	options.CallTimeout = 1500 * time.Millisecond
	options.PollInterval = time.Hour
	service := New(options)
	started := time.Now()
	id, _ := service.Watch("", nil)
	defer service.Unwatch(id)
	waitFor(t, "the three answering repos", 1200*time.Millisecond, func() bool {
		state, ok := sink.last()
		if !ok {
			return false
		}
		checked := 0
		for _, repo := range state.Repos {
			if repo.Checked {
				checked++
			}
		}
		return checked == 3
	})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the answering repos waited %v behind the hanging one", elapsed)
	}
	// The hanging repo times out, is marked slow, and from then on may hold
	// only the slow lane.
	waitFor(t, "the hanging repo to time out", 3*time.Second, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		repo := service.repos[roots[0]]
		return repo != nil && repo.slow && !repo.checking
	})
}

func TestAttributionGroupsByLastEditorAndLeavesTheRestOutside(t *testing.T) {
	roots := makeRepos(t, "proj")
	root := roots[0]
	now := time.Now().UnixMilli()
	source := func(time.Time) []ChatActivity {
		return []ChatActivity{
			{ChatID: "chat-a", TabID: "tab-a", Title: "Panel derecho", ProviderID: "claude", CWD: root, LastActivityAt: now - 1000,
				Edits: []Edit{{Path: filepath.Join(root, "ui", "Queue.tsx"), At: now - 120_000}, {Path: filepath.Join(root, "shared.go"), At: now - 90_000}}},
			{ChatID: "chat-b", TabID: "tab-b", Title: "Revisión", ProviderID: "codex", CWD: root, LastActivityAt: now - 5000,
				Edits: []Edit{{Path: filepath.Join(root, "shared.go"), At: now - 30_000}, {Path: filepath.Join(root, "gone.go"), At: now - 10_000}}},
		}
	}
	git := &fakeGit{behave: func(ctx context.Context, dir string, args []string) ([]byte, error) {
		if args[0] == "status" {
			return []byte("# branch.oid abc\x00# branch.head main\x00# branch.upstream origin/main\x00# branch.ab +2 -0\x00" +
				"1 .M N... 100644 100644 100644 a a ui/Queue.tsx\x00" +
				"1 .M N... 100644 100644 100644 a a shared.go\x00" +
				"? notes.md\x00"), nil
		}
		return []byte("18\t3\tui/Queue.tsx\x004\t1\tshared.go\x00"), nil
	}}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := &published{}
	service := New(fastOptions(git, source, sink))
	id, _ := service.Watch("", nil)
	defer service.Unwatch(id)
	var state State
	waitFor(t, "the checked repo", time.Second, func() bool {
		var ok bool
		state, ok = sink.last()
		return ok && len(state.Repos) == 1 && state.Repos[0].Checked
	})
	repo := state.Repos[0]
	if repo.Branch != "main" || repo.Ahead != 2 || repo.Behind != 0 || repo.Adds != 25 || repo.Dels != 4 {
		t.Fatalf("repo row %+v", repo)
	}
	byPath := map[string]FileView{}
	for _, file := range repo.Files {
		byPath[file.Path] = file
	}
	if got := byPath["ui/Queue.tsx"]; got.ChatID != "chat-a" || got.TabID != "tab-a" || got.At != now-120_000 {
		t.Fatalf("Queue.tsx %+v", got)
	}
	if got := byPath["shared.go"]; got.ChatID != "chat-b" {
		t.Fatalf("shared.go should belong to its last editor: %+v", got)
	}
	if got := byPath["notes.md"]; got.ChatID != "" || !got.New || got.Adds != 3 {
		t.Fatalf("notes.md is a new file no chat edited: %+v", got)
	}
	var chats []string
	for _, chat := range state.Chats {
		chats = append(chats, chat.ChatID+"="+chat.Title)
	}
	sort.Strings(chats)
	if strings.Join(chats, ",") != "chat-a=Panel derecho,chat-b=Revisión" {
		t.Fatalf("chats %v", chats)
	}
	// gone.go was edited but is not changed now: it is not a row.
	if _, ok := byPath["gone.go"]; ok {
		t.Fatal("an unchanged file became a row")
	}

	if diff, err := service.Diff(context.Background(), root, "notes.md"); err != nil || !diff.New || !strings.Contains(diff.Text, "+two") {
		t.Fatalf("new file diff %+v %v", diff, err)
	}
	if _, err := service.Diff(context.Background(), root, "../../etc/passwd"); err != ErrUnknownFile {
		t.Fatalf("diff outside the repo: %v", err)
	}
	if _, err := service.Diff(context.Background(), root, "never-changed.go"); err != ErrUnknownFile {
		t.Fatalf("diff of an unreported file: %v", err)
	}
}

func TestMissingGitOrNotARepoIsLeftOutSilently(t *testing.T) {
	roots := makeRepos(t, "broken", "fine")
	git := &fakeGit{behave: func(ctx context.Context, dir string, args []string) ([]byte, error) {
		if strings.HasSuffix(dir, "broken") {
			return nil, os.ErrNotExist
		}
		return cleanStatus(), nil
	}}
	sink := &published{}
	service := New(fastOptions(git, chatsIn(roots...), sink))
	id, _ := service.Watch("", nil)
	defer service.Unwatch(id)
	waitFor(t, "a state with only the working repo", time.Second, func() bool {
		state, ok := sink.last()
		return ok && len(state.Repos) == 1 && state.Repos[0].Root == roots[1] && state.Repos[0].Checked
	})
}

func TestEditInARepoNoChatWorksInAddsIt(t *testing.T) {
	roots := makeRepos(t, "home", "elsewhere")
	git := &fakeGit{}
	sink := &published{}
	service := New(fastOptions(git, chatsIn(roots[0]), sink))
	id, _ := service.Watch("", nil)
	defer service.Unwatch(id)
	waitFor(t, "the chat's repo", time.Second, func() bool { return len(git.statusStarts(roots[0])) == 1 })
	service.NoteEdit([]string{filepath.Join(roots[1], "x.txt")})
	waitFor(t, "the edited repo", time.Second, func() bool { return len(git.statusStarts(roots[1])) == 1 })
}
