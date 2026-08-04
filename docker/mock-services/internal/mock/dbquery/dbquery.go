// Package dbquery serves a Model Context Protocol (MCP) server as one route
// group inside the unified mock, letting an MCP client run read/write queries
// against the local stack's datastores by URL. It exposes three datasources —
// Postgres (every logical DB on the shared server), Redis (the stack's single
// instance, its two logical DBs), and Databend (via its HTTP query API) — behind
// two tools: `query` (run a statement/command verbatim, result as JSON) and
// `list_datasources` (what's reachable). Connection params come from the same
// DB_* env the composio group uses, so no new wiring is needed.
//
// The MCP transport is streamable-HTTP, reachable through traefik at
// http://mock-services.localhost:8080/db, and gated by a bearer token
// (MOCK_SERVICES_DB_MCP_TOKEN). Every other group here is native Fiber, but MCP
// libraries are net/http-based; this group is the one deliberate exception,
// mounting the MCP handler through fiber's http adaptor (see Register).
package dbquery

import (
	"crypto/subtle"
	"log"
	"os"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// defaultToken is the local-stack fallback bearer token used only when
// MOCK_SERVICES_DB_MCP_TOKEN is unset — enough to keep the endpoint working out
// of the box, while the compose block should set an explicit value.
const defaultToken = "clode-db-mcp-local"

// Handler is the db-query group. srv is the datasource executor; token is the
// bearer secret every request must present.
type Handler struct {
	srv   *sources
	token string
	mcp   *mcpserver.StreamableHTTPServer
}

// config is the resolved connection configuration, read from env with the same
// keys + local defaults the composio group uses (DB_*), plus Redis and Databend.
type config struct {
	pgHost, pgPort, pgUser, pgPassword, pgSSLMode string

	redisAddr, redisPassword string

	// databendEndpoint is the Databend HTTP query API base (e.g.
	// http://databend:8000). Empty disables the datasource with a clear error
	// rather than blocking Postgres/Redis.
	databendEndpoint, databendUser, databendPassword string
}

// loadConfig resolves connection params from the environment.
func loadConfig() config {
	return config{
		pgHost:     envOr("DB_HOST", "db"),
		pgPort:     envOr("DB_PORT", "5432"),
		pgUser:     envOr("DB_USER", "postgres"),
		pgPassword: envOr("DB_PASSWORD", "postgres"),
		pgSSLMode:  envOr("DB_SSL_MODE", "disable"),

		redisAddr:     envOr("DB_MCP_REDIS_ADDR", "redis:6379"),
		redisPassword: envOr("DB_MCP_REDIS_PASSWORD", envOr("REDIS_PASSWORD", "clode-redis-local")),

		databendEndpoint: envOr("DB_MCP_DATABEND_ENDPOINT", ""),
		databendUser:     envOr("DB_MCP_DATABEND_USER", "root"),
		databendPassword: envOr("DB_MCP_DATABEND_PASSWORD", ""),
	}
}

// New reads configuration from the environment and builds the group handler. It
// never dials on construction — connections are opened lazily per query — so a
// down datastore surfaces as a tool error, not a failed boot.
func New() *Handler {
	token := os.Getenv("MOCK_SERVICES_DB_MCP_TOKEN")
	if token == "" {
		token = defaultToken
		log.Printf("[db] MOCK_SERVICES_DB_MCP_TOKEN unset, using local default token")
	}

	srv := newSources(loadConfig())

	mcpSrv := mcpserver.NewMCPServer("mock-services-db", "0.1.0",
		mcpserver.WithToolCapabilities(false))
	registerTools(mcpSrv, srv)

	// disableLocalhostProtection: the handler sits behind traefik, so requests
	// arrive with Host mock-services.localhost over the docker bridge; the MCP
	// library's DNS-rebinding guard is meant for a directly-exposed listener and
	// would otherwise second-guess a proxied Host. endpointPath is cosmetic here
	// (ServeHTTP dispatches by method, not path) but kept aligned with the mount.
	httpSrv := mcpserver.NewStreamableHTTPServer(mcpSrv,
		mcpserver.WithEndpointPath("/db"),
		mcpserver.WithStateLess(true),
		mcpserver.WithDisableLocalhostProtection(true),
	)

	return &Handler{srv: srv, token: token, mcp: httpSrv}
}

// Register mounts the MCP endpoint on the already-prefixed (/db) router. The MCP
// server is a net/http.Handler, so — uniquely in this codebase — it is bridged
// with fiber's official http adaptor. That exception is confined to this one
// third-party handler; our own handlers stay native Fiber. The bearer-token
// check runs as a Fiber middleware in front of the bridge.
func (h *Handler) Register(r fiber.Router) {
	bridge := adaptor.HTTPHandler(h.mcp)
	// MCP streamable-HTTP uses POST (calls), GET (server->client SSE), and
	// DELETE (session end); match every method on the group root.
	r.All("/", h.authThen(bridge))
}

// authThen wraps a Fiber handler with the bearer-token gate. A missing or
// mismatched token is rejected before the request reaches the MCP handler.
func (h *Handler) authThen(next fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !h.authorized(c) {
			return c.Status(fiber.StatusUnauthorized).
				JSON(fiber.Map{"error": "missing or invalid bearer token"})
		}
		return next(c)
	}
}

// authorized reports whether the request carries the configured bearer token,
// compared in constant time.
func (h *Handler) authorized(c *fiber.Ctx) bool {
	auth := c.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	got := strings.TrimSpace(auth[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
