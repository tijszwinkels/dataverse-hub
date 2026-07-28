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
// Authentication required. The response exposes the refs the hub has been
// publishing and the publisher's error output, which is operational detail
// rather than public data — so it follows the same gate as GET /auth/realms,
// the hub's existing non-object endpoint: any authenticated identity, 401
// otherwise. A nil mirror still answers, reporting enabled:false, so an
// operator can tell "mirroring is off" apart from "this hub predates the
// feature".
func handleFreenetStatus(w http.ResponseWriter, r *http.Request, mirror *freenet.Mirror) {
	if auth.AuthPubkey(r) == "" {
		writeError(w, r, http.StatusUnauthorized, "authentication required", "UNAUTHORIZED")
		return
	}
	json.NewEncoder(w).Encode(mirror.Status())
}
