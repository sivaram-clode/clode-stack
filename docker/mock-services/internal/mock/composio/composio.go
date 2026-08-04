// Package composio is a self-contained mock of the Composio v3.1 API, served as
// one route group inside the unified mock. It lets the local stack's
// toolkit-proxy connect and use a fixed, limited set of toolkits/tools without
// real Composio credentials or a browser OAuth round-trip against a real
// provider. State is persisted in Postgres (see db.go), keyed by connect-ref, so
// tool calls are consistent and survive restarts.
//
// toolkit-proxy points COMPOSIO_BASE_URL at http://mock-services:8080/composio; its
// typed client appends /api/v3.1/... and its /cli passthrough forwards raw
// paths, so every route here lives under the /composio group prefix.
package composio

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Handler is the composio group. store is nil when DB bootstrap failed — catalog
// routes still work, data routes return 503, so a broken DB is loud not silent.
type Handler struct {
	store     *store
	publicURL string
}

// New reads configuration from the environment, provisions the mock database +
// schema, and returns the group handler. A DB failure is logged (not fatal): the
// unified mock keeps running and this group serves 503 on data routes.
func New() *Handler {
	publicURL := strings.TrimRight(envOr("COMPOSIO_MOCK_PUBLIC_URL", "http://mock-services.localhost:8080/composio"), "/")

	cfg := dbConfig{
		Host:     envOr("DB_HOST", "db"),
		Port:     envOr("DB_PORT", "5432"),
		User:     envOr("DB_USER", "postgres"),
		Password: envOr("DB_PASSWORD", "postgres"),
		SSLMode:  envOr("DB_SSL_MODE", "disable"),
		DBName:   envOr("COMPOSIO_MOCK_DB_NAME", "composio_mock"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := openStore(ctx, cfg)
	if err != nil {
		log.Printf("[composio] DB bootstrap failed, data routes will 503: %v", err)
		return &Handler{publicURL: publicURL}
	}
	log.Printf("[composio] schema ready (db=%s)", cfg.DBName)
	return &Handler{store: st, publicURL: publicURL}
}

// Register mounts every route on the already-prefixed (/composio) router.
func (h *Handler) Register(r fiber.Router) {
	// Auth configs
	r.Get("/api/v3.1/auth_configs", h.listAuthConfigs)
	r.Post("/api/v3.1/auth_configs", h.createAuthConfig)

	// Connected accounts
	r.Post("/api/v3.1/connected_accounts/link", h.createLink)
	r.Get("/api/v3.1/connected_accounts", h.listConnections)
	r.Get("/api/v3.1/connected_accounts/:id", h.getConnection)
	r.Patch("/api/v3.1/connected_accounts/:id/status", h.updateStatus)
	r.Patch("/api/v3.1/connected_accounts/:id", h.updateConnection)
	r.Post("/api/v3.1/connected_accounts/:id/refresh", h.refreshConnection)
	r.Delete("/api/v3.1/connected_accounts/:id", h.deleteConnection)

	// Toolkits + tools
	r.Get("/api/v3.1/toolkits", h.listToolkits)
	r.Get("/api/v3.1/tools/enum", h.toolsEnum)
	r.Get("/api/v3.1/tools", h.listTools)
	r.Post("/api/v3.1/tools/execute/:slug", h.executeTool)

	// OAuth landing page (browser)
	r.Get("/oauth", h.oauthLanding)
	r.Get("/oauth/status", h.oauthStatus)
	r.Post("/oauth/complete", h.oauthComplete)

	// Tool Router passthrough (stubs — exercised via /cli)
	for _, v := range []string{"/api/v3/tool_router/session", "/api/v3.1/tool_router/session"} {
		r.Post(v, h.toolRouterSession)
		r.Post(v+"/:id/:action", h.toolRouterSubpath)
	}

	// Triggers (stubs — keep the surface from 404ing)
	r.Get("/api/v3.1/triggers_types", h.emptyPage)
	r.Get("/api/v3.1/triggers_types/:slug", h.triggerType)
	r.Get("/api/v3.1/trigger_instances/active", h.emptyPage)
	r.Post("/api/v3.1/trigger_instances/:slug/upsert", h.upsertTrigger)
	r.Delete("/api/v3.1/trigger_instances/manage/:id", h.deleteTrigger)
}

// ── Auth configs ──────────────────────────────────────────────────────────────

func (h *Handler) listAuthConfigs(c *fiber.Ctx) error {
	slug := firstQuery(c, "toolkit_slug", "toolkit")
	items := []fiber.Map{}
	for _, t := range toolkits {
		if slug != "" && t.Slug != slug {
			continue
		}
		items = append(items, fiber.Map{
			"id":                  authConfigID(t.Slug),
			"auth_scheme":         "OAUTH2",
			"is_composio_managed": true,
			"toolkit":             toolkitObj(t),
		})
	}
	return c.JSON(pageItems(applyLimit(c, items)))
}

func (h *Handler) createAuthConfig(c *fiber.Ctx) error {
	var body struct {
		Toolkit struct {
			Slug string `json:"slug"`
		} `json:"toolkit"`
	}
	_ = c.BodyParser(&body)
	if body.Toolkit.Slug == "" {
		return errEnvelope(c, fiber.StatusBadRequest, "toolkit.slug is required")
	}
	return c.JSON(fiber.Map{"auth_config": fiber.Map{"id": authConfigID(body.Toolkit.Slug)}})
}

// ── Connected accounts ────────────────────────────────────────────────────────

func (h *Handler) createLink(c *fiber.Ctx) error {
	if h.store == nil {
		return errEnvelope(c, fiber.StatusServiceUnavailable, "database unavailable")
	}
	var body struct {
		AuthConfigID string  `json:"auth_config_id"`
		UserID       string  `json:"user_id"`
		Alias        *string `json:"alias"`
		CallbackURL  string  `json:"callback_url"`
	}
	_ = c.BodyParser(&body)

	slug := toolkitFromAuthConfigID(body.AuthConfigID)
	if _, ok := toolkitBySlug(slug); !ok {
		return errEnvelope(c, fiber.StatusBadRequest, "unknown or missing auth_config_id")
	}
	accountRef := newID("ca_mock")

	if err := h.store.createConnection(c.UserContext(), connRow{
		AccountRef:   accountRef,
		UserID:       body.UserID,
		Toolkit:      slug,
		AuthConfigID: body.AuthConfigID,
		Alias:        body.Alias,
	}); err != nil {
		return errEnvelope(c, fiber.StatusInternalServerError, err.Error())
	}

	redirect := h.publicURL + "/oauth?account=" + url.QueryEscape(accountRef)
	if body.CallbackURL != "" {
		redirect += "&callback=" + url.QueryEscape(body.CallbackURL)
	}
	return c.JSON(fiber.Map{
		"link_token":           newID("lnk"),
		"redirect_url":         redirect,
		"expires_at":           "2099-12-31T23:59:59Z",
		"connected_account_id": accountRef,
	})
}

func (h *Handler) listConnections(c *fiber.Ctx) error {
	if h.store == nil {
		return errEnvelope(c, fiber.StatusServiceUnavailable, "database unavailable")
	}
	userIDs := multiQuery(c, "user_ids", "user_id")
	toolkitSlugs := multiQuery(c, "toolkit_slugs", "toolkit_slug")
	statusFilter := lowerSet(multiQuery(c, "statuses", "status"))

	rows, err := h.store.listConnections(c.UserContext(), userIDs, toolkitSlugs)
	if err != nil {
		return errEnvelope(c, fiber.StatusInternalServerError, err.Error())
	}
	items := []fiber.Map{}
	for _, r := range rows {
		m := h.accountObj(r, false)
		if len(statusFilter) > 0 && !statusFilter[strings.ToLower(m["status"].(string))] {
			continue
		}
		items = append(items, m)
	}
	return c.JSON(pageItems(applyLimit(c, items)))
}

func (h *Handler) getConnection(c *fiber.Ctx) error {
	r, ok, err := h.lookup(c)
	if err != nil {
		return err
	}
	if !ok || r.IsDeleted {
		return errEnvelope(c, fiber.StatusNotFound, "connection not found")
	}
	return c.JSON(h.accountObj(r, true))
}

func (h *Handler) updateConnection(c *fiber.Ctx) error {
	r, ok, err := h.lookup(c)
	if err != nil {
		return err
	}
	if !ok {
		return errEnvelope(c, fiber.StatusNotFound, "connection not found")
	}
	var body struct {
		Alias string `json:"alias"`
	}
	_ = c.BodyParser(&body)
	if err := h.store.setAlias(c.UserContext(), r.AccountRef, body.Alias); err != nil {
		return errEnvelope(c, fiber.StatusInternalServerError, err.Error())
	}
	return c.JSON(fiber.Map{"success": true, "id": r.AccountRef, "status": h.store.deriveStatus(r)})
}

func (h *Handler) updateStatus(c *fiber.Ctx) error {
	r, ok, err := h.lookup(c)
	if err != nil {
		return err
	}
	if !ok {
		return errEnvelope(c, fiber.StatusNotFound, "connection not found")
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	_ = c.BodyParser(&body)
	if err := h.store.setDisabled(c.UserContext(), r.AccountRef, !body.Enabled); err != nil {
		return errEnvelope(c, fiber.StatusInternalServerError, err.Error())
	}
	r.IsDisabled = !body.Enabled
	return c.JSON(fiber.Map{"success": true, "id": r.AccountRef, "status": h.store.deriveStatus(r)})
}

func (h *Handler) refreshConnection(c *fiber.Ctx) error {
	r, ok, err := h.lookup(c)
	if err != nil {
		return err
	}
	if !ok {
		return errEnvelope(c, fiber.StatusNotFound, "connection not found")
	}
	var body struct {
		RedirectURL string `json:"redirect_url"`
	}
	_ = c.BodyParser(&body)
	redirect := body.RedirectURL
	if redirect == "" {
		redirect = h.publicURL + "/oauth?account=" + url.QueryEscape(r.AccountRef)
	}
	return c.JSON(fiber.Map{"id": r.AccountRef, "status": h.store.deriveStatus(r), "redirect_url": redirect})
}

func (h *Handler) deleteConnection(c *fiber.Ctx) error {
	r, ok, err := h.lookup(c)
	if err != nil {
		return err
	}
	if !ok {
		return errEnvelope(c, fiber.StatusNotFound, "connection not found")
	}
	if err := h.store.softDelete(c.UserContext(), r.AccountRef); err != nil {
		return errEnvelope(c, fiber.StatusInternalServerError, err.Error())
	}
	return c.JSON(fiber.Map{"success": true})
}

// lookup loads the :id connection, or (…, false, nil) if absent. A store outage
// returns a non-nil fiber error the caller should propagate.
func (h *Handler) lookup(c *fiber.Ctx) (connRow, bool, error) {
	if h.store == nil {
		return connRow{}, false, errEnvelope(c, fiber.StatusServiceUnavailable, "database unavailable")
	}
	r, ok, err := h.store.getConnection(c.UserContext(), c.Params("id"))
	if err != nil {
		return connRow{}, false, errEnvelope(c, fiber.StatusInternalServerError, err.Error())
	}
	return r, ok, nil
}

func (h *Handler) accountObj(r connRow, includeState bool) fiber.Map {
	tk, _ := toolkitBySlug(r.Toolkit)
	tk.Slug = r.Toolkit // preserve slug even if unknown to the catalog
	ts := r.CreatedAt.UTC().Format(time.RFC3339)
	m := fiber.Map{
		"id":          r.AccountRef,
		"user_id":     r.UserID,
		"status":      h.store.deriveStatus(r),
		"alias":       r.Alias,
		"word_id":     nil,
		"is_disabled": r.IsDisabled,
		"created_at":  ts,
		"updated_at":  ts,
		"toolkit":     toolkitObj(tk),
		"auth_config": fiber.Map{"auth_scheme": "OAUTH2"},
	}
	if includeState {
		m["state"] = fiber.Map{"val": fiber.Map{"access_token": "mock-access-token-" + r.AccountRef}}
	}
	return m
}

// ── Toolkits + tools ──────────────────────────────────────────────────────────

func (h *Handler) listToolkits(c *fiber.Ctx) error {
	search := strings.ToLower(firstQuery(c, "search"))
	items := []fiber.Map{}
	for _, t := range toolkits {
		if search != "" && !strings.Contains(strings.ToLower(t.Name), search) && !strings.Contains(t.Slug, search) {
			continue
		}
		items = append(items, fiber.Map{
			"slug":                          t.Slug,
			"name":                          t.Name,
			"no_auth":                       false,
			"auth_schemes":                  []string{"OAUTH2"},
			"composio_managed_auth_schemes": []string{"OAUTH2"},
			"meta": fiber.Map{
				"logo":           logo(t.Slug),
				"description":    t.Description,
				"app_url":        "https://mock.composio.local/" + t.Slug,
				"tools_count":    countTools(t.Slug),
				"triggers_count": 0,
				"categories":     []any{},
			},
		})
	}
	return c.JSON(toolsPageItems(applyLimit(c, items)))
}

func (h *Handler) listTools(c *fiber.Ctx) error {
	toolkitSlug := firstQuery(c, "toolkit_slug", "toolkit")
	toolSlugs := lowerSet(multiQuery(c, "tool_slugs", "tool_slug"))
	tagFilter := lowerSet(multiQuery(c, "tags", "tags[]"))
	scopeFilter := lowerSet(multiQuery(c, "scopes", "scopes[]"))
	query := strings.ToLower(firstQuery(c, "query", "search"))

	items := []fiber.Map{}
	for _, t := range tools {
		if toolkitSlug != "" && t.Toolkit != toolkitSlug {
			continue
		}
		if len(toolSlugs) > 0 && !toolSlugs[strings.ToLower(t.Slug)] {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(t.Name), query) && !strings.Contains(strings.ToLower(t.Description), query) {
			continue
		}
		if len(tagFilter) > 0 && !anyIn(tagFilter, tagsFor(t)) {
			continue
		}
		if len(scopeFilter) > 0 && !anyIn(scopeFilter, scopesFor(t)) {
			continue
		}
		items = append(items, toolObj(t))
	}
	return c.JSON(toolsPageItems(applyLimit(c, items)))
}

func (h *Handler) toolsEnum(c *fiber.Ctx) error {
	slugs := make([]string, 0, len(tools))
	for _, t := range tools {
		slugs = append(slugs, t.Slug)
	}
	return c.JSON(slugs)
}

func toolObj(t tool) fiber.Map {
	tk, _ := toolkitBySlug(t.Toolkit)
	return fiber.Map{
		"slug":               t.Slug,
		"name":               t.Name,
		"description":        t.Description,
		"human_description":  t.Description,
		"toolkit":            fiber.Map{"slug": t.Toolkit, "name": tk.Name, "logo": logo(t.Toolkit)},
		"input_parameters":   fiber.Map{"type": "object", "properties": fiber.Map{}},
		"output_parameters":  fiber.Map{"type": "object", "properties": fiber.Map{}},
		"no_auth":            false,
		"version":            "latest",
		"available_versions": []string{"latest"},
		"scopes":             scopesFor(t),
		"scope_requirements": nil,
		"tags":               tagsFor(t),
		"is_deprecated":      false,
	}
}

// ── Execute ───────────────────────────────────────────────────────────────────

func (h *Handler) executeTool(c *fiber.Ctx) error {
	if h.store == nil {
		return errEnvelope(c, fiber.StatusServiceUnavailable, "database unavailable")
	}
	slug := c.Params("slug")
	t, ok := toolBySlug(slug)
	if !ok {
		return c.JSON(execResult(false, "unknown tool "+slug, nil))
	}
	var body struct {
		UserID             string         `json:"user_id"`
		Arguments          map[string]any `json:"arguments"`
		ConnectedAccountID string         `json:"connected_account_id"`
	}
	_ = c.BodyParser(&body)

	accountRef, err := h.resolveAccount(c, body.ConnectedAccountID, body.UserID, t.Toolkit)
	if err != nil {
		return c.JSON(execResult(false, err.Error(), nil))
	}
	if body.Arguments == nil {
		body.Arguments = map[string]any{}
	}
	data, err := executeTool(c.UserContext(), h.store, accountRef, slug, body.Arguments)
	if err != nil {
		return c.JSON(execResult(false, err.Error(), nil))
	}
	return c.JSON(execResult(true, "", data))
}

// resolveAccount picks the connect-ref for an execute: the explicit one the
// proxy sends, else the most recent active connection for (user, toolkit).
func (h *Handler) resolveAccount(c *fiber.Ctx, explicit, userID, toolkit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	var users []string
	if userID != "" {
		users = []string{userID}
	}
	rows, err := h.store.listConnections(c.UserContext(), users, []string{toolkit})
	if err != nil {
		return "", err
	}
	for i := len(rows) - 1; i >= 0; i-- { // most recent first
		if h.store.deriveStatus(rows[i]) == statusActive {
			return rows[i].AccountRef, nil
		}
	}
	return "", fmt.Errorf("no active %s connection for this user", toolkit)
}

func execResult(ok bool, errMsg string, data any) fiber.Map {
	if data == nil {
		data = fiber.Map{}
	}
	return fiber.Map{"successful": ok, "error": errMsg, "log_id": newID("log"), "data": data}
}

// ── Tool Router + triggers (stubs) ────────────────────────────────────────────

func (h *Handler) toolRouterSession(c *fiber.Ctx) error {
	var body map[string]any
	_ = c.BodyParser(&body)
	var cfg any = fiber.Map{}
	if body != nil {
		if v, ok := body["config"]; ok {
			cfg = v
		}
	}
	return c.JSON(fiber.Map{"session_id": "sess_mock_1", "received_config": cfg})
}

func (h *Handler) toolRouterSubpath(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"session_id": c.Params("id"), "action": c.Params("action"), "ok": true})
}

func (h *Handler) emptyPage(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"items": []any{}, "next_cursor": nil, "total_items": 0})
}

