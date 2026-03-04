package object

import "testing"

func TestIsPubkeyRealm(t *testing.T) {
	tests := []struct {
		name  string
		realm string
		want  bool
	}{
		// Valid compressed P-256 pubkeys (44-char base64url, 33 bytes, starts with 0x02 or 0x03)
		{"valid pubkey 02 prefix", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ", true},
		// Not a pubkey
		{"dataverse001", "dataverse001", false},
		{"short string", "abc", false},
		{"empty string", "", false},
		// 44 chars but invalid base64url
		{"44 chars invalid base64", "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!", false},
		// Valid base64url, 44 chars, but wrong first byte (not 0x02 or 0x03)
		{"wrong prefix byte 00", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA0", false},
		// 43 chars (too short)
		{"43 chars", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODx", false},
		// 45 chars (too long)
		{"45 chars", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJx", false},
		// Named realm
		{"named realm", "my_private_realm", false},
		// Realistic: another valid pubkey with 03 prefix
		{"valid pubkey 03 prefix", "A6yU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPubkeyRealm(tt.realm)
			if got != tt.want {
				t.Errorf("IsPubkeyRealm(%q) = %v, want %v", tt.realm, got, tt.want)
			}
		})
	}
}

func TestPubkeyRealms(t *testing.T) {
	tests := []struct {
		name   string
		realms InField
		want   []string
	}{
		{"no pubkey realms", InField{"dataverse001"}, nil},
		{"one pubkey realm", InField{"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}, []string{"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}},
		{"mixed", InField{"dataverse001", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}, []string{"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}},
		{"empty", InField{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PubkeyRealms(tt.realms)
			if len(got) != len(tt.want) {
				t.Errorf("PubkeyRealms(%v) = %v, want %v", tt.realms, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("PubkeyRealms(%v)[%d] = %q, want %q", tt.realms, i, got[i], tt.want[i])
				}
			}
		})
	}
}
