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
		// No in anywhere
		{"no in field", `{"signature":"x","item":{"id":"a","pubkey":"b","created_at":"c"}}`},
		// Wrong in on envelope (legacy format)
		{"wrong in on envelope", `{"in":"wrong","signature":"x","item":{"id":"a","pubkey":"b","created_at":"c"}}`},
		// Wrong in in item (new format)
		{"wrong in in item", `{"is":"instructionGraph001","signature":"x","item":{"in":["other"],"id":"a","pubkey":"b","created_at":"c"}}`},
		// Empty array in item
		{"empty array in item", `{"is":"instructionGraph001","signature":"x","item":{"in":[],"id":"a","pubkey":"b","created_at":"c"}}`},
		// No signature (new format)
		{"no signature", `{"is":"instructionGraph001","item":{"in":["dataverse001"],"id":"a","pubkey":"b","created_at":"c"}}`},
		// No item
		{"no item", `{"is":"instructionGraph001","signature":"x"}`},
		// No signature (legacy format)
		{"no signature legacy", `{"in":"dataverse001","item":{"id":"a","pubkey":"b","created_at":"c"}}`},
		// No item (legacy format)
		{"no item legacy", `{"in":"dataverse001","signature":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := VerifyEnvelope([]byte(tt.json)); err == nil {
				t.Error("expected error for invalid input")
			}
		})
	}
}

func TestVerifyAcceptsBothInFormats(t *testing.T) {
	data := loadTestFixture(t, "root.json")

	// Fixture uses new format (is on envelope, in inside item) — verify it passes
	t.Run("new format (in inside item)", func(t *testing.T) {
		if err := VerifyEnvelope(data); err != nil {
			t.Errorf("expected new format to pass, got: %v", err)
		}
	})

	// Convert to legacy format: move in from item to envelope
	t.Run("legacy format (in on envelope)", func(t *testing.T) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		// Remove "is" from envelope, add "in" on envelope
		delete(raw, "is")
		raw["in"], _ = json.Marshal("dataverse001")

		// Remove "in" from item and re-sign
		var item map[string]any
		json.Unmarshal(raw["item"], &item)
		delete(item, "in")
		itemBytes, _ := json.Marshal(item)
		raw["item"] = itemBytes

		legacy, _ := json.Marshal(raw)
		// This will fail signature check since item changed, but should pass the in check
		err := VerifyEnvelope(legacy)
		if err == nil {
			t.Log("legacy format passed (signature happens to match)")
		} else if err.Error() == "missing or wrong 'in' marker" {
			t.Errorf("legacy 'in' on envelope should be accepted, got: %v", err)
		}
		// Other errors (signature failure) are expected since we changed item
	})

	// Test in on envelope as array (legacy with array)
	t.Run("legacy format (in array on envelope)", func(t *testing.T) {
		var raw map[string]json.RawMessage
		json.Unmarshal(data, &raw)
		delete(raw, "is")
		raw["in"], _ = json.Marshal([]string{"dataverse001"})
		var item map[string]any
		json.Unmarshal(raw["item"], &item)
		delete(item, "in")
		itemBytes, _ := json.Marshal(item)
		raw["item"] = itemBytes

		legacy, _ := json.Marshal(raw)
		err := VerifyEnvelope(legacy)
		if err != nil && err.Error() == "missing or wrong 'in' marker" {
			t.Errorf("legacy 'in' array on envelope should be accepted, got: %v", err)
		}
	})
}

func TestVerifyNewEnvelopeFormat(t *testing.T) {
	// Verify the new format works end-to-end with real signatures
	fixtures := []string{"root.json", "identity.json", "core_types.json", "page.json", "app_with_page.json", "blob.json"}
	for _, f := range fixtures {
		t.Run(f, func(t *testing.T) {
			data := loadTestFixture(t, f)

			// Verify structure: should have "is" on envelope and "in" inside item
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			if _, hasIs := raw["is"]; !hasIs {
				t.Error("fixture should have 'is' on envelope")
			}
			if _, hasIn := raw["in"]; hasIn {
				t.Error("fixture should NOT have 'in' on envelope (new format)")
			}

			// Parse item and check in field
			var item map[string]json.RawMessage
			json.Unmarshal(raw["item"], &item)
			if _, hasIn := item["in"]; !hasIn {
				t.Error("fixture item should have 'in' field")
			}

			// Verify signature
			if err := VerifyEnvelope(data); err != nil {
				t.Errorf("expected valid signature, got: %v", err)
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
