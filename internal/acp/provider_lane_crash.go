package acp

// handleUnexpectedBridgeExit terminates process-local work and lets the chat
// actor own the only durable recovery decision. The manager never creates,
// resumes, aliases, or replays a chat in response to a host crash.
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
		job.StopReason = "engine-crash"
		if job.startOpts.ProviderLaneManaged {
			job.actorRecoveryPending.Store(true)
		}
	}
	bridge.mu.Unlock()

	// Bridge.Close publishes LaneDetached for actor-owned attachments. The actor
	// then retries only the same ThreadRef and reconciles the admitted operation.
	bridge.Close(false, cause)
}
