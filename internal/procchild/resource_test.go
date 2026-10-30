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
