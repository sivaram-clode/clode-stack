package oauthmock

import "github.com/gofiber/fiber/v2"

// GoogleProvider mocks Google's OAuth endpoints, matching the real paths and
// userinfo shape raksha's Google provider expects (relative to the group's
// /oauth-mock prefix):
//
//	authorize  GET  /o/oauth2/auth
//	token      POST /token
//	userinfo   GET  /oauth2/v2/userinfo   -> {id,email,name,picture,verified_email}
type GoogleProvider struct{}

func (GoogleProvider) Name() string { return "google" }

func (GoogleProvider) register(r fiber.Router, core *Core) {
	const authorize = "/o/oauth2/auth"
	r.Get(authorize, core.consent("google"))
	r.Post(authorize, core.approve)
	r.Post("/token", core.token)
	r.Get("/oauth2/v2/userinfo", func(ctx *fiber.Ctx) error {
		p, ok := core.bearer(ctx)
		if !ok {
			return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_token"})
		}
		return ctx.JSON(fiber.Map{
			"id":             deriveHexID(p.Email),
			"email":          p.Email,
			"name":           p.Name,
			"picture":        "",
			"verified_email": true,
		})
	})
}
