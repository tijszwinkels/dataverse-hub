package serving

import (
	"fmt"
	"net/http"

	"github.com/tijszwinkels/dataverse-hub/auth"
)

// handleLogoutPage returns a GET /logout handler that invalidates the server
// session and serves an HTML page that clears client-side storage.
func handleLogoutPage(a *auth.AuthStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.LogoutAndClearCookie(w, r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, logoutPageHTML)
	}
}

const logoutPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Signed Out</title>
<style>
:root {
  color-scheme: light;
  --bg: #f3efe6;
  --panel: #fffdf8;
  --ink: #1b1a17;
  --muted: #70685b;
  --line: #d8cfbf;
  --accent: #1d5c4d;
  --accent-ink: #f5f8f2;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100vh;
  font-family: "Iowan Old Style", "Palatino Linotype", serif;
  color: var(--ink);
  background:
    radial-gradient(circle at top left, rgba(29,92,77,.12), transparent 30%),
    radial-gradient(circle at bottom right, rgba(139,61,37,.10), transparent 28%),
    var(--bg);
  display: grid;
  place-items: center;
  padding: 24px;
}
.card {
  width: min(480px, 100%);
  background: var(--panel);
  border: 1px solid var(--line);
  box-shadow: 0 24px 60px rgba(27,26,23,.12);
  padding: 40px 36px;
  text-align: center;
}
h1 {
  margin: 0 0 12px;
  font-size: clamp(28px, 5vw, 40px);
  line-height: 1.1;
}
p {
  margin: 0 0 18px;
  color: var(--muted);
  font-size: 16px;
  line-height: 1.5;
}
a.btn {
  display: inline-block;
  appearance: none;
  border: 0;
  background: var(--accent);
  color: var(--accent-ink);
  padding: 12px 24px;
  font: inherit;
  text-decoration: none;
  cursor: pointer;
}
a.btn:hover { opacity: .9; }
</style>
</head>
<body>
<main class="card">
  <h1>Signed Out</h1>
  <p>Your session has been cleared.</p>
  <a class="btn" href="/">Home</a>
</main>
<script>
(function() {
  // Clear both client-side storage systems
  ['dv_page_auth_pubkey', 'dv_page_auth_username', 'dv_page_auth_mode',
   'dv_auth_pubkey', 'dv_auth_identity_ref', 'dv_auth_name',
   'dv_auth_username', 'dv_auth_auth_mode', 'dv_auth_onboarded'].forEach(function(k) {
    localStorage.removeItem(k);
  });

  function clearDB(name) {
    try {
      var req = indexedDB.open(name, 1);
      req.onupgradeneeded = function(e) { e.target.result.createObjectStore('keys'); };
      req.onsuccess = function(e) {
        try {
          var tx = e.target.result.transaction('keys', 'readwrite');
          tx.objectStore('keys').delete('privateKey');
        } catch (ex) { /* ignore */ }
      };
    } catch (e) { /* ignore */ }
  }
  clearDB('dv_page_auth');
  clearDB('dv_auth');
})();
</script>
</body>
</html>`
