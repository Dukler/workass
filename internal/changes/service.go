// Package changes is the daemon side of the activity pane's «Cambios» tab:
// which git repos the owner's chats touched recently, what changed in each,
// and which chat changed it.
//
// Git is the expensive part, and the owner was once burned by a version that
// ran it after every send. So git runs only while at least one client watches
// the tab (Watch/Unwatch), and only on these triggers: a watch starting, an
// agent's edit tool call touching a repo (at most once every EditDebounce per
// repo), the window regaining focus, and a light poll every PollInterval that
// backs off to PollMax for a repo whose git was slow. Nothing here is called
// from message admission, prompting or turn end. At most MaxGit git processes
// run at once, each under CallTimeout, and a repo known to be slow may hold
// only one of those slots, so it never delays the others.
//
// Who changed a file comes from the chats' stored tool events (Source), never
// from git.
package changes

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Runner runs one read-only git command in dir and returns its stdout. It must
// stop when ctx ends.
type Runner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// ChatActivity is one recent chat: who it is, where it works, and the files
// its agents' edit tool calls named.
type ChatActivity struct {
	ChatID         string
	TabID          string
	Title          string
	ProviderID     string
	CWD            string
	LastActivityAt int64
	Edits          []Edit
}

// Edit is one file an agent edit tool call named, as an absolute path, and
// when (unix ms).
type Edit struct {
	Path string
	At   int64
}

// Source lists the chats active since the given time with their edits. It
// reads stored chat state only and must not start git.
type Source func(since time.Time) []ChatActivity

// Options configures a Service. Zero durations take the defaults below.
type Options struct {
	Runner  Runner
	Source  Source
	Publish func(State)
	Redact  func(string) string
	Logf    func(format string, args ...any)

	CallTimeout   time.Duration
	EditDebounce  time.Duration
	PollInterval  time.Duration
	PollMax       time.Duration
	SlowThreshold time.Duration
	WatchTTL      time.Duration
	Window        time.Duration
	PublishDelay  time.Duration
	MaxGit        int
	MaxRepos      int
}

const (
	DefaultCallTimeout   = 5 * time.Second
	DefaultEditDebounce  = 3 * time.Second
	DefaultPollInterval  = 15 * time.Second
	DefaultPollMax       = 2 * time.Minute
	DefaultSlowThreshold = 2 * time.Second
	DefaultWatchTTL      = 90 * time.Second
	// DefaultWindow bounds attribution: only chats active in the last seven
	// days are read, and only their tool calls from those days.
	DefaultWindow       = 7 * 24 * time.Hour
	DefaultPublishDelay = 120 * time.Millisecond
	DefaultMaxGit       = 2
	DefaultMaxRepos     = 24
)

// ErrNotWatching refuses a diff while no client watches the tab: git runs only
// while someone looks.
var ErrNotWatching = errors.New("changes: no client is watching")

// ErrUnknownFile refuses a diff of a file the last check did not report.
var ErrUnknownFile = errors.New("changes: file is not a known change")

// State is the `changes:state` payload and the `changes:watch` reply's state.
type State struct {
	Seq        uint64     `json:"seq"`
	WindowDays int        `json:"windowDays"`
	Repos      []RepoView `json:"repos"`
	Chats      []ChatView `json:"chats"`
}

// RepoView is one repo row. Ahead and Behind are left out when zero or
// unknown. Checked is false until the repo's first check answers.
type RepoView struct {
	Root           string     `json:"root"`
	Name           string     `json:"name"`
	Branch         string     `json:"branch,omitempty"`
	Detached       bool       `json:"detached,omitempty"`
	Upstream       string     `json:"upstream,omitempty"`
	Ahead          int        `json:"ahead,omitempty"`
	Behind         int        `json:"behind,omitempty"`
	Adds           int        `json:"adds"`
	Dels           int        `json:"dels"`
	Files          []FileView `json:"files"`
	FilesTruncated bool       `json:"filesTruncated,omitempty"`
	Checked        bool       `json:"checked"`
	Checking       bool       `json:"checking,omitempty"`
	CheckedAt      int64      `json:"checkedAt,omitempty"`
	ActivityAt     int64      `json:"activityAt,omitempty"`
}

