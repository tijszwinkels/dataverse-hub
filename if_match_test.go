package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tijszwinkels/dataverse-hub/object"
)

// doPutWithHeaders is doPut with arbitrary extra request headers (e.g. If-Match).
func doPutWithHeaders(t *testing.T, ts *httptest.Server, ref string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/"+ref, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// storedRevision GETs the raw object and returns its revision.
func storedRevision(t *testing.T, ts *httptest.Server, ref string) int {
	t.Helper()
	resp := doGet(t, ts, "/"+ref)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d", ref, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var env object.Envelope
	json.Unmarshal(body, &env)
	var item object.Item
	json.Unmarshal(env.Item, &item)
	return item.Revision
}

// TestPutSuccessReturnsETag: a successful create returns the new revision ETag.
func TestPutSuccessReturnsETag(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000001"
	ref := pubkey + "." + id

	data := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	resp := doPut(t, ts, ref, data)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: expected 201, got %d: %s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("ETag"), `"1"`; got != want {
		t.Errorf("ETag on create: got %q, want %q", got, want)
	}
}

// TestPutIfMatchMatchProceeds: If-Match equal to the stored revision proceeds,
// and the response carries the new revision ETag.
func TestPutIfMatchMatchProceeds(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000002"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	doPut(t, ts, ref, data1).Body.Close()

	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp := doPutWithHeaders(t, ts, ref, data2, map[string]string{"If-Match": `"1"`})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("If-Match match: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got, want := resp.Header.Get("ETag"), `"2"`; got != want {
		t.Errorf("ETag after update: got %q, want %q", got, want)
	}
	if rev := storedRevision(t, ts, ref); rev != 2 {
		t.Errorf("stored revision: got %d, want 2", rev)
	}
}

// TestPutIfMatchMismatchFails412: If-Match differing from the stored revision
// fails with 412 and does not mutate the stored object.
func TestPutIfMatchMismatchFails412(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000003"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	doPut(t, ts, ref, data1).Body.Close()

	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp := doPutWithHeaders(t, ts, ref, data2, map[string]string{"If-Match": `"5"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("If-Match mismatch: expected 412, got %d: %s", resp.StatusCode, body)
	}
	var apiErr object.APIError
	json.NewDecoder(resp.Body).Decode(&apiErr)
	resp.Body.Close()
	if apiErr.Code != "PRECONDITION_FAILED" {
		t.Errorf("expected code PRECONDITION_FAILED, got %q", apiErr.Code)
	}

	// Object must still be at revision 1.
	if rev := storedRevision(t, ts, ref); rev != 1 {
		t.Errorf("stored revision after failed precondition: got %d, want 1", rev)
	}
}

// TestPutWithoutIfMatchUnchanged: absent If-Match keeps the current behavior —
// a higher revision succeeds, an equal/stale revision is a 409 REVISION_CONFLICT.
func TestPutWithoutIfMatchUnchanged(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000004"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	doPut(t, ts, ref, data1).Body.Close()

	// Higher revision, no If-Match → 200 (unchanged behavior).
	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp := doPut(t, ts, ref, data2)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("no If-Match, higher rev: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Stale revision, no If-Match → 409 REVISION_CONFLICT (unchanged behavior).
	resp = doPut(t, ts, ref, data1)
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("no If-Match, stale rev: expected 409, got %d: %s", resp.StatusCode, body)
	}
	var apiErr object.APIError
	json.NewDecoder(resp.Body).Decode(&apiErr)
	resp.Body.Close()
	if apiErr.Code != "REVISION_CONFLICT" {
		t.Errorf("expected code REVISION_CONFLICT, got %q", apiErr.Code)
	}
}

// TestPutIfMatchStarExisting: If-Match: * passes when the object exists.
func TestPutIfMatchStarExisting(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000005"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	doPut(t, ts, ref, data1).Body.Close()

	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp := doPutWithHeaders(t, ts, ref, data2, map[string]string{"If-Match": "*"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("If-Match *: expected 200, got %d: %s", resp.StatusCode, body)
	}
}

// TestPutIfMatchStarNotExisting: If-Match: * fails 412 when the object does not
// yet exist (RFC 9110: * requires a current representation).
func TestPutIfMatchStarNotExisting(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000006"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	resp := doPutWithHeaders(t, ts, ref, data1, map[string]string{"If-Match": "*"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("If-Match * on new object: expected 412, got %d: %s", resp.StatusCode, body)
	}
	// Nothing must have been stored.
	get := doGet(t, ts, "/"+ref)
	if get.StatusCode != http.StatusNotFound {
		t.Errorf("object should not exist after failed *: GET got %d", get.StatusCode)
	}
	get.Body.Close()
}

// TestPutIfMatchConcreteNotExisting: a concrete If-Match tag fails 412 when the
// object does not yet exist (no current representation to match).
func TestPutIfMatchConcreteNotExisting(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000007"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	resp := doPutWithHeaders(t, ts, ref, data1, map[string]string{"If-Match": `"1"`})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf(`If-Match "1" on new object: expected 412, got %d: %s`, resp.StatusCode, body)
	}
}

// TestPutIfMatchConcurrentRace: many concurrent If-Match PUTs from the same base
// revision race to write the next revision. Exactly one must win (200); every
// other must lose with 412, and the object ends at the winner's revision.
func TestPutIfMatchConcurrentRace(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000008"
	ref := pubkey + "." + id

	// Seed revision 1.
	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	if resp := doPut(t, ts, ref, data1); resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed PUT: expected 201, got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// All racers PUT the identical revision-2 object with If-Match: "1".
	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)

	const n = 16
	var ok, failed int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := doPutWithHeaders(t, ts, ref, data2, map[string]string{"If-Match": `"1"`})
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			switch resp.StatusCode {
			case http.StatusOK:
				atomic.AddInt32(&ok, 1)
			case http.StatusPreconditionFailed:
				atomic.AddInt32(&failed, 1)
			default:
				t.Errorf("unexpected status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	if ok != 1 {
		t.Errorf("expected exactly 1 winner, got %d (412s: %d)", ok, failed)
	}
	if failed != n-1 {
		t.Errorf("expected %d losers with 412, got %d", n-1, failed)
	}
	if rev := storedRevision(t, ts, ref); rev != 2 {
		t.Errorf("final stored revision: got %d, want 2", rev)
	}
}

// TestPutIfMatchMultipleTags: a comma-separated If-Match list passes if any tag
// matches the stored revision.
func TestPutIfMatchMultipleTags(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	priv, pubkey := testKeypair(t)
	id := "1f000000-0000-4000-8000-000000000009"
	ref := pubkey + "." + id

	data1 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 1)
	doPut(t, ts, ref, data1).Body.Close()

	data2 := signedObjectWithRevision(t, priv, pubkey, id, []string{"dataverse001"}, "NOTE", 2)
	resp := doPutWithHeaders(t, ts, ref, data2, map[string]string{"If-Match": `"7", "1", "3"`})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("If-Match list with match: expected 200, got %d: %s", resp.StatusCode, body)
	}
}
