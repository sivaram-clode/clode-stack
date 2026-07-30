package composio

// oauth.go serves the fake OAuth consent screen. The connect flow returns a
// redirect_url pointing here; the page shows a short loading delay and a loud
// "this is a mock" banner so a human or agent driving a browser understands
// they are looking at fake, limited-access data — then polls until the
// connection flips ACTIVE and returns to the caller's callback.

import (
	"encoding/json"

	"github.com/gofiber/fiber/v2"
)

// oauthLanding renders the consent page for ?account=&callback=.
func (h *Handler) oauthLanding(c *fiber.Ctx) error {
	if h.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).SendString("composio mock: database unavailable")
	}
	account := c.Query("account")
	conn, ok, err := h.store.getConnection(c.UserContext(), account)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
	}
	if !ok {
		return c.Status(fiber.StatusNotFound).SendString("unknown connection")
	}
	c.Type("html")
	active := h.store.deriveStatus(conn) == statusActive
	return c.SendString(renderLanding(account, conn.Toolkit, c.Query("callback"), h.publicURL, active))
}

// oauthStatus reports the connection's derived status (polled by the page).
func (h *Handler) oauthStatus(c *fiber.Ctx) error {
	if h.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "database unavailable"})
	}
	conn, ok, err := h.store.getConnection(c.UserContext(), c.Query("account"))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	if !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "unknown connection"})
	}
	return c.JSON(fiber.Map{"status": h.store.deriveStatus(conn)})
}

// oauthComplete flips the connection ACTIVE immediately (the "Complete now"
// button), so a browser-driven flow need not wait out the grace window.
func (h *Handler) oauthComplete(c *fiber.Ctx) error {
	if h.store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "database unavailable"})
	}
	account := c.Query("account")
	if err := h.store.activate(c.UserContext(), account); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"status": statusActive})
}

// renderLanding builds the consent HTML. account/toolkit/callback are injected
// as JSON literals so they are safe inside the inline script. `active` server-
// renders the already-connected state (e.g. reopening a completed link).
func renderLanding(account, toolkit, callback, base string, active bool) string {
	accJSON, _ := json.Marshal(account)
	cbJSON, _ := json.Marshal(callback)
	baseJSON, _ := json.Marshal(base)
	activeJSON, _ := json.Marshal(active)
	tk := htmlEscape(toolkit)

	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mock Composio — Connect ` + tk + `</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body { margin: 0; font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
         background: #0f172a; color: #e2e8f0; display: grid; place-items: center; min-height: 100vh; }
  .card { width: min(440px, 92vw); background: #1e293b; border: 1px solid #334155;
          border-radius: 14px; padding: 28px; box-shadow: 0 10px 40px rgba(0,0,0,.4); }
  .banner { background: #7c2d12; color: #ffedd5; border: 1px solid #9a3412; border-radius: 8px;
            padding: 10px 12px; font-size: 13px; line-height: 1.4; margin-bottom: 20px; }
  h1 { font-size: 18px; margin: 0 0 4px; }
  .sub { color: #94a3b8; font-size: 13px; margin: 0 0 22px; }
  .row { display: flex; align-items: center; gap: 12px; margin: 18px 0; }
  .row[hidden] { display: none; }
  .spinner { width: 22px; height: 22px; border: 3px solid #475569; border-top-color: #38bdf8;
             border-radius: 50%; animation: spin 0.8s linear infinite; flex: none; }
  @keyframes spin { to { transform: rotate(360deg); } }
  .ok { width: 22px; height: 22px; border-radius: 50%; background: #16a34a; color: #fff;
        display: grid; place-items: center; font-size: 14px; flex: none; }
  .muted { color: #94a3b8; font-size: 13px; }
  .btn { margin-top: 18px; width: 100%; padding: 10px; border: 0; border-radius: 8px;
         background: #38bdf8; color: #082f49; font-weight: 600; font-size: 14px; cursor: pointer; }
  .btn:disabled { opacity: .5; cursor: default; }
  code { background: #0f172a; padding: 1px 6px; border-radius: 4px; font-size: 12px; color: #cbd5e1; }
</style>
</head>
<body>
  <div class="card">
    <div class="banner">🧪 <strong>Mock Composio</strong> — local test provider. This is not a real
      OAuth login. Data is fake and stored only in the local mock database; access is limited.</div>
    <h1>Connect ` + tk + `</h1>
    <p class="sub">Connection <code>` + htmlEscape(account) + `</code></p>
    <div class="row" id="state" hidden>
      <div class="spinner"></div>
      <div class="muted" id="stateText">Completing authorization…</div>
    </div>
    <button class="btn" id="authorize">Authorize &amp; Connect</button>
  </div>
<script>
  const ACCOUNT = ` + string(accJSON) + `;
  const CALLBACK = ` + string(cbJSON) + `;
  const BASE = ` + string(baseJSON) + `;
  const ALREADY_ACTIVE = ` + string(activeJSON) + `;
  const stateEl = document.getElementById('state');
  const btn = document.getElementById('authorize');

  function showConnected() {
    stateEl.hidden = false;
    stateEl.innerHTML = '<div class="ok">&#10003;</div><div>Connected. You can close this tab.</div>';
    btn.hidden = true;
    if (CALLBACK) { setTimeout(() => { window.location.href = CALLBACK; }, 1200); }
  }

  // Nothing completes until the user authorizes — mirrors real Composio, where
  // the connection stays pending until the OAuth URL is opened and approved.
  btn.addEventListener('click', async () => {
    btn.hidden = true;
    stateEl.hidden = false;               // show the loading widget (simulated token exchange)
    await new Promise(r => setTimeout(r, 1400));
    try {
      await fetch(BASE + '/oauth/complete?account=' + encodeURIComponent(ACCOUNT), { method: 'POST' });
    } catch (e) {
      stateEl.innerHTML = '<div class="muted">Failed to complete — retry.</div>';
      btn.hidden = false;
      return;
    }
    showConnected();
  });

  if (ALREADY_ACTIVE) { showConnected(); }
</script>
</body>
</html>`
}

// htmlEscape escapes the few characters that matter for text interpolated into
// element content / attributes here (ids and slugs are already tame).
func htmlEscape(s string) string {
	repl := map[rune]string{'&': "&amp;", '<': "&lt;", '>': "&gt;", '"': "&quot;", '\'': "&#39;"}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if e, ok := repl[r]; ok {
			out = append(out, []rune(e)...)
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}
