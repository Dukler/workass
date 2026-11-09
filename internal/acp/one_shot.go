package acp

import "strings"

// A one-shot prompt: the provider's CLI answers one message read from stdin
// and exits, with no tools, project, settings, MCP servers or saved session.
// It serves small side tasks such as naming a chat. A provider offers it by
// registering a providerOneShotStrategy in its adapter; the rest of Workass
// never names a provider or builds its command line.
type providerOneShotStrategy interface {
	// Models are the models offered for one-shot prompts, cheapest first.
	Models() []OneShotModel
	// Args is the whole command line for one prompt, given the executable.
	Args(command, model, system string) []string
}

// OneShotModel is one model a provider offers for one-shot prompts.
type OneShotModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OneShotModels lists the models a provider offers for one-shot prompts,
// none when it offers no one-shot prompt.
func OneShotModels(providerID string) []OneShotModel {
	strategy := providerAdapterForID(providerID).oneShot
	if strategy == nil {
		return nil
	}
	return append([]OneShotModel(nil), strategy.Models()...)
}

// OneShotCommand is the command line for one one-shot prompt to an enabled
// provider with a model it offers. It names why when there is none.
func (m *Manager) OneShotCommand(providerID, model, system string) ([]string, string) {
	strategy := providerAdapterForID(providerID).oneShot
	if strategy == nil {
		return nil, "this agent cannot answer one-shot prompts"
	}
	offered := false
	for _, candidate := range strategy.Models() {
		offered = offered || candidate.ID == strings.TrimSpace(model)
	}
	if !offered {
		return nil, "this agent does not offer that model here"
	}
	cfg, ok := m.providerSnapshot(providerID)
	if !ok || !cfg.Enabled || cfg.DisabledByUser {
		return nil, "agent not available"
	}
	command := launchCommand(cfg)
	if command == "" {
		return nil, "agent not available"
	}
	return strategy.Args(command, strings.TrimSpace(model), system), ""
}
