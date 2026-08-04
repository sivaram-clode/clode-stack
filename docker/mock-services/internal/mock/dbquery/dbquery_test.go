package dbquery

// Unit tests for the db-query group that need no datastore: the command
// tokenizer, value normalization, the bearer-token gate, and datasource
// listing. A live-Postgres path is exercised only when DB_HOST is set (matching
// the composio group's convention), so `go test ./...` stays hermetic.

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"KEYS *", []string{"KEYS", "*"}},
		{"  GET   foo  ", []string{"GET", "foo"}},
		{`SET k "a b c"`, []string{"SET", "k", "a b c"}},
		{`HSET h field 'v 1'`, []string{"HSET", "h", "field", "v 1"}},
		{"", nil},
	}
	for _, c := range cases {
		got, err := tokenize(c.in)
		if err != nil {
			t.Fatalf("tokenize(%q): %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokenize(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
	if _, err := tokenize(`SET k "unterminated`); err == nil {
		t.Error("expected error on unterminated quote")
	}
}

func TestParseRedisDB(t *testing.T) {
	if n, err := parseRedisDB("1"); err != nil || n != 1 {
		t.Errorf("parseRedisDB(1) = %d, %v", n, err)
	}
	if _, err := parseRedisDB("-1"); err == nil {
		t.Error("expected error for negative db index")
	}
	if _, err := parseRedisDB("x"); err == nil {
		t.Error("expected error for non-numeric db index")
	}
}

func TestNormalizeRedisValue(t *testing.T) {
	got := normalizeRedisValue([]any{[]byte("a"), []byte("b")})
	if !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Errorf("normalizeRedisValue = %#v", got)
	}
}

func TestAuthGate(t *testing.T) {
	t.Setenv("MOCK_SERVICES_DB_MCP_TOKEN", "secret-token")
	h := New()

	app := fiber.New()
	h.Register(app.Group("/db"))

	// No token → 401.
	if code := statusFor(t, app, ""); code != fiber.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", code)
	}
	// Wrong token → 401.
	if code := statusFor(t, app, "Bearer nope"); code != fiber.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", code)
	}
	// Correct token → passes the gate (not 401; the MCP handler answers).
	if code := statusFor(t, app, "Bearer secret-token"); code == fiber.StatusUnauthorized {
		t.Errorf("correct token was rejected: got %d", code)
	}
}

func statusFor(t *testing.T, app *fiber.App, auth string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest("POST", "/db", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

func TestListPostgresDBs(t *testing.T) {
	if os.Getenv("DB_HOST") == "" {
		t.Skip("DB_HOST not set; skipping live postgres test")
	}
	src := newSources(loadConfig())
	dbs, err := src.listPostgresDBs(t.Context())
	if err != nil {
		t.Fatalf("listPostgresDBs: %v", err)
	}
	if len(dbs) == 0 {
		t.Error("expected at least one logical database")
	}
}

func TestQueryPostgresRoundTrip(t *testing.T) {
	if os.Getenv("DB_HOST") == "" {
		t.Skip("DB_HOST not set; skipping live postgres test")
	}
	src := newSources(loadConfig())
	res, err := src.queryPostgres(t.Context(), "postgres", "SELECT 1 AS n")
	if err != nil {
		t.Fatalf("queryPostgres: %v", err)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), `"n":1`) {
		t.Errorf("unexpected result: %s", raw)
	}
}
