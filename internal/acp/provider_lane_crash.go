package acp

// handleUnexpectedBridgeExit terminates process-local work and lets the chat
// actor publish the interrupted terminal receipt. The manager never creates,
// resumes, aliases, replays, or polls a chat in response to a host crash.
//
// An engine that was running and then died is a crash, never a login verdict:
// its stderr routinely contains credential-flavored words from ordinary CLI
// startup and shutdown logs, so classifying exits here used to permanently
// disable providers across whole fleets after every update wave.
func (m *Manager) handleUnexpectedBridgeExit(bridge *Bridge, cause error) {
	if bridge == nil {
		return
	}
	m.abandonAdoptedHarnessTurns(bridge)
	bridge.mu.Lock()
	for _, job := range bridge.jobsBySession {
		if job == nil || job.internal || job.Status != "running" {
			continue
		}
		job.CrashInterrupted = true
		job.Interrupted = true
		job.StopReason = "engine-crash"
	}
	bridge.mu.Unlock()

	// Bridge.Close publishes LaneDetached for actor-owned attachments. The actor
	// immediately terminalizes that visible turn and retains the saved ThreadRef
	// for the next distinct prompt.
	bridge.Close(false, cause)
}
