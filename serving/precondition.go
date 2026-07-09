package serving

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tijszwinkels/dataverse-hub/object"
)

// ifMatchResult is the outcome of evaluating an If-Match precondition on a PUT.
type ifMatchResult int

const (
	// ifMatchAbsent — no If-Match header; the caller keeps its legacy behavior.
	ifMatchAbsent ifMatchResult = iota
	// ifMatchPass — the precondition is satisfied; the write may proceed.
	ifMatchPass
	// ifMatchFail — the precondition failed; the caller must reply 412.
	ifMatchFail
)

// revisionETag is the strong entity-tag for an object at the given revision.
// It mirrors the ETag handleGetObject serves for the raw object representation,
// so a client can round-trip GET's ETag straight back into a PUT's If-Match.
func revisionETag(revision int) string {
	return `"` + strconv.Itoa(revision) + `"`
}

// checkConditionalWrite gates a pending PUT on both the If-Match precondition
// and the revision-monotonicity rule, in RFC-correct order (precondition before
// conflict). It writes the appropriate error response (412 or 409) and reports
// whether the caller must abort. Shared by the Hub and Proxy write paths so the
// conditional-write semantics live in one place.
//
// existingMeta/exists describe the currently-stored object; incomingRevision is
// the revision of the object being written. Callers must hold the per-ref write
// lock so the check and the subsequent write are atomic.
func checkConditionalWrite(w http.ResponseWriter, r *http.Request, ifMatch string, existingMeta object.ObjectMeta, exists bool, incomingRevision int) (abort bool) {
	if evaluateIfMatch(ifMatch, existingMeta, exists) == ifMatchFail {
		writeError(w, r, http.StatusPreconditionFailed,
			ifMatchFailMessage(existingMeta, exists),
			"PRECONDITION_FAILED")
		return true
	}
	if exists && existingMeta.Revision >= incomingRevision {
		writeError(w, r, http.StatusConflict,
			fmt.Sprintf("existing revision %d >= incoming %d", existingMeta.Revision, incomingRevision),
			"REVISION_CONFLICT")
		return true
	}
	return false
}

// evaluateIfMatch applies RFC 9110 §13.1.1 If-Match semantics to a PUT that
// targets an object whose current state is described by (existingMeta, exists).
//
//   - No If-Match header        → ifMatchAbsent (preserve legacy behavior).
//   - If-Match: *               → pass iff the object currently exists.
//   - If-Match: "r"[, "s", …]   → pass iff the object exists and its revision
//                                 matches one of the listed entity-tags.
//
// Comparison is the strong comparison function: weak tags (W/"…") never match,
// and a concrete tag can never match a resource with no current representation.
func evaluateIfMatch(header string, existingMeta object.ObjectMeta, exists bool) ifMatchResult {
	header = strings.TrimSpace(header)
	if header == "" {
		return ifMatchAbsent
	}
	if header == "*" {
		if exists {
			return ifMatchPass
		}
		return ifMatchFail
	}
	if !exists {
		return ifMatchFail
	}
	current := revisionETag(existingMeta.Revision)
	for _, tag := range splitETagList(header) {
		if tag == current { // strong comparison; a W/ prefix will not equal current
			return ifMatchPass
		}
	}
	return ifMatchFail
}

// ifMatchFailMessage builds the human-readable 412 body message, naming the
// stored entity-tag the client's If-Match did not match.
func ifMatchFailMessage(existingMeta object.ObjectMeta, exists bool) string {
	if !exists {
		return "If-Match precondition failed: object does not exist"
	}
	return "If-Match precondition failed: current ETag is " + revisionETag(existingMeta.Revision)
}

// splitETagList splits a comma-separated If-Match value into trimmed entity
// tags. Entity tags in this hub are quoted revision numbers, which never
// contain commas, so a plain split is sufficient.
func splitETagList(header string) []string {
	parts := strings.Split(header, ",")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			tags = append(tags, p)
		}
	}
	return tags
}
