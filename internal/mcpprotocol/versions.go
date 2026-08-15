// Package mcpprotocol defines the MCP protocol revisions supported by every
// Workass-owned MCP transport. Keeping revision negotiation here prevents the
// authenticated HTTP endpoints and the ACP stdio bridge from drifting apart.
package mcpprotocol

const (
	ModernVersion       = "2026-07-28"
	LatestLegacyVersion = "2025-11-25"
)

var legacyVersions = [...]string{
	LatestLegacyVersion,
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
	"2024-10-07",
}

// IsLegacy reports whether version belongs to the initialize-based MCP era.
func IsLegacy(version string) bool {
	for _, supported := range legacyVersions {
		if version == supported {
			return true
		}
	}
	return false
}

// NegotiateLegacy preserves a supported client revision. An initialize client
// proposing an unknown revision receives Workass's latest legacy revision and
// may then accept it or disconnect according to the MCP negotiation contract.
func NegotiateLegacy(requested string) string {
	if IsLegacy(requested) {
		return requested
	}
	return LatestLegacyVersion
}

// LegacyVersions returns a copy ordered newest to oldest.
func LegacyVersions() []string {
	versions := make([]string, len(legacyVersions))
	copy(versions, legacyVersions[:])
	return versions
}

// AllVersions returns every revision exposed by Workass, modern first.
func AllVersions() []string {
	return append([]string{ModernVersion}, LegacyVersions()...)
}
