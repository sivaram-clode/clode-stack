// Package aws implements the EC2-wire-to-docker translation — the `aws` API
// group of the unified deployer. It exposes ServeEC2 (the EC2 query protocol)
// and ServeAdmin (the /_admin control plane) for mounting under the server's
// route groups, and shares its docker client with the narnia + baghira groups.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/client"
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

	// admin holds runtime-mutable knobs pushed via the /_admin/* HTTP API.
	// Today it's just the default image; more knobs (default instance type,
	// per-service_type overrides, etc.) can slot in without changing the
	// endpoint shape — the caller PATCHes only what it wants to set.
	adminMu sync.RWMutex
	admin   adminConfig
}

// adminConfig captures runtime-mutable defaults set via the admin HTTP API.
// Extend with more fields freely; every field is optional.
type adminConfig struct {
	DefaultImage string `json:"default_image,omitempty"`
}

// defaultImage returns the currently-configured default image (empty when
// unset), used by RunInstances to override whatever the caller sent.
func (m *Mock) defaultImage() string {
	m.adminMu.RLock()
	defer m.adminMu.RUnlock()
	return m.admin.DefaultImage
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
		log.Printf("ec2mock: rehydrate: %v (continuing with empty state)", err)
	}
	return m, nil
}

// Close releases the docker client.
func (m *Mock) Close() error { return m.docker.Close() }

// ServeHTTP dispatches EC2 wire calls. Every EC2 API call is POST / with a
// form-urlencoded body carrying Action=<verb>&Version=<v>&<params>. Signatures
// are ignored — the mock trusts callers (localhost only, no auth surface).
//
// The /_admin/* prefix is out-of-band: a mock-only control plane for pushing
// runtime config in (seed.sh uses it to sync the docker image string with
// pool-manager's svc_configs). GET /health is a plain liveness probe used by
// compose healthchecks and the `-healthcheck` self-check subcommand.
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	case strings.HasPrefix(r.URL.Path, "/_admin/"):
		m.ServeAdmin(w, r)
	default:
		m.ServeEC2(w, r)
	}
}

// Docker returns the shared docker client so sibling API groups (narnia,
// baghira) drive the same daemon connection rather than dialing their own.
func (m *Mock) Docker() *client.Client { return m.docker }

// ServeEC2 handles the EC2 query-protocol surface: a POST with a
// form-urlencoded body carrying Action=<verb>&Version=<v>&<params>. It is the
// http.Handler mounted under the `aws` route group (root + /aws) — the SDK
// dials the endpoint root, so behavior is unchanged from a bare EC2 endpoint.
func (m *Mock) ServeEC2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST /", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "MalformedRequest", err.Error())
		return
	}
	req, err := parseQueryRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "MalformedRequest", err.Error())
		return
	}

	// Incoming instance ids (present for Start/Stop/Terminate/Reboot; empty for
	// RunInstances, which mints ids, and for the Describe*/CancelSpot stubs).
	ids := req.listValues("InstanceId")
	idsField := ""
	if len(ids) > 0 {
		idsField = " ids=" + strings.Join(ids, ",")
	}

	// Capture the status code (handlers call writeXML/writeError → WriteHeader)
	// so the completion line reports it.
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	switch req.Action {
	case "RunInstances":
		m.handleRunInstances(rec, req)
	case "DescribeInstances":
		m.handleDescribeInstances(rec, req)
	case "StopInstances":
		m.handleStopInstances(rec, req)
	case "StartInstances":
		m.handleStartInstances(rec, req)
	case "TerminateInstances":
		m.handleTerminateInstances(rec, req)
	case "RebootInstances":
		m.handleRebootInstances(rec, req)
	case "CancelSpotInstanceRequests":
		// brahmi calls this on terminate for spot instances. We don't track
		// spot lifecycle — accept and return an empty success set.
		m.handleCancelSpotRequests(rec, req)
	case "DescribeInstanceAttribute":
		// Sometimes probed by callers checking hibernation eligibility etc.
		m.handleDescribeInstanceAttribute(rec, req)
	case "DescribeSubnets":
		// Stub — return an empty set. brahmi's resolveSubnet path errors
		// "no subnet matched selector" if a selector is set with an empty
		// response, which is the correct signal to the operator: leave
		// AGENT_VM_SUBNET_SELECTOR unset when pointing at the mock.
		m.handleDescribeSubnets(rec, req)
	case "DescribeSecurityGroups":
		// Same as above — brahmi's resolveSecurityGroups gate errors on an
		// empty result set. Callers must leave AGENT_VM_SG_SELECTOR unset.
		m.handleDescribeSecurityGroups(rec, req)
	default:
		writeError(rec, http.StatusNotImplemented, "UnsupportedAction", fmt.Sprintf("action %q not implemented", req.Action))
	}

	// One precise line per action: action, incoming ids, and HTTP status.
	// Lifecycle actions read cleaner without the Action= prefix.
	if isLifecycleAction(req.Action) {
		log.Printf("ec2mock: %s%s status=%d", req.Action, idsField, rec.status)
	} else {
		log.Printf("ec2mock: Action=%s%s status=%d", req.Action, idsField, rec.status)
	}
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

// statusRecorder wraps http.ResponseWriter to capture the status code for the
// completion log line. Defaults to 200 since handlers that only Write (never
// WriteHeader) leave the status implicit, matching net/http.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// serveAdmin routes the mock-only /_admin/* HTTP surface. Not part of the
// EC2 wire protocol — used by the operator (seed.sh in clode-stack) to
// push runtime knobs without a restart.
//
//	PUT /_admin/config/default-image  {"image":"<ref>"}
//	  Sets the docker image RunInstances launches instead of whatever the
//	  caller passed in ImageId / AGENT_IMAGE. Empty string clears the
//	  override.
//	GET /_admin/config/default-image
//	  Symmetric readback of the PUT above. Returns
//	  {"default_image": "<ref>"} with an empty string when unset. Consumed
//	  by clode-stack's cleanup/wipe scripts as the authoritative source of
//	  truth for "which docker image is ec2mock launching right now" — they
//	  use it to sweep every container/volume created for that image.
//	GET /_admin/config
//	  Returns the full adminConfig JSON. Kept as the "everything" view
//	  once more knobs land here.
func (m *Mock) ServeAdmin(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/_admin/config/default-image":
		var body struct {
			Image string `json:"image"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		m.adminMu.Lock()
		m.admin.DefaultImage = strings.TrimSpace(body.Image)
		m.adminMu.Unlock()
		log.Printf("ec2mock: admin: default_image=%q", body.Image)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"default_image": body.Image})
	case r.Method == http.MethodGet && r.URL.Path == "/_admin/config/default-image":
		m.adminMu.RLock()
		img := m.admin.DefaultImage
		m.adminMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"default_image": img})
	case r.Method == http.MethodGet && r.URL.Path == "/_admin/config":
		m.adminMu.RLock()
		out := m.admin
		m.adminMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}
