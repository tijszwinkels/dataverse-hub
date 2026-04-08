package realm

import (
	"testing"

	"github.com/tijszwinkels/dataverse-hub/object"
)

func TestIsPublicRead(t *testing.T) {
	tests := []struct {
		name   string
		realms object.InField
		want   bool
	}{
		{"has dataverse001", object.InField{"dataverse001"}, true},
		{"has server-public", object.InField{"server-public"}, true},
		{"only pubkey realm", object.InField{"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}, false},
		{"both dataverse001 and pubkey", object.InField{"dataverse001", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}, true},
		{"server-public and pubkey", object.InField{"server-public", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}, true},
		{"empty", object.InField{}, false},
		{"nil", nil, false},
		{"other realm only", object.InField{"acme_internal"}, false},
		{"dataverse001 and other", object.InField{"dataverse001", "acme_internal"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPublicRead(tt.realms)
			if got != tt.want {
				t.Errorf("IsPublicRead(%v) = %v, want %v", tt.realms, got, tt.want)
			}
		})
	}
}

func TestIsGlobalObject(t *testing.T) {
	tests := []struct {
		name   string
		realms object.InField
		want   bool
	}{
		{"has dataverse001", object.InField{"dataverse001"}, true},
		{"has server-public", object.InField{"server-public"}, false},
		{"both dataverse001 and server-public", object.InField{"dataverse001", "server-public"}, true},
		{"only pubkey realm", object.InField{"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}, false},
		{"empty", object.InField{}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsGlobalObject(tt.realms)
			if got != tt.want {
				t.Errorf("IsGlobalObject(%v) = %v, want %v", tt.realms, got, tt.want)
			}
		})
	}
}

func TestIsPublicObject(t *testing.T) {
	// IsPublicObject is a backward-compat alias for IsPublicRead
	tests := []struct {
		name   string
		realms object.InField
		want   bool
	}{
		{"has dataverse001", object.InField{"dataverse001"}, true},
		{"has server-public", object.InField{"server-public"}, true},
		{"only pubkey realm", object.InField{"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"}, false},
		{"empty", object.InField{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPublicObject(tt.realms)
			if got != tt.want {
				t.Errorf("IsPublicObject(%v) = %v, want %v", tt.realms, got, tt.want)
			}
		})
	}
}

func TestCanRead_ServerPublic(t *testing.T) {
	pk := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"

	tests := []struct {
		name       string
		realms     []string
		authPubkey string
		want       bool
	}{
		{"server-public no auth", []string{"server-public"}, "", true},
		{"server-public with auth", []string{"server-public"}, pk, true},
		{"server-public and private no auth", []string{"server-public", pk}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanRead(tt.realms, tt.authPubkey, nil)
			if got != tt.want {
				t.Errorf("CanRead(%v, %q, nil) = %v, want %v", tt.realms, tt.authPubkey, got, tt.want)
			}
		})
	}
}

func TestValidateRealmsForPut_ServerPublic(t *testing.T) {
	pk := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"

	tests := []struct {
		name         string
		realms       []string
		signerPubkey string
		want         bool
	}{
		{"server-public accepted", []string{"server-public"}, pk, true},
		{"server-public and pubkey", []string{"server-public", pk}, pk, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateRealmsForPut(tt.realms, tt.signerPubkey, nil)
			if got != tt.want {
				t.Errorf("ValidateRealmsForPut(%v, %q, nil) = %v, want %v", tt.realms, tt.signerPubkey, got, tt.want)
			}
		})
	}
}

func TestHasMatchingRealm(t *testing.T) {
	pk := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	otherPk := "A6yU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"

	tests := []struct {
		name       string
		realms     []string
		authPubkey string
		want       bool
	}{
		{"matching pubkey in realms", []string{pk}, pk, true},
		{"no match", []string{pk}, otherPk, false},
		{"empty pubkey", []string{pk}, "", false},
		{"empty realms", nil, pk, false},
		{"mixed realms with match", []string{"dataverse001", pk}, pk, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasMatchingRealm(tt.realms, tt.authPubkey)
			if got != tt.want {
				t.Errorf("HasMatchingRealm(%v, %q) = %v, want %v", tt.realms, tt.authPubkey, got, tt.want)
			}
		})
	}
}
