package acp

import "context"

// handleUnexpectedBridgeExit terminates process-local work and lets the chat
// actor own the only durable recovery decision. The manager never creates,
// resumes, aliases, or replays a chat in response to a host crash.
func (m *Manager) handleUnexpectedBridgeExit(bridge *Bridge, cause error) {
	if bridge == nil {
		return
	}
	hint, policyErr := m.markProviderNeedsLogin(context.Background(), bridge.ProviderID(), cause)
	if hint != "" || policyErr != nil {
		m.abandonAdoptedHarnessTurns(bridge)
		closeCause := cause
		reason := providerStatusNeedsLogin
		if policyErr != nil {
			closeCause = policyErr
			reason = "authentication-policy-invalid"
		}
		bridge.Close(false, closeCause)
		m.opts.Logf("acp host closed", map[string]any{
			"provider": bridge.ProviderID(), "reason": reason,
		})
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
