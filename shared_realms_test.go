package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/serving"
	"github.com/tijszwinkels/dataverse-hub/storage"
)

// testHubWithSharedRealms creates a hub with shared realm config for testing.
func testHubWithSharedRealms(t *testing.T, realmConfig map[string][]string) (*httptest.Server, *realm.SharedRealms, func()) {
	t.Helper()

	dir := t.TempDir()
	store, err := storage.NewStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	shared := realm.NewSharedRealms()
	shared.Load(realmConfig)

	index := storage.NewIndex(shared)
	limiter := auth.NewRateLimiter(1000, 100000)
	authStore := auth.NewAuthStore(168 * time.Hour)
	hub := serving.NewHub(store, index, limiter, authStore, "", shared)

	ts := httptest.NewServer(hub.Router())
	return ts, shared, func() {
		ts.Close()
		limiter.Stop()
		authStore.Stop()
	}
}

// --- PUT validation ---

func TestPutInSharedRealm_Accepted(t *testing.T) {
	priv, pubkey := testKeypair(t)
	realmName := pubkey + ".acme-team"

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		realmName: {pubkey},
	})
	defer cleanup()

	id := "a0000001-0001-4001-8001-000000000001"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{realmName}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT in shared realm: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPutInUnconfiguredRealm_Rejected(t *testing.T) {
	priv, pubkey := testKeypair(t)

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		"pk.acme-team": {pubkey},
	})
	defer cleanup()

	id := "a0000002-0002-4002-8002-000000000002"
	ref := pubkey + "." + id
	// "pk.unknown" is not configured
	data := signedObject(t, priv, pubkey, id, []string{"pk.unknown"}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT in unconfigured realm: expected 400, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPutInSharedRealmPlusDataverse_Accepted(t *testing.T) {
	priv, pubkey := testKeypair(t)
	realmName := pubkey + ".team"

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		realmName: {pubkey},
	})
	defer cleanup()

	id := "a0000003-0003-4003-8003-000000000003"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{"dataverse001", realmName}, "NOTE")

	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT in shared+dataverse: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// --- GET access control ---

func TestGetSharedRealmObject_MemberAccess(t *testing.T) {
	priv, pubkey := testKeypair(t)
	otherPriv, otherPubkey := testKeypair(t)
	realmName := pubkey + ".team"

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		realmName: {pubkey, otherPubkey},
	})
	defer cleanup()

	// Create object in shared realm
	id := "b0000001-0001-4001-8001-000000000001"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{realmName}, "NOTE")
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Unauthenticated GET → 404
	resp = doGet(t, ts, "/"+ref)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unauth: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Authenticate as realm member (other user)
	token := authenticateAs(t, ts, otherPriv, otherPubkey)

	// GET as member → 200
	resp = doGetWithToken(t, ts, "/"+ref, token)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET as member: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestGetSharedRealmObject_NonMemberDenied(t *testing.T) {
	priv, pubkey := testKeypair(t)
	nonMemberPriv, nonMemberPubkey := testKeypair(t)
	realmName := pubkey + ".team"

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		realmName: {pubkey}, // nonMember NOT included
	})
	defer cleanup()

	// Create object in shared realm
	id := "b0000002-0002-4002-8002-000000000002"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{realmName}, "NOTE")
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Authenticate as non-member
	token := authenticateAs(t, ts, nonMemberPriv, nonMemberPubkey)

	// GET as non-member → 404
	resp = doGetWithToken(t, ts, "/"+ref, token)
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET as non-member: expected 404, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// --- Search with members_only filter ---

func TestSearchMembersOnly_FilterNonMemberContributions(t *testing.T) {
	priv, pubkey := testKeypair(t)
	memberPriv, memberPubkey := testKeypair(t)
	nonMemberPriv, nonMemberPubkey := testKeypair(t)
	realmName := pubkey + ".team"

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		realmName: {pubkey, memberPubkey}, // nonMember NOT included
	})
	defer cleanup()

	// Member creates an object in the shared realm
	id1 := "c0000001-0001-4001-8001-000000000001"
	ref1 := pubkey + "." + id1
	data1 := signedObject(t, priv, pubkey, id1, []string{realmName}, "NOTE")
	resp := doPut(t, ts, ref1, data1)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT member obj: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Non-member creates an object in the shared realm
	// (hub accepts it because it's a configured realm — write is not gated)
	id2 := "c0000002-0002-4002-8002-000000000002"
	ref2 := nonMemberPubkey + "." + id2
	data2 := signedObject(t, nonMemberPriv, nonMemberPubkey, id2, []string{realmName}, "NOTE")
	resp = doPut(t, ts, ref2, data2)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT non-member obj: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Authenticate as member
	token := authenticateAs(t, ts, memberPriv, memberPubkey)

	// Search with members_only=true (default) — should only see member's object
	resp = doGetWithToken(t, ts, "/search", token)
	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 1 {
		t.Fatalf("members_only search: expected 1 item, got %d", len(list.Items))
	}

	// Search with members_only=false — should see both
	resp = doGetWithToken(t, ts, "/search?members_only=false", token)
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 2 {
		t.Fatalf("members_only=false search: expected 2 items, got %d", len(list.Items))
	}
}

