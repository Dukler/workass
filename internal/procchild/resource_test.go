package procchild

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The resource gate §13 P4 asks for: "two jobs on one resource serialize FIFO
// and a killed holder releases". Both are properties of the HOLD being the
// supervisor's pid rather than a lease with a lifetime — so the second test
// kills a holder the only way that matters, by making its pid unanswerable,
// and asserts the queue moves on with nothing having been renewed, revoked or
// swept.

func TestTwoJobsOnOneResourceSerializeFIFO(t *testing.T) {
	state := t.TempDir()
	alive := map[int]bool{100: true, 200: true}
	var mu sync.Mutex
	probe := func(pid int) bool {
		mu.Lock()
		defer mu.Unlock()
		return alive[pid]
	}
	first, err := Acquire(AcquireRequest{
		StateDir: state, Resource: "gpu0", ChildID: "xwfirstjob01", Label: "first",
		PID: 100, Alive: probe, Poll: time.Millisecond,
	})
	if err != nil || first == nil {
		t.Fatalf("the first claim on a free resource must be granted at once: %v", err)
	}

	positions := make(chan int, 4)
	granted := make(chan *Lease, 1)
	failed := make(chan error, 1)
	go func() {
		lease, err := Acquire(AcquireRequest{
			StateDir: state, Resource: "gpu0", ChildID: "xwsecondjob1", Label: "second",
			PID: 200, Alive: probe, Poll: time.Millisecond,
			OnQueued: func(position, total int) { positions <- position },
		})
		if err != nil {
			failed <- err
			return
		}
		granted <- lease
	}()
	t.Cleanup(func() {
		select {
		case err := <-failed:
			t.Errorf("the second claim must queue, not fail: %v", err)
		default:
		}
	})

	select {
	case position := <-positions:
		if position != 2 {
			t.Fatalf("the second waiter is at position %d, want 2", position)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second waiter never reported a queue position")
	}
	select {
	case <-granted:
		t.Fatal("the second claim was granted while the first was still holding")
	case <-time.After(50 * time.Millisecond):
	}

	queue := queueWith(state, "gpu0", probe)
	if len(queue) != 2 || !queue[0].Holding || queue[0].ChildID != "xwfirstjob01" || queue[1].Position != 2 {
		t.Fatalf("the queue must be visible and in FIFO order, got %+v", queue)
	}

	first.Release()
	select {
	case lease := <-granted:
		if lease == nil || lease.Resource() != "gpu0" {
			t.Fatalf("the second waiter did not take the hold: %+v", lease)
		}
		lease.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("releasing the hold did not grant it to the waiter")
	}
	if queue := queueWith(state, "gpu0", probe); len(queue) != 0 {
		t.Fatalf("every claim was released, so the queue must be empty, got %+v", queue)
	}
}

func TestAKilledHolderReleasesItsResource(t *testing.T) {
	state := t.TempDir()
	alive := map[int]bool{100: true, 200: true}
	var mu sync.Mutex
	probe := func(pid int) bool {
		mu.Lock()
		defer mu.Unlock()
		return alive[pid]
	}
	if _, err := Acquire(AcquireRequest{
		StateDir: state, Resource: "gpu0", ChildID: "xwholderjob1", Label: "holder",
		PID: 100, Alive: probe, Poll: time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	granted := make(chan error, 1)
	go func() {
		_, err := Acquire(AcquireRequest{
			StateDir: state, Resource: "gpu0", ChildID: "xwwaiterjob1", Label: "waiter",
			PID: 200, Alive: probe, Poll: time.Millisecond,
		})
		granted <- err
	}()
	select {
	case <-granted:
		t.Fatal("the waiter was granted the hold while the holder was alive")
	case <-time.After(50 * time.Millisecond):
	}
	// SIGKILL, an OOM and a power cut are the same case: the pid stops
	// answering and NOTHING was released by the holder itself. Its claim file
	// is still on disk, which is exactly the state a lease with a lifetime
	// would have had to wait out.
	mu.Lock()
	alive[100] = false
	mu.Unlock()
	select {
	case err := <-granted:
		if err != nil {
			t.Fatalf("the waiter must be granted the hold after the holder dies: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a dead holder never released its resource")
	}
	queue := queueWith(state, "gpu0", probe)
	if len(queue) != 1 || queue[0].ChildID != "xwwaiterjob1" || !queue[0].Holding {
		t.Fatalf("the waiter must be the holder and the dead claim must be gone, got %+v", queue)
	}
}

func TestResourceQueueIsVisibleOnDisk(t *testing.T) {
	state := t.TempDir()
	lease, err := Acquire(AcquireRequest{
		StateDir: state, Resource: "gpu0", ChildID: "xwvisiblejob", Label: "training run",
		Owner: "wa:m-7f3ad2/chat/app-chat-1", PID: os.Getpid(), Poll: time.Millisecond,
		MaxHold: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	entries, err := os.ReadDir(ResourceQueueDir(state, "gpu0"))
	if err != nil || len(entries) != 1 {
		// A queue only the daemon honours is not a lock (§10): a human on the
		// machine must be able to see who is holding by listing a directory.
		t.Fatalf("the queue must be one file per waiter on disk, got %v / %v", entries, err)
	}
	if names := Resources(state); len(names) != 1 || names[0] != "gpu0" {
		t.Fatalf("Resources must report the queues that exist, got %v", names)
	}
	row := Queue(state, "gpu0")[0]
	if row.Owner != "wa:m-7f3ad2/chat/app-chat-1" || row.Label != "training run" || row.MaxHoldMs != (2*time.Hour).Milliseconds() {
		t.Fatalf("the queue row must name its owner, label and declared hold, got %+v", row)
	}
}

func TestResourceNamesThatCouldEscapeTheQueueAreRefused(t *testing.T) {
	for _, name := range []string{"", "../gpu0", "gpu/0", "gpu\\0", ".", "a b"} {
		if NormalizeResource(name) != "" {
			t.Fatalf("resource name %q must be refused: a name with a separator puts the queue somewhere other than the queue", name)
		}
		if dir := ResourceQueueDir(t.TempDir(), name); dir != "" {
			t.Fatalf("resource %q resolved to a queue directory %q", name, dir)
		}
	}
	if NormalizeResource("gpu0") != "gpu0" || NormalizeResource("node-1.local") != "node-1.local" {
		t.Fatal("ordinary resource names must be accepted")
	}
	_ = filepath.Separator
}

// settableClock is the time a claim sees, moved by the test.
type settableClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *settableClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *settableClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// The live failure: a job queued for 2h26m and then reported as having held the
// GPU for all of it, under a minute after getting it.
func TestHoldTimeStartsWhenTheResourceIsAcquiredNotWhenItQueued(t *testing.T) {
	state := t.TempDir()
	queuedAt := time.Date(2026, 11, 1, 21, 29, 31, 0, time.UTC)
	acquiredAt := queuedAt.Add(2*time.Hour + 26*time.Minute)
	clock := &settableClock{at: queuedAt}
	alive := func(int) bool { return true }
	first, err := Acquire(AcquireRequest{
		StateDir: state, Resource: "gpu0", ChildID: "xwfirstjob01", PID: 100,
		Alive: alive, Poll: time.Millisecond, Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan struct{})
	granted := make(chan *Lease, 1)
	go func() {
		lease, err := Acquire(AcquireRequest{
			StateDir: state, Resource: "gpu0", ChildID: "xwsecondjob1", PID: 200, MaxHold: 15 * time.Minute,
			Alive: alive, Poll: time.Millisecond, Now: clock.now,
			OnQueued: func(int, int) { close(queued) },
		})
		if err != nil {
			t.Error(err)
		}
		granted <- lease
	}()
	<-queued

	dir := ResourceQueueDir(state, "gpu0")
	waiting := readQueue(dir, "gpu0", alive, acquiredAt)[1]
	if waiting.Holding || waiting.HeldSince != "" || waiting.HeldForMs != 0 {
		t.Fatalf("a waiter holds nothing, got %+v", waiting)
	}

	clock.set(acquiredAt)
	first.Release()
	lease := <-granted
	defer lease.Release()

	row := readQueue(dir, "gpu0", alive, acquiredAt.Add(40*time.Second))[0]
	if !row.Holding || row.HeldForMs != (40*time.Second).Milliseconds() {
		t.Fatalf("hold time must count from the grant, got %+v", row)
	}
	if row.HeldSince != acquiredAt.Format(time.RFC3339Nano) || row.Since != queuedAt.Format(time.RFC3339Nano) {
		t.Fatalf("the row must keep the queue time and the grant time apart, got %+v", row)
	}
}

// A supervisor from before the grant stamp keeps running across an update, and its
// claim says only when it queued. That is not a hold time, whatever the reader does
// about having none.
func TestAClaimWithNoGrantStampClaimsNoHoldTime(t *testing.T) {
	state := t.TempDir()
	dir := ResourceQueueDir(state, "gpu0")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	queuedAt := time.Date(2026, 11, 1, 21, 29, 31, 0, time.UTC)
	old := `{"ticket":"01","pid":100,"childId":"xwolderjob01","label":"old","since":"` + queuedAt.Format(time.RFC3339Nano) + `","maxHoldMs":900000}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "q-01-xwolderjob01.claim"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := readQueue(dir, "gpu0", func(int) bool { return true }, queuedAt.Add(3*time.Hour))
	if len(rows) != 1 || !rows[0].Holding || rows[0].HeldForMs != 0 || rows[0].HeldSince != "" {
		t.Fatalf("got %+v", rows)
	}
}

// A reader removes any claim it cannot parse, so the rewrite that stamps a grant
// must never show a reader a partial file: it would free a resource in use.
func TestStampingAGrantNeverLosesTheClaimToAReader(t *testing.T) {
	state := t.TempDir()
	dir := ResourceQueueDir(state, "gpu0")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	alive := func(int) bool { return true }
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				readQueue(dir, "gpu0", alive, time.Now())
			}
		}
	}()
	for i := 0; i < 200; i++ {
		lease, err := Acquire(AcquireRequest{
			StateDir: state, Resource: "gpu0", ChildID: "xwstampedjob", PID: 100, Alive: alive, Poll: time.Millisecond,
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if _, err := os.Stat(lease.path); err != nil {
			t.Fatalf("attempt %d: a reader took the claim while it was being stamped: %v", i, err)
		}
		lease.Release()
	}
}
