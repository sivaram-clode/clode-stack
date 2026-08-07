// Package aws implements the EC2-wire-to-docker translation — the `aws` API
// group of the unified deployer. It exposes ServeEC2 (the EC2 query protocol)
// for mounting under the server's route groups, and shares its docker client
// with the narnia + baghira groups. RunInstances launches the image the caller
// asks for (user-data AGENT_IMAGE, else ImageId) — there is no server-side
// default-image override.
package aws

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/client"
	"github.com/gofiber/fiber/v2"
)

// Config is the mock server configuration.
type Config struct {
	// DockerSocket is the docker daemon socket path. Empty defers to DOCKER_HOST
	// / DOCKER_API_VERSION env vars, then /var/run/docker.sock.
	DockerSocket string
	// Network is the docker network launched containers attach to. Defaults to
	// the daemon default ("bridge") if empty.
	Network string
	// EntrypointOverride, if set, replaces the image's ENTRYPOINT on every
	// RunInstances launch. Useful for images whose default entrypoint expects
	// systemd / cloud-init to be present — a light shim script that reads
	// /etc/clode-agent/agent.env and execs the intended binary works fine.
	EntrypointOverride string
	// PullPolicy mirrors Kubernetes' imagePullPolicy for RunInstances launches.
	// One of "IfNotPresent" (default — pull only if absent locally), "Always"
	// (pull every time), or "Never" (never pull; use whatever's on the daemon).
	// Never is the sensible default for stacks that ship images under a
	// local-only namespace like "clode-stack/…", where a pull would hit
	// Docker Hub and 401.
	PullPolicy string
}

// Pull-policy string constants matching Kubernetes' imagePullPolicy names.
const (
	PullIfNotPresent = "IfNotPresent"
	PullAlways       = "Always"
	PullNever        = "Never"
)

// Mock is the EC2-wire-to-docker translator.
type Mock struct {
	docker *client.Client
	state  *State
	cfg    Config
}

// New wires the docker client and state. Returns an error if the docker daemon
// isn't reachable — catch it early rather than fail on first RunInstances.
func New(cfg Config) (*Mock, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if cfg.DockerSocket != "" {
		opts = append(opts, client.WithHost("unix://"+cfg.DockerSocket))
	} else {
		opts = append(opts, client.FromEnv)
	}
	dc, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	// Ping so misconfigured sockets fail here, not on first RunInstances.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dc.Ping(pingCtx); err != nil {
		return nil, fmt.Errorf("docker ping: %w", err)
	}
	if cfg.Network == "" {
		cfg.Network = "bridge"
	}
	if cfg.PullPolicy == "" {
		cfg.PullPolicy = PullIfNotPresent
	}
	m := &Mock{docker: dc, state: NewState(), cfg: cfg}
	// Rehydrate in-memory state from container labels so a mock restart
	// doesn't orphan the containers it previously launched. Best-effort —
	// a failure here logs and continues (fresh state is still usable, the
	// user just loses the ability to Describe/Stop/Terminate pre-existing
	// instances via the mock).
	if err := m.rehydrate(context.Background()); err != nil {
		log.Printf("aws: rehydrate: %v (continuing with empty state)", err)
	}
	return m, nil
}

// Close releases the docker client.
func (m *Mock) Close() error { return m.docker.Close() }

// Register mounts the aws group's routes on r: the EC2 query protocol at the
// endpoint root (POST / and POST /aws — the SDK dials the root). The /health
// liveness probe is owned by the server (internal/server), shared across every
// group.
func (m *Mock) Register(r fiber.Router) {
	r.Post("/", m.ServeEC2)
	r.Post("/aws", m.ServeEC2)
}

// Docker returns the shared docker client so sibling API groups (narnia,
// baghira) drive the same daemon connection rather than dialing their own.
func (m *Mock) Docker() *client.Client { return m.docker }

// ServeEC2 handles the EC2 query-protocol surface: a POST with a
// form-urlencoded body carrying Action=<verb>&Version=<v>&<params>. Mounted at
// the endpoint root (+ /aws) so the SDK, which dials the root, reaches it —
// behavior unchanged from a bare EC2 endpoint. Signatures are ignored (the mock
// trusts callers; localhost only, no auth surface).
func (m *Mock) ServeEC2(c *fiber.Ctx) error {
	req, err := parseQueryRequest(c.Body())
	if err != nil {
		return writeError(c, fiber.StatusBadRequest, "MalformedRequest", err.Error())
	}

	// Incoming instance ids (present for Start/Stop/Terminate/Reboot; empty for
	// RunInstances, which mints ids, and for the Describe*/CancelSpot stubs).
	ids := req.listValues("InstanceId")
	idsField := ""
	if len(ids) > 0 {
		idsField = " ids=" + strings.Join(ids, ",")
	}

	var handlerErr error
	switch req.Action {
	case "RunInstances":
		handlerErr = m.handleRunInstances(c, req)
	case "DescribeInstances":
		handlerErr = m.handleDescribeInstances(c, req)
	case "StopInstances":
		handlerErr = m.handleStopInstances(c, req)
	case "StartInstances":
		handlerErr = m.handleStartInstances(c, req)
	case "TerminateInstances":
		handlerErr = m.handleTerminateInstances(c, req)
	case "RebootInstances":
		handlerErr = m.handleRebootInstances(c, req)
	case "CancelSpotInstanceRequests":
		// brahmi calls this on terminate for spot instances. We don't track
		// spot lifecycle — accept and return an empty success set.
		handlerErr = m.handleCancelSpotRequests(c, req)
	case "DescribeInstanceAttribute":
		// Sometimes probed by callers checking hibernation eligibility etc.
		handlerErr = m.handleDescribeInstanceAttribute(c, req)
	case "DescribeSubnets":
		// Stub — return an empty set. brahmi's resolveSubnet path errors
		// "no subnet matched selector" if a selector is set with an empty
		// response, which is the correct signal to the operator: leave
		// AGENT_VM_SUBNET_SELECTOR unset when pointing at the mock.
		handlerErr = m.handleDescribeSubnets(c, req)
	case "DescribeSecurityGroups":
		// Same as above — brahmi's resolveSecurityGroups gate errors on an
		// empty result set. Callers must leave AGENT_VM_SG_SELECTOR unset.
		handlerErr = m.handleDescribeSecurityGroups(c, req)
	default:
		handlerErr = writeError(c, fiber.StatusNotImplemented, "UnsupportedAction", fmt.Sprintf("action %q not implemented", req.Action))
	}

	// One precise line per action: action, incoming ids, and HTTP status.
	// Lifecycle actions read cleaner without the Action= prefix.
	if isLifecycleAction(req.Action) {
		log.Printf("aws: %s%s status=%d", req.Action, idsField, c.Response().StatusCode())
	} else {
		log.Printf("aws: Action=%s%s status=%d", req.Action, idsField, c.Response().StatusCode())
	}
	return handlerErr
}

// isLifecycleAction reports whether an action mutates container lifecycle,
// selecting the cleaner log wording for those events.
func isLifecycleAction(action string) bool {
	switch action {
	case "RunInstances", "StartInstances", "StopInstances", "TerminateInstances", "RebootInstances":
		return true
	}
	return false
}
