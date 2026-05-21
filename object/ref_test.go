package object

import (
	"strings"
	"testing"
)

func TestIsValidRef(t *testing.T) {
	// 44-char pubkey (compressed P-256) + canonical UUID.
	const validPubkey44 = "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	const validUUID = "346bef5e-94ff-4f7a-bcf6-d78ae1e1541c"
	const validRef = validPubkey44 + "." + validUUID

	// 43-char pubkey is also accepted (32-byte raw key, no padding).
	const validPubkey43 = "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZ"
	const validRef43 = validPubkey43 + "." + validUUID

	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{"valid ref, 44-char pubkey", validRef, true},
		{"valid ref, 43-char pubkey", validRef43, true},
		{"valid ref, zero UUID", validPubkey44 + ".00000000-0000-0000-0000-000000000000", true},
		{"valid ref, uppercase hex UUID", validPubkey44 + ".346BEF5E-94FF-4F7A-BCF6-D78AE1E1541C", true},

		{"empty string", "", false},
		{"missing dot", validPubkey44 + validUUID, false},
		{"only a dot", ".", false},

		{"pubkey too short (42 chars)", strings.Repeat("A", 42) + "." + validUUID, false},
		{"pubkey too long (45 chars)", strings.Repeat("A", 45) + "." + validUUID, false},
		{"pubkey contains slash", strings.Repeat("A", 43) + "/." + validUUID, false},
		{"pubkey contains plus", strings.Repeat("A", 43) + "+." + validUUID, false},
		{"pubkey contains dot", strings.Repeat("A", 43) + "." + "." + validUUID, false},

		{"UUID with non-hex digit g", validPubkey44 + ".g46bef5e-94ff-4f7a-bcf6-d78ae1e1541c", false},
		{"UUID with non-hex digit z", validPubkey44 + ".346befze-94ff-4f7a-bcf6-d78ae1e1541c", false},
		{"UUID missing dash", validPubkey44 + ".346bef5e94ff-4f7a-bcf6-d78ae1e1541c0", false},
		{"UUID extra dash mid-segment", validPubkey44 + ".346bef-e-94ff-4f7a-bcf6-d78ae1e1541c", false},
		{"UUID too short", validPubkey44 + ".346bef5e-94ff-4f7a-bcf6-d78ae1e1541", false},
		{"UUID too long", validPubkey44 + ".346bef5e-94ff-4f7a-bcf6-d78ae1e1541cc", false},

		{"scanner: .env", ".env", false},
		{"scanner: wp-config.php", "wp-config.php", false},
		{"scanner: robots.txt", "robots.txt", false},
		{"scanner: info.php", "info.php", false},
		{"scanner: .git", ".git", false},

		{"very long input", strings.Repeat("A", 500), false},
		{"long string with valid prefix", validRef + strings.Repeat("x", 200), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidRef(tt.ref); got != tt.want {
				t.Errorf("IsValidRef(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}
