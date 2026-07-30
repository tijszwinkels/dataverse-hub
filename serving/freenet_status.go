package serving

import (
	"encoding/json"
	"net/http"

	"github.com/tijszwinkels/dataverse-hub/auth"
	"github.com/tijszwinkels/dataverse-hub/freenet"
)

// handleFreenetStatus serves GET /freenet/status: queue depth, counters and
// recent job transitions for the Freenet write-through mirror.
//
// Authentication required, following GET /auth/realms — the hub's existing
// non-object endpoint: any authenticated identity, 401 otherwise.
//
// Note what that gate is and is not. The hub has no operator/admin concept:
// anyone can generate a keypair and complete the public challenge flow, so
// "authenticated" here means "not an anonymous scanner", not "trusted".
// The payload is scoped accordingly — refs (public objects by construction),
// counters and timings. The publisher's raw output is deliberately *not*
// included, since it can carry filesystem paths and node details; it goes to
// the hub log and the failed/ job file, which need filesystem access to read.
//
// The route is only registered when a mirror is configured, so this handler
// never sees a nil mirror in practice; the nil-safe Status() call keeps it
// correct regardless.
func handleFreenetStatus(w http.ResponseWriter, r *http.Request, mirror *freenet.Mirror) {
	if auth.AuthPubkey(r) == "" {
		writeError(w, r, http.StatusUnauthorized, "authentication required", "UNAUTHORIZED")
		return
	}
	json.NewEncoder(w).Encode(mirror.Status())
}
