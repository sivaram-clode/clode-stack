package main

import "github.com/gofiber/fiber/v2"

// GitHubProvider mocks GitHub's OAuth endpoints, matching the real paths and
// response shapes raksha's GitHub provider expects:
//
//	authorize  GET  /login/oauth/authorize
//	token      POST /login/oauth/access_token
//	user       GET  /user          -> {id,login,name,email,avatar_url}
//	emails     GET  /user/emails    -> [{email,primary,verified}]
//
// (raksha falls back to /user/emails when /user returns no email — both are
// served here so either path works. See raksha internal/auth/oauth/github.go.)
type GitHubProvider struct{}

func (GitHubProvider) Name() string { return "github" }

func (GitHubProvider) Register(app *fiber.App, core *Core) {
	const authorize = "/login/oauth/authorize"
	app.Get(authorize, core.Consent("github", authorize))
	app.Post(authorize, core.Approve)
	app.Post("/login/oauth/access_token", core.Token)
	app.Get("/user", func(ctx *fiber.Ctx) error {
		p, ok := core.Bearer(ctx)
		if !ok {
			return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"message": "Bad credentials"})
		}
		return ctx.JSON(fiber.Map{
			"id":         deriveNumericID(p.Email),
			"login":      deriveLogin(p.Email),
			"name":       p.Name,
			"email":      p.Email,
			"avatar_url": "",
		})
	})
	app.Get("/user/emails", func(ctx *fiber.Ctx) error {
		p, ok := core.Bearer(ctx)
		if !ok {
			return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"message": "Bad credentials"})
		}
		return ctx.JSON([]fiber.Map{
			{"email": p.Email, "primary": true, "verified": true},
		})
	})
}
