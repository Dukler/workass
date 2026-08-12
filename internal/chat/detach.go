package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"workass/internal/provider"
)

// DetachOperationID returns the stable identity for one exact disposable
// attachment. Length-prefixing prevents concatenation ambiguity while keeping
// the raw connection id out of durable operation names and receipts.
func DetachOperationID(chatID string, laneID provider.LaneID, connectionID string, generation uint64) provider.OperationID {
	payload := fmt.Sprintf("chat:%d:%s|lane:%d:%s|connection:%d:%s|generation:%d",
		len(strings.TrimSpace(chatID)), strings.TrimSpace(chatID),
		len(strings.TrimSpace(string(laneID))), strings.TrimSpace(string(laneID)),
		len(strings.TrimSpace(connectionID)), strings.TrimSpace(connectionID),
		generation,
	)
	sum := sha256.Sum256([]byte(payload))
	return provider.OperationID("lane-detach:" + hex.EncodeToString(sum[:16]))
}

func detachEffectID(operationID provider.OperationID) string {
	return string(provider.NormalizeOperationID(string(operationID)))
}
