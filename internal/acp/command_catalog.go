package acp

// Claude commands surface (docs/specs/claude-commands-surface.md): the daemon
// half of the slash-commands / subagent-types / output-styles plumbing. The
// host clamps its catalog before sending; the daemon RE-applies the same §2
// clamps defensively (host binary and daemon can skew), runs every string
// through the redaction prefilter, and keeps one memory-only cache entry per
// chat. Nothing here is ever persisted: not into session-state.json, not into
// chat archives (the 64 MiB hydration lesson).

import (
	"errors"
	"strings"

	providercontract "workass/internal/provider"
)

// §2 clamps — identical numbers on the host (scripts/claude-native-host.mjs).
const (
	maxCatalogCommands = 512
	maxCatalogAgents   = 128
	maxCatalogStyles   = 64
	maxCatalogNameLen  = 80
	maxCatalogDescLen  = 200
	maxCatalogAliases  = 4
)

// commandCatalogEntry is one chat's cached catalog. Keyed by tab/chat, NOT by
// session id — compaction and recovery rotate the session id while the chat
// identity stays put — so the current session id lives inside the entry. A nil
// catalog is a deliberate UNKNOWN (an old host attached and proved nothing).
type commandCatalogEntry struct {
	sessionID string
	catalog   *CommandCatalog
}

func commandCatalogKey(tabID, chatID string) string {
	return strings.TrimSpace(tabID) + "\x00" + strings.TrimSpace(chatID)
}

