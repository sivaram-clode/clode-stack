// Package narnia is the deployer-facade API group. It stands in for narnia +
// narnia-workers: jumbo posts a deployment batch here (metadata only), this
// group pulls each deployment's real config back from jumbo, runs/stops the
// container via the shared deploy package, and posts the terminal status
// callback so jumbo moves the deployment out of "accepted". jumbo stays the
// sole book-keeper; this group is the "k8s" that actually places workloads.
package narnia

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/sivaram-clode/mock-services/internal/client/jumbo"
	"github.com/sivaram-clode/mock-services/internal/deploy"
)

// deployTimeout bounds a single background deploy (config pull → docker run →
// status callback). The HTTP request returns 201 immediately; this runs after.
const deployTimeout = 3 * time.Minute

// Deployer is the subset of *deploy.Deployer this group needs.
type Deployer interface {
	Run(ctx context.Context, s deploy.Spec) (string, error)
	Stop(ctx context.Context, serviceID string) error
}

// Handler wires the deployer + jumbo client for the narnia routes.
type Handler struct {
	deploy Deployer
	jumbo  *jumbo.Client
}

// New builds the narnia group handler.
func New(d Deployer, jc *jumbo.Client) *Handler { return &Handler{deploy: d, jumbo: jc} }

// Register mounts the narnia routes on the given (already-prefixed) router.
func (h *Handler) Register(r fiber.Router) {
	r.Post("/internal/deployments/batch", h.batch)
	r.Post("/internal/deletion-jobs", h.delete)
	r.Post("/internal/deletion-jobs/bulk", h.delete)
}

// batchRequest is the metadata-only body jumbo sends (image/env/ports are NOT
// here — they are pulled per deployment from jumbo's config endpoint).
type batchRequest struct {
	BatchID     string `json:"batch_id"`
	Deployments []struct {
		DeploymentID string `json:"deployment_id"`
	} `json:"deployments"`
}

// batch accepts a deployment batch, acks 201 immediately (like narnia), then
// processes each deployment asynchronously.
func (h *Handler) batch(c *fiber.Ctx) error {
	var req batchRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "invalid body: " + err.Error()})
	}
	for _, d := range req.Deployments {
		if d.DeploymentID == "" {
			continue
		}
		go h.process(d.DeploymentID)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"success":      true,
		"batch_id":     req.BatchID,
		"jobs_created": len(req.Deployments),
	})
}

// process pulls the deployment config, runs or stops the container per the
// desired replica count, and posts the terminal status back to jumbo.
func (h *Handler) process(deploymentID string) {
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()

	cfg, err := h.jumbo.GetDeploymentConfig(ctx, deploymentID)
	if err != nil {
		h.fail(ctx, deploymentID, "pull config: "+err.Error())
		return
	}
	st := parseSettings(cfg.Settings)
	replicas := st.replicas()
	env := mergeKVs(cfg.Vars, cfg.Secrets)

	if replicas == 0 {
		if err := h.deploy.Stop(ctx, cfg.ServiceID); err != nil {
			h.fail(ctx, deploymentID, "scale down: "+err.Error())
			return
		}
		log.Printf("[narnia] scaled down %s (service=%s)", cfg.ServiceSlug, cfg.ServiceID)
		h.complete(ctx, deploymentID, cfg.ServiceSlug, 0)
		return
	}

	if st.Image == "" {
		h.fail(ctx, deploymentID, "no image in settings")
		return
	}
	spec := deploy.Spec{
		ServiceID:     cfg.ServiceID,
		Slug:          cfg.ServiceSlug,
		Image:         st.Image,
		Env:           env,
		Privileged:    st.RunAsRoot,
		ContainerPort: st.ContainerPort,
	}
	if _, err := h.deploy.Run(ctx, spec); err != nil {
		h.fail(ctx, deploymentID, "deploy: "+err.Error())
		return
	}
	log.Printf("[narnia] deployed %s (service=%s image=%s port=%d)", cfg.ServiceSlug, cfg.ServiceID, st.Image, st.ContainerPort)
	h.complete(ctx, deploymentID, cfg.ServiceSlug, st.ContainerPort)
}

