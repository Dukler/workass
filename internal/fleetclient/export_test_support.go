package fleetclient

// Test support, in the production package on purpose: these expose facts the
// mesh already computes so a test asserts the real value rather than a copy of
// the logic that produced it.

// PeerEnrolmentPayloadForTest is what this daemon states about itself when it
// enrols with a peer (§4). It carries no secret: the derived token never
// crosses the wire, and the address a peer records is the one its own socket
// reports.
func (m *Mesh) PeerEnrolmentPayloadForTest() map[string]any {
	return m.peerEnrolmentPayload()
}

// BeatRecordInterval is how often a live link's heartbeat is written down.
func BeatRecordInterval() int64 { return int64(beatRecordInterval) }
