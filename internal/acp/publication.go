package acp

import "sync"

type providerPublication struct {
	mu    sync.Mutex
	users int
}

// Retain one lock for each chat with an active or waiting publication. Count
// waiters before locking so a releasing caller cannot replace their lock. The
// empty key serializes global/unowned events without holding up owned chats.
func (m *Manager) lockProviderPublication(chatID string) *providerPublication {
	m.providerPublicationMu.Lock()
	if m.providerPublications == nil {
		m.providerPublications = make(map[string]*providerPublication)
	}
	publication := m.providerPublications[chatID]
	if publication == nil {
		publication = &providerPublication{}
		m.providerPublications[chatID] = publication
	}
	publication.users++
	m.providerPublicationMu.Unlock()
	publication.mu.Lock()
	return publication
}

func (m *Manager) unlockProviderPublication(chatID string, publication *providerPublication) {
	publication.mu.Unlock()
	m.providerPublicationMu.Lock()
	publication.users--
	if publication.users == 0 {
		delete(m.providerPublications, chatID)
	}
	m.providerPublicationMu.Unlock()
}
