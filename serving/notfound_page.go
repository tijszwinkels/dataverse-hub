package serving

import (
	"fmt"
	"html"
	"net/http"

	"github.com/tijszwinkels/dataverse-hub/auth"
)

// serve404Page serves a user-friendly 404 HTML page with a login form.
func serve404Page(w http.ResponseWriter, r *http.Request) {
	serveLoginHTML(w, r, http.StatusNotFound,
		"Object Not Found",
		"The requested object could not be found. If this is a private object, it may become available after you sign in.")
}

// serveLoginPage serves a login page (triggered by ?login=true). Returns 200.
func serveLoginPage(w http.ResponseWriter, r *http.Request) {
	serveLoginHTML(w, r, http.StatusOK,
		"Sign In",
		"Sign in to access private objects on this hub.")
}

func serveLoginHTML(w http.ResponseWriter, r *http.Request, status int, heading, description string) {
	pubkey := auth.AuthPubkey(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	authBlock := ""
	if pubkey != "" {
		escaped := html.EscapeString(pubkey)
		short := escaped
		if len(short) > 12 {
			short = short[:6] + "…" + short[len(short)-6:]
		}
		authBlock = fmt.Sprintf(loginAuthBlock, short, escaped)
	}

	fmt.Fprintf(w, loginPageHTML,
		html.EscapeString(heading),
		html.EscapeString(heading),
		html.EscapeString(description),
		authBlock)
}

// loginAuthBlock is injected when the user is already authenticated.
// %s placeholders: 1=short pubkey, 2=full pubkey (title tooltip).
const loginAuthBlock = `
  <div class="auth-info">
    <span class="auth-label">Signed in as</span>
    <code title="%[2]s">%[1]s</code>
    <button class="sign-out" id="btn-signout" type="button">Sign Out</button>
  </div>
  <p class="muted-hint">This object is not available with your current identity. Try signing in with a different one.</p>`

// loginPageHTML is the full page template.
// Placeholders: 1=title, 2=heading, 3=description, 4=auth block.
const loginPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%[1]s</title>
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
  --warn: #8b3d25;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  min-height: 100vh;
  font-family: "Iowan Old Style", "Palatino Linotype", serif;
  color: var(--ink);
  background:
    radial-gradient(circle at top left, rgba(29,92,77,.12), transparent 30%%),
    radial-gradient(circle at bottom right, rgba(139,61,37,.10), transparent 28%%),
    var(--bg);
  display: grid;
  place-items: center;
  padding: 24px;
}
.card {
  width: min(560px, 100%%);
  background: var(--panel);
  border: 1px solid var(--line);
  box-shadow: 0 24px 60px rgba(27,26,23,.12);
  padding: 28px;
}
h1 {
  margin: 0 0 10px;
  font-size: clamp(28px, 5vw, 40px);
  line-height: 1.1;
}
p {
  margin: 0 0 14px;
  color: var(--muted);
  font-size: 16px;
  line-height: 1.5;
}
.muted-hint {
  font-size: 14px;
  color: var(--muted);
}
.auth-info {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  padding: 12px 14px;
  background: rgba(29,92,77,.06);
  border: 1px solid var(--line);
  margin-bottom: 14px;
}
.auth-label {
  font-size: 13px;
  text-transform: uppercase;
  letter-spacing: .08em;
  color: var(--muted);
}
.auth-info code {
  font-size: 14px;
  color: var(--ink);
}
.sign-out {
  appearance: none;
  border: 1px solid var(--line);
  background: transparent;
  color: var(--warn);
  padding: 5px 12px;
  font: inherit;
  font-size: 13px;
  cursor: pointer;
  margin-left: auto;
}
.sign-out:hover {
  background: rgba(139,61,37,.08);
}
hr {
  border: 0;
  border-top: 1px solid var(--line);
  margin: 20px 0;
}
h2 {
  margin: 0 0 14px;
  font-size: 20px;
}
.tabs {
  display: flex;
  gap: 8px;
  margin: 14px 0;
}
.tab {
  appearance: none;
  border: 1px solid var(--line);
  background: transparent;
  color: var(--ink);
  padding: 10px 14px;
  cursor: pointer;
  font: inherit;
}
.tab.active {
  background: var(--ink);
  color: var(--panel);
  border-color: var(--ink);
}
.panel { display: none; }
.panel.active { display: block; }
label {
  display: block;
  margin: 0 0 6px;
  font-size: 13px;
  text-transform: uppercase;
  letter-spacing: .08em;
  color: var(--muted);
}
input, textarea {
  width: 100%%;
  border: 1px solid var(--line);
  background: #fff;
  color: var(--ink);
  padding: 12px 13px;
  font: inherit;
  margin: 0 0 14px;
}
textarea { min-height: 128px; resize: vertical; }
button.primary {
  appearance: none;
  border: 0;
  background: var(--accent);
  color: var(--accent-ink);
  padding: 12px 16px;
  font: inherit;
  cursor: pointer;
  min-width: 160px;
}
.row {
  display: flex;
  gap: 10px;
  align-items: center;
  flex-wrap: wrap;
}
.status {
  min-height: 24px;
  margin-top: 14px;
  color: var(--muted);
}
.status.error { color: var(--warn); }
.status.ok { color: var(--accent); }
.hidden { display: none; }
@media (max-width: 640px) {
  .card { padding: 22px; }
  .row > * { width: 100%%; }
  button.primary { width: 100%%; }
}
</style>
</head>
<body>
<main class="card">
  <h1>%[2]s</h1>
  <p>%[3]s</p>
