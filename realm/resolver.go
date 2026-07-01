package realm

// RealmResolver is the read-side interface for shared-realm membership lookups.
// Both the TOML-based SharedRealms and the graph-based GraphSharedRealms
// implement it, and Merged combines them per the precedence rules (decision 7:
// graph authoritative, TOML override).
//
// All methods must be safe for concurrent use.
type RealmResolver interface {
	// IsSharedRealm reports whether name is a known shared realm.
	IsSharedRealm(name string) bool

	// IsMember reports whether pubkey is a member of the named shared realm.
	IsMember(realmName, pubkey string) bool

	// RealmsForPubkey returns all shared realms the pubkey belongs to.
	// Order is unspecified; callers should sort if needed.
	RealmsForPubkey(pubkey string) []string

	// Count returns the number of known shared realms.
	Count() int
}

// Compile-time assertions that both implementations satisfy the interface.
var (
	_ RealmResolver = (*SharedRealms)(nil)
	_ RealmResolver = (*GraphSharedRealms)(nil)
)

// Merged combines a graph resolver (authoritative) with a TOML resolver
// (additive local override), implementing decision 7: graph is the source of
// truth, TOML is a hub-operator supplement layered on top.
//
// Precedence semantics (additive union, NOT deny-capable):
//   - IsSharedRealm(R): true if EITHER source knows R.
//   - IsMember(R, pk): true if EITHER source grants membership. TOML can
//     ADD members the graph doesn't list (force-grant), but it CANNOT revoke
//     a graph-granted member — there is no per-member deny in this view.
//     Revocation is done in the graph by editing members at a higher revision
//     (decision 5); an explicit deny-list was deferred.
//   - RealmsForPubkey(pk): union of both sources (deduped).
//   - Count(): union count.
//
// A nil graph or nil toml component is treated as empty.
type Merged struct {
	Graph *GraphSharedRealms
	Toml  *SharedRealms
}

// NewMerged returns a Merged resolver. Either argument may be nil.
func NewMerged(graph *GraphSharedRealms, toml *SharedRealms) *Merged {
	return &Merged{Graph: graph, Toml: toml}
}

func (m *Merged) IsSharedRealm(name string) bool {
	if m.Toml != nil && m.Toml.IsSharedRealm(name) {
		return true
	}
	if m.Graph != nil && m.Graph.IsSharedRealm(name) {
		return true
	}
	return false
}

func (m *Merged) IsMember(realmName, pubkey string) bool {
	if m.Toml != nil && m.Toml.IsMember(realmName, pubkey) {
		return true
	}
	if m.Graph != nil && m.Graph.IsMember(realmName, pubkey) {
		return true
	}
	return false
}

func (m *Merged) RealmsForPubkey(pubkey string) []string {
	seen := make(map[string]struct{})
	var result []string
	if m.Toml != nil {
		for _, r := range m.Toml.RealmsForPubkey(pubkey) {
			if _, ok := seen[r]; !ok {
				seen[r] = struct{}{}
				result = append(result, r)
			}
		}
	}
	if m.Graph != nil {
		for _, r := range m.Graph.RealmsForPubkey(pubkey) {
			if _, ok := seen[r]; !ok {
				seen[r] = struct{}{}
				result = append(result, r)
			}
		}
	}
	return result
}

func (m *Merged) Count() int {
	if m.Graph == nil {
		if m.Toml == nil {
			return 0
		}
		return m.Toml.Count()
	}
	if m.Toml == nil {
		return m.Graph.Count()
	}
	// Union count.
	seen := make(map[string]struct{})
	for _, r := range m.Toml.realmsKeys() {
		seen[r] = struct{}{}
	}
	for _, r := range m.Graph.realmsKeys() {
		seen[r] = struct{}{}
	}
	return len(seen)
}
