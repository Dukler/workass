package procchild

// The caller that is still waiting.
//
// `workass run` prints its receipt — outcome, exit code, output file — to the
// process that started it, the moment the job ends. When that process is still
// there to read it (an agent's foreground shell call, or a shell the agent's
// own harness backgrounded and reports on when it exits), the chat already HAS
// the outcome, and a notice on top of it is a stale duplicate that arrives as a
// fresh turn behind whatever the chat did next (bug-20261116-7ab17943). When
// the caller is gone — the shell was killed by a tool timeout, the job was
// sent off with `&` and the shell returned, the provider restarted — nobody is
// reading the receipt, and the notice is the only way the chat finds out.
//
// So the supervisor records, with its outcome, whether its caller was still
// there to receive it. The daemon suppresses the notice on that fact alone; the
// row, the receipt and the history are unchanged.

// SettledPayloadCallerReceived is the settled-event field that says the
// process that started this run was still attached when the outcome was
// recorded, so the outcome reached it as the receipt and no chat notice is owed.
const SettledPayloadCallerReceived = "callerReceived"

// WatchCaller captures the process that started this run and returns a probe
// answering, later, whether that same process is still there to read the
// receipt. Call it once, early, in the supervisor's own process.
func WatchCaller() func() bool {
	return watchCaller()
}