// FileView is one changed file. ChatID/TabID/At name the chat whose agent
// edited it last; a file no chat edited has none ("Fuera de Workass").
type FileView struct {
	Path    string `json:"path"`
	Adds    int    `json:"adds"`
	Dels    int    `json:"dels"`
	New     bool   `json:"new,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
	ChatID  string `json:"chatId,omitempty"`
	TabID   string `json:"tabId,omitempty"`
	At      int64  `json:"at,omitempty"`
}

// ChatView is a chat some file is attributed to.
type ChatView struct {
	ChatID     string `json:"chatId"`
	TabID      string `json:"tabId"`
	Title      string `json:"title"`
	ProviderID string `json:"providerId,omitempty"`
	At         int64  `json:"at,omitempty"`
}

type watcher struct {
	expires time.Time
	gone    chan struct{}
}

type repoState struct {
	root       string
	status     *repoStatus
	missing    bool
	checking   bool
	pending    bool
	live       bool
	slow       bool
	lastStart  time.Time
	checkedAt  time.Time
	interval   time.Duration
	activityAt int64
	poll       *time.Timer
	debounce   *time.Timer
}

func (r *repoState) stopTimers() {
	if r.poll != nil {
		r.poll.Stop()
		r.poll = nil
	}
	if r.debounce != nil {
		r.debounce.Stop()
		r.debounce = nil
	}
}

// Service schedules the git checks and builds the tab's state.
type Service struct {
	opts     Options
	finder   *rootFinder
	slots    chan struct{}
	slowLane chan struct{}
	watched  atomic.Bool
	gitRuns  atomic.Int64

	mu         sync.Mutex
	gen        uint64
	runCtx     context.Context
	cancelRun  context.CancelFunc
	watchers   map[string]*watcher
	nextWatch  uint64
	repos      map[string]*repoState
	publishing *time.Timer
	janitor    *time.Timer
	snapshot   *State
	seq        uint64
}

// New builds a Service. Nothing runs until the first Watch.
func New(opts Options) *Service {
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	if opts.EditDebounce <= 0 {
		opts.EditDebounce = DefaultEditDebounce
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.PollMax <= 0 {
		opts.PollMax = DefaultPollMax
	}
	if opts.SlowThreshold <= 0 {
		opts.SlowThreshold = DefaultSlowThreshold
	}
	if opts.WatchTTL <= 0 {
		opts.WatchTTL = DefaultWatchTTL
	}
	if opts.Window <= 0 {
		opts.Window = DefaultWindow
	}
	if opts.PublishDelay <= 0 {
		opts.PublishDelay = DefaultPublishDelay
	}
	if opts.MaxGit <= 0 {
		opts.MaxGit = DefaultMaxGit
	}
	if opts.MaxRepos <= 0 {
		opts.MaxRepos = DefaultMaxRepos
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Redact == nil {
		opts.Redact = func(text string) string { return text }
	}
	return &Service{
		opts: opts, finder: newRootFinder(),
		slots: make(chan struct{}, opts.MaxGit), slowLane: make(chan struct{}, 1),
		watchers: map[string]*watcher{}, repos: map[string]*repoState{},
	}
}

// Watched reports whether any client watches the tab. It is one atomic load,
// cheap enough for the provider event path to call on every tool event.
func (s *Service) Watched() bool { return s != nil && s.watched.Load() }

// GitRuns counts the git processes this service has started.
func (s *Service) GitRuns() int64 { return s.gitRuns.Load() }

// Watch starts a watcher, or renews the one named id. A renewal changes
// nothing else; a new watcher is the "tab opened" trigger. done, when not
// nil, ends the watcher when it closes (the client's socket). The reply holds
// the last built state, nil before the first one: the checks themselves run
// in the background and arrive through Publish.
func (s *Service) Watch(id string, done <-chan struct{}) (string, *State) {
	s.mu.Lock()
	now := time.Now()
	if existing := s.watchers[id]; id != "" && existing != nil {
		existing.expires = now.Add(s.opts.WatchTTL)
		snapshot := s.snapshot
		s.mu.Unlock()
		return id, snapshot
	}
	s.nextWatch++
	id = "w" + strconv.FormatUint(s.nextWatch, 10) + "-" + strconv.FormatInt(now.UnixNano()%1_000_000, 36)
	w := &watcher{expires: now.Add(s.opts.WatchTTL), gone: make(chan struct{})}
	s.watchers[id] = w
	if len(s.watchers) == 1 {
		s.startLocked()
	}
	gen := s.gen
	snapshot := s.snapshot
	s.mu.Unlock()
	if done != nil {
		go func() {
			select {
			case <-done:
				s.Unwatch(id)
			case <-w.gone:
			}
		}()
	}
	go s.refreshAll(gen)
	return id, snapshot
}

// Unwatch ends a watcher. When it was the last one every timer stops, every
// git process still running is cancelled, and no git starts until the next
// Watch.
func (s *Service) Unwatch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.watchers[id]
	if w == nil {
		return
	}
	delete(s.watchers, id)
	close(w.gone)
	if len(s.watchers) == 0 {
		s.stopLocked()
	}
}

// Focus is the window regaining focus: every repo is checked again unless it
// was checked within EditDebounce.
func (s *Service) Focus() {
	if !s.Watched() {
		return
	}
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	go s.refreshAll(gen)
}

// NoteEdit is an agent edit tool call naming paths (absolute). While nobody
// watches it returns at once; otherwise the repos holding those paths are
// checked, each at most once every EditDebounce.
func (s *Service) NoteEdit(paths []string) {
	if !s.Watched() || len(paths) == 0 {
		return
	}
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	go s.noteEdit(gen, append([]string(nil), paths...))
}

func (s *Service) noteEdit(gen uint64, paths []string) {
	roots := map[string]struct{}{}
	for _, path := range paths {
		if root := s.finder.rootOf(filepath.Dir(path)); root != "" {
			roots[root] = struct{}{}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || len(s.watchers) == 0 {
		return
	}
	for root := range roots {
		repo := s.repos[root]
		if repo == nil {
			if len(s.repos) >= s.opts.MaxRepos {
				continue
			}
			repo = &repoState{root: root, live: true, interval: s.opts.PollInterval, activityAt: time.Now().UnixMilli()}
			s.repos[root] = repo
		}
		s.requestLocked(repo)
	}
}

// State returns the last built state, or nil.
func (s *Service) State() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

func (s *Service) startLocked() {
	s.gen++
	s.runCtx, s.cancelRun = context.WithCancel(context.Background())
	s.watched.Store(true)
	s.finder.forget()
	gen := s.gen
	every := s.opts.WatchTTL / 3
	var sweep func()
	sweep = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if gen != s.gen {
			return
		}
		now := time.Now()
		for id, w := range s.watchers {
			if now.After(w.expires) {
				delete(s.watchers, id)
				close(w.gone)
			}
		}
		if len(s.watchers) == 0 {
			s.stopLocked()
			return
		}
		s.janitor = time.AfterFunc(every, sweep)
	}
	s.janitor = time.AfterFunc(every, sweep)
}

func (s *Service) stopLocked() {
	s.gen++
	s.watched.Store(false)
	if s.cancelRun != nil {
		s.cancelRun()
	}
	if s.janitor != nil {
		s.janitor.Stop()
		s.janitor = nil
	}
	for _, repo := range s.repos {
		repo.stopTimers()
		repo.pending = false
		repo.checking = false
		if repo.live {
			delete(s.repos, repo.root)
		}
	}
}

// refreshAll reads the chats, updates the repo set and asks for every repo to
// be checked. Reading the chats happens without the lock held.
func (s *Service) refreshAll(gen uint64) {
	index := s.readActivity()
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || len(s.watchers) == 0 {
		return
	}
	s.applyIndexLocked(index, false)
	for _, repo := range s.repos {
		s.requestLocked(repo)
	}
	s.schedulePublishLocked()
}

// requestLocked asks for one repo check, at most once every EditDebounce.
func (s *Service) requestLocked(repo *repoState) {
	if len(s.watchers) == 0 {
		return
	}
	if repo.checking {
		repo.pending = true
		return
	}
	wait := time.Until(repo.lastStart.Add(s.opts.EditDebounce))
	if wait > 0 {
		if repo.debounce == nil {
			gen, root := s.gen, repo.root
			repo.debounce = time.AfterFunc(wait, func() { s.fire(gen, root, true) })
		}
		return
	}
	s.startCheckLocked(repo)
}

// fire runs a repo's debounce or poll timer.
func (s *Service) fire(gen uint64, root string, debounce bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo := s.repos[root]
	if gen != s.gen || repo == nil || len(s.watchers) == 0 {
		return
	}
	if debounce {
		repo.debounce = nil
	} else {
		repo.poll = nil
	}
	if repo.checking {
		repo.pending = true
		return
	}
	if !debounce {
		// The poll is the light check; it never waits for the edit debounce.
		s.startCheckLocked(repo)
		return
	}
	s.requestLocked(repo)
}

func (s *Service) startCheckLocked(repo *repoState) {
	repo.stopTimers()
	repo.checking = true
	repo.pending = false
	repo.lastStart = time.Now()
	gen, ctx, root, slow := s.gen, s.runCtx, repo.root, repo.slow
	s.schedulePublishLocked()
	go s.runCheck(gen, ctx, root, slow)
}

func (s *Service) runCheck(gen uint64, ctx context.Context, root string, slow bool) {
	status, took, err := s.check(ctx, root, slow)
	s.mu.Lock()
	defer s.mu.Unlock()
	repo := s.repos[root]
	if repo == nil {
		return
	}
	if gen != s.gen {
		return
	}
	repo.checking = false
	timedOut := errors.Is(err, context.DeadlineExceeded)
	switch {
	case err == nil:
		repo.status, repo.missing, repo.checkedAt = &status, false, time.Now()
	case timedOut:
		// Keep what the last answer said; the repo is slow, not gone.
	case errors.Is(err, context.Canceled):
		return
	default:
		// Not a repo, or no git: leave it out without a word.
		repo.status, repo.missing = nil, true
	}
	switch {
	case repo.missing:
		repo.slow, repo.interval = false, s.opts.PollMax
	case timedOut || took > s.opts.SlowThreshold:
		repo.slow = true
		next := repo.interval * 2
		if next < s.opts.PollInterval*2 {
			next = s.opts.PollInterval * 2
		}
		repo.interval = min(next, s.opts.PollMax)
	default:
		repo.slow, repo.interval = false, s.opts.PollInterval
	}
	if repo.pending {
		repo.pending = false
		s.requestLocked(repo)
	}
	if repo.poll == nil && !repo.checking {
		repo.poll = time.AfterFunc(repo.interval, func() { s.fire(gen, root, false) })
	}
	s.schedulePublishLocked()
}

// check runs the two git commands for one repo under one slot. took is the
// time git itself ran, not the wait for a slot.
func (s *Service) check(ctx context.Context, root string, slow bool) (repoStatus, time.Duration, error) {
	release, err := s.acquire(ctx, slow)
	if err != nil {
		return repoStatus{}, 0, err
	}
	started := time.Now()
	out, err := s.git(ctx, root, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		release()
		return repoStatus{}, time.Since(started), err
	}
	status := parseStatusV2(out)
	if !status.initial && hasTracked(status.files) {
		numstatOut, numstatErr := s.git(ctx, root, "diff", "--numstat", "-z", "--no-renames", "--no-ext-diff", "--no-color", "HEAD", "--")
		if numstatErr == nil {
			status.applyNumstat(parseNumstatZ(numstatOut))
		} else if ctx.Err() != nil || errors.Is(numstatErr, context.DeadlineExceeded) {
			release()
			return repoStatus{}, time.Since(started), numstatErr
		}
	}
	took := time.Since(started)
	release()
	status.countUntracked(root)
	status.capFiles()
	return status, took, nil
}

func hasTracked(files []fileStatus) bool {
	for _, file := range files {
		if !file.untracked {
			return true
		}
	}
	return false
}

// acquire takes one of the MaxGit slots. A slow repo first takes the single
// slow lane, so slow repos together never hold more than one slot.
func (s *Service) acquire(ctx context.Context, slow bool) (func(), error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if slow {
		select {
		case s.slowLane <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		if slow {
			<-s.slowLane
		}
		return nil, ctx.Err()
	}
	return func() {
		<-s.slots
		if slow {
			<-s.slowLane
		}
	}, nil
}

// git runs one command under CallTimeout. A deadline is reported as
// context.DeadlineExceeded whatever the runner returned.
func (s *Service) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.opts.CallTimeout)
	defer cancel()
	s.gitRuns.Add(1)
	out, err := s.opts.Runner(callCtx, dir, args...)
	if callCtx.Err() != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, context.DeadlineExceeded
	}
	return out, err
}

func (s *Service) schedulePublishLocked() {
	if s.publishing != nil || s.opts.Publish == nil {
		return
	}
	s.publishing = time.AfterFunc(s.opts.PublishDelay, s.publish)
}

// publish rebuilds the state (re-reading attribution from the chats, without
// the lock) and hands it to Publish.
func (s *Service) publish() {
	index := s.readActivity()
	s.mu.Lock()
	s.publishing = nil
	if len(s.watchers) > 0 {
		s.applyIndexLocked(index, true)
	}
	state := s.buildLocked(index)
	s.mu.Unlock()
	if s.opts.Publish != nil {
		s.opts.Publish(state)
	}
}

func (s *Service) readActivity() activityIndex {
	if s.opts.Source == nil {
		return activityIndex{}
	}
	return buildIndex(s.finder, s.opts.Source(time.Now().Add(-s.opts.Window)))
}

// applyIndexLocked makes the repo set the repos the chats touched (the most
// recently active MaxRepos of them) plus any an edit added since. A repo new
// to the set is checked when request is set; refreshAll asks for every repo
// itself.
func (s *Service) applyIndexLocked(index activityIndex, request bool) {
	keep := map[string]int64{}
	roots := make([]string, 0, len(index.repos))
	for root := range index.repos {
		roots = append(roots, root)
	}
	sort.Slice(roots, func(i, j int) bool {
		if index.repos[roots[i]] != index.repos[roots[j]] {
			return index.repos[roots[i]] > index.repos[roots[j]]
		}
		return roots[i] < roots[j]
	})
	if len(roots) > s.opts.MaxRepos {
		roots = roots[:s.opts.MaxRepos]
	}
	for _, root := range roots {
		keep[root] = index.repos[root]
	}
	for root, repo := range s.repos {
		if at, ok := keep[root]; ok {
			repo.activityAt, repo.live = max(repo.activityAt, at), false
			continue
		}
		if repo.live {
			continue
		}
		repo.stopTimers()
		delete(s.repos, root)
	}
	for root, at := range keep {
		if s.repos[root] != nil {
			continue
		}
		repo := &repoState{root: root, interval: s.opts.PollInterval, activityAt: at}
		s.repos[root] = repo
		if request {
			s.requestLocked(repo)
		}
	}
}

func (s *Service) buildLocked(index activityIndex) State {
	s.seq++
	state := State{Seq: s.seq, WindowDays: int(s.opts.Window / (24 * time.Hour)), Repos: []RepoView{}, Chats: []ChatView{}}
	chats := map[string]*ChatView{}
	for _, repo := range s.repos {
		if repo.missing {
			continue
		}
		view := RepoView{
			Root: repo.root, Name: filepath.Base(repo.root), Files: []FileView{},
			Checking: repo.checking, ActivityAt: repo.activityAt,
		}
		if repo.status != nil {
			status := repo.status
			view.Checked = true
			view.CheckedAt = repo.checkedAt.UnixMilli()
			view.Branch, view.Detached, view.Upstream = status.branch, status.detached, status.upstream
			if status.abKnown {
				view.Ahead, view.Behind = status.ahead, status.behind
			}
			view.FilesTruncated = status.filesTruncated
			edits := index.files[repo.root]
			for _, file := range status.files {
				fv := FileView{
					Path: file.path, Adds: file.adds, Dels: file.dels, Binary: file.binary,
					New:     file.untracked || (len(file.code) > 0 && file.code[0] == 'A'),
					Deleted: len(file.code) == 2 && (file.code[0] == 'D' || file.code[1] == 'D'),
				}
				if edit, ok := edits[pathKey(file.path)]; ok {
					if chat, known := index.chats[edit.chatID]; known {
						fv.ChatID, fv.TabID, fv.At = chat.ChatID, chat.TabID, edit.at
						entry := chats[chat.ChatID]
						if entry == nil {
							entry = &ChatView{ChatID: chat.ChatID, TabID: chat.TabID, Title: chat.Title, ProviderID: chat.ProviderID}
							chats[chat.ChatID] = entry
						}
						entry.At = max(entry.At, edit.at)
						view.ActivityAt = max(view.ActivityAt, edit.at)
					}
				}
				view.Adds += fv.Adds
				view.Dels += fv.Dels
				view.Files = append(view.Files, fv)
			}
		}
		state.Repos = append(state.Repos, view)
	}
	sort.Slice(state.Repos, func(i, j int) bool {
		if state.Repos[i].ActivityAt != state.Repos[j].ActivityAt {
			return state.Repos[i].ActivityAt > state.Repos[j].ActivityAt
		}
		return state.Repos[i].Root < state.Repos[j].Root
	})
	for _, chat := range chats {
		state.Chats = append(state.Chats, *chat)
	}
	sort.Slice(state.Chats, func(i, j int) bool {
		if state.Chats[i].At != state.Chats[j].At {
			return state.Chats[i].At > state.Chats[j].At
		}
		return state.Chats[i].ChatID < state.Chats[j].ChatID
	})
	s.snapshot = &state
	return state
}
