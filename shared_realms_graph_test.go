package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
)

// graphRealmHub builds a hub whose ONLY shared-realm config is the graph
// (no TOML). Returns the server, the graph index, and a cleanup func.
func graphRealmHub(t *testing.T) (*httptest.Server, *realm.GraphSharedRealms, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	shared := realm.NewSharedRealms() // empty TOML
	index := storage.NewIndex(shared)
	index.SetGraphRealms(realm.NewGraphSharedRealms())
	limiter := auth.NewRateLimiter(1000, 100000)
	authStore := auth.NewAuthStore(168 * time.Hour)
	hub := serving.NewHub(store, index, limiter, authStore, "", shared)
	ts := httptest.NewServer(hub.Router())
	return ts, index.GraphRealms(), func() {
		ts.Close()
		limiter.Stop()
		authStore.Stop()
	}
}

// sharedRealmObject builds a signed SHARED_REALM envelope with the given members.
func sharedRealmObject(t *testing.T, priv *ecdsa.PrivateKey, owner, realmName string, memberPKs []string, revision int) (string, []byte) {
	t.Helper()
	id := realm.RealmID(realmName)
	ref := owner + "." + id
	memberRels := make([]map[string]string, len(memberPKs))
	for i, pk := range memberPKs {
		memberRels[i] = map[string]string{"ref": pk + ".00000000-0000-0000-0000-0000000000ff"}
	}
	item := map[string]any{
		"in":         []string{"dataverse001", owner},
		"id":         id,
		"pubkey":     owner,
		"created_at": "2026-06-30T12:00:00Z",
		"updated_at": "2026-06-30T12:00:00Z",
		"revision":   revision,
		"type":       realm.TypeSharedRealm,
		"name":       "Test Team",
		"content":    map[string]string{"realm": realmName},
		"relations":  map[string]any{"member": memberRels},
	}
	return ref, buildSignedObject(t, priv, item)
}

// --- PUT validation ---

func TestGraphSharedRealm_PutAccepted(t *testing.T) {
	ownerPriv, owner := testKeypair(t)
	_, member := testKeypair(t)
	realmName := owner + ".AcmeTeam"

	ts, g, cleanup := graphRealmHub(t)
	defer cleanup()

	ref, data := sharedRealmObject(t, ownerPriv, owner, realmName, []string{member}, 1)
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT SHARED_REALM: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	if !g.IsSharedRealm(realmName) {
		t.Error("expected realm to be registered in graph after PUT")
	}
	if !g.IsMember(realmName, member) {
		t.Error("expected member to be registered after PUT")
	}
}

func TestGraphSharedRealm_RejectsWrongID(t *testing.T) {
	ownerPriv, owner := testKeypair(t)
	_, member := testKeypair(t)
	realmName := owner + ".AcmeTeam"

	ts, _, cleanup := graphRealmHub(t)
	defer cleanup()

	ref, data := sharedRealmObject(t, ownerPriv, owner, realmName, []string{member}, 1)
	// Corrupt the id so it no longer equals RealmID(realmName).
	var env map[string]json.RawMessage
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	var item map[string]any
	json.Unmarshal(env["item"], &item)
	item["id"] = "00000000-0000-0000-0000-0000000000aa"
	itemJSON, _ := json.Marshal(item)
	env["item"] = itemJSON
	data, _ = json.Marshal(env)
	// ref in URL must match the (corrupted) item id for the handler to proceed;
	// rebuild ref from the corrupted id.
	ref = owner + "." + "00000000-0000-0000-0000-0000000000aa"

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for wrong id, got %d: %s", resp.StatusCode, body)
	}
}

func TestGraphSharedRealm_RejectsNonOwnerSigner(t *testing.T) {
	_, owner := testKeypair(t)
	otherPriv, other := testKeypair(t)
	_, member := testKeypair(t)
	realmName := owner + ".AcmeTeam" // owner-prefixed by `owner`

	ts, _, cleanup := graphRealmHub(t)
	defer cleanup()

	// Sign with `other` but claim a realm owned by `owner`.
	ref, data := sharedRealmObject(t, otherPriv, other, realmName, []string{member}, 1)
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for non-owner signer, got %d: %s", resp.StatusCode, body)
	}
}

