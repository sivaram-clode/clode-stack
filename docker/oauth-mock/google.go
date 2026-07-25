package main

import "github.com/gofiber/fiber/v2"

// GoogleProvider mocks Google's OAuth endpoints, matching the real paths and
// userinfo shape raksha's Google provider expects:
//
//	authorize  GET  /o/oauth2/auth
//	token      POST /token
//	userinfo   GET  /oauth2/v2/userinfo   -> {id,email,name,picture,verified_email}
type GoogleProvider struct{}

func (GoogleProvider) Name() string { return "google" }

func (GoogleProvider) Register(app *fiber.App, core *Core) {
	const authorize = "/o/oauth2/auth"
	app.Get(authorize, core.Consent("google", authorize))
	app.Post(authorize, core.Approve)
	app.Post("/token", core.Token)
	app.Get("/oauth2/v2/userinfo", func(ctx *fiber.Ctx) error {
		p, ok := core.Bearer(ctx)
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
