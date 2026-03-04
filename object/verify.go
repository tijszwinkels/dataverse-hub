package object

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// VerifyEnvelope validates a signed dataverse001 envelope.
// It checks the magic marker, required fields, and ECDSA P-256 signature.
// Accepts both old format (in on envelope) and new format (in inside item).
func VerifyEnvelope(data []byte) error {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	if env.Signature == "" {
		return errors.New("missing signature")
	}
	if len(env.Item) == 0 {
		return errors.New("missing item")
	}

	// Parse item early — needed for both ResolveIn and field validation
	var item Item
	if err := json.Unmarshal(env.Item, &item); err != nil {
		return fmt.Errorf("invalid item: %w", err)
	}

	// Check realm membership (supports both old and new format)
	// Accept objects in dataverse001 or in a pubkey-realm
	realms := ResolveIn(&env, &item)
	if !realms.Contains("dataverse001") && len(PubkeyRealms(realms)) == 0 {
		return errors.New("missing or wrong 'in' marker")
	}

	if item.Pubkey == "" {
		return errors.New("missing item.pubkey")
	}
	if item.ID == "" {
		return errors.New("missing item.id")
	}
	if item.CreatedAt == "" {
		return errors.New("missing item.created_at")
	}

	// Decode pubkey: base64url -> 33-byte compressed EC point
	pubkeyBytes, err := base64.RawURLEncoding.DecodeString(item.Pubkey)
	if err != nil {
		return fmt.Errorf("invalid pubkey encoding: %w", err)
	}
	if len(pubkeyBytes) != 33 {
		return fmt.Errorf("pubkey must be 33 bytes (compressed), got %d", len(pubkeyBytes))
	}

	pubkey, err := DecompressP256(pubkeyBytes)
	if err != nil {
		return fmt.Errorf("invalid pubkey: %w", err)
	}

	// Canonical JSON of item (sorted keys, compact)
	canonical, err := CanonicalJSON(env.Item)
	if err != nil {
		return fmt.Errorf("canonical JSON failed: %w", err)
	}

	// Hash
	hash := sha256.Sum256(canonical)

	// Decode signature: base64 (standard) -> ASN.1 DER -> (r, s)
	sigBytes, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return fmt.Errorf("invalid signature encoding: %w", err)
	}

	var sig struct {
		R, S *big.Int
	}
	if _, err := asn1.Unmarshal(sigBytes, &sig); err != nil {
		return fmt.Errorf("invalid signature ASN.1: %w", err)
	}

	// Verify
	if !ecdsa.Verify(pubkey, hash[:], sig.R, sig.S) {
		return errors.New("signature verification failed")
	}

	return nil
}

// ParseEnvelope parses a signed envelope and returns the envelope and parsed item.
// Does NOT verify the signature — call VerifyEnvelope first.
func ParseEnvelope(data []byte) (*Envelope, *Item, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var item Item
	if err := json.Unmarshal(env.Item, &item); err != nil {
		return nil, nil, fmt.Errorf("invalid item: %w", err)
	}
	return &env, &item, nil
}

// CanonicalJSON produces compact JSON with sorted keys, matching `jq -cS`.
// Uses SetEscapeHTML(false) to avoid Go's default HTML escaping of <, >, &.
func CanonicalJSON(data []byte) ([]byte, error) {
	var obj any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	// Encode appends a newline — trim it
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

// DecompressP256 takes a 33-byte compressed EC point and returns the public key.
func DecompressP256(compressed []byte) (*ecdsa.PublicKey, error) {
	curve := elliptic.P256()
	x, y := elliptic.UnmarshalCompressed(curve, compressed)
	if x == nil {
		return nil, errors.New("failed to decompress P-256 point")
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}