func TestSearchMembersOnly_PublicObjectsAlwaysVisible(t *testing.T) {
	_, pubkey := testKeypair(t)
	nonMemberPriv, nonMemberPubkey := testKeypair(t)
	realmName := pubkey + ".team"

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		realmName: {pubkey},
	})
	defer cleanup()

	// Non-member creates a public object (dataverse001)
	id := "c0000003-0003-4003-8003-000000000003"
	ref := nonMemberPubkey + "." + id
	data := signedObject(t, nonMemberPriv, nonMemberPubkey, id, []string{"dataverse001"}, "POST")
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT public: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unauthenticated search with members_only (default) — public objects visible
	resp = doGet(t, ts, "/search")
	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 1 {
		t.Fatalf("public always visible: expected 1 item, got %d", len(list.Items))
	}
}

// --- /auth/realms endpoint ---

func TestAuthRealms_Unauthenticated(t *testing.T) {
	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		"pk.team": {"alice"},
	})
	defer cleanup()

	resp := doGet(t, ts, "/auth/realms")
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /auth/realms unauth: expected 401, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestAuthRealms_ReturnsUserRealms(t *testing.T) {
	memberPriv2, memberPubkey2 := testKeypair(t)

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		"pk.alpha": {memberPubkey2, "other"},
		"pk.beta":  {memberPubkey2},
		"pk.gamma": {"someone-else"},
	})
	defer cleanup()

	token := authenticateAs(t, ts, memberPriv2, memberPubkey2)
	resp := doGetWithToken(t, ts, "/auth/realms", token)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /auth/realms: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Realms []string `json:"realms"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()

	if len(result.Realms) != 2 {
		t.Fatalf("expected 2 realms, got %d: %v", len(result.Realms), result.Realms)
	}
}

func TestAuthRealms_EmptyForNonMember(t *testing.T) {
	priv, pubkey := testKeypair(t)

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		"pk.team": {"other-pubkey"},
	})
	defer cleanup()

	token := authenticateAs(t, ts, priv, pubkey)
	resp := doGetWithToken(t, ts, "/auth/realms", token)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /auth/realms: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Realms []string `json:"realms"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()

	if len(result.Realms) != 0 {
		t.Fatalf("expected 0 realms, got %d: %v", len(result.Realms), result.Realms)
	}
}

// --- Mixed-realm test: all realm types in one store ---

