package main

import (
	"encoding/json"
	"os"
	"testing"
)

func loadTestFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("failed to read fixture %s: %v", name, err)
	}
	return data
}

func TestVerifyValidObjects(t *testing.T) {
	fixtures := []string{"root.json", "identity.json", "core_types.json"}
	for _, f := range fixtures {
		t.Run(f, func(t *testing.T) {
			data := loadTestFixture(t, f)
			if err := VerifyEnvelope(data); err != nil {
				t.Errorf("expected valid signature for %s, got: %v", f, err)
			}
		})
	}
}

func TestVerifyTamperedItem(t *testing.T) {
	data := loadTestFixture(t, "root.json")

	// Parse, tamper with item content, re-marshal
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	var item map[string]any
	if err := json.Unmarshal(raw["item"], &item); err != nil {
		t.Fatal(err)
	}
	item["type"] = "TAMPERED"
	tamperedItem, _ := json.Marshal(item)
	raw["item"] = tamperedItem

	tampered, _ := json.Marshal(raw)
	if err := VerifyEnvelope(tampered); err == nil {
		t.Error("expected verification to fail for tampered item")
	}
}

func TestVerifyTamperedSignature(t *testing.T) {
	data := loadTestFixture(t, "root.json")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	// Replace signature with garbage
	raw["signature"], _ = json.Marshal("AAAA" + string(raw["signature"][1:5]) + "BBBB")
	tampered, _ := json.Marshal(raw)

	if err := VerifyEnvelope(tampered); err == nil {
		t.Error("expected verification to fail for tampered signature")
	}
}

func TestVerifyMissingFields(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"no in field", `{"signature":"x","item":{"id":"a","pubkey":"b","created_at":"c"}}`},
		{"wrong in", `{"in":"wrong","signature":"x","item":{"id":"a","pubkey":"b","created_at":"c"}}`},
		{"no signature", `{"in":"dataverse001","item":{"id":"a","pubkey":"b","created_at":"c"}}`},
		{"no item", `{"in":"dataverse001","signature":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := VerifyEnvelope([]byte(tt.json)); err == nil {
				t.Error("expected error for invalid input")
			}
		})
	}
}

func TestCanonicalJSON(t *testing.T) {
	// Verify our canonical JSON matches expected output
	input := `{"z":1,"a":2,"m":{"b":1,"a":2}}`
	expected := `{"a":2,"m":{"a":2,"b":1},"z":1}`

	result, err := canonicalJSON([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != expected {
		t.Errorf("canonical JSON mismatch\ngot:  %s\nwant: %s", result, expected)
	}
}
