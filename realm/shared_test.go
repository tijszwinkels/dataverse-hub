package realm

import (
	"sort"
	"testing"
)

func TestSharedRealms_IsMember(t *testing.T) {
	s := NewSharedRealms()
	s.Load(map[string][]string{
		"pk.acme-team": {"alice", "bob"},
		"pk.dev-team":  {"bob", "charlie"},
	})

	tests := []struct {
		name      string
		realm     string
		pubkey    string
		want      bool
	}{
		{"member of acme", "pk.acme-team", "alice", true},
		{"member of both", "pk.acme-team", "bob", true},
		{"not member", "pk.acme-team", "charlie", false},
		{"member of dev", "pk.dev-team", "charlie", true},
		{"unknown realm", "pk.unknown", "alice", false},
		{"empty pubkey", "pk.acme-team", "", false},
		{"empty realm", "", "alice", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.IsMember(tt.realm, tt.pubkey); got != tt.want {
				t.Errorf("IsMember(%q, %q) = %v, want %v", tt.realm, tt.pubkey, got, tt.want)
			}
		})
	}
}

func TestSharedRealms_IsSharedRealm(t *testing.T) {
	s := NewSharedRealms()
	s.Load(map[string][]string{
		"pk.acme-team": {"alice"},
	})

	if !s.IsSharedRealm("pk.acme-team") {
		t.Error("expected pk.acme-team to be a shared realm")
	}
	if s.IsSharedRealm("pk.unknown") {
		t.Error("expected pk.unknown to NOT be a shared realm")
	}
	if s.IsSharedRealm("dataverse001") {
		t.Error("expected dataverse001 to NOT be a shared realm")
	}
}

func TestSharedRealms_RealmsForPubkey(t *testing.T) {
	s := NewSharedRealms()
	s.Load(map[string][]string{
		"pk.acme-team": {"alice", "bob"},
		"pk.dev-team":  {"bob", "charlie"},
		"pk.ops-team":  {"bob"},
	})

	// bob is in all three
	realms := s.RealmsForPubkey("bob")
	sort.Strings(realms)
	if len(realms) != 3 {
		t.Fatalf("expected 3 realms for bob, got %d: %v", len(realms), realms)
	}

	// alice is in one
	realms = s.RealmsForPubkey("alice")
	if len(realms) != 1 || realms[0] != "pk.acme-team" {
		t.Fatalf("expected [pk.acme-team] for alice, got %v", realms)
	}

	// unknown pubkey
	realms = s.RealmsForPubkey("nobody")
	if len(realms) != 0 {
		t.Fatalf("expected 0 realms for nobody, got %v", realms)
	}

	// empty pubkey
	realms = s.RealmsForPubkey("")
	if realms != nil {
		t.Fatalf("expected nil for empty pubkey, got %v", realms)
	}
}

func TestSharedRealms_Count(t *testing.T) {
	s := NewSharedRealms()
	if s.Count() != 0 {
		t.Fatalf("expected 0, got %d", s.Count())
	}
	s.Load(map[string][]string{
		"a": {"x"},
		"b": {"y"},
	})
	if s.Count() != 2 {
		t.Fatalf("expected 2, got %d", s.Count())
	}
}

func TestSharedRealms_LoadReplacesConfig(t *testing.T) {
	s := NewSharedRealms()
	s.Load(map[string][]string{
		"old-realm": {"alice"},
	})
	if !s.IsMember("old-realm", "alice") {
		t.Fatal("expected alice in old-realm")
	}

	// Replace with new config
	s.Load(map[string][]string{
		"new-realm": {"bob"},
	})
	if s.IsMember("old-realm", "alice") {
		t.Error("old-realm should be gone after Load")
	}
	if !s.IsMember("new-realm", "bob") {
		t.Error("expected bob in new-realm after Load")
	}
	if s.Count() != 1 {
		t.Errorf("expected 1 realm after Load, got %d", s.Count())
	}
}

func TestValidateRealmsForPut(t *testing.T) {
	// Use realistic 44-char base64url pubkeys (valid compressed P-256 points start with 02 or 03)
	pk := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	otherPK := "A6yU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"

	shared := NewSharedRealms()
	shared.Load(map[string][]string{
		pk + ".acme-team": {pk, otherPK},
	})

	tests := []struct {
		name         string
		realms       []string
		signerPubkey string
		shared       *SharedRealms
		want         bool
	}{
		{"dataverse001", []string{"dataverse001"}, pk, shared, true},
		{"self-owned pubkey-realm", []string{pk}, pk, shared, true},
		{"other pubkey-realm", []string{otherPK}, pk, shared, false},
		{"configured shared realm", []string{pk + ".acme-team"}, "charlie", shared, true},
		{"unconfigured realm", []string{"pk.unknown"}, pk, shared, false},
		{"dataverse001 + shared", []string{"dataverse001", pk + ".acme-team"}, "x", shared, true},
		{"empty realms", []string{}, pk, shared, false},
		{"nil shared config", []string{pk + ".acme-team"}, pk, nil, false},
		{"nil shared dataverse001", []string{"dataverse001"}, pk, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateRealmsForPut(tt.realms, tt.signerPubkey, tt.shared); got != tt.want {
				t.Errorf("ValidateRealmsForPut(%v, %q) = %v, want %v", tt.realms, tt.signerPubkey, got, tt.want)
			}
		})
	}
}

func TestCanRead(t *testing.T) {
	shared := NewSharedRealms()
	shared.Load(map[string][]string{
		"pk.acme-team": {"alice", "bob"},
	})

	tests := []struct {
		name       string
		realms     []string
		authPubkey string
		shared     *SharedRealms
		want       bool
	}{
		// Public objects: always readable
		{"public no auth", []string{"dataverse001"}, "", shared, true},
		{"public with auth", []string{"dataverse001"}, "alice", shared, true},

		// Pubkey-realm: owner can read
		{"owner reads own", []string{"alice"}, "alice", shared, true},
		{"stranger can't read private", []string{"alice"}, "bob", shared, false},
		{"no auth can't read private", []string{"alice"}, "", shared, false},

		// Shared realm: member can read
		{"member reads shared", []string{"pk.acme-team"}, "alice", shared, true},
		{"other member reads shared", []string{"pk.acme-team"}, "bob", shared, true},
		{"non-member can't read shared", []string{"pk.acme-team"}, "charlie", shared, false},
		{"no auth can't read shared", []string{"pk.acme-team"}, "", shared, false},

		// Multi-realm: ANY matching realm grants access
		{"multi-realm member match", []string{"pk.acme-team", "alice"}, "alice", shared, true},
		{"multi-realm shared match only", []string{"pk.acme-team", "charlie"}, "bob", shared, true},
		{"public + shared", []string{"dataverse001", "pk.acme-team"}, "charlie", shared, true},

		// nil shared config
		{"nil shared no effect", []string{"pk.acme-team"}, "alice", nil, false},
		{"nil shared public still works", []string{"dataverse001"}, "", nil, true},
		{"nil shared owner still works", []string{"alice"}, "alice", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanRead(tt.realms, tt.authPubkey, tt.shared); got != tt.want {
				t.Errorf("CanRead(%v, %q, shared) = %v, want %v", tt.realms, tt.authPubkey, got, tt.want)
			}
		})
	}
}
