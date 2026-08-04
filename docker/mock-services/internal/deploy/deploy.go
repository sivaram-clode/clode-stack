// Package deploy is the shared docker service-deployer used by the narnia
// (deploy/delete) and baghira (pod status) API groups. It runs one container
// per service, keyed by the jumbo service id (a docker label) and named after
// the service slug, and gives each container the k8s-style DNS aliases that
// in-cluster consumers dial. It deliberately holds no in-memory state — docker
// itself is the registry, so a mock restart loses nothing.
package deploy

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Labels stamped on every deployed service container. LabelServiceID is the
// baghira lookup key (jumbo service uuid); LabelSlug is the human/DNS name;
// LabelDeployed marks the container as one this deployer owns so the
// clode-stack agent-sweep can reclaim it. These are intentionally distinct
// from the aws group's `aws.mock.instance-id` label so the EC2 rehydrate scan
// never mistakes a deployed service for an EC2 instance.
const (
	LabelServiceID = "aws.mock.service-id"
	LabelSlug      = "aws.mock.slug"
	LabelDeployed  = "aws.mock.deployed-service"
	labelTrue      = "true"
)

// Pull-policy string constants matching Kubernetes' imagePullPolicy names.
const (
	PullIfNotPresent = "IfNotPresent"
	PullAlways       = "Always"
	PullNever        = "Never"
)

// Deployer runs service containers on a shared docker daemon + network.
type Deployer struct {
	docker  *client.Client
	network string
	pull    string
}

// New returns a Deployer bound to a docker client, target network, and pull
// policy (all shared with the aws group in the unified process).
func New(dc *client.Client, network, pullPolicy string) *Deployer {
	if pullPolicy == "" {
		pullPolicy = PullIfNotPresent
	}
	return &Deployer{docker: dc, network: network, pull: pullPolicy}
}

// Spec is the resolved input for a single service deployment.
type Spec struct {
	ServiceID     string
	Slug          string
	Image         string
	Env           map[string]string
	Privileged    bool
	ContainerPort int
}

// Replica is the normalized status of one running (or not) service container,
// consumed by the baghira group to build its k8s-style replica envelope.
type Replica struct {
	Name       string
	Namespace  string
	Status     string
	Ready      string
	Restarts   int
	Age        string
	Containers []string
}

// Run reconciles the desired state for a service to "one running container":
// any existing container for the same service id (or slug name) is removed,
// then a fresh one is created and started with the k8s DNS aliases and labels.
// Returns the new container id.
func (d *Deployer) Run(ctx context.Context, s Spec) (string, error) {
	if err := d.pullIfMissing(ctx, s.Image); err != nil {
		return "", err
	}
	// Reconcile: drop any prior container for this service (redeploy / retag)
	// and any stale container squatting the slug name.
	if err := d.removeByServiceID(ctx, s.ServiceID); err != nil {
		return "", err
	}
	_ = d.docker.ContainerRemove(ctx, s.Slug, container.RemoveOptions{Force: true})

	env := make([]string, 0, len(s.Env))
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}

	cfg := &container.Config{
		Image: s.Image,
		Env:   env,
		Labels: map[string]string{
			LabelServiceID: s.ServiceID,
			LabelSlug:      s.Slug,
			LabelDeployed:  labelTrue,
		},
		// Start as root regardless of the image USER: matches the production
		// deployer's runAsRoot contract (entrypoints that chown a state dir
		// then drop privileges need it).
		User: "0",
	}
	hostCfg := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		Privileged:    s.Privileged,
	}

	// k8s-derivable DNS names in-cluster consumers dial. ikki's aramb provider
	// probes `{slug}-backend-main.{slug}.svc`; jumbo stores the private host as
	// `{slug}.clode.internal`. docker's embedded DNS resolves both dotted
	// aliases on the shared user-defined network to this container.
	var netCfg *network.NetworkingConfig
	if d.network != "" && d.network != "bridge" {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				d.network: {Aliases: dnsAliases(s.Slug)},
			},
		}
	}

	resp, err := d.docker.ContainerCreate(ctx, cfg, hostCfg, netCfg, &specs.Platform{OS: "linux"}, s.Slug)
	if err != nil {
		return "", fmt.Errorf("container create %s: %w", s.Slug, err)
	}
	if err := d.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("container start %s: %w", s.Slug, err)
	}
	return resp.ID, nil
}

// Stop removes every container for a service id (scale-to-zero / deletion).
// It is a no-op when nothing matches.
func (d *Deployer) Stop(ctx context.Context, serviceID string) error {
	return d.removeByServiceID(ctx, serviceID)
}

// Replicas returns the status of every container for a service id (zero or one
// in practice). Empty slice means "not deployed" — the baghira group maps that
// to an empty data array (unhealthy).
func (d *Deployer) Replicas(ctx context.Context, serviceID string) ([]Replica, error) {
	list, err := d.listByServiceID(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	reps := make([]Replica, 0, len(list))
	for _, c := range list {
		info, err := d.docker.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}
		running := info.State != nil && info.State.Running
		status := "Pending"
		ready := "0/1"
		if running {
			status = "Running"
			ready = "1/1"
		} else if info.State != nil && (info.State.Status == "exited" || info.State.Status == "dead") {
			status = "Stopped"
		}
		reps = append(reps, Replica{
			Name:       strings.TrimPrefix(info.Name, "/"),
			Namespace:  info.Config.Labels[LabelSlug],
			Status:     status,
			Ready:      ready,
			Restarts:   info.RestartCount,
			Age:        age(info.State),
			Containers: []string{strings.TrimPrefix(info.Name, "/")},
		})
	}
	return reps, nil
}

// dnsAliases builds the in-cluster DNS names for a service slug.
func dnsAliases(slug string) []string {
	return []string{
		fmt.Sprintf("%s-backend-main.%s.svc", slug, slug),
		fmt.Sprintf("%s.clode.internal", slug),
	}
}

// removeByServiceID force-removes every container carrying the service-id label.
func (d *Deployer) removeByServiceID(ctx context.Context, serviceID string) error {
	list, err := d.listByServiceID(ctx, serviceID)
	if err != nil {
		return err
	}
	for _, c := range list {
		if err := d.docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("remove %s: %w", c.ID, err)
		}
	}
	return nil
}

// listByServiceID lists all (running or not) containers for a service id.
func (d *Deployer) listByServiceID(ctx context.Context, serviceID string) ([]container.Summary, error) {
	f := filters.NewArgs()
	f.Add("label", LabelServiceID+"="+serviceID)
	return d.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
}

// pullIfMissing honors the pull policy (Never = never dial a registry, the
// sensible default for locally-built clode-stack/* images).
func (d *Deployer) pullIfMissing(ctx context.Context, ref string) error {
	switch d.pull {
	case PullNever:
		return nil
	case PullIfNotPresent, "":
		if _, err := d.docker.ImageInspect(ctx, ref); err == nil {
			return nil
		}
	case PullAlways:
		// fall through
	default:
		return fmt.Errorf("unknown pull policy %q", d.pull)
	}
	rc, err := d.docker.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// age renders a coarse human duration since the container started, matching
// baghira's "5m"-style field. Best-effort — empty on any parse failure.
func age(state *container.State) string {
	if state == nil || state.StartedAt == "" {
		return ""
	}
	started, err := time.Parse(time.RFC3339Nano, state.StartedAt)
	if err != nil {
		return ""
	}
	d := time.Since(started)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
