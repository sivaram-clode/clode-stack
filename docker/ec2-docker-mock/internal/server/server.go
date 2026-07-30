// Package server assembles the single Fiber app that fronts every API group.
// One HTTP server, three self-identifying route groups — each carrying a
// scoped-logging middleware so a line's prefix names the group that served it:
//
//	aws     → the EC2 query protocol (root + /aws) and the /_admin control plane
//	narnia  → /narnia/*  deploy + delete + status callbacks
//	baghira → /baghira/* pod-status (replicas)
//
// The aws group is the existing EC2-to-docker engine, mounted verbatim via the
// net/http adaptor so its behavior is unchanged; narnia and baghira are native
// Fiber handlers.
package server

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"

	"github.com/sivaram-clode/ec2-docker-mock/internal/mock/aws"
	"github.com/sivaram-clode/ec2-docker-mock/internal/mock/baghira"
	"github.com/sivaram-clode/ec2-docker-mock/internal/mock/composio"
	"github.com/sivaram-clode/ec2-docker-mock/internal/mock/narnia"
)

// New builds the Fiber app wiring the three API groups + a liveness probe.
func New(awsMock *aws.Mock, nh *narnia.Handler, bh *baghira.Handler) *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		AppName:               "unified-deployer",
	})

	// Liveness — used by the compose healthcheck.
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// aws group: the AWS SDK dials the endpoint root, so the EC2 handler lives
	// at "/" (and /aws for an explicitly-namespaced base). The /_admin control
	// plane shares the group. All mounted via the net/http adaptor.
	ec2 := adaptor.HTTPHandlerFunc(awsMock.ServeEC2)
	app.Post("/", scoped("aws"), ec2)
	app.Post("/aws", scoped("aws"), ec2)
	app.All("/_admin/*", scoped("aws"), adaptor.HTTPHandlerFunc(awsMock.ServeAdmin))

	// narnia + baghira groups — native Fiber, one prefix each.
	nh.Register(app.Group("/narnia", scoped("narnia")))
	bh.Register(app.Group("/baghira", scoped("baghira")))

	// composio group — a Postgres-backed mock of the Composio API for the local
	// stack's toolkit-proxy. Self-constructing (no shared deps): it bootstraps
	// its own database on New(); a DB failure degrades to 503 on data routes
	// rather than taking down the unified mock.
	composio.New().Register(app.Group("/composio", scoped("composio")))

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