func (h *Handler) triggerType(c *fiber.Ctx) error {
	slug := c.Params("slug")
	return c.JSON(fiber.Map{"slug": slug, "name": slug, "description": "mock trigger", "toolkit": fiber.Map{"slug": ""}, "config": fiber.Map{}, "payload": fiber.Map{}})
}

func (h *Handler) upsertTrigger(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"trigger_id": newID("ti")})
}

func (h *Handler) deleteTrigger(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"success": true})
}

// ── Shared helpers ────────────────────────────────────────────────────────────

func authConfigID(slug string) string { return "ac_mock_" + slug }

// toolkitFromAuthConfigID reverses authConfigID (ac_mock_<slug>).
func toolkitFromAuthConfigID(id string) string {
	return strings.TrimPrefix(id, "ac_mock_")
}

func toolkitObj(t toolkit) fiber.Map {
	return fiber.Map{"slug": t.Slug, "name": t.Name, "description": t.Description, "logo": logo(t.Slug)}
}

func countTools(slug string) int {
	n := 0
	for _, t := range tools {
		if t.Toolkit == slug {
			n++
		}
	}
	return n
}

func pageItems(items []fiber.Map) fiber.Map {
	return fiber.Map{"items": items, "next_cursor": nil, "total_items": len(items)}
}

