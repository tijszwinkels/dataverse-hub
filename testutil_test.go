package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/tijszwinkels/dataverse-hub/object"
)

func loadTestFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("failed to read fixture %s: %v", name, err)
	}
	return data
}

// testKeypair generates a fresh P-256 keypair for testing.
func testKeypair(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	compressed := elliptic.MarshalCompressed(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y)
	pubkeyStr := base64.RawURLEncoding.EncodeToString(compressed)
	return priv, pubkeyStr
}

// signChallenge signs a challenge string with the private key, returning base64 signature.
func signChallenge(t *testing.T, priv *ecdsa.PrivateKey, challenge string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(challenge))
	r, s, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// buildSignedObject canonicalizes the item, signs it with priv, and returns the
// full signed envelope bytes. The item's "pubkey" must match priv.
func buildSignedObject(t *testing.T, priv *ecdsa.PrivateKey, item map[string]any) []byte {
	t.Helper()
	itemJSON, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := object.CanonicalJSON(itemJSON)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(canonical)
	r, s, err := ecdsa.Sign(rand.Reader, priv, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]any{
		"is":        "instructionGraph001",
		"item":      json.RawMessage(itemJSON),
		"signature": base64.StdEncoding.EncodeToString(der),
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// makeBlobWithPageRelation builds a signed PNG BLOB that carries a `page`
// relation pointing at pageRef. Returns the object's ref and envelope bytes.
// Used to verify the page-relation viewer wins over raw BLOB serving for
// HTML-accepting clients.
func makeBlobWithPageRelation(t *testing.T, pageRef string) (string, []byte) {
	t.Helper()
	priv, pubkey := testKeypair(t)
	id := "11111111-2222-4333-8444-555555555555"
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-02-11T18:00:00+01:00",
		"in":         []string{"dataverse001"},
		"type":       "BLOB",
		"content": map[string]any{
			"mime_type": "image/png",
			"data":      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC",
			"filename":  "red.png",
			"size":      69,
		},
		"relations": map[string]any{
			"page": []map[string]any{{"ref": pageRef}},
		},
	}
	return pubkey + "." + id, buildSignedObject(t, priv, item)
}
