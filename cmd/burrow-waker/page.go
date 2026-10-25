package main

import (
	"encoding/json"
	"html/template"
	"net/http"
)

// fallbackPage is a self-contained Wake button, for use without the PWA — for
// instance straight after setting up a waker, before the app is installed
// (docs/04-API.md §8).
//
// No external anything: the CSP below is as strict as the agent's, with one
// nonce-free exception for the inline handler, which is why the script is a
// single hard-coded line rather than anything built from data.
var fallbackPage = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Wake PC</title>
<style>
  :root { color-scheme: dark light; font-family: -apple-system, system-ui, sans-serif; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         background: #14161a; color: #e8eaed; }
  main { text-align: center; padding: 24px; }
  h1 { font-size: 17px; font-weight: 600; color: #9aa0a6; margin: 0 0 24px; }
  #state { font-size: 15px; margin: 0 0 24px; min-height: 22px; }
  button { font: inherit; font-size: 17px; font-weight: 600; padding: 18px 36px;
           border: 0; border-radius: 14px; background: #4c8bf5; color: #fff;
           min-width: 220px; min-height: 56px; }
  button:disabled { opacity: .5; }
</style>
</head>
<body>
<main>
  <h1>Burrow waker</h1>
  <p id="state">Checking…</p>
  <button id="wake">Wake PC</button>
</main>
<script src="/app.js"></script>
</body>
</html>`))

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/app.js" {
		s.handleAppJS(w, r)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'unsafe-inline'; "+
			"base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	_ = fallbackPage.Execute(w, nil)
}

// appJS is served as a separate file so the page needs no inline script, and
// the CSP can stay at script-src 'self'.
const appJS = `const stateEl = document.getElementById('state');
const btn = document.getElementById('wake');

async function refresh() {
  try {
    const r = await fetch('/api/status', { cache: 'no-store' });
    const j = await r.json();
    stateEl.textContent = {
      online: 'The PC is online.',
      booting: 'The PC is booting…',
      offline: 'The PC is off.'
    }[j.pc] || j.pc;
    btn.disabled = j.pc === 'online';
  } catch (e) {
    stateEl.textContent = 'Could not reach the waker.';
  }
}

btn.addEventListener('click', async () => {
  btn.disabled = true;
  stateEl.textContent = 'Sending…';
  try {
    const r = await fetch('/api/wake', { method: 'POST', headers: { 'X-Burrow': '1' } });
    if (r.status === 429) {
      const j = await r.json();
      stateEl.textContent = 'Too many wakes. Try again in ' + j.retryAfterSec + 's.';
    } else {
      stateEl.textContent = 'Wake sent. This takes up to three minutes.';
    }
  } catch (e) {
    stateEl.textContent = 'Could not send the wake.';
  }
  setTimeout(() => { btn.disabled = false; }, 30000);
});

refresh();
setInterval(refresh, 5000);
`

func (s *server) handleAppJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(appJS))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
