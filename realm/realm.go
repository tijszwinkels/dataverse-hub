package realm

import "github.com/tijszwinkels/dataverse-hub/object"

// IsPublicObject checks if the object belongs to the public dataverse001 realm.
func IsPublicObject(realms object.InField) bool {
	return realms.Contains("dataverse001")
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
