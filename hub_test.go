package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// testHub creates a Hub with a temp store directory and returns the server + cleanup func.
func testHub(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	index := NewIndex()
	limiter := NewRateLimiter(1000, 100000) // generous limits for tests
	hub := NewHub(store, index, limiter)

	ts := httptest.NewServer(hub.Router())
	return ts, func() {
		ts.Close()
		limiter.Stop()
	}
}

func TestPutAndGetObject(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var item Item
	var env Envelope
	json.Unmarshal(data, &env)
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// GET
	resp = doGet(t, ts, "/v1/objects/"+ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Verify the returned object is valid JSON with the correct ref
	var gotEnv Envelope
	if err := json.Unmarshal(body, &gotEnv); err != nil {
		t.Fatalf("GET returned invalid JSON: %v", err)
	}
	var gotItem Item
	json.Unmarshal(gotEnv.Item, &gotItem)
	if gotItem.Ref() != ref {
		t.Errorf("GET returned wrong ref: got %s, want %s", gotItem.Ref(), ref)
	}
}

func TestPutRevisionConflict(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT first time
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first PUT expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// PUT same revision again -> 409
	resp = doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("second PUT expected 409, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPutInvalidSignature(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")

	// Tamper with content
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var item map[string]any
	json.Unmarshal(raw["item"], &item)
	item["type"] = "TAMPERED"
	tamperedItem, _ := json.Marshal(item)
	raw["item"] = tamperedItem
	tampered, _ := json.Marshal(raw)

	ref := item["pubkey"].(string) + "." + item["id"].(string)
	resp := doPut(t, ts, ref, tampered)
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("tampered PUT expected 400, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPutRefMismatch(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	resp := doPut(t, ts, "wrong.ref-value", data)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong ref PUT expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestGetNotFound(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	resp := doGet(t, ts, "/v1/objects/nonexistent.00000000-0000-0000-0000-000000000000")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestListObjects(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	// PUT all three fixtures
	fixtures := []string{"root.json", "identity.json", "core_types.json"}
	for _, f := range fixtures {
		data := loadTestFixture(t, f)
		var env Envelope
		json.Unmarshal(data, &env)
		var item Item
		json.Unmarshal(env.Item, &item)
		resp := doPut(t, ts, item.Ref(), data)
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT %s expected 201, got %d: %s", f, resp.StatusCode, body)
		}
		resp.Body.Close()
	}

	// List all
	resp := doGet(t, ts, "/v1/objects")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list expected 200, got %d", resp.StatusCode)
	}
	var list ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	if len(list.Items) != 3 {
		t.Errorf("expected 3 items, got %d", len(list.Items))
	}

	// List by pubkey
	resp = doGet(t, ts, "/v1/objects?by=AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 3 {
		t.Errorf("expected 3 items for pubkey filter, got %d", len(list.Items))
	}

	// List by type
	resp = doGet(t, ts, "/v1/objects?type=ROOT")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Items) != 1 {
		t.Errorf("expected 1 ROOT item, got %d", len(list.Items))
	}
}

func TestListPagination(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	// PUT all three fixtures
	fixtures := []string{"root.json", "identity.json", "core_types.json"}
	for _, f := range fixtures {
		data := loadTestFixture(t, f)
		var env Envelope
		json.Unmarshal(data, &env)
		var item Item
		json.Unmarshal(env.Item, &item)
		resp := doPut(t, ts, item.Ref(), data)
		resp.Body.Close()
	}

	// Page with limit=2
	resp := doGet(t, ts, "/v1/objects?limit=2")
	var list ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	if len(list.Items) != 2 {
		t.Fatalf("page 1: expected 2 items, got %d", len(list.Items))
	}
	if !list.HasMore {
		t.Error("page 1: expected has_more=true")
	}
	if list.Cursor == nil {
		t.Fatal("page 1: expected cursor")
	}

	// Page 2
	resp = doGet(t, ts, "/v1/objects?limit=2&cursor="+*list.Cursor)
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	if len(list.Items) != 1 {
		t.Errorf("page 2: expected 1 item, got %d", len(list.Items))
	}
	if list.HasMore {
		t.Error("page 2: expected has_more=false")
	}
}

func TestInboundRelations(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	// Load all fixtures from testdata + extra objects that have relations
	putAllFixtures(t, ts)

	// The root object is referenced by identity (via "root" relation) and core_types (via "root" relation)
	rootRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000"
	resp := doGet(t, ts, "/v1/objects/"+rootRef+"/inbound")
	var list ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	// core_types has a root relation to root
	if len(list.Items) == 0 {
		t.Error("expected at least 1 inbound relation to root")
	}

	// Filter by relation type
	resp = doGet(t, ts, "/v1/objects/"+rootRef+"/inbound?relation=root")
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	for _, raw := range list.Items {
		var env Envelope
		json.Unmarshal(raw, &env)
		var item Item
		json.Unmarshal(env.Item, &item)
		// Verify this item actually has a root relation pointing at rootRef
		found := false
		for _, rel := range item.Relations["root"] {
			var rr RelationRef
			json.Unmarshal(rel, &rr)
			if rr.Ref == rootRef {
				found = true
			}
		}
		if !found {
			t.Errorf("inbound item %s does not have root relation to %s", item.Ref(), rootRef)
		}
	}
}

func TestInboundRelationsWithStoredFixtures(t *testing.T) {
	// Use a temp dir and seed it with fixtures, then rebuild index
	dir := t.TempDir()

	// Copy fixtures into the store dir with proper filenames
	fixtures := map[string]string{
		"root.json":       "",
		"identity.json":   "",
		"core_types.json": "",
	}
	for f := range fixtures {
		data := loadTestFixture(t, f)
		var env Envelope
		json.Unmarshal(data, &env)
		var item Item
		json.Unmarshal(env.Item, &item)
		ref := item.Ref()
		fixtures[f] = ref
		os.WriteFile(filepath.Join(dir, ref+".json"), data, 0644)
	}

	store, _ := NewStore(dir)
	index := NewIndex()
	count, _, err := index.Rebuild(store)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 objects indexed, got %d", count)
	}

	// Check that root has inbound relations
	rootRef := fixtures["root.json"]
	inbound := index.GetInbound(rootRef, InboundFilters{})
	if len(inbound) == 0 {
		t.Error("expected inbound relations to root after rebuild")
	}
}

func TestRateLimitHeaders(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	resp := doGet(t, ts, "/v1/objects")
	if resp.Header.Get("X-RateLimit-Limit") == "" {
		t.Error("expected X-RateLimit-Limit header")
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "" {
		t.Error("expected X-RateLimit-Remaining header")
	}
	resp.Body.Close()
}

// --- helpers ---

func doPut(t *testing.T, ts *httptest.Server, ref string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/v1/objects/"+ref, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func doGet(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func putAllFixtures(t *testing.T, ts *httptest.Server) {
	t.Helper()
	fixtures := []string{"root.json", "identity.json", "core_types.json"}
	for _, f := range fixtures {
		data := loadTestFixture(t, f)
		var env Envelope
		json.Unmarshal(data, &env)
		var item Item
		json.Unmarshal(env.Item, &item)
		resp := doPut(t, ts, item.Ref(), data)
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("PUT %s: expected 201, got %d: %s", f, resp.StatusCode, body)
		}
		resp.Body.Close()
	}
}
