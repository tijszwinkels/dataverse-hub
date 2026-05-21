package object

// IsValidRef reports whether s has the canonical dataverse ref shape:
// <base64url-pubkey>.<uuid> where the pubkey is 43-44 chars of [A-Za-z0-9_-]
// and the UUID is the canonical 8-4-4-4-12 hex form.
//
// This is a cheap shape check — it does not verify the pubkey decodes to a
// valid EC point or that the object exists. Its purpose is to short-circuit
// path-derived refs from scanner traffic (/.env, /wp-config.php, …) before
// the caller does any expensive work (upstream calls, disk reads).
func IsValidRef(s string) bool {
	// 43-44 chars pubkey + '.' + 36 chars UUID = 80 or 81 chars total.
	if len(s) < 80 || len(s) > 81 {
		return false
	}
	dot := len(s) - 37 // UUID is 36 chars, plus the '.' separator
	if s[dot] != '.' {
		return false
	}
	if !isBase64URLChars(s[:dot]) {
		return false
	}
	return isCanonicalUUID(s[dot+1:])
}

// isBase64URLChars reports whether every byte of s is a base64url character.
func isBase64URLChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// isCanonicalUUID reports whether s is a canonical 8-4-4-4-12 hex UUID.
// Hex digits are case-insensitive (RFC 4122 §3).
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') ||
				(c >= 'a' && c <= 'f') ||
				(c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}
