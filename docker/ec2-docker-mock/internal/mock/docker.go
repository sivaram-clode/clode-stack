package mock

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// containerLabelInstanceID is the label carrying our fake EC2 instance id on
// every launched container. The image-id / instance-type / volume-name
// labels are load-bearing for boot rehydration — Rehydrate() reconstructs
// an InstanceRecord from them so the mock survives its own restart without
// orphaning the containers it launched.
const (
	containerLabelInstanceID   = "aws.mock.instance-id"
	containerLabelSpot         = "aws.mock.lifecycle-spot"
	containerLabelHibernated   = "aws.mock.hibernated"
	containerLabelImageID      = "aws.mock.image-id"
	containerLabelInstanceType = "aws.mock.instance-type"
	containerLabelVolumeName   = "aws.mock.volume-name"
	containerLabelLaunchUnix   = "aws.mock.launch-unix"
	tagLabelPrefix             = "aws.tag."
	labelValueTrue             = "true"
)

// runContainerParams bundles the RunInstances-mapped-to-docker inputs.
type runContainerParams struct {
	instanceID         string
	image              string
	imageID            string // AMI id echoed back by DescribeInstances
	instanceType       string // returned as-is; persisted as a label for Rehydrate
	envVars            map[string]string
	tags               map[string]string
	volumeName         string
	entrypointOverride string
	networkName        string
	spot               bool
	launchTime         time.Time
}

// pullIfMissing honors the configured PullPolicy:
//   - Never          → skip pull entirely; ContainerCreate fails cleanly if
//                      the image isn't on the daemon (matches k8s Never).
//   - IfNotPresent   → inspect first; pull only when absent locally.
//   - Always         → pull every time even if already present.
//
// Pull output is drained (buffered pulls fail progress reporting otherwise).
func (m *Mock) pullIfMissing(ctx context.Context, ref string) error {
	switch m.cfg.PullPolicy {
	case PullNever:
		return nil
	case PullIfNotPresent, "":
		if _, err := m.docker.ImageInspect(ctx, ref); err == nil {
			return nil
		}
	case PullAlways:
		// fall through to pull
	default:
		return fmt.Errorf("unknown pull_policy %q (want %s|%s|%s)",
			m.cfg.PullPolicy, PullIfNotPresent, PullAlways, PullNever)
	}
	rc, err := m.docker.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("drain pull: %w", err)
	}
	return nil
}

// ensureVolume creates the docker volume backing $BENJI_HOME if it doesn't
// exist. Named volumes survive `docker rm` unless the caller passes RemoveVolumes.
func (m *Mock) ensureVolume(ctx context.Context, name string) error {
	_, err := m.docker.VolumeInspect(ctx, name)
	if err == nil {
		return nil
	}
	_, err = m.docker.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Driver: "local",
		Labels: map[string]string{"aws.mock.owned": labelValueTrue},
	})
	return err
}

// runContainer creates and starts a container to back an EC2 instance.
// benji-vm mounts $BENJI_HOME at /home/node/.benji — the named volume preserves
// it across `docker stop` / `docker start` cycles, matching EBS persistence
// semantics.
func (m *Mock) runContainer(ctx context.Context, p runContainerParams) (string, error) {
	if err := m.pullIfMissing(ctx, p.image); err != nil {
		return "", err
	}
	if err := m.ensureVolume(ctx, p.volumeName); err != nil {
		return "", err
	}

	envSlice := make([]string, 0, len(p.envVars))
	for k, v := range p.envVars {
		envSlice = append(envSlice, k+"="+v)
	}

	labels := map[string]string{
		containerLabelInstanceID:   p.instanceID,
		containerLabelImageID:      p.imageID,
		containerLabelInstanceType: p.instanceType,
		containerLabelVolumeName:   p.volumeName,
		containerLabelLaunchUnix:   fmt.Sprintf("%d", p.launchTime.Unix()),
	}
	if p.spot {
		labels[containerLabelSpot] = labelValueTrue
	}
	for k, v := range p.tags {
		labels[tagLabelPrefix+k] = v
	}

	cfg := &container.Config{
		Image:  p.image,
		Env:    envSlice,
		Labels: labels,
		// Start as root regardless of the image's USER: the agent entrypoint
		// chowns the fresh state volume then gosu-drops to the service user —
		// the same contract the production deployer's runAsRoot setting
		// provides. Without this, images that default to a non-root USER hit
		// "tar: Cannot mkdir: Permission denied" on first-boot seeding.
		User: "0",
	}
	if p.entrypointOverride != "" {
		cfg.Entrypoint = []string{p.entrypointOverride}
	}

	hostCfg := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: p.volumeName,
				Target: "/home/node/.benji",
			},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		// A real VM instance owns a full OS: its agent image may run its own
		// dockerd (docker-in-docker), mount API filesystems, etc. Privileged
		// is the container-level equivalent of that contract.
		Privileged: true,
	}

	var netCfg *network.NetworkingConfig
	if p.networkName != "" && p.networkName != "bridge" {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				p.networkName: {},
			},
		}
	}

	resp, err := m.docker.ContainerCreate(ctx, cfg, hostCfg, netCfg, &specs.Platform{OS: "linux"}, p.instanceID)
	if err != nil {
		return "", fmt.Errorf("container create: %w", err)
	}
	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("container start: %w", err)
	}
	return resp.ID, nil
}