// --- Read gating ---

func TestGraphSharedRealm_MemberCanRead_NonMemberCannot(t *testing.T) {
	ownerPriv, owner := testKeypair(t)
	memberPriv, member := testKeypair(t)
	strangerPriv, stranger := testKeypair(t)
	realmName := owner + ".AcmeTeam"

	ts, g, cleanup := graphRealmHub(t)
	defer cleanup()

	// 1. Owner publishes the SHARED_REALM with `member` as a member.
	srRef, srData := sharedRealmObject(t, ownerPriv, owner, realmName, []string{member}, 1)
	if resp := doPut(t, ts, srRef, srData); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT SHARED_REALM: %d %s", resp.StatusCode, body)
	}

	// 2. Owner publishes a private NOTE in the shared realm.
	noteID := "22222222-3333-4444-9555-666666666666"
	noteRef := owner + "." + noteID
	noteData := signedObject(t, ownerPriv, owner, noteID, []string{realmName}, "NOTE")
	if resp := doPut(t, ts, noteRef, noteData); resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT NOTE in shared realm: %d %s", resp.StatusCode, body)
	}

	// Sanity: graph knows the realm + member.
	if !g.IsMember(realmName, member) {
		t.Fatal("graph should know member")
	}

	// 3. Unauthenticated → 404 (existence hidden).
	resp := doGet(t, ts, "/"+noteRef)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unauthenticated: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Member authenticated → 200.
	memberToken := authenticateAs(t, ts, memberPriv, member)
	resp = doGetWithToken(t, ts, "/"+noteRef, memberToken)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("member: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// 5. Stranger authenticated → 404.
	strangerToken := authenticateAs(t, ts, strangerPriv, stranger)
	resp = doGetWithToken(t, ts, "/"+noteRef, strangerToken)
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("stranger: expected 404, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// --- Revocation via higher revision ---

func TestGraphSharedRealm_RevocationByEdit(t *testing.T) {
	ownerPriv, owner := testKeypair(t)
	_, member := testKeypair(t)
	realmName := owner + ".AcmeTeam"

	ts, g, cleanup := graphRealmHub(t)
	defer cleanup()

	// rev 1: member is in.
	srRef, srData := sharedRealmObject(t, ownerPriv, owner, realmName, []string{member}, 1)
	if resp := doPut(t, ts, srRef, srData); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT rev1: %d", resp.StatusCode)
	}
	if !g.IsMember(realmName, member) {
		t.Fatal("member should be in after rev1")
	}

	// rev 2: member removed (update of existing ref → 200).
	_, srData2 := sharedRealmObject(t, ownerPriv, owner, realmName, nil, 2)
	resp := doPut(t, ts, srRef, srData2)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT rev2: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if g.IsMember(realmName, member) {
		t.Error("member should be revoked after rev2")
	}
}

// --- /auth/realms surfaces graph membership ---

func TestGraphSharedRealm_AuthRealmsEndpoint(t *testing.T) {
	ownerPriv, owner := testKeypair(t)
	memberPriv, member := testKeypair(t)
	realmName := owner + ".AcmeTeam"

	ts, _, cleanup := graphRealmHub(t)
	defer cleanup()

	srRef, srData := sharedRealmObject(t, ownerPriv, owner, realmName, []string{member}, 1)
	if resp := doPut(t, ts, srRef, srData); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT SHARED_REALM: %d", resp.StatusCode)
	}

	token := authenticateAs(t, ts, memberPriv, member)
	resp := doGetWithToken(t, ts, "/auth/realms", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /auth/realms: %d", resp.StatusCode)
	}
	var out struct {
		Realms []string `json:"realms"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	found := false
	for _, r := range out.Realms {
		if r == realmName {
			found = true
		}
	}
	if !found {
		t.Errorf("/auth/realms = %v, expected to contain %s", out.Realms, realmName)
	}
}
