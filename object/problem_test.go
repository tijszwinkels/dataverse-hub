package object

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAcceptsProblemJSON(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"", true},                         // no preference → JSON
		{"*/*", true},                      // curl default
		{"application/json", true},         // explicit JSON
		{"application/problem+json", true}, // asks for problem docs
		{"application/*", true},            // any application subtype
		{"text/html,application/xhtml+xml,*/*;q=0.8", true}, // real browser (has */*)
		{"text/html, application/json", true},               // mixed, accepts JSON
		{"text/html", false},                                // HTML-only client
		{"text/html; charset=utf-8", false},                 // HTML-only with params
		{"image/png", false},                                // non-JSON, non-wildcard
		{"Application/JSON", true},                          // case-insensitive media type
		{"APPLICATION/*", true},                             // case-insensitive wildcard
		{"application/json;q=0", false},                     // explicit refusal (RFC 7231 q=0)
		{"application/json;q=0.5", true},                    // positive quality
		{"*/*;q=0", false},                                  // refuses everything
	}
	for _, c := range cases {
		if got := AcceptsProblemJSON(c.accept); got != c.want {
			t.Errorf("AcceptsProblemJSON(%q) = %v, want %v", c.accept, got, c.want)
		}
	}
}

// Every code the hub emits must resolve to a non-empty, stable title and an
// actionable next_action — the error message is the product.
func TestProblemCatalogCoverage(t *testing.T) {
	codes := []string{
		"NOT_FOUND", "INTERNAL", "INVALID_OBJECT", "REALM_FORBIDDEN",
		"REF_MISMATCH", "INVALID_SIGNATURE", "REVISION_CONFLICT",
		"UNAUTHORIZED", "INVALID_REQUEST", "CHALLENGE_EXPIRED", "RATE_LIMITED",
		"METHOD_NOT_ALLOWED", "PRECONDITION_FAILED", "INVALID_SHARED_REALM",
	}
	for _, code := range codes {
		p := ProblemFor(400, "some detail", code)
		if p.Title == "" {
			t.Errorf("code %q: empty title", code)
		}
		if p.NextAction == "" {
			t.Errorf("code %q: empty next_action", code)
		}
	}
}

// An unknown code still yields a usable problem rather than a blank one.
func TestProblemForUnknownCode(t *testing.T) {
	p := ProblemFor(500, "boom", "SOMETHING_NEW")
	if p.Title == "" || p.NextAction == "" {
		t.Errorf("unknown code fallback must fill title and next_action, got %+v", p)
	}
	if p.Code != "SOMETHING_NEW" || p.Detail != "boom" || p.Status != 500 {
		t.Errorf("unknown code must preserve code/detail/status, got %+v", p)
	}
}

func TestWriteProblemJSONClient(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept", "application/json")

	WriteProblem(rec, req, http.StatusNotFound, "object not found", "NOT_FOUND")

	if ct := rec.Header().Get("Content-Type"); ct != ProblemMediaType {
		t.Errorf("Content-Type = %q, want %q", ct, ProblemMediaType)
	}
	if v := rec.Header().Get("Vary"); v != "Accept" {
		t.Errorf("Vary = %q, want Accept", v)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if p.Title == "" || p.Detail != "object not found" || p.NextAction == "" {
		t.Errorf("problem fields incomplete: %+v", p)
	}
	if p.Status != http.StatusNotFound || p.Code != "NOT_FOUND" {
		t.Errorf("status/code wrong: %+v", p)
	}
}

// A client that accepts only text/html keeps the pre-existing legacy error body.
func TestWriteProblemHTMLOnlyClient(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept", "text/html")

	WriteProblem(rec, req, http.StatusNotFound, "object not found", "NOT_FOUND")

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("HTML-only client Content-Type = %q, want application/json", ct)
	}
	var legacy APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &legacy); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if legacy.Error != "object not found" || legacy.Code != "NOT_FOUND" {
		t.Errorf("legacy body wrong: %+v", legacy)
	}
}
