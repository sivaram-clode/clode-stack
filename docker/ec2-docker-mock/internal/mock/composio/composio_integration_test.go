package composio

// Integration test for the composio mock group against a real Postgres. It is
// skipped unless COMPOSIO_MOCK_TEST_DSN-style DB_* env is present (set by the
// harness that starts a throwaway Postgres), so `go test ./...` stays hermetic
// in CI without a database.

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

func newTestApp(t *testing.T) *fiber.App {
	t.Helper()
	if os.Getenv("DB_HOST") == "" {
		t.Skip("DB_HOST not set; skipping composio integration test")
	}
	h := New()
	if h.store == nil {
		t.Fatal("store is nil — DB bootstrap failed")
	}
	app := fiber.New()
	h.Register(app.Group("/composio"))
	return app
}

func do(t *testing.T, app *fiber.App, method, path string, body any) map[string]any {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s -> %d: %s", method, path, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s: bad json %q: %v", method, path, raw, err)
	}
	return out
}

func TestCatalog(t *testing.T) {
	app := newTestApp(t)

	tk := do(t, app, "GET", "/composio/api/v3.1/toolkits", nil)
	if int(tk["total_items"].(float64)) != len(toolkits) {
		t.Fatalf("toolkits total = %v, want %d", tk["total_items"], len(toolkits))
	}

	// tag filter returns only read tools for gmail
	rd := do(t, app, "GET", "/composio/api/v3.1/tools?toolkit_slug=gmail&tags=readonly", nil)
	items := rd["items"].([]any)
	if len(items) == 0 {
		t.Fatal("expected readonly gmail tools")
	}
	for _, it := range items {
		tags := it.(map[string]any)["tags"].([]any)
		if tags[0].(string) != "readonly" {
			t.Fatalf("non-readonly tool leaked: %v", it)
		}
	}
}

func TestConnectAndConsistency(t *testing.T) {
	app := newTestApp(t)
	user := "user_" + newID("t")

	// Create auth config + link → account_ref
	ac := do(t, app, "POST", "/composio/api/v3.1/auth_configs", map[string]any{"toolkit": map[string]any{"slug": "googlecalendar"}})
	authID := ac["auth_config"].(map[string]any)["id"].(string)

	link := do(t, app, "POST", "/composio/api/v3.1/connected_accounts/link", map[string]any{
		"auth_config_id": authID, "user_id": user,
	})
	acct := link["connected_account_id"].(string)
	if !strings.HasPrefix(acct, "ca_mock_") {
		t.Fatalf("bad account_ref %q", acct)
	}

	// Before completing the OAuth URL, the connection is still INITIATED — there
	// is no time-based auto-activation.
	pending := do(t, app, "GET", "/composio/api/v3.1/connected_accounts/"+acct, nil)
	if pending["status"] != "INITIATED" {
		t.Fatalf("pre-complete status = %v, want INITIATED", pending["status"])
	}

	// Complete the connection (the Authorize click / headless POST), then it is ACTIVE.
	do(t, app, "POST", "/composio/oauth/complete?account="+acct, nil)
	det := do(t, app, "GET", "/composio/api/v3.1/connected_accounts/"+acct, nil)
	if det["status"] != "ACTIVE" {
		t.Fatalf("status = %v, want ACTIVE", det["status"])
	}
	if det["state"].(map[string]any)["val"].(map[string]any)["access_token"] == "" {
		t.Fatal("expected a mock access_token in state")
	}

	// Create an event, then list — it must appear (the core requirement).
	do(t, app, "POST", "/composio/api/v3.1/tools/execute/GOOGLECALENDAR_CREATE_EVENT", map[string]any{
		"user_id": user, "connected_account_id": acct,
		"arguments": map[string]any{"summary": "standup", "start": "2026-08-01T09:00:00Z", "end": "2026-08-01T09:15:00Z"},
	})
	listed := do(t, app, "POST", "/composio/api/v3.1/tools/execute/GOOGLECALENDAR_LIST_EVENTS", map[string]any{
		"user_id": user, "connected_account_id": acct, "arguments": map[string]any{},
	})
	events := listed["data"].(map[string]any)["events"].([]any)
	if len(events) != 1 || events[0].(map[string]any)["summary"] != "standup" {
		t.Fatalf("event did not round-trip: %v", events)
	}

	// A different connect-ref must NOT see it (per-ref isolation).
	other := do(t, app, "POST", "/composio/api/v3.1/connected_accounts/link", map[string]any{
		"auth_config_id": authID, "user_id": user,
	})["connected_account_id"].(string)
	otherList := do(t, app, "POST", "/composio/api/v3.1/tools/execute/GOOGLECALENDAR_LIST_EVENTS", map[string]any{
		"user_id": user, "connected_account_id": other, "arguments": map[string]any{},
	})
	if n := len(otherList["data"].(map[string]any)["events"].([]any)); n != 0 {
		t.Fatalf("isolation broken: other ref saw %d events", n)
	}
}

