package vhost

import (
	"crypto/sha256"
	"encoding/hex"
)

// PageHash computes a deterministic, DNS-safe subdomain from a composite ref.
// Returns 16 hex characters (first 8 bytes of SHA-256).
func PageHash(ref string) string {
	h := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(h[:8])
}
