package realm

import "github.com/tijszwinkels/dataverse-hub/object"

// IsPublicRead returns true if the object is readable without authentication.
// Both "dataverse001" (global) and "server-public" (hub-local) objects are public-read.
func IsPublicRead(realms object.InField) bool {
	return realms.Contains("dataverse001") || realms.Contains("server-public")
}

// IsGlobalObject returns true if the object should be propagated to upstream hubs.
// Only "dataverse001" objects are global; "server-public" objects stay on the hub.
func IsGlobalObject(realms object.InField) bool {
	return realms.Contains("dataverse001")
}

// IsPublicObject checks if the object is publicly readable.
// Deprecated: use IsPublicRead for read-access checks, IsGlobalObject for propagation.
func IsPublicObject(realms object.InField) bool {
	return IsPublicRead(realms)
}

// HasMatchingRealm checks if the authenticated pubkey appears in the object's realms list.
func HasMatchingRealm(realms []string, authPubkey string) bool {
	if authPubkey == "" {
		return false
	}
	for _, r := range realms {
		if r == authPubkey {
			return true
		}
	}
	return false
}

// ValidateRealmsForPut checks that at least one realm is acceptable for storage.
// Accepts: "dataverse001", "server-public", a self-owned pubkey-realm (matches signerPubkey),
// or a configured shared realm. Returns true if valid.
func ValidateRealmsForPut(realms []string, signerPubkey string, shared *SharedRealms) bool {
	for _, r := range realms {
		if r == "dataverse001" || r == "server-public" {
			return true
		}
		if object.IsPubkeyRealm(r) && r == signerPubkey {
			return true
		}
		if shared != nil && shared.IsSharedRealm(r) {
			return true
		}
	}
	return false
}

// CanRead checks if the given pubkey can read an object with these realms.
// Public objects are always readable. Private objects require matching
// pubkey-realm or shared-realm membership.
func CanRead(realms []string, authPubkey string, shared *SharedRealms) bool {
	if IsPublicObject(realms) {
		return true
	}
	if HasMatchingRealm(realms, authPubkey) {
		return true
	}
	if authPubkey != "" && shared != nil {
		for _, r := range realms {
			if shared.IsMember(r, authPubkey) {
				return true
			}
		}
	}
	return false
}
