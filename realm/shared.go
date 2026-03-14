package realm

import "sync"

// SharedRealms holds the shared realm configuration.
// Thread-safe for concurrent reads; replaced atomically on Load().
type SharedRealms struct {
	mu     sync.RWMutex
	realms map[string][]string // realm name -> member pubkeys (as set for fast lookup)
}

// NewSharedRealms creates an empty SharedRealms.
func NewSharedRealms() *SharedRealms {
	return &SharedRealms{
		realms: make(map[string][]string),
	}
}

// Load replaces the realm config atomically.
func (s *SharedRealms) Load(realms map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.realms = realms
}

// IsSharedRealm checks if the name is a configured shared realm.
func (s *SharedRealms) IsSharedRealm(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.realms[name]
	return ok
}

// IsMember checks if pubkey is a member of the given realm.
func (s *SharedRealms) IsMember(realmName, pubkey string) bool {
	if pubkey == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	members, ok := s.realms[realmName]
	if !ok {
		return false
	}
	for _, m := range members {
		if m == pubkey {
			return true
		}
	}
	return false
}

// RealmsForPubkey returns all shared realms the pubkey belongs to.
func (s *SharedRealms) RealmsForPubkey(pubkey string) []string {
	if pubkey == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []string
	for name, members := range s.realms {
		for _, m := range members {
			if m == pubkey {
				result = append(result, name)
				break
			}
		}
	}
	return result
}

// Count returns the number of configured shared realms.
func (s *SharedRealms) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.realms)
}
