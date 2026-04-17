package decoder

import (
	"fmt"
	"sync"
)

// RegistryKey uniquely identifies a decoder by operation type, command, and data kind.
type RegistryKey struct {
	OPType    string   `json:"opType"`
	OPCommand string   `json:"opCommand"`
	Kind      DataKind `json:"kind"`
}

// Registry is a thread-safe map of decoders keyed by RegistryKey.
type Registry struct {
	mu       sync.RWMutex
	decoders map[RegistryKey]Decoder
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{decoders: make(map[RegistryKey]Decoder)}
}

// Register adds a decoder for the given key.
func (r *Registry) Register(key RegistryKey, dec Decoder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decoders[key] = dec
}

// Lookup finds a decoder for the given parameters.
// If no exact match is found, it falls back to an empty OPCommand.
func (r *Registry) Lookup(opType, opCommand string, kind DataKind) (Decoder, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	key := RegistryKey{OPType: opType, OPCommand: opCommand, Kind: kind}
	if dec, ok := r.decoders[key]; ok {
		return dec, nil
	}

	// Fallback: try with empty OPCommand.
	fallback := RegistryKey{OPType: opType, OPCommand: "", Kind: kind}
	if dec, ok := r.decoders[fallback]; ok {
		return dec, nil
	}

	return nil, fmt.Errorf("no decoder registered for (%s, %s, %s)", opType, opCommand, kind)
}

// Keys returns all registered keys.
func (r *Registry) Keys() []RegistryKey {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]RegistryKey, 0, len(r.decoders))
	for k := range r.decoders {
		keys = append(keys, k)
	}
	return keys
}