// clipCatalogString redacts first, then clips: redaction can lengthen a value
// ("x=[redacted]"), so clipping after keeps the §2 limit honest, and a clip of
// already-redacted text can never reintroduce a secret. Clipping counts runes so
// a defensive re-clamp of a host-clipped multibyte string never splits one.
func clipCatalogString(v any, limit int) string {
	s := redactSensitiveText(asString(v))
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// parseCommandCatalog re-clamps and redacts one host commandCatalog payload.
// A payload that is not a JSON object is invalid → nil (UNKNOWN). Within a
// valid payload, an absent list stays nil (UNKNOWN) and an empty list stays
// empty (proven) — the two must never collapse. Overflow entries are dropped
// and counted on top of whatever the host already counted; too-long fields are
// clipped, never dropped.
func parseCommandCatalog(raw any) *CommandCatalog {
	obj, ok := raw.(map[string]any)
	if !ok || obj == nil {
		return nil
	}
	catalog := &CommandCatalog{
		OutputStyle:       clipCatalogString(obj["outputStyle"], maxCatalogNameLen),
		CommandsTruncated: numberOrZero(obj["commandsTruncated"]),
		AgentsTruncated:   numberOrZero(obj["agentsTruncated"]),
		StylesTruncated:   numberOrZero(obj["stylesTruncated"]),
	}
	if asOf, ok := int64FromAny(obj["asOf"]); ok {
		catalog.AsOf = asOf
	}
	if rawCommands, ok := obj["commands"].([]any); ok {
		kept := min(len(rawCommands), maxCatalogCommands)
		catalog.CommandsTruncated += len(rawCommands) - kept
		catalog.Commands = make([]CommandCatalogCommand, 0, kept)
		for _, entry := range rawCommands[:kept] {
			command := mapFromAny(entry)
			row := CommandCatalogCommand{
				Name:         clipCatalogString(command["name"], maxCatalogNameLen),
				Description:  clipCatalogString(command["description"], maxCatalogDescLen),
				ArgumentHint: clipCatalogString(command["argumentHint"], maxCatalogNameLen),
			}
			if rawAliases, ok := command["aliases"].([]any); ok && len(rawAliases) > 0 {
				row.Aliases = make([]string, 0, min(len(rawAliases), maxCatalogAliases))
				for _, alias := range rawAliases[:min(len(rawAliases), maxCatalogAliases)] {
					row.Aliases = append(row.Aliases, clipCatalogString(alias, maxCatalogNameLen))
				}
			}
			catalog.Commands = append(catalog.Commands, row)
		}
	}
	if rawAgents, ok := obj["agents"].([]any); ok {
		kept := min(len(rawAgents), maxCatalogAgents)
		catalog.AgentsTruncated += len(rawAgents) - kept
		catalog.Agents = make([]CommandCatalogAgent, 0, kept)
		for _, entry := range rawAgents[:kept] {
			agent := mapFromAny(entry)
			catalog.Agents = append(catalog.Agents, CommandCatalogAgent{
				Name:        clipCatalogString(agent["name"], maxCatalogNameLen),
				Description: clipCatalogString(agent["description"], maxCatalogDescLen),
				Model:       clipCatalogString(agent["model"], maxCatalogNameLen),
			})
		}
	}
	if rawStyles, ok := obj["availableOutputStyles"].([]any); ok {
		kept := min(len(rawStyles), maxCatalogStyles)
		catalog.StylesTruncated += len(rawStyles) - kept
		catalog.AvailableOutputStyles = make([]string, 0, kept)
		for _, style := range rawStyles[:kept] {
			catalog.AvailableOutputStyles = append(catalog.AvailableOutputStyles, clipCatalogString(style, maxCatalogNameLen))
		}
	}
	return catalog
}

// applyCommandCatalog is the ONE apply path for both a session-open reply's
// commandCatalog field (attachSession) and a mid-session
// _workass_claude_commands update: parse + re-clamp + redact, replace the
// chat's memory-only cache entry wholesale, and announce it. Gated on the
// registered provider command-catalog facet. Other providers never emit the
// field, and a stray payload must not invent a surface for them. A
// missing/invalid payload stores nil (UNKNOWN, never proven-empty) and
// announces nothing.
func (b *Bridge) applyCommandCatalog(sessionID string, raw any) *CommandCatalog {
	return providerAdapterForID(b.ProviderID()).commands.Apply(b, sessionID, raw)
}

// storeCommandCatalog first commits the complete capability snapshot through
// the chat actor, then updates the manager's replaceable runtime cache and
// emits the frozen additive event. The manager cache is never recovery
// authority: late clients and restarted daemons project the actor lane.
func (m *Manager) storeCommandCatalog(tabID, chatID, sessionID string, catalog *CommandCatalog) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" && chatID == "" {
		// A spare or ephemeral attach has no chat identity to key or notify;
		// the catalog still rides its SessionInfo and is published at adoption.
		return
	}
	if lane := m.providerLaneForSession(sessionID); lane != nil {
		lane.mu.Lock()
		lane.info.CommandCatalog = cloneCommandCatalog(catalog)
		lane.mu.Unlock()
		snapshot := lane.AttachmentSnapshot()
		if err := lane.emit(providercontract.Event{
			Kind:       providercontract.EventLaneCapabilities,
			Attachment: &snapshot,
		}); err != nil {
			m.opts.Logf("provider command catalog rejected before publication", map[string]any{
				"chatId": chatID, "sessionId": sessionID, "error": redactSensitiveText(err.Error()),
			})
			return
		}
		// The frozen event and manager cache use the exact post-commit snapshot.
		catalog = cloneCommandCatalog(lane.info.CommandCatalog)
	}
	m.mu.Lock()
	m.commandCatalogs[commandCatalogKey(tabID, chatID)] = &commandCatalogEntry{sessionID: sessionID, catalog: catalog}
	m.mu.Unlock()
	if catalog == nil {
		// UNKNOWN replaced the cache; there is nothing to announce.
		return
	}
	m.emit("chat:commands", map[string]any{
		"tabId":          nullableString(tabID),
		"chatId":         nullableString(chatID),
		"sessionId":      sessionID,
		"commandCatalog": catalog,
	})
}

func cloneCommandCatalog(catalog *CommandCatalog) *CommandCatalog {
	if catalog == nil {
		return nil
	}
	out := *catalog
	out.Commands = append([]CommandCatalogCommand(nil), catalog.Commands...)
	for i := range out.Commands {
		out.Commands[i].Aliases = append([]string(nil), catalog.Commands[i].Aliases...)
	}
	out.Agents = append([]CommandCatalogAgent(nil), catalog.Agents...)
	out.AvailableOutputStyles = append([]string(nil), catalog.AvailableOutputStyles...)
	return &out
}

