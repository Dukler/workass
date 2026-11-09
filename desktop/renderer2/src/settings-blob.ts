// The daemon's app-settings blob, reached through the frozen settings:get /
// settings:set channels. settings:get replies wrap the blob as
// `{ settings, models, modes, taskKinds }`; settings:set REPLACES the stored
// blob (it does not deep-merge), so a write MUST round-trip every existing key
// and touch only its own — otherwise it would wipe keys another surface owns.

// Pull the flat settings object out of a settings:get reply. Returns {} when
// the reply is missing or malformed, so a read degrades instead of throwing.
export function settingsFromReply(raw: unknown): Record<string, unknown> {
  if (raw && typeof raw === 'object') {
    const inner = (raw as Record<string, unknown>).settings;
    if (inner && typeof inner === 'object') return inner as Record<string, unknown>;
  }
  return {};
}
