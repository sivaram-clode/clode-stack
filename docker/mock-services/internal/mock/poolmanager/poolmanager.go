package poolmanager

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/sivaram-clode/mock-services/internal/deploy"
)

// claimTimeout bounds the whole synchronous claim (create → deploy → wait →
// transfer). Kept under brahmi's 120s pool-claim HTTP timeout.
const claimTimeout = 110 * time.Second

// healthTimeout bounds the wait for the deployed container to report Running.
// Best-effort: on timeout the claim still returns (consumers run their own
// readiness probes), so a slow image start doesn't fail the claim.
const healthTimeout = 80 * time.Second

// replicaChecker is the subset of *deploy.Deployer used to poll health — the
// same live-container view the baghira group serves.
type replicaChecker interface {
	Replicas(ctx context.Context, serviceID string) ([]deploy.Replica, error)
}

// Handler is the /pool-manager route group: an on-demand, origin-aware
// stand-in for the real pool-manager.
type Handler struct {
	cfg      Config
	jumbo    *jumboClient
	docker   *client.Client
	replicas replicaChecker
}

// New builds the poolmanager handler. docker is the shared client (used only to
// map the caller IP → container and to read slug labels); replicas is the
// deployer's live-container view.
func New(cfg Config, docker *client.Client, replicas replicaChecker) *Handler {
	return &Handler{
		cfg:      cfg,
		jumbo:    newJumboClient(cfg.JumboBaseURL),
		docker:   docker,
		replicas: replicas,
	}
}

// Register mounts the pool-manager claim API on the (already /pool-manager
// prefixed) router, matching the real pool-manager's routes so brahmi + ikki
// need no change beyond their base URL.
func (h *Handler) Register(r fiber.Router) {
	r.Post("/api/v1/claim", h.claim)
	r.Get("/api/v1/status", h.status)
	r.Get("/api/v1/config/:serviceType", h.config)
	r.Get("/api/v1/verify-service/:slug", h.verifyService)
}

type claimRequest struct {
	ProjectID     string `json:"projectId"`
	ApplicationID string `json:"applicationId"`
	ServiceType   string `json:"serviceType"`
}

type claimResponse struct {
	Status        string    `json:"status"`
	ServiceID     string    `json:"serviceId"`
	ServiceSlug   string    `json:"serviceSlug"`
	AgentID       string    `json:"agentId"`
	TransferredAt time.Time `json:"transferredAt"`
}

// claim resolves the caller's image and provisions it on demand: create the
// jumbo service under the pool org, push the rebuilt service-config, deploy via
// jumbo, wait until it's up, then transfer ownership to the caller's org.
func (h *Handler) claim(c *fiber.Ctx) error {
	var req claimRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"status": "ERROR", "message": "invalid request body"})
	}
	if req.ServiceType == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"status": "ERROR", "message": "serviceType is required"})
	}
	authHeader := c.Get("Authorization")

	ctx, cancel := context.WithTimeout(context.Background(), claimTimeout)
	defer cancel()

	// (1) resolve image for the caller.
	origin := h.resolveOrigin(ctx, c.IP())
	entry, ok := loadProvision(h.cfg.ProvisionPath).resolve(origin, req.ServiceType)
	if !ok {
		log.Printf("[pool-manager] no image configured for origin=%q serviceType=%q", origin, req.ServiceType)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"status": "ERROR", "message": "no image configured for serviceType " + req.ServiceType,
		})
	}
	log.Printf("[pool-manager] claim: origin=%q serviceType=%s image=%s", origin, req.ServiceType, entry.Image)

	// (2) provision via jumbo. Mint the pool-owner token first — it authorizes
	// the create, the deploy trigger, and the ownership transfer as the pool owner.
	token, err := mintPoolOwnerToken(ctx, h.cfg.RakshaBaseURL, h.cfg.OrgID, h.cfg.OwnerID)
	if err != nil {
		log.Printf("[pool-manager] mint pool-owner token failed: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"status": "ERROR", "message": "mint pool-owner token: " + err.Error()})
	}

	agentID := uuid.NewString()
	name := "odp-" + req.ServiceType + "-" + uuid.NewString()[:8]

	svcID, slug, err := h.jumbo.createService(ctx, h.cfg, name, req.ServiceType, token)
	if err != nil {
		log.Printf("[pool-manager] createService failed: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"status": "ERROR", "message": err.Error()})
	}

	cfgEntry, ok := buildConfigEntry(req.ServiceType, svcID, slug, entry.Image, entry.Env, agentID)
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"status": "ERROR", "message": "unsupported serviceType (no service-config template): " + req.ServiceType,
		})
	}
	if err := h.jumbo.pushConfig(ctx, cfgEntry, token); err != nil {
		log.Printf("[pool-manager] pushConfig failed: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"status": "ERROR", "message": err.Error()})
	}

	if err := h.jumbo.triggerDeploy(ctx, h.cfg, svcID, token); err != nil {
		log.Printf("[pool-manager] triggerDeploy failed: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"status": "ERROR", "message": err.Error()})
	}

	// Wait until the container is up (best-effort — see healthTimeout).
	h.waitHealthy(ctx, svcID.String())

	// (3) transfer ownership to the org resolved from the caller's token.
	if org := orgFromToken(authHeader); org != "" {
		if err := h.jumbo.transferOwnership(ctx, svcID, token, org, req.ProjectID, req.ApplicationID); err != nil {
			// Non-fatal: the container is already up and registers on its own.
			log.Printf("[pool-manager] transferOwnership (svc=%s org=%s) failed: %v", svcID, org, err)
		} else {
			log.Printf("[pool-manager] transferred svc=%s slug=%s → org=%s", svcID, slug, org)
		}
	} else {
		log.Printf("[pool-manager] no org in caller token — skipping ownership transfer for svc=%s", svcID)
	}

	return c.JSON(claimResponse{
		Status:        "SUCCESS",
		ServiceID:     svcID.String(),
		ServiceSlug:   slug,
		AgentID:       agentID,
		TransferredAt: time.Now(),
	})
}

// status answers brahmi's pre-claim availability probe. Capacity is on demand,
// so the pool always reports available.
func (h *Handler) status(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"status":        "healthy",
		"hotPoolCount":  1,
		"coldPoolCount": 0,
		"pendingCount":  0,
		"hotPoolSize":   1,
		"coldPoolSize":  0,
	})
}

