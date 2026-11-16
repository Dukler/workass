package procchild

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// bug-20261116-7ab17943: a caller that waited for its run already holds the
// outcome, so the settle says so and its chat is not told a second time. A
// caller that is gone, or a job owed to somebody else, still gets its notice.

func settledOf(t *testing.T, reporter *recordingReporter) Event {
	t.Helper()
	events := spoolOf(t, reporter)
	if len(events) == 0 || events[len(events)-1].Kind != KindSettled {
		t.Fatalf("want a settled event last, got %v", kindsOf(events))
	}
	return events[len(events)-1]
}

func TestASettleSaysTheWaitingCallerReceivedItsOutcome(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		owner   string
		waiting func() bool
		command []string
		want    bool
	}{
		{name: "caller waiting", waiting: func() bool { return true }, command: []string{"/bin/sh", "-c", "exit 0"}, want: true},
		{name: "caller waiting, job failed", waiting: func() bool { return true }, command: []string{"/bin/sh", "-c", "exit 1"}, want: true},
		{name: "caller waiting, preflight refused", waiting: func() bool { return true }, command: []string{"/no/such/command-7ab17943"}, want: true},
		{name: "caller gone", waiting: func() bool { return false }, command: []string{"/bin/sh", "-c", "exit 0"}},
		{name: "no caller probe", command: []string{"/bin/sh", "-c", "exit 0"}},
		// The caller here is not the chat that is owed the outcome.
		{name: "owed to another chat", owner: "wa:m-other/chat/c-1", waiting: func() bool { return true }, command: []string{"/bin/sh", "-c", "exit 0"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			spec, reporter := fastSpec(t, t.TempDir(), testCase.command...)
			spec.Owner, spec.CallerWaiting = testCase.owner, testCase.waiting
			if _, err := Run(spec); err != nil {
				t.Fatal(err)
			}
			got, _ := settledOf(t, reporter).Payload[SettledPayloadCallerReceived].(bool)
			if got != testCase.want {
				t.Fatalf("%s = %v, want %v (payload %+v)", SettledPayloadCallerReceived, got, testCase.want, settledOf(t, reporter).Payload)
			}
		})
	}
}

// The probe itself, against real processes: a parent that is still there, and
// a parent that exited while its child kept running (the `&` a shell returns
// from, or the shell a tool timeout killed).
func TestWatchCallerSeesWhetherTheStartingProcessIsStillThere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shell fixture is POSIX; the Windows probe waits on a handle")
	}
	if os.Getenv("WORKASS_CALLER_PROBE_CHILD") == "1" {
		// The child: report what the probe says now, and again after the
		// parent has had time to exit.
		probe := WatchCaller()
		first := probe()
		time.Sleep(400 * time.Millisecond)
		second := probe()
		_ = os.WriteFile(os.Getenv("WORKASS_CALLER_PROBE_OUT"), []byte(boolWord(first)+" "+boolWord(second)), 0o600)
		os.Exit(0)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, detach bool) string {
		out := filepath.Join(t.TempDir(), "probe")
		// stdout is a real file: a receipt sent to the null device is never
		// read, and the probe says so.
		script := `"$0" -test.run '^TestWatchCallerSeesWhetherTheStartingProcessIsStillThere$' </dev/null >"$1" 2>&1; exit 0`
		if detach {
			// The shell returns at once and its child is re-parented.
			script = `"$0" -test.run '^TestWatchCallerSeesWhetherTheStartingProcessIsStillThere$' </dev/null >"$1" 2>&1 & exit 0`
		}
		cmd := exec.Command("/bin/sh", "-c", script, self, out+".log")
		cmd.Env = append(os.Environ(), "WORKASS_CALLER_PROBE_CHILD=1", "WORKASS_CALLER_PROBE_OUT="+out)
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(out); err == nil && len(data) > 0 {
				return string(data)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the probe child never reported")
		return ""
	}
	if got := run(t, false); got != "waiting waiting" {
		t.Fatalf("a shell that waits for its child is its caller throughout, got %q", got)
	}
	if got := run(t, true); !strings.HasSuffix(got, " gone") {
		t.Fatalf("a shell that returned is no longer there to read a receipt, got %q", got)
	}
}

func boolWord(waiting bool) string {
	if waiting {
		return "waiting"
	}
	return "gone"
}
