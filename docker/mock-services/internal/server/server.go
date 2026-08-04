// Package server assembles the single Fiber app that fronts every mocked
// dependency. One HTTP server, one self-identifying route group per mock — each
// carrying a scoped-logging middleware so a line's prefix names the group that
// served it:
//
//	aws        → the EC2 query protocol (root + /aws) and the /_admin control plane
//	narnia     → /narnia/*      deploy + delete + status callbacks
//	baghira    → /baghira/*     pod-status (replicas)
//	composio   → /composio/*    Postgres-backed Composio API mock
//	oauth-mock → /oauth-mock/*  Google/GitHub authorize/token/userinfo + consent
//
// Every group is native Fiber, exposing one Register(fiber.Router); this file
// mounts each with a single line.
package server

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/sivaram-clode/mock-services/internal/mock/aws"
	"github.com/sivaram-clode/mock-services/internal/mock/baghira"
	"github.com/sivaram-clode/mock-services/internal/mock/composio"
	"github.com/sivaram-clode/mock-services/internal/mock/narnia"
	"github.com/sivaram-clode/mock-services/internal/mock/oauthmock"
)

// New builds the Fiber app wiring every mock's route group + a liveness probe.
func New(awsMock *aws.Mock, nh *narnia.Handler, bh *baghira.Handler) *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		AppName:               "mock-services",
		// The oauth-mock group renders its consent screen with Fiber's html
		// view engine (embedded template); no other group uses Views.
		Views: oauthmock.Engine(),
	})

	// Liveness — used by the compose healthcheck.
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// aws group: the AWS SDK dials the endpoint root, so the EC2 handler lives
	// at "/" (and /aws for an explicitly-namespaced base); the /_admin control
	// plane shares the group. Native Fiber.
	awsMock.Register(app.Group("/", scoped("aws")))

	// narnia + baghira groups — native Fiber, one prefix each.
	nh.Register(app.Group("/narnia", scoped("narnia")))
	bh.Register(app.Group("/baghira", scoped("baghira")))

	// composio group — a Postgres-backed mock of the Composio API for the local
	// stack's toolkit-proxy. Self-constructing (no shared deps): it bootstraps
	// its own database on New(); a DB failure degrades to 503 on data routes
	// rather than taking down the unified mock.
	composio.New().Register(app.Group("/composio", scoped("composio")))

	// oauth-mock group — Google/GitHub authorize/token/userinfo + the consent
	// screen (Fiber view engine). raksha points GOOGLE_OAUTH_BASE_URL here.
	oauthmock.Register(app.Group("/oauth-mock", scoped("oauth-mock")))

	return app
}

// scoped returns a middleware that logs one line per request tagged with the
// group name, e.g. `[narnia] POST /narnia/internal/deployments/batch -> 201 (2ms)`.
func scoped(group string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		log.Printf("[%s] %s %s -> %d (%s)", group, c.Method(), c.OriginalURL(),
			c.Response().StatusCode(), time.Since(start).Round(time.Millisecond))
		return err
	}
}