// complete posts status=completed with the private-URL outputs jumbo persists
// (uppercase keys — that's what jumbo's expected_outputs mapping reads).
func (h *Handler) complete(ctx context.Context, deploymentID, slug string, port int) {
	outputs := map[string]any{"status": "completed"}
	if port > 0 {
		host := fmt.Sprintf("%s.clode.internal", slug)
		outputs["PRIVATE_URL"] = fmt.Sprintf("http://%s:%d", host, port)
		outputs["PRIVATE_HOST"] = host
		outputs["PRIVATE_PORT"] = port
	}
	if err := h.jumbo.PutDeploymentStatus(ctx, deploymentID, map[string]any{
		"status":  "completed",
		"outputs": outputs,
	}); err != nil {
		log.Printf("[narnia] status callback (completed) %s failed: %v", deploymentID, err)
	}
}

// fail posts status=failed with the reason, and logs it locally.
func (h *Handler) fail(ctx context.Context, deploymentID, reason string) {
	log.Printf("[narnia] deployment %s failed: %s", deploymentID, reason)
	if err := h.jumbo.PutDeploymentStatus(ctx, deploymentID, map[string]any{
		"status":        "failed",
		"error_message": reason,
		"outputs":       map[string]any{"status": "failed", "failure_reason": reason},
	}); err != nil {
		log.Printf("[narnia] status callback (failed) %s failed: %v", deploymentID, err)
	}
}

// deleteRequest covers the single, bulk-array, and batch shapes jumbo's
// deletion client may send; every non-empty service id found is stopped.
type deleteRequest struct {
	ServiceID   string   `json:"service_id"`
	ServiceIDs  []string `json:"service_ids"`
	Deployments []struct {
		ServiceID string `json:"service_id"`
	} `json:"deployments"`
}

// delete stops (removes) the containers for the requested service ids.
func (h *Handler) delete(c *fiber.Ctx) error {
	var req deleteRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "message": "invalid body: " + err.Error()})
	}
	ids := map[string]struct{}{}
	if req.ServiceID != "" {
		ids[req.ServiceID] = struct{}{}
	}
	for _, id := range req.ServiceIDs {
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	for _, d := range req.Deployments {
		if d.ServiceID != "" {
			ids[d.ServiceID] = struct{}{}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	for id := range ids {
		if err := h.deploy.Stop(ctx, id); err != nil {
			log.Printf("[narnia] delete service %s: %v", id, err)
		} else {
			log.Printf("[narnia] deleted service %s", id)
		}
	}
	return c.Status(fiber.StatusOK).JSON(fiber.Map{"success": true, "deleted": len(ids)})
}

// settings is the minimal slice of the service settings JSON we act on:
// image, container port, root flag, and the replica count under regions[0].
type settings struct {
	Image         string `json:"image"`
	ContainerPort int    `json:"containerPort"`
	RunAsRoot     bool   `json:"runAsRoot"`
	Regions       []struct {
		Replicas int `json:"replicas"`
	} `json:"regions"`
}

// replicas returns the desired replica count (regions[0].replicas), defaulting
// to 1 when no region block is present (a bare deploy = run it).
func (s settings) replicas() int {
	if len(s.Regions) == 0 {
		return 1
	}
	return s.Regions[0].Replicas
}

// parseSettings extracts the settings we need; unknown/malformed JSON yields a
// zero-value settings (image empty → the caller fails the deploy cleanly).
func parseSettings(raw json.RawMessage) settings {
	var s settings
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// mergeKVs turns the vars and secrets JSON (both passed straight through to the
// container env) into a K=V map, secrets last so they win on a key clash.
func mergeKVs(vars, secrets json.RawMessage) map[string]string {
	out := map[string]string{}
	for k, v := range parseKVs(vars) {
		out[k] = v
	}
	for k, v := range parseKVs(secrets) {
		out[k] = v
	}
	return out
}

// parseKVs accepts either the object form {"K":"V"} or the array form
// [{"key":"K","value":"V"}] and returns a flat string map. Values are
// stringified as-is; no schema modeling.
func parseKVs(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		for k, v := range obj {
			out[k] = stringify(v)
		}
		return out
	}
	var arr []struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, kv := range arr {
			if kv.Key != "" {
				out[kv.Key] = stringify(kv.Value)
			}
		}
	}
	return out
}

// stringify renders a JSON scalar as an env value (strings verbatim).
func stringify(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
