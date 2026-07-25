// Package main — oauth-mock: a stand-in for real OAuth providers' authorize
// + token + userinfo endpoints, for local/CDP testing where a provider's
// hosted consent screen can't be driven by an automated browser.
//
// File map:
//
//	main.go     entrypoint — env, build app, listen
//	server.go   Provider interface + shared Core (consent, authorize, token)
//	store.go    Profile + in-memory grant store
//	derive.go   pure email->name/id derivations
//	google.go   Google provider (its real endpoint paths + userinfo shape)
//	github.go   GitHub provider (ditto)
//	views/consent.html   the consent screen (edit without touching Go)
//
// Adding a provider is one file implementing Provider — the core is untouched.
package main

import (
	"bytes"
	"html/template"
	"log"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// Provider plugs one OAuth provider's real-shaped endpoints into the mock.
// Add a provider by implementing this and listing it in main().
type Provider interface {
	// Name is the provider slug (google, github) — shown on the consent screen.
	Name() string
	// Register wires the provider's authorize/token/userinfo routes onto app,
	// reusing core for the shared consent screen + code/token issuance.
	Register(app *fiber.App, core *Core)
}

// Core is the machinery every provider reuses: the grant store, the consent
// screen, and the authorize/token handlers. Providers add only their own
// userinfo serialization on top (Core.Bearer resolves the profile).
type Core struct {
	store        *store
	defaultEmail string
	consent      *template.Template
}

func newCore(defaultEmail, templatePath string) (*Core, error) {
	if defaultEmail == "" {
		defaultEmail = "testuser@clode.dev"
	}
	t, err := template.ParseFiles(templatePath)
	if err != nil {
		return nil, err
	}
	return &Core{store: newStore(), defaultEmail: defaultEmail, consent: t}, nil
}

// consentData drives the consent screen (views/consent.html). Provider labels
// the heading; Action is the form's POST target (the provider's authorize path).
type consentData struct {
	Provider, Action, RedirectURI, State, Email string
}

// Consent renders the consent screen for a provider's authorize path (GET).
// name labels the screen; authorizePath is the form's POST target.
func (c *Core) Consent(name, authorizePath string) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		redirectURI := ctx.Query("redirect_uri")
		if redirectURI == "" {
			return fiber.NewError(fiber.StatusBadRequest, "missing redirect_uri")
		}
		var buf bytes.Buffer
		if err := c.consent.Execute(&buf, consentData{
			Provider:    name,
			Action:      authorizePath,
			RedirectURI: redirectURI,
			State:       ctx.Query("state"),
			Email:       c.defaultEmail,
		}); err != nil {
			return err
		}
		ctx.Type("html")
		return ctx.Send(buf.Bytes())
	}
}

// Approve completes the consent form (POST). On deny it mirrors the provider's
// access_denied redirect; on approve it derives a profile from the typed email,
// plants an auth code, and 302s back to redirect_uri?code=&state=.
func (c *Core) Approve(ctx *fiber.Ctx) error {
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
	email := strings.TrimSpace(ctx.FormValue("email"))
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
	log.Printf("authorize approve: email=%q -> code=%s", email, code[:8])
	return ctx.Redirect(dest.String(), fiber.StatusFound)
}

// Token swaps an auth code for an access token (POST). It returns JSON, which
// the x/oauth2 library parses for both Google and GitHub token endpoints.
// Client credentials (Basic auth or body) are accepted and ignored.
func (c *Core) Token(ctx *fiber.Ctx) error {
	code := ctx.FormValue("code")
	p, ok := c.store.get(code)
	if !ok {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_grant"})
	}
	access := randToken()
	c.store.put(access, p) // reachable by the userinfo call
	log.Printf("token: code=%s... -> access=%s...", code[:8], access[:8])
	return ctx.JSON(fiber.Map{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"scope":        ctx.FormValue("scope"),
	})
}

// Bearer resolves the profile behind the request's token, for a provider's
// userinfo handler. Accepts both "Bearer <tok>" (Google) and "token <tok>"
// (GitHub) authorization schemes.
func (c *Core) Bearer(ctx *fiber.Ctx) (Profile, bool) {
	tok := strings.TrimSpace(ctx.Get("Authorization"))
	tok = strings.TrimSpace(strings.TrimPrefix(tok, "Bearer"))
	tok = strings.TrimSpace(strings.TrimPrefix(tok, "token"))
	return c.store.get(tok)
}

// newApp builds the Fiber app with the given providers registered. defaultEmail
// prefills the consent form; templatePath points at the consent HTML.
func newApp(defaultEmail, templatePath string, providers ...Provider) (*fiber.App, error) {
	core, err := newCore(defaultEmail, templatePath)
	if err != nil {
		return nil, err
	}
	// Immutable makes ctx string values (e.g. the submitted email) safe to
	// retain past the request: fasthttp reuses request buffers across requests,
	// so without this a stored Profile.Email would be overwritten by the next
	// request's body. Perf is irrelevant for a mock; correctness isn't.
	app := fiber.New(fiber.Config{DisableStartupMessage: true, Immutable: true})
	app.Get("/healthz", func(ctx *fiber.Ctx) error { return ctx.SendString("ok") })
	for _, p := range providers {
		p.Register(app, core)
		log.Printf("provider registered: %s", p.Name())
	}
	return app, nil
}

// setIf sets a query param only when the value is non-empty.
func setIf(q url.Values, key, val string) {
	if val != "" {
		q.Set(key, val)
	}
}
