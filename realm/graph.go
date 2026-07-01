package realm

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/tijszwinkels/dataverse-hub/object"
)

// SHARED_REALM is the object type that declares which pubkeys have read access
// to a shared realm. Objects of this type carry:
//
//   - item.type = "SHARED_REALM"
//   - item.content.realm   — the realm string (must be owner-prefixed)
//   - item.relations.member — refs whose pubkey portion is granted access
//
// The canonical object for a realm lives at the deterministic address
// {owner-pubkey}.{RealmID(realm)}; higher revision wins (decision 4/5).
const TypeSharedRealm = "SHARED_REALM"

// RelationMember is the relation name listing realm members.
const RelationMember = "member"

// realmEntry is the parsed membership for a single shared realm.
type realmEntry struct {
	owner    string   // owner pubkey (realm prefix before the first '.')
	members  []string // member pubkeys
	revision int      // highest revision seen (for conflict resolution)
	ref      string   // composite ref of the authoritative object
}

// GraphSharedRealms holds shared-realm membership sourced from SHARED_REALM
// objects in the graph. It implements the same read API as SharedRealms so the
// hub can consult either (or a merged view) interchangeably.
//
// Thread-safe for concurrent reads; replaced atomically on Load.
type GraphSharedRealms struct {
	mu      sync.RWMutex
	realms  map[string]*realmEntry // realm string -> entry
	byPK    map[string]map[string]struct{} // pubkey -> set of realm strings (index)
}

// NewGraphSharedRealms creates an empty GraphSharedRealms.
func NewGraphSharedRealms() *GraphSharedRealms {
	return &GraphSharedRealms{
		realms: make(map[string]*realmEntry),
		byPK:   make(map[string]map[string]struct{}),
	}
}

// sharedRealmContent is the item.content shape for a SHARED_REALM object.
type sharedRealmContent struct {
	Realm string `json:"realm"`
}

// ParseSharedRealm validates a SHARED_REALM item and returns its realm string,
// member pubkeys, and the authoritative ref. Returns an error if the object
// does not conform to the type contract:
//
//   - item.type must be "SHARED_REALM"
//   - content.realm must be present and owner-prefixed (decision 3)
//   - the signer (item.pubkey) must own the realm's prefix
//   - item.id must equal RealmID(content.realm) (deterministic address, decision 4)
//   - each relations.member entry's ref must be a valid composite key; the
//     pubkey portion (before the first '.') is the granted member (decision 2)
//
// signature verification is NOT done here — callers must have already verified
// the envelope (object.VerifyEnvelope) before parsing.
func ParseSharedRealm(item *object.Item) (realmName string, members []string, err error) {
	if item == nil {
		return "", nil, fmt.Errorf("nil item")
	}
	if item.Type != TypeSharedRealm {
		return "", nil, fmt.Errorf("type %q is not %q", item.Type, TypeSharedRealm)
	}

	// Parse content.realm.
	if len(item.Content) == 0 {
		return "", nil, fmt.Errorf("missing content.realm")
	}
	var c sharedRealmContent
	if err := json.Unmarshal(item.Content, &c); err != nil {
		return "", nil, fmt.Errorf("invalid content: %w", err)
	}
	if c.Realm == "" {
		return "", nil, fmt.Errorf("missing content.realm")
	}

	// Decision 3: realm must be owner-prefixed with a valid pubkey.
	owner, ok := splitRealmOwner(c.Realm)
	if !ok {
		return "", nil, fmt.Errorf("realm %q is not owner-prefixed ({pubkey}.{Name})", c.Realm)
	}
	// The signer must own the namespace.
	if item.Pubkey != owner {
		return "", nil, fmt.Errorf("signer %q does not own realm %q (owner=%q)", item.Pubkey, c.Realm, owner)
	}

	// Decision 4: item.id must be the deterministic hash of the realm.
	if wantID := RealmID(c.Realm); item.ID != wantID {
		return "", nil, fmt.Errorf("item.id %q != RealmID(realm) %q", item.ID, wantID)
	}

	// Decision 6: SHARED_REALM objects must propagate globally, so they MUST
	// include "dataverse001" in item.in. This keeps realm definitions discoverable
	// by every hub and prevents locally-authoritative realms that other hubs
	// never see. (A realm not in dataverse001 would be private to one hub and
	// could not be resolved by the deterministic address from elsewhere.)
	if !item.In.Contains("dataverse001") {
		return "", nil, fmt.Errorf("SHARED_REALM object must include \"dataverse001\" in item.in (decision 6: global propagation)")
	}

	// Decision 2: extract member pubkeys from relations.member refs.
	members = extractMemberPubkeys(item.Relations[RelationMember])

	return c.Realm, members, nil
}

