package realm

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/tijszwinkels/dataverse-hub/object"
)

// NamespaceUUID is the fixed UUID used as the UUIDv5 namespace for deriving
// SHARED_REALM object IDs from realm names.
//
// It is the dataverse root node's UUID (00000000-0000-0000-0000-000000000000).
// Using the root as the namespace ties derived realm IDs to the dataverse
// itself, and the all-zero UUID is a clean, well-known, unmistakable value.
//
// IMPORTANT: This value MUST NEVER change after the first SHARED_REALM object
// is published. Changing it would invalidate every existing realm address.
const NamespaceUUID = "00000000-0000-0000-0000-000000000000"

// namespaceBytes is the parsed NamespaceUUID as 16 bytes (in RFC 4122
// "UUID layout" order, i.e. the natural left-to-right display order). It is
// computed once at init via mustParseUUID, which panics on a malformed
// constant — a programmer error, not a runtime input.
var namespaceBytes = mustParseUUID(NamespaceUUID)

// RealmID computes the deterministic UUIDv5 for a realm name.
//
// Given a realm string R, any hub in any language computes:
//
//	id = uuid_v5(NamespaceUUID, R)
//
// and the canonical SHARED_REALM object lives at:
//
//	ref = owner_pubkey + "." + id
//
// where owner_pubkey = R.split(".")[0] (enforced by the prefix ownership rule).
//
// UUIDv5 (RFC 4122 §4.3) is: SHA-1(namespace_bytes || name_bytes), take the
// first 16 bytes, set the version (5) and variant (10xx) bits, format as
// 8-4-4-4-12 lowercase hex.
//
// Using UUIDv5 — a real standard — means independent implementations in any
// language produce the identical address from the identical realm string,
// with no custom byte-slicing to agree on.
func RealmID(realm string) string {
	// UUIDv5 hash input: namespace UUID bytes followed by the name bytes.
	h := sha1.New()
	h.Write(namespaceBytes[:])
	h.Write([]byte(realm))
	sum := h.Sum(nil) // 20 bytes; we use the first 16

	var id [16]byte
	copy(id[:], sum[:16])

	// Set version (5) in the high nibble of byte 6: 0101xxxx.
	id[6] = (id[6] & 0x0f) | 0x50
	// Set variant (10) in the high bits of byte 8: 10xxxxxx.
	id[8] = (id[8] & 0x3f) | 0x80

	return formatUUID(id)
}

// RealmRef computes the canonical composite ref for a SHARED_REALM object
// given its realm name. The owner pubkey is the portion of the realm string
// before the first '.'; it must be a valid pubkey realm (decision 3: prefix
// ownership rule).
//
//	ref = "{owner}.{uuid_v5(NS, realm)}"
//
// Returns an error if the realm is not owner-prefixed with a valid pubkey.
func RealmRef(realm string) (string, error) {
	owner, ok := splitRealmOwner(realm)
	if !ok {
		return "", fmt.Errorf("realm %q is not owner-prefixed ({pubkey}.{Name})", realm)
	}
	return owner + "." + RealmID(realm), nil
}

// splitRealmOwner returns the pubkey portion (before the first '.') of a realm
// string and whether it is a valid pubkey realm with a non-empty Name. The
// Name part after the dot may itself contain dots, so only the first dot
// splits; the Name must be non-empty (i.e. there must be at least one byte
// after the dot).
func splitRealmOwner(realm string) (string, bool) {
	for i := 0; i < len(realm); i++ {
		if realm[i] == '.' {
			if i == len(realm)-1 {
				return "", false // empty Name after the dot
			}
			owner := realm[:i]
			return owner, object.IsPubkeyRealm(owner)
		}
	}
	return "", false
}

// parseUUID and its helpers were removed in favor of mustParseUUID, which
// validates length, hyphen positions, and hex decoding instead of silently
// treating invalid input as zero bytes.

// mustParseUUID converts a canonical 8-4-4-4-12 UUID string into 16 bytes in
// display order. It panics with a clear message on malformed input. Intended
// only for compile-time-known constants; do not feed it user input.
func mustParseUUID(s string) [16]byte {
	const expectLen = 36
	if len(s) != expectLen {
		panic(fmt.Sprintf("mustParseUUID: expected 36-char UUID, got len=%d: %q", len(s), s))
	}
	hyphenAt := []int{8, 13, 18, 23}
	for _, p := range hyphenAt {
		if s[p] != '-' {
			panic(fmt.Sprintf("mustParseUUID: expected '-' at position %d in %q", p, s))
		}
	}
	hexStr := s[:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]
	var b [16]byte
	n, err := hex.Decode(b[:], []byte(hexStr))
	if err != nil || n != 16 {
		panic(fmt.Sprintf("mustParseUUID: invalid hex in %q: %v", s, err))
	}
	return b
}

// formatUUID renders 16 bytes as the canonical 8-4-4-4-12 lowercase hex form.
func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3],
		b[4], b[5],
		b[6], b[7],
		b[8], b[9],
		b[10], b[11], b[12], b[13], b[14], b[15])
}
