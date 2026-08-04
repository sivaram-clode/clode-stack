package oauthmock

import "github.com/gofiber/fiber/v2"

// GitHubProvider mocks GitHub's OAuth endpoints, matching the real paths and
// response shapes raksha's GitHub provider expects (relative to the group's
// /oauth-mock prefix):
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

func (GitHubProvider) register(r fiber.Router, core *Core) {
	const authorize = "/login/oauth/authorize"
	r.Get(authorize, core.consent("github"))
	r.Post(authorize, core.approve)
	r.Post("/login/oauth/access_token", core.token)
	r.Get("/user", func(ctx *fiber.Ctx) error {
		p, ok := core.bearer(ctx)
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
	r.Get("/user/emails", func(ctx *fiber.Ctx) error {
		p, ok := core.bearer(ctx)
		if !ok {
			return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"message": "Bad credentials"})
		}
		return ctx.JSON([]fiber.Map{
			{"email": p.Email, "primary": true, "verified": true},
		})
	})
}
