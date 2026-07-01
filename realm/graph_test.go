package realm

import (
	"encoding/json"
	"testing"

	"github.com/tijszwinkels/dataverse-hub/object"
)

const (
	testPK  = "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	testPK2 = "A6yU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
	testPK3 = "A7yU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"
)

// sharedRealmItem builds a minimal valid SHARED_REALM item for tests.
func sharedRealmItem(t *testing.T, owner, realmName string, memberPKs []string, revision int) *object.Item {
	t.Helper()
	content, _ := json.Marshal(sharedRealmContent{Realm: realmName})
	var memberRels []json.RawMessage
	for _, pk := range memberPKs {
		// member ref: {pubkey}.{some-uuid}
		raw, _ := json.Marshal(object.RelationRef{Ref: pk + ".00000000-0000-0000-0000-000000000001"})
		memberRels = append(memberRels, raw)
	}
	return &object.Item{
		In:        object.InField{"dataverse001", owner},
		ID:        RealmID(realmName),
		Pubkey:    owner,
		Type:      TypeSharedRealm,
		Revision:  revision,
		Content:   content,
		Relations: map[string][]json.RawMessage{RelationMember: memberRels},
	}
}

func TestParseSharedRealm_Valid(t *testing.T) {
	realmName := testPK + ".AcmeTeam"
	item := sharedRealmItem(t, testPK, realmName, []string{testPK2, testPK3}, 1)

	gotRealm, members, err := ParseSharedRealm(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotRealm != realmName {
		t.Errorf("realm = %q, want %q", gotRealm, realmName)
	}
	if len(members) != 2 {
		t.Fatalf("members = %v, want 2", members)
	}
	// owner is NOT auto-included as a member unless listed.
	for _, m := range members {
		if m != testPK2 && m != testPK3 {
			t.Errorf("unexpected member %q", m)
		}
	}
}

func TestParseSharedRealm_DeterministicIDRequired(t *testing.T) {
	realmName := testPK + ".AcmeTeam"
	item := sharedRealmItem(t, testPK, realmName, []string{testPK2}, 1)
	item.ID = "00000000-0000-0000-0000-000000000000" // wrong id

	if _, _, err := ParseSharedRealm(item); err == nil {
		t.Error("expected error for mismatched id, got nil")
	}
}

func TestParseSharedRealm_OwnerMustMatchSigner(t *testing.T) {
	realmName := testPK + ".AcmeTeam"
	item := sharedRealmItem(t, testPK, realmName, []string{testPK2}, 1)
	item.Pubkey = testPK2 // different signer

	if _, _, err := ParseSharedRealm(item); err == nil {
		t.Error("expected error for non-owner signer, got nil")
	}
}

func TestParseSharedRealm_RejectsNonOwnerPrefixedRealm(t *testing.T) {
	item := sharedRealmItem(t, testPK, "not-a-pubkey.Team", []string{testPK2}, 1)
	// Fix the id to match the (invalid) realm so we test the prefix check, not id.
	item.ID = RealmID("not-a-pubkey.Team")

	if _, _, err := ParseSharedRealm(item); err == nil {
		t.Error("expected error for non-owner-prefixed realm, got nil")
	}
}

func TestParseSharedRealm_RejectsWrongType(t *testing.T) {
	item := sharedRealmItem(t, testPK, testPK+".AcmeTeam", []string{testPK2}, 1)
	item.Type = "POST"

	if _, _, err := ParseSharedRealm(item); err == nil {
		t.Error("expected error for non-SHARED_REALM type, got nil")
	}
}

func TestParseSharedRealm_RejectsMissingContent(t *testing.T) {
	item := sharedRealmItem(t, testPK, testPK+".AcmeTeam", []string{testPK2}, 1)
	item.Content = nil

	if _, _, err := ParseSharedRealm(item); err == nil {
		t.Error("expected error for missing content, got nil")
	}
}

func TestParseSharedRealm_RequiresDataverse001(t *testing.T) {
	// Decision 6: SHARED_REALM objects must propagate globally via dataverse001.
	item := sharedRealmItem(t, testPK, testPK+".AcmeTeam", []string{testPK2}, 1)
	// Remove dataverse001 from item.in — only the owner identity realm remains.
	item.In = object.InField{testPK}

	if _, _, err := ParseSharedRealm(item); err == nil {
		t.Error("expected error when dataverse001 is absent from item.in, got nil")
	}
}

func TestParseSharedRealm_SkipsInvalidMemberRefs(t *testing.T) {
	realmName := testPK + ".AcmeTeam"
	item := sharedRealmItem(t, testPK, realmName, []string{testPK2}, 1)
	// Append a malformed member ref and a non-pubkey ref.
	bad, _ := json.Marshal(object.RelationRef{Ref: "not-a-valid-ref"})
	nonpk, _ := json.Marshal(object.RelationRef{Ref: "short.id"})
	item.Relations[RelationMember] = append(item.Relations[RelationMember], bad, nonpk)

	_, members, err := ParseSharedRealm(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 1 || members[0] != testPK2 {
		t.Errorf("members = %v, want [%s] (invalid refs skipped)", members, testPK2)
	}
}

func TestParseSharedRealm_DedupsMembers(t *testing.T) {
	realmName := testPK + ".AcmeTeam"
	item := sharedRealmItem(t, testPK, realmName, []string{testPK2, testPK2, testPK2}, 1)

	_, members, err := ParseSharedRealm(item)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 1 {
		t.Errorf("members = %v, want 1 (deduped)", members)
	}
}

func TestGraphSharedRealms_AddAndRead(t *testing.T) {
	g := NewGraphSharedRealms()
	realmA := testPK + ".AcmeTeam"
	g.Add(sharedRealmItem(t, testPK, realmA, []string{testPK2, testPK3}, 1))

	if !g.IsSharedRealm(realmA) {
		t.Error("expected realmA to be a shared realm")
	}
	if !g.IsMember(realmA, testPK2) {
		t.Error("expected testPK2 to be a member")
	}
	if g.IsMember(realmA, testPK) {
		t.Error("owner should NOT be auto-member unless listed")
	}
	if g.IsMember(realmA, "nobody") {
		t.Error("nobody should not be a member")
	}

	// RealmsForPubkey index
	r := g.RealmsForPubkey(testPK2)
	if len(r) != 1 || r[0] != realmA {
		t.Errorf("RealmsForPubkey(testPK2) = %v, want [%s]", r, realmA)
	}
	if g.Count() != 1 {
		t.Errorf("Count = %d, want 1", g.Count())
	}
}

func TestGraphSharedRealms_HigherRevisionWins(t *testing.T) {
	g := NewGraphSharedRealms()
	realmA := testPK + ".AcmeTeam"

	// rev 2: members {pk2, pk3}
	g.Add(sharedRealmItem(t, testPK, realmA, []string{testPK2, testPK3}, 2))
	// rev 1: stale, should be ignored
	g.Add(sharedRealmItem(t, testPK, realmA, []string{testPK2}, 1))
	if got := g.Members(realmA); len(got) != 2 {
		t.Errorf("after stale rev 1, members = %v, want 2", got)
	}

	// rev 3: revokes pk3
	g.Add(sharedRealmItem(t, testPK, realmA, []string{testPK2}, 3))
	if g.IsMember(realmA, testPK3) {
		t.Error("pk3 should be revoked by rev 3")
	}
	if !g.IsMember(realmA, testPK2) {
		t.Error("pk2 should still be a member after rev 3")
	}
	// pk3 no longer in the pubkey index
	if r := g.RealmsForPubkey(testPK3); len(r) != 0 {
		t.Errorf("RealmsForPubkey(pk3) = %v, want []", r)
	}
}

func TestGraphSharedRealms_TieBreakDeterministic(t *testing.T) {
	g := NewGraphSharedRealms()
	realmA := testPK + ".AcmeTeam"
	// Same realm, same revision, same ref => deterministic (idempotent).
	item := sharedRealmItem(t, testPK, realmA, []string{testPK2}, 5)
	g.Add(item)
	g.Add(item)
	if got := g.Members(realmA); len(got) != 1 {
		t.Errorf("idempotent add: members = %v, want 1", got)
	}
}

func TestGraphSharedRealms_Remove(t *testing.T) {
	g := NewGraphSharedRealms()
	realmA := testPK + ".AcmeTeam"
	item := sharedRealmItem(t, testPK, realmA, []string{testPK2}, 1)
	g.Add(item)

	g.Remove(item.Ref())
	if g.IsSharedRealm(realmA) {
		t.Error("realm should be gone after Remove")
	}
	if r := g.RealmsForPubkey(testPK2); len(r) != 0 {
		t.Errorf("RealmsForPubkey after remove = %v, want []", r)
	}
}

func TestGraphSharedRealms_LoadReplaces(t *testing.T) {
	g := NewGraphSharedRealms()
	realmA := testPK + ".AcmeTeam"
	realmB := testPK + ".DevTeam"
	g.Add(sharedRealmItem(t, testPK, realmA, []string{testPK2}, 1))

	g.Load([]*object.Item{
		sharedRealmItem(t, testPK, realmB, []string{testPK3}, 1),
	})
	if g.IsSharedRealm(realmA) {
		t.Error("realmA should be gone after Load")
	}
	if !g.IsSharedRealm(realmB) {
		t.Error("realmB should be present after Load")
	}
}

func TestGraphSharedRealms_IgnoresMalformed(t *testing.T) {
	g := NewGraphSharedRealms()
	// Wrong type.
	badType := sharedRealmItem(t, testPK, testPK+".AcmeTeam", []string{testPK2}, 1)
	badType.Type = "POST"
	g.Add(badType)
	// Non-owner signer: realm prefix is testPK, signer is testPK2.
	wrongOwner := sharedRealmItem(t, testPK, testPK+".AcmeTeam", []string{testPK2}, 1)
	wrongOwner.Pubkey = testPK2
	g.Add(wrongOwner)
	// Mismatched id.
	wrongID := sharedRealmItem(t, testPK, testPK+".AcmeTeam", []string{testPK2}, 1)
	wrongID.ID = "00000000-0000-0000-0000-000000000000"
	g.Add(wrongID)

	if g.Count() != 0 {
		t.Errorf("Count = %d, want 0 (all malformed ignored)", g.Count())
	}
}

func TestGraphSharedRealms_EmptyMemberList(t *testing.T) {
	g := NewGraphSharedRealms()
	realmA := testPK + ".AcmeTeam"
	g.Add(sharedRealmItem(t, testPK, realmA, nil, 1))

	if !g.IsSharedRealm(realmA) {
		t.Error("realm with no members should still be a known shared realm")
	}
	if got := g.Members(realmA); len(got) != 0 {
		t.Errorf("Members = %v, want empty", got)
	}
}

func TestRefPubkey(t *testing.T) {
	cases := []struct {
		ref, want string
	}{
		{testPK + ".00000000-0000-0000-0000-000000000001", testPK},
		{testPK + ".some.name.with.dots", testPK},
		{"nodash", ""},
	}
	for _, c := range cases {
		if got := refPubkey(c.ref); got != c.want {
			t.Errorf("refPubkey(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}
