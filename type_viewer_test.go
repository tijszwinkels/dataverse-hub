package main

import (
	"bytes"
	"crypto/ecdsa"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/tijszwinkels/dataverse-hub/vhost"
)

const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

func signedViewerObject(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id, objectType string, revision int, realms []string, content map[string]any, relations map[string][]map[string]any) (string, []byte) {
	t.Helper()
	item := map[string]any{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": "2026-07-15T00:00:00Z",
		"in":         realms,
		"type":       objectType,
		"revision":   revision,
		"content":    content,
	}
	if len(relations) > 0 {
		item["relations"] = relations
	}
	return pubkey + "." + id, buildSignedObject(t, priv, item)
}

func viewerPage(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id string, revision int, realms []string, marker string) (string, []byte) {
	t.Helper()
	return signedViewerObject(t, priv, pubkey, id, "PAGE", revision, realms,
		map[string]any{"html": "<!doctype html><html><body>" + marker + "</body></html>"}, nil)
}

func typeWithViewer(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id string, revision int, realms []string, pageRef string) (string, []byte) {
	t.Helper()
	return signedViewerObject(t, priv, pubkey, id, "TYPE", revision, realms,
		map[string]any{"name": "VIEWED_TYPE", "schema": map[string]any{}},
		map[string][]map[string]any{"page": {{"ref": pageRef}}})
}

func viewedNote(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id string, revision int, realms []string, typeRef string, extraRelations map[string][]map[string]any) (string, []byte) {
	t.Helper()
	relations := map[string][]map[string]any{"type_def": {{"ref": typeRef}}}
	for name, entries := range extraRelations {
		relations[name] = entries
	}
	return signedViewerObject(t, priv, pubkey, id, "NOTE", revision, realms,
		map[string]any{"text": "original-object"}, relations)
}

func viewedBlob(t *testing.T, priv *ecdsa.PrivateKey, pubkey, id, typeRef string) (string, []byte) {
	t.Helper()
	return signedViewerObject(t, priv, pubkey, id, "BLOB", 1, []string{"dataverse001"},
		map[string]any{"mime_type": "image/png", "data": "iVBORw0KGgo="},
		map[string][]map[string]any{"type_def": {{"ref": typeRef}}})
}

func assertBodyContains(t *testing.T, resp *http.Response, marker string) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(marker)) {
		t.Fatalf("expected body marker %q, got %s", marker, body)
	}
	return body
}

