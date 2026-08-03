package acp

import "strings"

const (
	replayCompactionMarkerInProgress             = "Compacting..."
	replayCompactionMarkerCompleted              = "Compacting completed."
	replayCompactionMarkerFailed                 = "Compacting failed."
	replayCompactionMarkerFailedWithReasonPrefix = "Compacting failed: "
)

var replayCompactionExactMarkers = []string{
	replayCompactionMarkerInProgress,
	replayCompactionMarkerCompleted,
	replayCompactionMarkerFailed,
}

// sanitizeReplayContent removes the Claude adapter's injected compaction
// markers from replay-seed content. Everything that is not a marker is kept
// byte-verbatim — indentation and blank lines inside real messages (code
// blocks, paragraphs) must survive the seed unchanged.
func sanitizeReplayContent(s string) string {
	text := strings.ReplaceAll(s, "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		cleaned, drop := sanitizeReplayLine(line)
		if drop {
			continue
		}
		out = append(out, cleaned)
	}
	joined := strings.Join(out, "\n")
	// Dropped marker lines can leave runs of blank lines behind; collapse only
	// those artifacts (3+ newlines), never single paragraph breaks.
	for strings.Contains(joined, "\n\n\n") {
		joined = strings.ReplaceAll(joined, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(joined)
}

// sanitizeReplayLine returns the line with any leading/trailing marker
// fragments stripped, or drop=true when the whole line is a marker artifact.
// Non-marker lines are returned unmodified.
func sanitizeReplayLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return line, false
	}
	for _, marker := range replayCompactionExactMarkers {
		if trimmed == marker {
			return "", true
		}
	}
	if strings.HasPrefix(trimmed, replayCompactionMarkerFailedWithReasonPrefix) {
		return "", true
	}
	// The in-progress marker can be glued to streamed text with no newline;
	// strip embedded prefix/suffix markers from the raw line, preserving the
	// rest of the line's bytes.
	for {
		before := line
		for _, marker := range replayCompactionExactMarkers {
			line = strings.TrimPrefix(line, marker)
			line = strings.TrimSuffix(line, marker)
		}
		if idx := strings.LastIndex(line, replayCompactionMarkerFailedWithReasonPrefix); idx > 0 {
			line = strings.TrimRight(line[:idx], " \t")
		}
		if line == before {
			return line, false
		}
		if strings.TrimSpace(line) == "" {
			return "", true
		}
	}
}