// verifyProviderLaneCommandCatalog makes chat:commands an explicitly
// classified post-commit projection. A lane-owned command event can never be
// used as an alternate semantic ingress.
func (m *Manager) verifyProviderLaneCommandCatalog(payload map[string]any) error {
	sessionID := strings.TrimSpace(asString(payload["sessionId"]))
	lane := m.providerLaneForSession(sessionID)
	if lane == nil {
		return nil // Standalone manager fixture or ephemeral provider probe.
	}
	if lane.identity.ChatID != strings.TrimSpace(asString(payload["chatId"])) || lane.owner.TabID != strings.TrimSpace(asString(payload["tabId"])) {
		return errors.New("command catalog event changed its actor lane owner")
	}
	catalog, _ := payload["commandCatalog"].(*CommandCatalog)
	lane.mu.Lock()
	committed := lane.info.CommandCatalog
	lane.mu.Unlock()
	if catalog == nil || committed == nil || catalog.AsOf != committed.AsOf {
		return errors.New("command catalog event was not committed through the actor lane")
	}
	return nil
}

// forgetCommandCatalogSessionLocked drops cache entries owned by a forgotten/
// closed session. Caller holds m.mu. Entries replaced by a newer session id
// (compaction, recovery) no longer match and are left alone.
func (m *Manager) forgetCommandCatalogSessionLocked(sessionID string) {
	for key, entry := range m.commandCatalogs {
		if entry != nil && entry.sessionID == sessionID {
			delete(m.commandCatalogs, key)
		}
	}
}

// forgetCommandCatalogChatLocked drops every cache entry for a logical chat's
// tab at the explicit chat-delete boundary. Caller holds m.mu.
func (m *Manager) forgetCommandCatalogChatLocked(tabID string) {
	tabID = strings.TrimSpace(tabID)
	for key := range m.commandCatalogs {
		if entryTab, _, ok := strings.Cut(key, "\x00"); ok && entryTab == tabID {
			delete(m.commandCatalogs, key)
		}
	}
}

type providerCommandCatalogStrategy interface {
	Supported(*Bridge) bool
	Apply(*Bridge, string, any) *CommandCatalog
}

type unsupportedCommandCatalogStrategy struct{}

func (unsupportedCommandCatalogStrategy) Supported(*Bridge) bool { return false }
func (unsupportedCommandCatalogStrategy) Apply(*Bridge, string, any) *CommandCatalog {
	return nil
}

type capabilityCommandCatalogStrategy struct {
	capability string
}

func (s capabilityCommandCatalogStrategy) Supported(bridge *Bridge) bool {
	return bridge != nil && bridge.hasProviderCapability(s.capability)
}

func (s capabilityCommandCatalogStrategy) Apply(bridge *Bridge, sessionID string, raw any) *CommandCatalog {
	if bridge == nil {
		return nil
	}
	catalog := parseCommandCatalog(raw)
	tabID, chatID := bridge.chatIdentity()
	bridge.manager.storeCommandCatalog(tabID, chatID, sessionID, catalog)
	return catalog
}

func (b *Bridge) supportsProviderCommandCatalog() bool {
	return providerAdapterForID(b.ProviderID()).commands.Supported(b)
}

// ChatCommands answers the additive chat:commands-get invoke:
// supported:false for providers without the registered facet, for chats this daemon has never
// attached, or when the host never advertised workassClaudeCommandCatalog;
// live:false when the chat's engine is hibernated or gone (the catalog, if
// any, is a cached snapshot); commandCatalog null = UNKNOWN.
func (m *Manager) ChatCommands(tabID, chatID string) map[string]any {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	m.mu.Lock()
	entry := m.commandCatalogs[commandCatalogKey(tabID, chatID)]
	bridge := m.bridgeForFallbackLocked(SessionOptions{TabID: tabID, ChatID: chatID})
	m.mu.Unlock()
	reply := map[string]any{"supported": false, "live": false, "commandCatalog": nil}
	if bridge == nil || !bridge.supportsProviderCommandCatalog() {
		return reply
	}
	reply["supported"] = true
	reply["live"] = !bridge.Closed() && !bridge.Hibernated()
	if entry != nil && entry.catalog != nil {
		reply["commandCatalog"] = entry.catalog
	}
	return reply
}
