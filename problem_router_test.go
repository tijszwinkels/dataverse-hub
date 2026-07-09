package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestHubRouterNotFoundProblem: an unmatched route path returns problem+json.
func TestHubRouterNotFoundProblem(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	resp := doGet(t, ts, "/no/such/route/here")
	assertProblem(t, resp, http.StatusNotFound, "NOT_FOUND")
}

// TestHubRouterMethodNotAllowedProblem: a wrong method on a known route returns
// problem+json with the 405 status preserved.
func TestHubRouterMethodNotAllowedProblem(t *testing.T) {
	ts, cleanup := testHub(t)
	defer cleanup()

	_, pubkey := testKeypair(t)
	ref := pubkey + ".abcabcab-1111-4111-8111-abcabcabcabc"

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/"+ref, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// RFC 7231 §6.5.5: a 405 MUST carry an Allow header listing supported methods.
	allow := resp.Header.Get("Allow")
	if !strings.Contains(allow, "GET") || !strings.Contains(allow, "PUT") {
		t.Errorf("405 Allow header = %q, want it to list GET and PUT", allow)
	}
	assertProblem(t, resp, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
}

// TestProxyRouterFallbacksProblem: the same 404/405 fallbacks apply in proxy mode.
func TestProxyRouterFallbacksProblem(t *testing.T) {
	proxySrv, _, cleanup := testRootAndProxy(t)
	defer cleanup()

	resp := doGet(t, proxySrv, "/no/such/route/here")
	assertProblem(t, resp, http.StatusNotFound, "NOT_FOUND")

	_, pubkey := testKeypair(t)
	req, _ := http.NewRequest(http.MethodDelete, proxySrv.URL+"/"+pubkey+".abcabcab-2222-4222-8222-abcabcabcabc", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertProblem(t, resp2, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
}
