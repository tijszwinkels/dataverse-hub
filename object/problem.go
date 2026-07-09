package object

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ProblemMediaType is the RFC 9457 media type for problem details.
const ProblemMediaType = "application/problem+json"

// Problem is an RFC 9457 (application/problem+json) error body.
//
// The three members that matter to an LLM consumer are Title (a short,
// stable summary of the problem class), Detail (the specific cause of this
// occurrence) and NextAction (one concrete recovery step — the error message
// is the product). Status mirrors the HTTP status code per RFC 9457 §3.1.
//
// Code is kept as an extension member: it is the machine-stable identifier
// callers dispatch on, and preserves backward compatibility with clients that
// read the legacy {error, code} body. The RFC "type" member is intentionally
// omitted (it defaults to "about:blank"); Code already serves as the stable
// problem identifier and no problem-type documentation URLs are published.
type Problem struct {
	Title      string `json:"title"`
	Status     int    `json:"status"`
	Detail     string `json:"detail"`
	NextAction string `json:"next_action"`
	Code       string `json:"code,omitempty"`
}

// problemInfo is the stable, per-code half of a Problem: a human-readable
// title and an LLM-actionable next step. Detail and Status vary per occurrence.
type problemInfo struct {
	title      string
	nextAction string
}

// problemCatalog maps a machine code to its stable title and next_action.
// Every code the hub passes to writeError should have an entry here; unknown
// codes fall back to problemFallback so a response is never left without
// guidance.
var problemCatalog = map[string]problemInfo{
	"NOT_FOUND": {
		title:      "Not found",
		nextAction: "Confirm the ref (pubkey.uuid) is correct and the object has been published to this hub. If it may be private, authenticate first (GET /auth/challenge, sign it, POST /auth/token) and retry with 'Authorization: Bearer <token>'.",
	},
	"INTERNAL": {
		title:      "Internal server error",
		nextAction: "This is a server-side fault, not a problem with your request. Retry after a short backoff; if it persists, report the ref and time to the hub operator.",
	},
	"INVALID_OBJECT": {
		title:      "Invalid object",
		nextAction: "Send a well-formed dataverse envelope: JSON with an 'item' (including pubkey, type, timestamp) and a valid 'signature', under the 10MB limit. Fix the body and retry the PUT.",
	},
	"REALM_FORBIDDEN": {
		title:      "Realm not permitted",
		nextAction: "Set the object's realm (item.in) to 'dataverse001', 'server-public', a pubkey-realm equal to item.pubkey, or a shared realm you belong to, then re-sign and retry. A pubkey-realm must match the signing key.",
	},
	"REF_MISMATCH": {
		title:      "Ref mismatch",
		nextAction: "PUT to the object's own ref: the URL must equal '<item.pubkey>.<item.id>' from the signed item. Use that path, or correct the item so its computed ref matches the URL.",
	},
	"INVALID_SIGNATURE": {
		title:      "Invalid signature",
		nextAction: "Re-sign with the correct P-256/ECDSA key. For PUT, sign the canonical item bytes with the key for item.pubkey; for /auth/token, sign the exact challenge string from GET /auth/challenge.",
	},
	"REVISION_CONFLICT": {
		title:      "Revision conflict",
		nextAction: "Fetch the current object (GET /<ref>), set the item's `revision` field above the stored revision, re-sign, and PUT again.",
	},
	"UNAUTHORIZED": {
		title:      "Authentication required",
		nextAction: "Authenticate before calling this endpoint: GET /auth/challenge, sign the challenge, POST /auth/token to obtain a bearer token, then retry with 'Authorization: Bearer <token>'.",
	},
	"INVALID_REQUEST": {
		title:      "Invalid request",
		nextAction: "Send a JSON body containing the fields this endpoint requires (for /auth/token: pubkey, challenge, signature) and retry.",
	},
	"CHALLENGE_EXPIRED": {
		title:      "Challenge expired or unknown",
		nextAction: "Request a fresh challenge (GET /auth/challenge) and complete POST /auth/token promptly; challenges are single-use and short-lived.",
	},
	"RATE_LIMITED": {
		title:      "Rate limit exceeded",
		nextAction: "Slow down and retry after the number of seconds given in the 'Retry-After' response header.",
	},
	"METHOD_NOT_ALLOWED": {
		title:      "Method not allowed",
		nextAction: "Use a method this endpoint supports: GET to read and PUT to write /<ref>; GET for /search and /<ref>/inbound. Retry with the correct HTTP method.",
	},
	"PRECONDITION_FAILED": {
		title:      "Precondition failed",
		nextAction: "Your If-Match header did not match the object's current ETag (the detail names the current one). GET /<ref>, take its ETag into If-Match, and retry the PUT — or drop If-Match to fall back to revision-only conflict checking.",
	},
	"INVALID_SHARED_REALM": {
		title:      "Invalid SHARED_REALM object",
		nextAction: "Fix the SHARED_REALM object to satisfy its type contract: use an owner-prefixed realm whose key you sign with, set the object id to RealmID(realm), and include 'dataverse001' in item.in. Re-sign and retry.",
	},
}

// problemFallback backs any code missing from the catalog, so an unmapped code
// still yields a titled, actionable problem rather than a blank one.
var problemFallback = problemInfo{
	title:      "Request failed",
	nextAction: "Inspect the 'detail' field for the specific cause, adjust the request accordingly, and retry.",
}

// ProblemFor builds an RFC 9457 Problem for the given status, detail and code,
// filling title and next_action from the catalog (or the fallback).
func ProblemFor(status int, detail, code string) Problem {
	info, ok := problemCatalog[code]
	if !ok {
		info = problemFallback
	}
	return Problem{
		Title:      info.title,
		Status:     status,
		Detail:     detail,
		NextAction: info.nextAction,
		Code:       code,
	}
}

// AcceptsProblemJSON reports whether a client with the given Accept header
// should receive an application/problem+json body. True when the header is
// absent (no preference — default to JSON for API responses) or lists a
// JSON-compatible or wildcard media range. False only when the client asked
// exclusively for non-JSON types (e.g. "text/html"), so those keep the legacy
// body. Note real browsers send "*/*", so they do receive problem+json.
func AcceptsProblemJSON(accept string) bool {
	accept = strings.TrimSpace(accept)
	if accept == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch mt {
		case "*/*", "application/*", "application/json", ProblemMediaType:
			return true
		}
	}
	return false
}

// WriteProblem writes an RFC 9457 error response, content-negotiated on Accept.
//
// Clients that accept JSON or a wildcard (curl's */*, agents, and browsers —
// which send */*) receive application/problem+json with title/detail/
// next_action. A client that accepts only non-JSON types (e.g. text/html)
// keeps the pre-existing legacy {error, code} body, so HTML-only paths are
// unchanged. Vary: Accept is always set for cache correctness.
//
// Caller must not have written the response yet; headers set here override any
// Content-Type installed by upstream middleware.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, detail, code string) {
	w.Header().Set("Vary", "Accept")
	if AcceptsProblemJSON(r.Header.Get("Accept")) {
		w.Header().Set("Content-Type", ProblemMediaType)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(ProblemFor(status, detail, code))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(APIError{Error: detail, Code: code})
}