func conditionalGet(t *testing.T, tsURL, path, etag string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, tsURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestTypeDefaultViewerRepresentationsAndPrecedence(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageRef, page := viewerPage(t, priv, pubkey, "10000000-0000-4000-8000-000000000001", 1, []string{"dataverse001"}, "TYPE-VIEWER")
	directRef, direct := viewerPage(t, priv, pubkey, "10000000-0000-4000-8000-000000000002", 1, []string{"dataverse001"}, "DIRECT-VIEWER")
	typeRef, typeData := typeWithViewer(t, priv, pubkey, "10000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, pageRef)
	noteRef, note := viewedNote(t, priv, pubkey, "10000000-0000-4000-8000-000000000004", 1, []string{"dataverse001"}, typeRef, nil)
	directNoteRef, directNote := viewedNote(t, priv, pubkey, "10000000-0000-4000-8000-000000000005", 1, []string{"dataverse001"}, typeRef,
		map[string][]map[string]any{"page": {{"ref": directRef}}})
	brokenDirectRef := pubkey + ".10000000-0000-4000-8000-000000000008"
	brokenDirectNoteRef, brokenDirectNote := viewedNote(t, priv, pubkey, "10000000-0000-4000-8000-000000000009", 1, []string{"dataverse001"}, typeRef,
		map[string][]map[string]any{"page": {{"ref": brokenDirectRef}}})
	inlineRef, inline := signedViewerObject(t, priv, pubkey, "10000000-0000-4000-8000-000000000006", "PAGE", 1, []string{"dataverse001"},
		map[string]any{"html": "<html><body>INLINE-PAGE</body></html>"},
		map[string][]map[string]any{"page": {{"ref": directRef}}, "type_def": {{"ref": typeRef}}})
	blobRef, blob := viewedBlob(t, priv, pubkey, "10000000-0000-4000-8000-000000000007", typeRef)

	for ref, data := range map[string][]byte{
		pageRef: page, directRef: direct, typeRef: typeData, noteRef: note,
		directNoteRef: directNote, brokenDirectNoteRef: brokenDirectNote, inlineRef: inline, blobRef: blob,
	} {
		putOK(t, ts, ref, data)
	}

	assertBodyContains(t, doGetWithAccept(t, ts, "/"+noteRef, browserAccept), "TYPE-VIEWER")
	assertBodyContains(t, doGetWithAccept(t, ts, "/"+noteRef+"/page", "application/json"), "TYPE-VIEWER")
	assertBodyContains(t, doGetWithAccept(t, ts, "/"+directNoteRef, browserAccept), "DIRECT-VIEWER")
	assertBodyContains(t, doGetWithAccept(t, ts, "/"+brokenDirectNoteRef, browserAccept), "TYPE-VIEWER")
	assertBodyContains(t, doGetWithAccept(t, ts, "/"+inlineRef, browserAccept), "INLINE-PAGE")
	assertBodyContains(t, doGetWithAccept(t, ts, "/"+blobRef, browserAccept), "TYPE-VIEWER")

	jsonResp := doGetWithAccept(t, ts, "/"+noteRef, "application/json")
	assertBodyContains(t, jsonResp, "original-object")
	if ct := jsonResp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("JSON representation changed Content-Type: %q", ct)
	}

	projectedJSON := doGetWithAccept(t, ts, "/"+noteRef+"/json", "text/html")
	assertBodyContains(t, projectedJSON, "original-object")
	if ct := projectedJSON.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("/json representation changed Content-Type: %q", ct)
	}

	rawResp := doGetWithAccept(t, ts, "/"+blobRef+"/raw", "text/html")
	defer rawResp.Body.Close()
	if rawResp.StatusCode != http.StatusOK || rawResp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("/raw changed: status=%d content-type=%q", rawResp.StatusCode, rawResp.Header.Get("Content-Type"))
	}
}

func TestTypeDefaultViewerFallsBackSafely(t *testing.T) {
	ts, _, cleanup := testHubWithViewer(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	missingType := pubkey + ".20000000-0000-4000-8000-000000000001"
	noteRef, note := viewedNote(t, priv, pubkey, "20000000-0000-4000-8000-000000000002", 1, []string{"dataverse001"}, missingType, nil)
	blobRef, blob := viewedBlob(t, priv, pubkey, "20000000-0000-4000-8000-000000000003", missingType)
	putOK(t, ts, noteRef, note)
	putOK(t, ts, blobRef, blob)

	assertBodyContains(t, doGetWithAccept(t, ts, "/"+noteRef, browserAccept), "DEFAULT-VIEWER")
	rawBlob := doGetWithAccept(t, ts, "/"+blobRef, browserAccept)
	defer rawBlob.Body.Close()
	if rawBlob.StatusCode != http.StatusOK || rawBlob.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("raw BLOB must precede generic fallback: status=%d content-type=%q", rawBlob.StatusCode, rawBlob.Header.Get("Content-Type"))
	}

	plain, plainCleanup := testHub(t)
	defer plainCleanup()
	putOK(t, plain, noteRef, note)
	resp := doGetWithAccept(t, plain, "/"+noteRef, browserAccept)
	assertBodyContains(t, resp, "original-object")
	assertProblem(t, doGet(t, plain, "/"+noteRef+"/page"), http.StatusConflict, "NO_PAGE")
}

