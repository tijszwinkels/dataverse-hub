package main

import "encoding/base64"

// IsPubkeyRealm checks if a realm string looks like a compressed P-256 pubkey.
// Must be 44-char base64url that decodes to 33 bytes starting with 0x02 or 0x03.
func IsPubkeyRealm(realm string) bool {
	if len(realm) != 44 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(realm)
	if err != nil || len(raw) != 33 {
		return false
	}
	return raw[0] == 0x02 || raw[0] == 0x03
}

// IsPublicObject checks if the object belongs to the public dataverse001 realm.
func IsPublicObject(realms InField) bool {
	return realms.Contains("dataverse001")
}

// PubkeyRealms returns all pubkey-realm strings from a realm list.
func PubkeyRealms(realms InField) []string {
	var result []string
	for _, r := range realms {
		if IsPubkeyRealm(r) {
			result = append(result, r)
		}
	}
	return result
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