func toolsPageItems(items []fiber.Map) fiber.Map {
	return fiber.Map{"items": items, "next_cursor": nil, "total_pages": 1, "current_page": 1, "total_items": len(items)}
}

// applyLimit trims the item slice to a ?limit= if a positive one is supplied.
func applyLimit(c *fiber.Ctx, items []fiber.Map) []fiber.Map {
	n := c.QueryInt("limit", 0)
	if n > 0 && n < len(items) {
		return items[:n]
	}
	return items
}

// firstQuery returns the first non-empty value among the given query keys.
func firstQuery(c *fiber.Ctx, keys ...string) string {
	for _, k := range keys {
		if v := c.Query(k); v != "" {
			return v
		}
	}
	return ""
}

// multiQuery collects all values for the given keys, splitting comma-separated
// lists and supporting repeated params — covering `k`, `k[]`, and `k=a,b` forms.
func multiQuery(c *fiber.Ctx, keys ...string) []string {
	var out []string
	args := c.Context().QueryArgs()
	for _, k := range keys {
		for _, raw := range args.PeekMulti(k) {
			for _, part := range strings.Split(string(raw), ",") {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

func lowerSet(vals []string) map[string]bool {
	if len(vals) == 0 {
		return nil
	}
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[strings.ToLower(v)] = true
	}
	return m
}

// anyIn reports whether any of vals is present in the set.
func anyIn(set map[string]bool, vals []string) bool {
	for _, v := range vals {
		if set[strings.ToLower(v)] {
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// errEnvelope replies with Composio's error shape ({"error":{message,status}}),
// which toolkit-proxy's ParseUpstreamError decodes.
func errEnvelope(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": fiber.Map{"message": msg, "status": status}})
}