%[4]s
  <p id="auto-note" class="hidden">Found a stored key. Trying to sign you in automatically.</p>

  <hr>
  <h2>Sign In</h2>

  <div class="tabs">
    <button class="tab active" id="tab-pass" type="button">Username + Password</button>
    <button class="tab" id="tab-pem" type="button">Private Key PEM</button>
  </div>

  <section class="panel active" id="panel-pass">
    <label for="username">Username</label>
    <input id="username" autocomplete="username">
    <label for="password">Password</label>
    <input id="password" type="password" autocomplete="current-password">
    <div class="row">
      <button class="primary" id="btn-pass" type="button">Sign In</button>
    </div>
  </section>

  <section class="panel" id="panel-pem">
    <label for="pem">Private Key (PEM)</label>
    <textarea id="pem" spellcheck="false" placeholder="-----BEGIN PRIVATE KEY-----"></textarea>
    <div class="row">
      <button class="primary" id="btn-pem" type="button">Sign In With PEM</button>
    </div>
  </section>

  <div class="status" id="status"></div>
</main>
<script>
(function() {
'use strict';

var statusEl = document.getElementById('status');
var autoNoteEl = document.getElementById('auto-note');
var usernameEl = document.getElementById('username');
var passwordEl = document.getElementById('password');
var pemEl = document.getElementById('pem');
var subtle = crypto.subtle;

// Sign-out button (only present when authenticated)
var signoutBtn = document.getElementById('btn-signout');
if (signoutBtn) {
  signoutBtn.addEventListener('click', async function() {
    try {
      await fetch('/auth/logout', { method: 'POST', credentials: 'include' });
    } catch (e) { /* ignore */ }
    // Clear both storage systems
    ['dv_page_auth_pubkey', 'dv_page_auth_username', 'dv_page_auth_mode',
     'dv_auth_pubkey', 'dv_auth_identity_ref', 'dv_auth_name',
     'dv_auth_username', 'dv_auth_auth_mode'].forEach(function(k) {
      localStorage.removeItem(k);
    });
    clearDB('dv_page_auth');
    clearDB('dv_auth');
    window.location.reload();
  });
}

function clearDB(name) {
  try {
    var req = indexedDB.open(name, 1);
    req.onupgradeneeded = function(e) { e.target.result.createObjectStore('keys'); };
    req.onsuccess = function(e) {
      var db = e.target.result;
      try {
        var tx = db.transaction('keys', 'readwrite');
        tx.objectStore('keys').delete('privateKey');
      } catch (ex) { /* ignore */ }
    };
  } catch (e) { /* ignore */ }
}

function setStatus(msg, cls) {
  statusEl.textContent = msg || '';
  statusEl.className = 'status' + (cls ? ' ' + cls : '');
}

function setMode(mode) {
  var isPass = mode === 'pass';
  document.getElementById('tab-pass').classList.toggle('active', isPass);
  document.getElementById('tab-pem').classList.toggle('active', !isPass);
  document.getElementById('panel-pass').classList.toggle('active', isPass);
  document.getElementById('panel-pem').classList.toggle('active', !isPass);
}

document.getElementById('tab-pass').addEventListener('click', function() { setMode('pass'); });
document.getElementById('tab-pem').addEventListener('click', function() { setMode('pem'); });

function bytesToBigInt(bytes) {
  var h = '0x';
  for (var i = 0; i < bytes.length; i++) h += bytes[i].toString(16).padStart(2, '0');
  return BigInt(h);
}

function bigIntToBytes(n, len) {
  var hex = n.toString(16).padStart(len * 2, '0');
  var bytes = new Uint8Array(len);
  for (var i = 0; i < len; i++) bytes[i] = parseInt(hex.substr(i * 2, 2), 16);
  return bytes;
}

function base64urlEncode(bytes) {
  var b = '';
  for (var i = 0; i < bytes.length; i++) b += String.fromCharCode(bytes[i]);
  return btoa(b).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function base64urlDecode(s) {
  s = s.replace(/-/g, '+').replace(/_/g, '/');
  while (s.length %% 4) s += '=';
  var bin = atob(s);
  var bytes = new Uint8Array(bin.length);
  for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

function bytesToBase64(bytes) {
  var b = '';
  for (var i = 0; i < bytes.length; i++) b += String.fromCharCode(bytes[i]);
  return btoa(b);
}

var P256_P = 0xFFFFFFFF00000001000000000000000000000000FFFFFFFFFFFFFFFFFFFFFFFFn;
var P256_A = P256_P - 3n;
var P256_N = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551n;
var P256_GX = 0x6B17D1F2E12C4247F8BCE6E563A440F277037D812DEB33A0F4A13945D898C296n;
var P256_GY = 0x4FE342E2FE1A7F9B8EE7EB4A7C0F9E162BCE33576B315ECECBB6406837BF51F5n;

function modP(a) { return ((a %% P256_P) + P256_P) %% P256_P; }
function modInv(a, m) {
  var oldR = ((a %% m) + m) %% m, r = m;
  var oldS = 1n, s = 0n;
  while (r !== 0n) {
    var q = oldR / r;
    var tmpR = r; r = oldR - q * r; oldR = tmpR;
    var tmpS = s; s = oldS - q * s; oldS = tmpS;
  }
  return ((oldS %% m) + m) %% m;
}
function ecAdd(x1, y1, x2, y2) {
  if (x1 === null) return [x2, y2];
  if (x2 === null) return [x1, y1];
  var lam;
  if (x1 === x2 && y1 === y2) {
    if (y1 === 0n) return [null, null];
    lam = modP((3n * x1 * x1 + P256_A) * modInv(2n * y1, P256_P));
  } else if (x1 === x2) {
    return [null, null];
  } else {
    lam = modP((y2 - y1) * modInv(((x2 - x1) %% P256_P + P256_P) %% P256_P, P256_P));
  }
  var x3 = modP(lam * lam - x1 - x2);
  var y3 = modP(lam * (x1 - x3) - y1);
  return [x3, y3];
}
function ecMul(k, x, y) {
  var rx = null, ry = null;
  var qx = x, qy = y;
  while (k > 0n) {
    if (k & 1n) {
      var p = ecAdd(rx, ry, qx, qy);
      rx = p[0];
      ry = p[1];
    }
    var d = ecAdd(qx, qy, qx, qy);
    qx = d[0];
    qy = d[1];
    k >>= 1n;
  }
  return [rx, ry];
}
function compressPoint(x, y) {
  var compressed = new Uint8Array(33);
  compressed[0] = (y & 1n) ? 0x03 : 0x02;
  compressed.set(bigIntToBytes(x, 32), 1);
  return compressed;
}
function p1363ToDer(sig) {
  var r = sig.slice(0, 32);
  var s = sig.slice(32, 64);
  function encInt(bytes) {
    var i = 0;
    while (i < bytes.length - 1 && bytes[i] === 0) i++;
    var t = bytes.slice(i);
    if (t[0] & 0x80) {
      var p = new Uint8Array(t.length + 1);
      p.set(t, 1);
      return p;
    }
    return t;
  }
  var rD = encInt(r), sD = encInt(s);
  var inner = new Uint8Array(2 + rD.length + 2 + sD.length);
  var o = 0;
  inner[o++] = 0x02; inner[o++] = rD.length; inner.set(rD, o); o += rD.length;
  inner[o++] = 0x02; inner[o++] = sD.length; inner.set(sD, o);
  var der = new Uint8Array(2 + inner.length);
  der[0] = 0x30; der[1] = inner.length; der.set(inner, 2);
  return der;
}

async function deriveSalt(username) {
  var enc = new TextEncoder();
  var hash = await subtle.digest('SHA-256', enc.encode('dataverse001:' + username));
  return new Uint8Array(hash);
}

async function deriveKeypair(username, password) {
  var enc = new TextEncoder();
  var salt = await deriveSalt(username);
  var baseKey = await subtle.importKey('raw', enc.encode(password), 'PBKDF2', false, ['deriveBits']);
  var seedBuf = await subtle.deriveBits(
    { name: 'PBKDF2', salt: salt, iterations: 600000, hash: 'SHA-256' }, baseKey, 256
  );
  var seed = new Uint8Array(seedBuf);
  var d = bytesToBigInt(seed) %% (P256_N - 1n) + 1n;
  var dBytes = bigIntToBytes(d, 32);
  var pub = ecMul(d, P256_GX, P256_GY);
  var jwk = {
    kty: 'EC', crv: 'P-256',
    d: base64urlEncode(dBytes),
    x: base64urlEncode(bigIntToBytes(pub[0], 32)),
    y: base64urlEncode(bigIntToBytes(pub[1], 32))
  };
  var privateKey = await subtle.importKey('jwk', jwk, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
  return { privateKey: privateKey, pubkey: base64urlEncode(compressPoint(pub[0], pub[1])) };
}

async function importPEM(pemText) {
  var lines = pemText.trim().split('\n');
  var isSEC1 = lines[0].indexOf('EC PRIVATE KEY') !== -1;
  var b64 = '';
  for (var i = 0; i < lines.length; i++) {
    var line = lines[i].trim();
    if (line.indexOf('-----') === 0) continue;
    b64 += line;
  }
  var der = base64urlDecode(b64.replace(/\+/g, '-').replace(/\//g, '_'));
  if (!isSEC1) {
    var key = await subtle.importKey('pkcs8', der.buffer, { name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign']);
    var jwk = await subtle.exportKey('jwk', key);
    var pubX = bytesToBigInt(base64urlDecode(jwk.x));
    var pubY = bytesToBigInt(base64urlDecode(jwk.y));
    var compressed = compressPoint(pubX, pubY);
    delete jwk.key_ops;
    delete jwk.ext;
    var signKey = await subtle.importKey('jwk', jwk, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
    return { privateKey: signKey, pubkey: base64urlEncode(compressed) };
  }
  var pos = 0;
  function readTag() {
    var tag = der[pos++];
    var len = der[pos++];
    if (len & 0x80) {
      var numBytes = len & 0x7f;
      len = 0;
      for (var j = 0; j < numBytes; j++) len = (len << 8) | der[pos++];
    }
    return { tag: tag, len: len, start: pos };
  }
  readTag();
  var ver = readTag();
  pos += ver.len;
  var privOctet = readTag();
  var dScalar = der.slice(privOctet.start, privOctet.start + privOctet.len);
  var dVal = bytesToBigInt(dScalar);
  var dBytes32 = bigIntToBytes(dVal, 32);
  var pubPoint = ecMul(dVal, P256_GX, P256_GY);
  var comp = compressPoint(pubPoint[0], pubPoint[1]);
  var jwk2 = {
    kty: 'EC', crv: 'P-256',
    d: base64urlEncode(dBytes32),
    x: base64urlEncode(bigIntToBytes(pubPoint[0], 32)),
    y: base64urlEncode(bigIntToBytes(pubPoint[1], 32))
  };
  var privKey = await subtle.importKey('jwk', jwk2, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign']);
  return { privateKey: privKey, pubkey: base64urlEncode(comp) };
}

async function authenticate(privateKey, pubkey) {
  var challengeResp = await fetch('/auth/challenge', { headers: { 'Accept': 'application/json' } });
  if (!challengeResp.ok) throw new Error('Challenge request failed (' + challengeResp.status + ')');
  var challengeData = await challengeResp.json();
  var sigBuf = await subtle.sign(
    { name: 'ECDSA', hash: 'SHA-256' },
    privateKey,
    new TextEncoder().encode(challengeData.challenge)
  );
  var derSig = p1363ToDer(new Uint8Array(sigBuf));
  var tokenResp = await fetch('/auth/token', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
    credentials: 'include',
    body: JSON.stringify({
      pubkey: pubkey,
      challenge: challengeData.challenge,
      signature: bytesToBase64(derSig)
    })
  });
  if (!tokenResp.ok) {
    var errData = await tokenResp.json().catch(function() { return {}; });
    throw new Error(errData.error || ('Token exchange failed (' + tokenResp.status + ')'));
  }
  // Store in both systems: dv_page_auth (this login page) and dv_auth (dataverse-write.js)
  localStorage.setItem('dv_page_auth_pubkey', pubkey);
  localStorage.setItem('dv_auth_pubkey', pubkey);
}

// Store private key in both IndexedDB databases for cross-system compatibility
function openDBByName(name) {
  return new Promise(function(resolve, reject) {
    var req = indexedDB.open(name, 1);
    req.onupgradeneeded = function(e) {
      e.target.result.createObjectStore('keys');
    };
    req.onsuccess = function(e) { resolve(e.target.result); };
    req.onerror = function(e) { reject(e.target.error); };
  });
}

function storeKeyInDB(db, privateKey) {
  return new Promise(function(resolve, reject) {
    var tx = db.transaction('keys', 'readwrite');
    tx.objectStore('keys').put(privateKey, 'privateKey');
    tx.oncomplete = function() { resolve(); };
    tx.onerror = function(e) { reject(e.target.error); };
  });
}

async function storeKey(privateKey) {
  // Store in both databases so both this page and dataverse-write.js can find it
  var dbs = ['dv_page_auth', 'dv_auth'];
  for (var i = 0; i < dbs.length; i++) {
    try {
      var db = await openDBByName(dbs[i]);
      await storeKeyInDB(db, privateKey);
    } catch (e) { /* best effort */ }
  }
}

function loadKey() {
  // Try dv_page_auth first (this page's DB), then dv_auth (dataverse-write.js DB)
  return loadKeyFrom('dv_page_auth').then(function(key) {
    if (key) return key;
    return loadKeyFrom('dv_auth');
  });
}

function loadKeyFrom(name) {
  return openDBByName(name).then(function(db) {
    return new Promise(function(resolve, reject) {
      var tx = db.transaction('keys', 'readonly');
      var req = tx.objectStore('keys').get('privateKey');
      req.onsuccess = function() { resolve(req.result || null); };
      req.onerror = function(e) { reject(e.target.error); };
    });
  }).catch(function() { return null; });
}

function finish() {
  // Strip ?login=true before reloading so the page shows content instead of login
  var url = new URL(window.location.href);
  url.searchParams.delete('login');
  if (url.search === '') url.search = '';
  window.location.replace(url.toString());
}

function saveAuthMeta(mode, username) {
  // dv_page_auth system
  localStorage.setItem('dv_page_auth_mode', mode);
  if (username) localStorage.setItem('dv_page_auth_username', username);
  // dv_auth system (dataverse-write.js)
  localStorage.setItem('dv_auth_auth_mode', mode);
  if (username) localStorage.setItem('dv_auth_username', username);
}

document.getElementById('btn-pass').addEventListener('click', async function() {
  var username = usernameEl.value.trim();
  var password = passwordEl.value;
  if (!username || !password) {
    setStatus('Enter your username and password.', 'error');
    return;
  }
  setStatus('Signing in...');
  try {
    var kp = await deriveKeypair(username, password);
    await authenticate(kp.privateKey, kp.pubkey);
    await storeKey(kp.privateKey);
    saveAuthMeta('pass', username);
    setStatus('Signed in. Reloading…', 'ok');
    finish();
  } catch (err) {
    setStatus(err.message, 'error');
  }
});

document.getElementById('btn-pem').addEventListener('click', async function() {
  var pem = pemEl.value.trim();
  if (!pem) {
    setStatus('Paste a PEM private key.', 'error');
    return;
  }
  setStatus('Signing in...');
  try {
    var kp = await importPEM(pem);
    await authenticate(kp.privateKey, kp.pubkey);
    await storeKey(kp.privateKey);
    saveAuthMeta('pem', null);
    setStatus('Signed in. Reloading…', 'ok');
    finish();
  } catch (err) {
    setStatus(err.message, 'error');
  }
});

passwordEl.addEventListener('keydown', function(e) {
  if (e.key === 'Enter') document.getElementById('btn-pass').click();
});

(async function() {
  // Skip auto-login if already authenticated server-side
  if (document.getElementById('btn-signout')) return;

  // Prevent auto-login loop: if we already tried for this URL, don't retry
  var autoKey = 'dv_autologin_' + window.location.pathname;
  if (sessionStorage.getItem(autoKey)) return;

  // Check both storage systems for stored credentials
  var storedPubkey = localStorage.getItem('dv_page_auth_pubkey')
    || localStorage.getItem('dv_auth_pubkey');
  var storedMode = localStorage.getItem('dv_page_auth_mode')
    || localStorage.getItem('dv_auth_auth_mode');
  var storedUsername = localStorage.getItem('dv_page_auth_username')
    || localStorage.getItem('dv_auth_username');
  if (storedUsername) usernameEl.value = storedUsername;
  if (!storedPubkey) return;
  var privateKey = await loadKey();
  if (!privateKey) return;
  autoNoteEl.classList.remove('hidden');
  setStatus('Found a stored key. Signing in…');
  sessionStorage.setItem(autoKey, '1');
  try {
    await authenticate(privateKey, storedPubkey);
    setStatus('Signed in. Reloading…', 'ok');
    finish();
  } catch (err) {
    autoNoteEl.classList.add('hidden');
    if (storedMode === 'pem') setMode('pem');
    setStatus('');
  }
})();
})();
</script>
</body>
</html>`