func TestMixedRealms_AllTypesInOneStore(t *testing.T) {
	ownerPriv, ownerPubkey := testKeypair(t)
	memberPriv, memberPubkey := testKeypair(t)
	outsiderPriv, outsiderPubkey := testKeypair(t)
	sharedRealm := ownerPubkey + ".team"

	ts, _, cleanup := testHubWithSharedRealms(t, map[string][]string{
		sharedRealm: {ownerPubkey, memberPubkey},
	})
	defer cleanup()

	// 1. Public object (dataverse001)
	pubID := "f0000001-0001-4001-8001-000000000001"
	pubRef := ownerPubkey + "." + pubID
	pubData := signedObject(t, ownerPriv, ownerPubkey, pubID, []string{"dataverse001"}, "POST")
	resp := doPut(t, ts, pubRef, pubData)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT public: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// 2. Private object (pubkey-realm)
	privID := "f0000002-0002-4002-8002-000000000002"
	privRef := ownerPubkey + "." + privID
	privData := signedObject(t, ownerPriv, ownerPubkey, privID, []string{ownerPubkey}, "NOTE")
	resp = doPut(t, ts, privRef, privData)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT private: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 3. Shared realm object (by owner)
	sharedID := "f0000003-0003-4003-8003-000000000003"
	sharedRef := ownerPubkey + "." + sharedID
	sharedData := signedObject(t, ownerPriv, ownerPubkey, sharedID, []string{sharedRealm}, "NOTE")
	resp = doPut(t, ts, sharedRef, sharedData)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT shared: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Shared realm object by non-member (write accepted, filtered by members_only)
	outsiderID := "f0000004-0004-4004-8004-000000000004"
	outsiderRef := outsiderPubkey + "." + outsiderID
	outsiderData := signedObject(t, outsiderPriv, outsiderPubkey, outsiderID, []string{sharedRealm}, "NOTE")
	resp = doPut(t, ts, outsiderRef, outsiderData)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT outsider shared: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. Public + shared (dual realm)
	dualID := "f0000005-0005-4005-8005-000000000005"
	dualRef := ownerPubkey + "." + dualID
	dualData := signedObject(t, ownerPriv, ownerPubkey, dualID, []string{"dataverse001", sharedRealm}, "POST")
	resp = doPut(t, ts, dualRef, dualData)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT dual realm: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// --- Unauthenticated: sees public + dual only ---
	resp = doGet(t, ts, "/search?members_only=false")
	var list object.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 2 {
		t.Fatalf("unauth search: expected 2 (public+dual), got %d", len(list.Items))
	}

	// --- Authenticate as member ---
	memberToken := authenticateAs(t, ts, memberPriv, memberPubkey)

	// Member sees: public(1) + shared(1) + dual(1) = 3 (members_only filters outsider)
	resp = doGetWithToken(t, ts, "/search", memberToken)
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 3 {
		t.Fatalf("member search (members_only=true): expected 3, got %d", len(list.Items))
	}

	// Member sees all 4 non-private with members_only=false
	resp = doGetWithToken(t, ts, "/search?members_only=false", memberToken)
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 4 {
		t.Fatalf("member search (members_only=false): expected 4, got %d", len(list.Items))
	}

	// --- Authenticate as owner ---
	ownerToken := authenticateAs(t, ts, ownerPriv, ownerPubkey)

	// Owner sees: public(1) + private(1) + shared(1) + dual(1) = 4 (members_only filters outsider)
	resp = doGetWithToken(t, ts, "/search", ownerToken)
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 4 {
		t.Fatalf("owner search (members_only=true): expected 4, got %d", len(list.Items))
	}

	// Owner with members_only=false sees all 5
	resp = doGetWithToken(t, ts, "/search?members_only=false", ownerToken)
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 5 {
		t.Fatalf("owner search (members_only=false): expected 5, got %d", len(list.Items))
	}

	// --- Authenticate as outsider ---
	outsiderToken := authenticateAs(t, ts, outsiderPriv, outsiderPubkey)

	// Outsider can GET the public object
	resp = doGetWithToken(t, ts, "/"+pubRef, outsiderToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("outsider GET public: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Outsider CANNOT GET the shared realm object (not a member)
	resp = doGetWithToken(t, ts, "/"+sharedRef, outsiderToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider GET shared: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Outsider CANNOT GET the private object
	resp = doGetWithToken(t, ts, "/"+privRef, outsiderToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider GET private: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Outsider CAN GET the dual-realm object (it's in dataverse001)
	resp = doGetWithToken(t, ts, "/"+dualRef, outsiderToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("outsider GET dual: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Hot reload ---

func TestSharedRealms_HotReload(t *testing.T) {
	priv, pubkey := testKeypair(t)
	otherPriv, otherPubkey := testKeypair(t)
	realmName := pubkey + ".team"

	ts, shared, cleanup := testHubWithSharedRealms(t, map[string][]string{
		realmName: {pubkey}, // only pubkey is member initially
	})
	defer cleanup()

	// Create object in shared realm
	id := "d0000001-0001-4001-8001-000000000001"
	ref := pubkey + "." + id
	data := signedObject(t, priv, pubkey, id, []string{realmName}, "NOTE")
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// otherPubkey is NOT a member — should get 404
	otherToken := authenticateAs(t, ts, otherPriv, otherPubkey)
	resp = doGetWithToken(t, ts, "/"+ref, otherToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("before reload: expected 404 for non-member, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Hot reload: add otherPubkey to realm
	shared.Load(map[string][]string{
		realmName: {pubkey, otherPubkey},
	})

	// Now otherPubkey IS a member — should get 200
	resp = doGetWithToken(t, ts, "/"+ref, otherToken)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("after reload: expected 200 for new member, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}
