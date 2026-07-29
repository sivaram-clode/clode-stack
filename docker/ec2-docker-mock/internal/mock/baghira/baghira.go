// Package baghira is the pod-status API group. It stands in for baghira's
// GET /api/v1/replicas: pool-manager (cloud/jumbo-deployer mode) polls it to
// learn whether a deployed service is healthy, and promotes a warm agent to a
// claimable slot when it is. The lookup is a single docker label query — the
// jumbo service id maps straight to the deployed container via aws.mock.service-id.
package baghira

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/sivaram-clode/ec2-docker-mock/internal/deploy"
)

// Replicator is the subset of *deploy.Deployer this group needs.
type Replicator interface {
	Replicas(ctx context.Context, serviceID string) ([]deploy.Replica, error)
}

// Handler wires the deployer for the baghira routes.
type Handler struct{ deploy Replicator }

// New builds the baghira group handler.
func New(d Replicator) *Handler { return &Handler{deploy: d} }

// Register mounts the baghira routes on the given (already-prefixed) router.
func (h *Handler) Register(r fiber.Router) {
	r.Get("/api/v1/replicas", h.replicas)
}

// replicaInfo mirrors baghira's ReplicaInfo wire shape. pool-manager reads
// status + ready (per replica) and the top-level status + data length.
type replicaInfo struct {
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	Status     string   `json:"status"`
	Ready      string   `json:"ready"`
	Restarts   int      `json:"restarts"`
	Age        string   `json:"age"`
	Containers []string `json:"containers"`
}

// replicas serves GET /api/v1/replicas?serviceIdentifier=<uuid>&idType=id.
// A missing/undeployed service returns SUCCESS with an empty data array, which
// pool-manager reads as "not healthy yet" — the same signal an empty k8s
// namespace produces in the real baghira.
func (h *Handler) replicas(c *fiber.Ctx) error {
	serviceID := c.Query("serviceIdentifier")
	if serviceID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "serviceIdentifier is required"})
	}
	reps, err := h.deploy.Replicas(c.Context(), serviceID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	data := make([]replicaInfo, 0, len(reps))
	for _, r := range reps {
		data = append(data, replicaInfo{
			Name:       r.Name,
			Namespace:  r.Namespace,
			Status:     r.Status,
			Ready:      r.Ready,
			Restarts:   r.Restarts,
			Age:        r.Age,
			Containers: r.Containers,
		})
	}
	return c.JSON(fiber.Map{"status": "SUCCESS", "data": data})
}