// containerState translates docker's status string to an EC2 InstanceState code.
// Hibernated is a mock-specific overlay tracked via label because docker's
// paused state is transient in the API response.
func (m *Mock) containerState(ctx context.Context, containerID string, hibernated bool) (InstanceState, error) {
	info, err := m.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		if isNoSuchContainer(err) {
			return stateFromCode(stateCodeTerminated), nil
		}
		return InstanceState{}, err
	}
	if hibernated {
		return stateFromCode(stateCodeStopped), nil
	}
	switch info.State.Status {
	case "created":
		return stateFromCode(stateCodePending), nil
	case "running":
		return stateFromCode(stateCodeRunning), nil
	case "paused":
		return stateFromCode(stateCodeStopped), nil
	case "restarting":
		return stateFromCode(stateCodePending), nil
	case "exited", "dead":
		return stateFromCode(stateCodeStopped), nil
	case "removing":
		return stateFromCode(stateCodeShuttingDown), nil
	default:
		return stateFromCode(stateCodeStopped), nil
	}
}

// containerIP returns the container name in place of an IPv4 — docker's
// embedded DNS resolves it on the shared user-defined network.
func (m *Mock) containerIP(ctx context.Context, containerID string) string {
	info, err := m.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(info.Name, "/")
}

// stopContainer maps to `docker stop`. Grace period matches EC2 default (~30s).
func (m *Mock) stopContainer(ctx context.Context, containerID string) error {
	grace := 30
	return m.docker.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &grace})
}

// pauseContainer maps to `docker pause`. This is the closest docker analogue of
// EC2 hibernate: the container's cgroup freezer holds every process mid-run,
// so `unpauseContainer` resumes them in-place (RAM state intact, TCP sockets
// still open to whatever peer they had — subject to the peer's own timeouts).
func (m *Mock) pauseContainer(ctx context.Context, containerID string) error {
	return m.docker.ContainerPause(ctx, containerID)
}

func (m *Mock) unpauseContainer(ctx context.Context, containerID string) error {
	return m.docker.ContainerUnpause(ctx, containerID)
}

// startContainer maps to `docker start`.
func (m *Mock) startContainer(ctx context.Context, containerID string) error {
	return m.docker.ContainerStart(ctx, containerID, container.StartOptions{})
}

// rehydrate scans the docker daemon for containers labeled with our
// instance-id and rebuilds InstanceRecord entries in state. Called from
// New() so a mock restart doesn't orphan the containers it previously
// launched — Describe/Stop/Terminate keep working against them.
//
// Rehydration is label-driven: we persisted the load-bearing fields
// (image id, instance type, volume name, launch time, tags) as
// `aws.mock.*` labels on the container, so we can reconstruct the record
// without any external state. Fields we cannot recover (e.g. spot-request
// linkage that never existed) stay at their zero values.
func (m *Mock) rehydrate(ctx context.Context) error {
	containers, err := m.docker.ContainerList(ctx, container.ListOptions{
		All: true, // include stopped/paused/exited — we still track those
	})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	recovered := 0
	for _, c := range containers {
		iid := c.Labels[containerLabelInstanceID]
		if iid == "" {
			continue
		}
		tags := map[string]string{}
		for k, v := range c.Labels {
			if suffix, ok := strings.CutPrefix(k, tagLabelPrefix); ok {
				tags[suffix] = v
			}
		}
		launchTime := time.Now().UTC()
		if s := c.Labels[containerLabelLaunchUnix]; s != "" {
			var unix int64
			for _, r := range s {
				if r < '0' || r > '9' {
					unix = 0
					break
				}
				unix = unix*10 + int64(r-'0')
			}
			if unix > 0 {
				launchTime = time.Unix(unix, 0).UTC()
			}
		}
		m.state.Put(&InstanceRecord{
			InstanceID:   iid,
			ContainerID:  c.ID,
			ImageID:      c.Labels[containerLabelImageID],
			InstanceType: c.Labels[containerLabelInstanceType],
			Tags:         tags,
			LaunchTime:   launchTime,
			Spot:         c.Labels[containerLabelSpot] == labelValueTrue,
			Hibernated:   c.State == "paused",
			VolumeName:   c.Labels[containerLabelVolumeName],
		})
		recovered++
	}
	if recovered > 0 {
		fmt.Printf("ec2mock: rehydrated %d instance record(s) from container labels\n", recovered)
	}
	return nil
}

// removeContainer maps to `docker rm -f`. The named volume is left alone by
// default so that a subsequent RunInstances with the same instance id (or
// tests inspecting durability) can pick it back up. Callers who want a fully
// clean slate should pass removeVolume=true.
func (m *Mock) removeContainer(ctx context.Context, containerID, volumeName string, removeVolume bool) error {
	err := m.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true, RemoveVolumes: false})
	if err != nil && !isNoSuchContainer(err) {
		return err
	}
	if removeVolume && volumeName != "" {
		if err := m.docker.VolumeRemove(ctx, volumeName, true); err != nil && !isNoSuchVolume(err) {
			return err
		}
	}
	return nil
}

// isNoSuchContainer detects the "no such container" error from the docker
// client, whose message is stable but whose error type isn't exported.
func isNoSuchContainer(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such container")
}

func isNoSuchVolume(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such volume") || strings.Contains(msg, "not found")
}