// config has no static per-type config to serve (no svc_configs); 404 is the
// "no config" signal brahmi's poller treats as a no-op.
func (h *Handler) config(c *fiber.Ctx) error {
	return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"status": "ERROR", "message": "no config"})
}

// verifyService reports whether a slug belongs to a container this group
// provisioned, by checking the deployer's slug label.
func (h *Handler) verifyService(c *fiber.Ctx) error {
	slug := c.Params("slug")
	valid := false
	if slug != "" && h.docker != nil {
		f := filters.NewArgs()
		f.Add("label", deploy.LabelSlug+"="+slug)
		list, err := h.docker.ContainerList(c.Context(), container.ListOptions{All: true, Filters: f})
		if err == nil && len(list) > 0 {
			valid = true
		}
	}
	return c.JSON(fiber.Map{"valid": valid, "poolStatus": ""})
}

// resolveOrigin maps the claim's source IP to the calling container and returns
// its origin key: the clode.fork label (forks) or com.docker.compose.service
// (baseline), else "default".
func (h *Handler) resolveOrigin(ctx context.Context, ip string) string {
	if h.docker == nil || ip == "" {
		return "default"
	}
	list, err := h.docker.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		log.Printf("[pool-manager] resolveOrigin: container list failed: %v", err)
		return "default"
	}
	for _, ctr := range list {
		if ctr.NetworkSettings == nil {
			continue
		}
		for _, net := range ctr.NetworkSettings.Networks {
			if net != nil && net.IPAddress == ip {
				if fork := ctr.Labels["clode.fork"]; fork != "" {
					return fork
				}
				if svc := ctr.Labels["com.docker.compose.service"]; svc != "" {
					return svc
				}
			}
		}
	}
	return "default"
}

// waitHealthy polls the deployer's replica view until the service reports one
// Running/ready container, or healthTimeout elapses (best-effort).
func (h *Handler) waitHealthy(ctx context.Context, serviceID string) {
	deadline := time.Now().Add(healthTimeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if reps, err := h.replicas.Replicas(ctx, serviceID); err == nil && replicasReady(reps) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				log.Printf("[pool-manager] waitHealthy: svc=%s not ready within %s (returning anyway)", serviceID, healthTimeout)
				return
			}
		}
	}
}

// replicasReady reports whether at least one replica is Running with ready==total.
func replicasReady(reps []deploy.Replica) bool {
	for _, r := range reps {
		if r.Status != "Running" {
			continue
		}
		parts := strings.SplitN(r.Ready, "/", 2)
		if len(parts) != 2 {
			continue
		}
		ready, err1 := strconv.Atoi(parts[0])
		total, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && ready == total && total > 0 {
			return true
		}
	}
	return false
}