// extractMemberPubkeys pulls the pubkey portion (before the first '.') from each
// member relation ref. Invalid/empty refs are skipped silently — a malformed
// entry does not invalidate the whole realm, but grants no access.
func extractMemberPubkeys(entries []json.RawMessage) []string {
	var members []string
	seen := make(map[string]struct{})
	for _, raw := range entries {
		var rr object.RelationRef
		if err := json.Unmarshal(raw, &rr); err != nil || rr.Ref == "" {
			continue
		}
		pk := refPubkey(rr.Ref)
		if pk == "" || !object.IsPubkeyRealm(pk) {
			continue
		}
		if _, dup := seen[pk]; dup {
			continue
		}
		seen[pk] = struct{}{}
		members = append(members, pk)
	}
	return members
}

// refPubkey returns the pubkey portion (before the first '.') of a composite
// ref, or "" if there is no dot. It does not validate the pubkey.
func refPubkey(ref string) string {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '.' {
			return ref[:i]
		}
	}
	return ""
}

// Add ingests one SHARED_REALM item into the index. If the item is malformed it
// is ignored. If a newer revision already exists for the same realm, the item
// is ignored (higher revision wins, ties broken by keeping the existing entry).
// Thread-safe.
func (g *GraphSharedRealms) Add(item *object.Item) {
	realmName, members, err := ParseSharedRealm(item)
	if err != nil {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	existing, ok := g.realms[realmName]
	if ok && existing.revision > item.Revision {
		return // stale
	}
	if ok && existing.revision == item.Revision && existing.ref >= item.Ref() {
		return // tie: keep existing deterministically
	}

	// Remove old membership index for this realm before re-adding.
	if ok {
		for _, m := range existing.members {
			delete(g.byPK[m], realmName)
			if len(g.byPK[m]) == 0 {
				delete(g.byPK, m)
			}
		}
	}

	g.realms[realmName] = &realmEntry{
		owner:    item.Pubkey,
		members:  members,
		revision: item.Revision,
		ref:      item.Ref(),
	}
	for _, m := range members {
		set, ok := g.byPK[m]
		if !ok {
			set = make(map[string]struct{})
			g.byPK[m] = set
		}
		set[realmName] = struct{}{}
	}
}

// Remove drops the entry for a realm whose authoritative ref matches the given
// ref. If a different ref is now authoritative (e.g. after a re-add), it is
// kept. This is used when an object is deleted from the store. Thread-safe.
func (g *GraphSharedRealms) Remove(ref string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, e := range g.realms {
		if e.ref == ref {
			for _, m := range e.members {
				delete(g.byPK[m], name)
				if len(g.byPK[m]) == 0 {
					delete(g.byPK, m)
				}
			}
			delete(g.realms, name)
			return
		}
	}
}

// Load replaces the entire graph membership from a set of items atomically.
// Malformed items are skipped. Thread-safe.
func (g *GraphSharedRealms) Load(items []*object.Item) {
	g.mu.Lock()
	g.realms = make(map[string]*realmEntry)
	g.byPK = make(map[string]map[string]struct{})
	g.mu.Unlock()

	for _, item := range items {
		g.Add(item)
	}
}

// Count returns the number of shared realms known from the graph.
func (g *GraphSharedRealms) Count() int {
	if g == nil {
		return 0
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.realms)
}

// IsSharedRealm checks if the name is a known graph-shared realm.
func (g *GraphSharedRealms) IsSharedRealm(name string) bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.realms[name]
	return ok
}

// IsMember checks if pubkey is a member of the given graph-shared realm.
func (g *GraphSharedRealms) IsMember(realmName, pubkey string) bool {
	if g == nil || pubkey == "" {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	e, ok := g.realms[realmName]
	if !ok {
		return false
	}
	for _, m := range e.members {
		if m == pubkey {
			return true
		}
	}
	return false
}

// RealmsForPubkey returns all graph-shared realms the pubkey belongs to.
func (g *GraphSharedRealms) RealmsForPubkey(pubkey string) []string {
	if g == nil || pubkey == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	set, ok := g.byPK[pubkey]
	if !ok {
		return nil
	}
	result := make([]string, 0, len(set))
	for name := range set {
		result = append(result, name)
	}
	return result
}

// Members returns the member pubkeys of a graph-shared realm (or nil). Mainly
// for diagnostics / GET /auth/realms.
func (g *GraphSharedRealms) Members(realmName string) []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	e, ok := g.realms[realmName]
	if !ok {
		return nil
	}
	out := make([]string, len(e.members))
	copy(out, e.members)
	return out
}

// realmsKeys returns all known realm names. Used by Merged for union Count.
func (g *GraphSharedRealms) realmsKeys() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	keys := make([]string, 0, len(g.realms))
	for k := range g.realms {
		keys = append(keys, k)
	}
	return keys
}
