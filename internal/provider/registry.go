package provider

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

type Registry struct {
	mu          sync.RWMutex
	definitions map[ID]Definition
}

func NewRegistry() *Registry {
	return &Registry{definitions: make(map[ID]Definition)}
}

func (r *Registry) Register(definition Definition) error {
	if r == nil {
		return errors.New("provider registry is nil")
	}
	definition = definition.Normalize()
	if err := definition.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.definitions[definition.Identity.ID]; exists {
		return fmt.Errorf("provider %q is already registered", definition.Identity.ID)
	}
	r.definitions[definition.Identity.ID] = definition
	return nil
}

func (r *Registry) Resolve(id ID) (Definition, bool) {
	if r == nil {
		return Definition{}, false
	}
	id = NormalizeID(string(id))
	r.mu.RLock()
	definition, ok := r.definitions[id]
	r.mu.RUnlock()
	return definition, ok
}

func (r *Registry) IDs() []ID {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	ids := make([]ID, 0, len(r.definitions))
	for id := range r.definitions {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
