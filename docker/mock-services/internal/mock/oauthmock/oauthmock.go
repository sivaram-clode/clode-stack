// Package oauthmock is the `oauth-mock` route group of mock-services: a
// stand-in for real OAuth providers' authorize + token + userinfo endpoints,
// for local/CDP testing where a provider's hosted consent screen can't be
// driven by an automated browser.
//
// File map:
//
//	oauthmock.go  Provider interface + shared Core (consent, authorize, token)
//	              + the Fiber view engine for the consent screen
//	store.go      Profile + in-memory grant store
//	derive.go     pure email->name/id derivations
//	google.go     Google provider (its real endpoint paths + userinfo shape)
//	github.go     GitHub provider (ditto)
//	consent.html  the consent screen, rendered via Fiber's html view engine
//
// Adding a provider is one file implementing Provider — the core is untouched.
package oauthmock

import (
	"embed"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"
)

//go:embed consent.html
var consentFS embed.FS

// Engine returns the Fiber html view engine that renders the consent screen.
// server.New attaches it to the app (fiber.Config.Views); the template is
// embedded, so nothing is read from disk at runtime.
func Engine() *html.Engine {
	return html.NewFileSystem(http.FS(consentFS), ".html")
}

// Provider plugs one OAuth provider's real-shaped endpoints onto the group.
// Add a provider by implementing this and listing it in Register.
type Provider interface {
	// Name is the provider slug (google, github) — shown on the consent screen.
	Name() string
	// register wires the provider's authorize/token/userinfo routes onto r,
	// reusing core for the shared consent screen + code/token issuance.
	register(r fiber.Router, core *Core)
}

// Register mounts the oauth-mock group on r: the shared consent machinery plus
// every provider's real-shaped endpoints. Self-contained — an in-memory grant
// store and DEFAULT_EMAIL are all it needs.
func Register(r fiber.Router) {
	core := &Core{store: newStore(), defaultEmail: defaultEmail()}
	for _, p := range []Provider{GoogleProvider{}, GitHubProvider{}} {
		p.register(r, core)
	}
}

func defaultEmail() string {
	if v := os.Getenv("DEFAULT_EMAIL"); v != "" {
		return v
	}
	return "testuser@clode.dev"
}

// Core is the machinery every provider reuses: the grant store, the consent
// screen, and the authorize/token handlers. Providers add only their own
// userinfo serialization on top (Core.bearer resolves the profile).
type Core struct {
	store        *store
	defaultEmail string
}

// consent renders the consent screen for a provider's authorize path (GET).
// name labels the screen. The form posts back to the SAME path it was served
// at (ctx.Path()), so it works under the group's /oauth-mock prefix without the
// provider having to know it.
func (c *Core) consent(name string) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		redirectURI := ctx.Query("redirect_uri")
		if redirectURI == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing redirect_uri")
		}
		return ctx.Render("consent", fiber.Map{
			"Provider":    name,
			"Action":      ctx.Path(),
			"RedirectURI": redirectURI,
			"State":       ctx.Query("state"),
			"Email":       c.defaultEmail,
		})
	}
}

// approve completes the consent form (POST). On deny it mirrors the provider's
// access_denied redirect; on approve it derives a profile from the typed email,
// plants an auth code, and 302s back to redirect_uri?code=&state=.
func (c *Core) approve(ctx *fiber.Ctx) error {
	dest, err := url.Parse(ctx.FormValue("redirect_uri"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "bad redirect_uri")
	}
	state := ctx.FormValue("state")
	rq := dest.Query()
	if ctx.FormValue("action") == "deny" {
		rq.Set("error", "access_denied")
		setIf(rq, "state", state)
		dest.RawQuery = rq.Encode()
		return ctx.Redirect(dest.String(), fiber.StatusFound)
	}
	// FormValue is backed by the reused request buffer; the store outlives this
	// handler (read back on the later token/userinfo request), so clone it.
	email := strings.Clone(strings.TrimSpace(ctx.FormValue("email")))
	if email == "" {
		email = c.defaultEmail
	}
	// Email is the only input; everything a provider emits is derived from it —
	// deterministically, so the same email is always the same user.
	code := randToken()
	c.store.put(code, Profile{Email: email, Name: deriveName(email)})
	rq.Set("code", code)
	setIf(rq, "state", state)
	dest.RawQuery = rq.Encode()
	return ctx.Redirect(dest.String(), fiber.StatusFound)
}

// token swaps an auth code for an access token (POST). It returns JSON, which
// the x/oauth2 library parses for both Google and GitHub token endpoints.
// Client credentials (Basic auth or body) are accepted and ignored.
func (c *Core) token(ctx *fiber.Ctx) error {
	code := ctx.FormValue("code")
	p, ok := c.store.get(code)
	if !ok {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_grant"})
	}
	access := randToken()
	c.store.put(access, p) // reachable by the userinfo call
	return ctx.JSON(fiber.Map{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"scope":        ctx.FormValue("scope"),
	})
}

// bearer resolves the profile behind the request's token, for a provider's
// userinfo handler. Accepts both "Bearer <tok>" (Google) and "token <tok>"
// (GitHub) authorization schemes.
func (c *Core) bearer(ctx *fiber.Ctx) (Profile, bool) {
	tok := strings.TrimSpace(ctx.Get("Authorization"))
	tok = strings.TrimSpace(strings.TrimPrefix(tok, "Bearer"))
	tok = strings.TrimSpace(strings.TrimPrefix(tok, "token"))
	return c.store.get(tok)
}

// setIf sets a query param only when the value is non-empty.
func setIf(q url.Values, key, val string) {
	if val != "" {
		q.Set(key, val)
	}
}