func TestSheetsRoundTrip(t *testing.T) {
	app := newTestApp(t)
	user := "user_" + newID("t")
	acct := do(t, app, "POST", "/composio/api/v3.1/connected_accounts/link", map[string]any{
		"auth_config_id": authConfigID("googlesheets"), "user_id": user,
	})["connected_account_id"].(string)

	do(t, app, "POST", "/composio/api/v3.1/tools/execute/GOOGLESHEETS_APPEND_VALUES", map[string]any{
		"connected_account_id": acct,
		"arguments":            map[string]any{"values": []any{[]any{"hi", 1}, []any{"bye", 2}}},
	})
	got := do(t, app, "POST", "/composio/api/v3.1/tools/execute/GOOGLESHEETS_BATCH_GET", map[string]any{
		"connected_account_id": acct, "arguments": map[string]any{},
	})
	vr := got["data"].(map[string]any)["valueRanges"].([]any)[0].(map[string]any)
	rows := vr["values"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %v", rows)
	}
}

func TestExecuteErrors(t *testing.T) {
	app := newTestApp(t)
	user := "user_" + newID("t")
	acct := do(t, app, "POST", "/composio/api/v3.1/connected_accounts/link", map[string]any{
		"auth_config_id": authConfigID("gmail"), "user_id": user,
	})["connected_account_id"].(string)

	// Missing required 'to' → predictable successful:false, not a crash.
	res := do(t, app, "POST", "/composio/api/v3.1/tools/execute/GMAIL_SEND_EMAIL", map[string]any{
		"connected_account_id": acct, "arguments": map[string]any{},
	})
	if res["successful"] != false || !strings.Contains(res["error"].(string), "'to'") {
		t.Fatalf("expected a clean failure, got %v", res)
	}

	// Unknown tool slug → clean failure.
	unk := do(t, app, "POST", "/composio/api/v3.1/tools/execute/NOPE_TOOL", map[string]any{
		"connected_account_id": acct, "arguments": map[string]any{},
	})
	if unk["successful"] != false {
		t.Fatalf("expected unknown tool to fail, got %v", unk)
	}
}

func TestDeriveStatus(t *testing.T) {
	s := &store{}
	now := time.Now()
	// Not completed → INITIATED, regardless of how much time has passed.
	if got := s.deriveStatus(connRow{CreatedAt: now.Add(-time.Hour)}); got != statusInitiated {
		t.Fatalf("uncompleted status = %s, want INITIATED", got)
	}
	if got := s.deriveStatus(connRow{CreatedAt: now, ActivatedAt: &now}); got != statusActive {
		t.Fatalf("activated status = %s, want ACTIVE", got)
	}
	if got := s.deriveStatus(connRow{CreatedAt: now, IsDisabled: true, ActivatedAt: &now}); got != statusInactive {
		t.Fatalf("disabled status = %s, want INACTIVE", got)
	}
}
