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

func TestInboundCounts(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putAllFixtures(t, ts)

	// root is referenced by identity and core_types via "root" relation
	// and by core_types via "core_types" relation from root itself
	rootRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000"

	// List objects with include=inbound_counts
	resp := doGet(t, ts, "/v1/objects?include=inbound_counts")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var list ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	// Find root in response and check _inbound_counts
	for _, raw := range list.Items {
		var obj map[string]json.RawMessage
		json.Unmarshal(raw, &obj)

		var item Item
		json.Unmarshal(obj["item"], &item)
		if item.Ref() != rootRef {
			continue
		}

		countsRaw, ok := obj["_inbound_counts"]
		if !ok {
			t.Fatal("root object missing _inbound_counts field")
		}
		var counts map[string]int
		if err := json.Unmarshal(countsRaw, &counts); err != nil {
			t.Fatalf("failed to parse _inbound_counts: %v", err)
		}
		// identity and core_types both have "root" relation to root
		if counts["root"] < 2 {
			t.Errorf("expected root relation count >= 2, got %d", counts["root"])
		}
		return
	}
	t.Error("root object not found in response")
}

func TestInboundCountsOnInbound(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putAllFixtures(t, ts)

	rootRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000"

	// Get inbound to root with include=inbound_counts
	resp := doGet(t, ts, "/v1/objects/"+rootRef+"/inbound?include=inbound_counts")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var list ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	if len(list.Items) == 0 {
		t.Fatal("expected at least 1 inbound item")
	}

	// Every item should have _inbound_counts field
	for _, raw := range list.Items {
		var obj map[string]json.RawMessage
		json.Unmarshal(raw, &obj)
		if _, ok := obj["_inbound_counts"]; !ok {
			t.Error("inbound item missing _inbound_counts field")
		}
	}
}

func TestNoInboundCountsWithoutParam(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putAllFixtures(t, ts)

	// Without include=inbound_counts, items should NOT have the field
	resp := doGet(t, ts, "/v1/objects")
	var list ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()

	for _, raw := range list.Items {
		var obj map[string]json.RawMessage
		json.Unmarshal(raw, &obj)
		if _, ok := obj["_inbound_counts"]; ok {
			t.Error("_inbound_counts should not be present without include param")
		}
	}
}

func TestETagAndNotModified(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	data := loadTestFixture(t, "root.json")
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	ref := item.Ref()

	// PUT
	resp := doPut(t, ts, ref, data)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET without If-None-Match: should return 200 with ETag
	resp = doGet(t, ts, "/v1/objects/"+ref)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header")
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body) == 0 {
		t.Fatal("expected body on 200")
	}

	// GET with matching If-None-Match: should return 304 with no body
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/objects/"+ref, nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", resp.StatusCode)
	}
	body304, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body304) != 0 {
		t.Errorf("expected empty body on 304, got %d bytes", len(body304))
	}

	// GET with non-matching If-None-Match: should return 200
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/objects/"+ref, nil)
	req.Header.Set("If-None-Match", `"999"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for non-matching etag, got %d", resp.StatusCode)
	}
	resp.Body.Close()
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

// --- PAGE content negotiation tests ---

func doGetWithAccept(t *testing.T, ts *httptest.Server, path, accept string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func putFixture(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()
	data := loadTestFixture(t, name)
	var env Envelope
	json.Unmarshal(data, &env)
	var item Item
	json.Unmarshal(env.Item, &item)
	resp := doPut(t, ts, item.Ref(), data)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT %s: expected 201, got %d: %s", name, resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPageServedAsHTML(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")

	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	// Browser request (Accept: text/html) should return HTML
	resp := doGetWithAccept(t, ts, "/v1/objects/"+pageRef, "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html content-type, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("expected HTML body, got: %s", body)
	}
}

func TestPageServedAsJSONWithoutAcceptHTML(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")

	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	// API request (no Accept or Accept: application/json) should return JSON
	resp := doGet(t, ts, "/v1/objects/"+pageRef)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("expected valid JSON envelope, got: %s", body)
	}
}

func TestPageRelationRedirect(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	putFixture(t, ts, "app_with_page.json")

	appRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.bbbbbbbb-cccc-4ddd-eeee-ffffffffffff"

	// Browser request for the app should serve the page's HTML
	resp := doGetWithAccept(t, ts, "/v1/objects/"+appRef, "text/html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html content-type, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(body, []byte("<h1>Hello Dataverse</h1>")) {
		t.Errorf("expected page HTML, got: %s", body)
	}
}

func TestPageRelationJSONForAPI(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	putFixture(t, ts, "app_with_page.json")

	appRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.bbbbbbbb-cccc-4ddd-eeee-ffffffffffff"

	// API request for the app should still return JSON
	resp := doGetWithAccept(t, ts, "/v1/objects/"+appRef, "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("expected valid JSON envelope, got: %s", body)
	}
}

func TestVaryHeaderPresent(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	// Both HTML and JSON responses must include Vary: Accept
	for _, accept := range []string{"text/html", "application/json"} {
		resp := doGetWithAccept(t, ts, "/v1/objects/"+pageRef, accept)
		vary := resp.Header.Get("Vary")
		if vary != "Accept" {
			t.Errorf("Accept=%q: expected Vary: Accept, got %q", accept, vary)
		}
		resp.Body.Close()
	}
}

func TestETagDiffersByRepresentation(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	// Get ETag for HTML representation
	htmlResp := doGetWithAccept(t, ts, "/v1/objects/"+pageRef, "text/html")
	htmlETag := htmlResp.Header.Get("ETag")
	htmlResp.Body.Close()

	// Get ETag for JSON representation
	jsonResp := doGetWithAccept(t, ts, "/v1/objects/"+pageRef, "application/json")
	jsonETag := jsonResp.Header.Get("ETag")
	jsonResp.Body.Close()

	if htmlETag == jsonETag {
		t.Errorf("HTML and JSON ETags must differ, both are %q", htmlETag)
	}
	if htmlETag == "" || jsonETag == "" {
		t.Errorf("ETags must not be empty: html=%q json=%q", htmlETag, jsonETag)
	}
}

func TestETagNotModifiedRespectsRepresentation(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	putFixture(t, ts, "page.json")
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.aaaaaaaa-bbbb-4ccc-dddd-eeeeeeeeeeee"

	// Get the HTML ETag
	htmlResp := doGetWithAccept(t, ts, "/v1/objects/"+pageRef, "text/html")
	htmlETag := htmlResp.Header.Get("ETag")
	htmlResp.Body.Close()

	// HTML ETag should produce 304 for HTML request
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/objects/"+pageRef, nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("If-None-Match", htmlETag)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("HTML ETag + HTML Accept: expected 304, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// HTML ETag should NOT produce 304 for JSON request
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/objects/"+pageRef, nil)
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("If-None-Match", htmlETag)
	resp2, _ := http.DefaultClient.Do(req2)
	if resp2.StatusCode == http.StatusNotModified {
		t.Errorf("HTML ETag + JSON Accept: should NOT get 304")
	}
	resp2.Body.Close()
}

func TestPageMissingHTMLField(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	// Use the root object (type ROOT, no html field) with Accept: text/html
	putFixture(t, ts, "root.json")

	rootRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.00000000-0000-0000-0000-000000000000"
	resp := doGetWithAccept(t, ts, "/v1/objects/"+rootRef, "text/html")
	// Should fall back to JSON since root is not a PAGE and has no page relation
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected JSON fallback, got content-type %q", ct)
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
