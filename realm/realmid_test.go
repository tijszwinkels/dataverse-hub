package realm

import (
	"testing"
)

func TestRealmID_MatchesStandardUUIDv5(t *testing.T) {
	// These expected values were computed with Python's stdlib:
	//   import uuid
	//   NS = uuid.UUID("00000000-0000-0000-0000-000000000000")
	//   uuid.uuid5(NS, "<realm>")
	// and must match exactly — proving the algorithm is the standard UUIDv5,
	// interoperable with any other implementation.
	tests := []struct {
		realm string
		want  string
	}{
		{
			"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam",
			"6bb7d6cc-1556-5a76-a910-edc802d4a2b7",
		},
		{
			"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.MyTeam",
			"56390347-d50c-5dce-970f-b0a0c7abb6de",
		},
		{
			"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.OtherTeam",
			"710628b0-3abc-5f46-9a09-9bceab872a55",
		},
		// Edge case: empty realm string (valid UUIDv5 input; RealmID stays pure
		// and validation-free, so this is allowed and must match Python).
		{
			"",
			"e129f27c-5103-5c5c-844b-cdf0a15e160d",
		},
		// Edge case: name with embedded dots — the whole string is the hash
		// input; only RealmRef's owner split cares about the first dot.
		{
			"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.team.alpha.beta",
			"77a2c246-bf6b-54a0-af79-6ec51e9eb4b5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.realm, func(t *testing.T) {
			got := RealmID(tt.realm)
			if got != tt.want {
				t.Errorf("RealmID(%q)\n  got  = %q\n  want = %q", tt.realm, got, tt.want)
			}
		})
	}
}

func TestRealmID_DeterministicAndDistinct(t *testing.T) {
	r1 := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam"
	r2 := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam"
	r3 := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.OtherTeam"

	a := RealmID(r1)
	b := RealmID(r2)
	c := RealmID(r3)

	if a != b {
		t.Errorf("same realm must hash identically: %q != %q", a, b)
	}
	if a == c {
		t.Errorf("different realms must hash distinctly: both %q", a)
	}
}

func TestRealmID_LongStringIsStable(t *testing.T) {
	// Very long realm string — should not panic and must be deterministic.
	long := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ."
	for i := 0; i < 10000; i++ {
		long += "x"
	}
	a := RealmID(long)
	b := RealmID(long)
	if a != b {
		t.Fatalf("long realm non-deterministic: %q != %q", a, b)
	}
	if len(a) != 36 {
		t.Fatalf("long realm id bad shape %q len=%d", a, len(a))
	}
}

func TestRealmID_VersionAndVariant(t *testing.T) {
	id := RealmID("AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam")
	// Version nibble (5) is the first hex digit of the 3rd group (index 14).
	if id[14] != '5' {
		t.Errorf("expected version nibble '5' at position 14, got %q in %q", id[14], id)
	}
	// Variant starts with '8','9','a', or 'b' at the first hex digit of the 4th group (index 19).
	switch id[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("expected variant nibble in [89ab] at position 19, got %q in %q", id[19], id)
	}
}

func TestRealmRef_ExactValue(t *testing.T) {
	realm := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam"
	ref, err := RealmRef(realm)
	if err != nil {
		t.Fatalf("RealmRef: %v", err)
	}
	want := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.6bb7d6cc-1556-5a76-a910-edc802d4a2b7"
	if ref != want {
		t.Errorf("RealmRef = %q, want %q", ref, want)
	}
}

func TestRealmRef_EmbeddedDotsInName(t *testing.T) {
	realm := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.team.alpha.beta"
	ref, err := RealmRef(realm)
	if err != nil {
		t.Fatalf("RealmRef: %v", err)
	}
	wantOwner := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	// Only the first dot splits the owner; the rest stays in the hashed name.
	if ref[:len(wantOwner)] != wantOwner || ref[len(wantOwner)] != '.' {
		t.Errorf("RealmRef = %q, want owner prefix %q.", ref, wantOwner)
	}
	if got := ref[len(wantOwner)+1:]; got != RealmID(realm) {
		t.Errorf("RealmRef id portion %q != RealmID %q", got, RealmID(realm))
	}
}

func TestRealmRef_RejectsInvalid(t *testing.T) {
	pk := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	tests := []struct {
		name  string
		realm string
	}{
		{"non-pubkey owner", "not-a-pubkey.TeamName"},
		{"no dot at all", pk},
		{"empty name after dot", pk + "."},
		{"empty string", ""},
		{"only a dot", "."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := RealmRef(tt.realm); err == nil {
				t.Errorf("RealmRef(%q) = _, nil; want error", tt.realm)
			}
		})
	}
}

func TestRealmRef_AcceptsDotOnlyName(t *testing.T) {
	// "pk.." splits to owner=pk, name="." (non-empty). That is a valid, if
	// unusual, realm name — only the owner prefix and non-empty name are
	// required, not the name's content. Confirm it is accepted.
	pk := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	ref, err := RealmRef(pk + "..")
	if err != nil {
		t.Fatalf("RealmRef(%q) = _, %v; want nil", pk+"..", err)
	}
	if ref[:len(pk)] != pk || ref[len(pk)] != '.' {
		t.Errorf("RealmRef = %q, want owner prefix %q.", ref, pk)
	}
}

func TestMustParseUUID(t *testing.T) {
	// Non-zero UUID exercises the parser's byte ordering and hex decoding, not
	// just the all-zero namespace (which a buggy parser would also pass).
	got := mustParseUUID("00112233-4455-6677-8899-aabbccddeeff")
	want := [16]byte{
		0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
	}
	if got != want {
		t.Errorf("mustParseUUID = %x, want %x", got, want)
	}
}

func TestMustParseUUID_ZeroNamespace(t *testing.T) {
	// The actual namespace must parse to all-zero bytes.
	got := mustParseUUID(NamespaceUUID)
	for i, b := range got {
		if b != 0 {
			t.Fatalf("namespace byte %d = %#x, want 0", i, b)
		}
	}
}

func TestMustParseUUID_PanicsOnMalformed(t *testing.T) {
	for _, bad := range []string{
		"too short",
		"00000000-0000-0000-0000-00000000000",  // 35 chars
		"0000000x-0000-0000-0000-000000000000",  // bad hex
		"00000000x0000-0000-0000-000000000000",  // missing hyphen
	} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("mustParseUUID(%q) did not panic", bad)
				}
			}()
			_ = mustParseUUID(bad)
		})
	}
}