func TestTypeDefaultViewerETagTracksTypeAndPage(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageARef, pageA1 := viewerPage(t, priv, pubkey, "30000000-0000-4000-8000-000000000001", 1, []string{"dataverse001"}, "PAGE-A-1")
	pageBRef, pageB := viewerPage(t, priv, pubkey, "30000000-0000-4000-8000-000000000002", 1, []string{"dataverse001"}, "PAGE-B-1")
	typeRef, type1 := typeWithViewer(t, priv, pubkey, "30000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, pageARef)
	noteRef, note := viewedNote(t, priv, pubkey, "30000000-0000-4000-8000-000000000004", 1, []string{"dataverse001"}, typeRef, nil)
	for ref, data := range map[string][]byte{pageARef: pageA1, pageBRef: pageB, typeRef: type1, noteRef: note} {
		putOK(t, ts, ref, data)
	}

	resp := doGetWithAccept(t, ts, "/"+noteRef+"/page", "application/json")
	assertBodyContains(t, resp, "PAGE-A-1")
	etag1 := resp.Header.Get("ETag")
	assert304(t, ts, "/"+noteRef+"/page", etag1)
	normal := doGetWithAccept(t, ts, "/"+noteRef, browserAccept)
	assertBodyContains(t, normal, "PAGE-A-1")
	if normalETag := normal.Header.Get("ETag"); normalETag != etag1 {
		t.Fatalf("GET and /page ETags differ: %q != %q", normalETag, etag1)
	}
	normal304, _ := http.NewRequest(http.MethodGet, ts.URL+"/"+noteRef, nil)
	normal304.Header.Set("Accept", browserAccept)
	normal304.Header.Set("If-None-Match", etag1)
	conditional, err := http.DefaultClient.Do(normal304)
	if err != nil {
		t.Fatal(err)
	}
	conditional.Body.Close()
	if conditional.StatusCode != http.StatusNotModified {
		t.Fatalf("normal GET conditional status = %d, want 304", conditional.StatusCode)
	}

	_, pageA2 := viewerPage(t, priv, pubkey, "30000000-0000-4000-8000-000000000001", 2, []string{"dataverse001"}, "PAGE-A-2")
	update := doPut(t, ts, pageARef, pageA2)
	if update.StatusCode != http.StatusOK {
		t.Fatalf("update PAGE: got %d", update.StatusCode)
	}
	update.Body.Close()
	resp = doGet(t, ts, "/"+noteRef+"/page")
	assertBodyContains(t, resp, "PAGE-A-2")
	etag2 := resp.Header.Get("ETag")
	if etag2 == etag1 {
		t.Fatal("PAGE revision did not invalidate inherited-viewer ETag")
	}

	_, type2 := typeWithViewer(t, priv, pubkey, "30000000-0000-4000-8000-000000000003", 2, []string{"dataverse001"}, pageBRef)
	update = doPut(t, ts, typeRef, type2)
	if update.StatusCode != http.StatusOK {
		t.Fatalf("update TYPE: got %d", update.StatusCode)
	}
	update.Body.Close()
	resp = doGet(t, ts, "/"+noteRef+"/page")
	assertBodyContains(t, resp, "PAGE-B-1")
	if etag3 := resp.Header.Get("ETag"); etag3 == etag2 {
		t.Fatal("TYPE page-link revision did not invalidate inherited-viewer ETag")
	}
}

func TestTypeDefaultViewerVhostKeepsRequestedRef(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageRef, page := viewerPage(t, priv, pubkey, "40000000-0000-4000-8000-000000000001", 1, []string{"dataverse001"}, "VHOST-TYPE-VIEWER")
	typeRef, typeData := typeWithViewer(t, priv, pubkey, "40000000-0000-4000-8000-000000000002", 1, []string{"dataverse001"}, pageRef)
	noteRef, note := viewedNote(t, priv, pubkey, "40000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, typeRef, nil)
	for ref, data := range map[string][]byte{pageRef: page, typeRef: typeData, noteRef: note} {
		putOK(t, ts, ref, data)
	}

	for _, suffix := range []string{"", "/page"} {
		resp := doGetWithHost(t, ts, "/"+noteRef+suffix, "example.com", browserAccept)
		if resp.StatusCode != http.StatusFound {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("%s: expected 302, got %d: %s", suffix, resp.StatusCode, body)
		}
		want := fmt.Sprintf("http://%s.example.com/%s%s", vhost.PageHash(pageRef), noteRef, suffix)
		if got := resp.Header.Get("Location"); got != want {
			t.Fatalf("redirect navigated away from requested ref: got %q want %q", got, want)
		}
		resp.Body.Close()
	}
}

func TestTypeDefaultViewerVhostPrivateDependencyIsNotCacheable(t *testing.T) {
	ts, _, cleanup := testHubWithVhost(t, "example.com", nil)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageRef, page := viewerPage(t, priv, pubkey, "41000000-0000-4000-8000-000000000001", 1, []string{pubkey}, "PRIVATE-VHOST-VIEWER")
	typeRef, typeData := typeWithViewer(t, priv, pubkey, "41000000-0000-4000-8000-000000000002", 1, []string{pubkey}, pageRef)
	noteRef, note := viewedNote(t, priv, pubkey, "41000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, typeRef, nil)
	for ref, data := range map[string][]byte{pageRef: page, typeRef: typeData, noteRef: note} {
		putOK(t, ts, ref, data)
	}
	token := authenticateAs(t, ts, priv, pubkey)
	resp := doGetWithHostAndToken(t, ts, "/"+noteRef, "example.com", browserAccept, token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("private inherited viewer redirect status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("private inherited viewer redirect Cache-Control = %q", got)
	}
}

func TestProxySynchronizesTypeDefaultViewer(t *testing.T) {
	proxy, root, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageRef, page := viewerPage(t, priv, pubkey, "50000000-0000-4000-8000-000000000001", 1, []string{"dataverse001"}, "PROXY-TYPE-VIEWER")
	typeRef, typeData := typeWithViewer(t, priv, pubkey, "50000000-0000-4000-8000-000000000002", 1, []string{"dataverse001"}, pageRef)
	noteRef, note := viewedNote(t, priv, pubkey, "50000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, typeRef, nil)
	for ref, data := range map[string][]byte{pageRef: page, typeRef: typeData, noteRef: note} {
		putOK(t, root, ref, data)
	}

	assertBodyContains(t, doGetWithAccept(t, proxy, "/"+noteRef, browserAccept), "PROXY-TYPE-VIEWER")
	assertBodyContains(t, doGet(t, proxy, "/"+noteRef+"/page"), "PROXY-TYPE-VIEWER")
}

func TestProxyRefreshesInheritedViewerETag(t *testing.T) {
	proxy, root, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageARef, pageA1 := viewerPage(t, priv, pubkey, "51000000-0000-4000-8000-000000000001", 1, []string{"dataverse001"}, "PROXY-PAGE-A-1")
	pageBRef, pageB := viewerPage(t, priv, pubkey, "51000000-0000-4000-8000-000000000002", 1, []string{"dataverse001"}, "PROXY-PAGE-B-1")
	typeRef, type1 := typeWithViewer(t, priv, pubkey, "51000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, pageARef)
	noteRef, note := viewedNote(t, priv, pubkey, "51000000-0000-4000-8000-000000000004", 1, []string{"dataverse001"}, typeRef, nil)
	for ref, data := range map[string][]byte{pageARef: pageA1, pageBRef: pageB, typeRef: type1, noteRef: note} {
		putOK(t, root, ref, data)
	}

	initial := doGet(t, proxy, "/"+noteRef+"/page")
	assertBodyContains(t, initial, "PROXY-PAGE-A-1")
	etag1 := initial.Header.Get("ETag")

	_, pageA2 := viewerPage(t, priv, pubkey, "51000000-0000-4000-8000-000000000001", 2, []string{"dataverse001"}, "PROXY-PAGE-A-2")
	updated := doPut(t, root, pageARef, pageA2)
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("root PAGE update status = %d", updated.StatusCode)
	}
	updated.Body.Close()
	pageRefresh := conditionalGet(t, proxy.URL, "/"+noteRef+"/page", etag1)
	assertBodyContains(t, pageRefresh, "PROXY-PAGE-A-2")
	etag2 := pageRefresh.Header.Get("ETag")
	if etag2 == etag1 {
		t.Fatal("proxy PAGE refresh did not change inherited-viewer ETag")
	}

	_, type2 := typeWithViewer(t, priv, pubkey, "51000000-0000-4000-8000-000000000003", 2, []string{"dataverse001"}, pageBRef)
	updated = doPut(t, root, typeRef, type2)
	if updated.StatusCode != http.StatusOK {
		t.Fatalf("root TYPE update status = %d", updated.StatusCode)
	}
	updated.Body.Close()
	typeRefresh := conditionalGet(t, proxy.URL, "/"+noteRef+"/page", etag2)
	assertBodyContains(t, typeRefresh, "PROXY-PAGE-B-1")
	if etag3 := typeRefresh.Header.Get("ETag"); etag3 == etag2 {
		t.Fatal("proxy TYPE refresh did not change inherited-viewer ETag")
	}
}

func TestProxyTypeDefaultViewerVhostKeepsRequestedRef(t *testing.T) {
	proxy, cleanup := testRootAndVhostProxy(t, "example.com")
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageRef, page := viewerPage(t, priv, pubkey, "52000000-0000-4000-8000-000000000001", 1, []string{"dataverse001"}, "PROXY-VHOST-VIEWER")
	typeRef, typeData := typeWithViewer(t, priv, pubkey, "52000000-0000-4000-8000-000000000002", 1, []string{"dataverse001"}, pageRef)
	noteRef, note := viewedNote(t, priv, pubkey, "52000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, typeRef, nil)
	for ref, data := range map[string][]byte{pageRef: page, typeRef: typeData, noteRef: note} {
		putOK(t, proxy, ref, data)
	}

	resp := doGetWithHost(t, proxy, "/"+noteRef+"/page", "example.com", "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected proxy 302, got %d: %s", resp.StatusCode, body)
	}
	want := fmt.Sprintf("http://%s.example.com/%s/page", vhost.PageHash(pageRef), noteRef)
	if got := resp.Header.Get("Location"); got != want {
		t.Fatalf("proxy redirect navigated away from requested ref: got %q want %q", got, want)
	}
}

func TestTypeDefaultViewerMalformedDependenciesFailSafely(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	missingPage := pubkey + ".70000000-0000-4000-8000-000000000001"
	typeNoPageRef, typeNoPage := signedViewerObject(t, priv, pubkey, "70000000-0000-4000-8000-000000000002", "TYPE", 1, []string{"dataverse001"}, map[string]any{"name": "NO_PAGE"}, nil)
	typeMissingPageRef, typeMissingPage := typeWithViewer(t, priv, pubkey, "70000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, missingPage)
	wrongTypeRef, wrongType := signedViewerObject(t, priv, pubkey, "70000000-0000-4000-8000-000000000004", "NOTE", 1, []string{"dataverse001"}, map[string]any{"text": "not a TYPE"},
		map[string][]map[string]any{"page": {{"ref": missingPage}}})
	malformedTypeID := "70000000-0000-4000-8000-000000000012"
	malformedTypeRef := pubkey + "." + malformedTypeID
	malformedType := buildSignedObject(t, priv, map[string]any{
		"id": malformedTypeID, "pubkey": pubkey, "created_at": "2026-07-15T00:00:00Z",
		"in": []string{"dataverse001"}, "type": "TYPE", "revision": 1, "content": map[string]any{"name": "MALFORMED_PAGE"},
		"relations": map[string]any{"page": []any{"not-an-object"}},
	})
	for ref, data := range map[string][]byte{
		typeNoPageRef: typeNoPage, typeMissingPageRef: typeMissingPage,
		wrongTypeRef: wrongType, malformedTypeRef: malformedType,
	} {
		putOK(t, ts, ref, data)
	}

	missingType := pubkey + ".70000000-0000-4000-8000-000000000005"
	cases := []struct {
		name    string
		id      string
		typeRef string
	}{
		{name: "missing TYPE", id: "70000000-0000-4000-8000-000000000006", typeRef: missingType},
		{name: "wrong TYPE object", id: "70000000-0000-4000-8000-000000000007", typeRef: wrongTypeRef},
		{name: "TYPE missing page", id: "70000000-0000-4000-8000-000000000008", typeRef: typeNoPageRef},
		{name: "missing PAGE", id: "70000000-0000-4000-8000-000000000009", typeRef: typeMissingPageRef},
		{name: "malformed TYPE page", id: "70000000-0000-4000-8000-000000000013", typeRef: malformedTypeRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, data := viewedNote(t, priv, pubkey, tc.id, 1, []string{"dataverse001"}, tc.typeRef, nil)
			putOK(t, ts, ref, data)
			assertProblem(t, doGet(t, ts, "/"+ref+"/page"), http.StatusConflict, "NO_PAGE")
			assertBodyContains(t, doGetWithAccept(t, ts, "/"+ref, browserAccept), "original-object")
		})
	}

	selfID := "70000000-0000-4000-8000-000000000010"
	selfRef := pubkey + "." + selfID
	self, selfData := viewedNote(t, priv, pubkey, selfID, 1, []string{"dataverse001"}, selfRef, nil)
	putOK(t, ts, self, selfData)
	assertProblem(t, doGet(t, ts, "/"+self+"/page"), http.StatusConflict, "NO_PAGE")

	malformedID := "70000000-0000-4000-8000-000000000011"
	malformedRef := pubkey + "." + malformedID
	malformedItem := map[string]any{
		"id": malformedID, "pubkey": pubkey, "created_at": "2026-07-15T00:00:00Z",
		"in": []string{"dataverse001"}, "type": "NOTE", "revision": 1, "content": map[string]any{"text": "malformed-type-def"},
		"relations": map[string]any{"type_def": []any{"not-an-object"}},
	}
	malformed := buildSignedObject(t, priv, malformedItem)
	putOK(t, ts, malformedRef, malformed)
	assertProblem(t, doGet(t, ts, "/"+malformedRef+"/page"), http.StatusConflict, "NO_PAGE")
}

func TestTypeDefaultViewerRespectsDependencyPrivacy(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageRef, page := viewerPage(t, priv, pubkey, "60000000-0000-4000-8000-000000000001", 1, []string{pubkey}, "PRIVATE-TYPE-VIEWER")
	typeRef, typeData := typeWithViewer(t, priv, pubkey, "60000000-0000-4000-8000-000000000002", 1, []string{pubkey}, pageRef)
	noteRef, note := viewedNote(t, priv, pubkey, "60000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, typeRef, nil)
	for ref, data := range map[string][]byte{pageRef: page, typeRef: typeData, noteRef: note} {
		putOK(t, ts, ref, data)
	}

	unauth := doGetWithAccept(t, ts, "/"+noteRef, browserAccept)
	assertBodyContains(t, unauth, "original-object")
	if ct := unauth.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("private viewer leaked as %q", ct)
	}
	assertProblem(t, doGet(t, ts, "/"+noteRef+"/page"), http.StatusConflict, "NO_PAGE")

	token := authenticateAs(t, ts, priv, pubkey)
	authReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/"+noteRef+"/page", nil)
	authReq.Header.Set("Authorization", "Bearer "+token)
	authResp, err := http.DefaultClient.Do(authReq)
	if err != nil {
		t.Fatal(err)
	}
	assertBodyContains(t, authResp, "PRIVATE-TYPE-VIEWER")
	if got := authResp.Header.Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("private dependency Cache-Control = %q, want private, no-cache", got)
	}
}

func TestProxyTypeDefaultViewerRespectsDependencyPrivacy(t *testing.T) {
	proxy, root, cleanup := testRootAndProxy(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	pageRef, page := viewerPage(t, priv, pubkey, "61000000-0000-4000-8000-000000000001", 1, []string{pubkey}, "PROXY-PRIVATE-TYPE-VIEWER")
	typeRef, typeData := typeWithViewer(t, priv, pubkey, "61000000-0000-4000-8000-000000000002", 1, []string{pubkey}, pageRef)
	noteRef, note := viewedNote(t, priv, pubkey, "61000000-0000-4000-8000-000000000003", 1, []string{"dataverse001"}, typeRef, nil)
	putOK(t, proxy, pageRef, page)
	putOK(t, proxy, typeRef, typeData)
	putOK(t, proxy, noteRef, note)

	assertProblem(t, doGet(t, proxy, "/"+noteRef+"/page"), http.StatusConflict, "NO_PAGE")
	token := authenticateAs(t, proxy, priv, pubkey)
	authReq, _ := http.NewRequest(http.MethodGet, proxy.URL+"/"+noteRef+"/page", nil)
	authReq.Header.Set("Authorization", "Bearer "+token)
	authResp, err := http.DefaultClient.Do(authReq)
	if err != nil {
		t.Fatal(err)
	}
	assertBodyContains(t, authResp, "PROXY-PRIVATE-TYPE-VIEWER")
	if got := authResp.Header.Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("proxy private dependency Cache-Control = %q, want private, no-cache", got)
	}

	// Viewer freshness must not push locally authoritative private dependencies.
	if resp := doGet(t, root, "/"+typeRef); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("private TYPE propagated upstream: status=%d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doGet(t, root, "/"+pageRef); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("private PAGE propagated upstream: status=%d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}
