package aws

import (
	"context"
	"log"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// availabilityZone is a mock-static AZ returned inside <placement/>. Callers
// that key on the AZ (rare in brahmi) get a stable value.
const availabilityZone = "mock-1a"

// imdsBaseForInstance builds the per-instance IMDS_BASE_URL kairo dials for its
// instance-identity document. The base host defaults to the mock-services name
// on the clode bridge; MOCK_IMDS_BASE overrides it. kairo appends
// /latest/... so the value is the imds group prefix plus the instance id.
func imdsBaseForInstance(instanceID string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("MOCK_IMDS_BASE")), "/")
	if base == "" {
		base = "http://mock-services:8080/imds"
	}
	return base + "/" + instanceID
}

// handleRunInstances launches one docker container per requested instance.
// EC2 RunInstances lets you ask for MinCount..MaxCount instances in a single
// call; brahmi always asks for exactly one, but we honor MaxCount.
func (m *Mock) handleRunInstances(c *fiber.Ctx, req *QueryRequest) error {
	ctx := context.Background()
	imageID := req.get("ImageId")
	if imageID == "" {
		return writeError(c, fiber.StatusBadRequest, "InvalidParameterValue", "ImageId is required")
	}
	instanceType := req.get("InstanceType")
	if instanceType == "" {
		instanceType = "t3.small" // arbitrary default; brahmi always sets this
	}

	// MaxCount / MinCount: default to 1. If MaxCount is set we launch that
	// many; brahmi never asks for >1 so this branch is rarely exercised.
	maxCount := 1
	if v := req.get("MaxCount"); v != "" {
		if n := parseIntSafe(v); n > 0 {
			maxCount = n
		}
	}

	// Deploy the INCOMING image — the one the caller asked for, no server-side
	// substitution. brahmi's aramb-vm path bakes its AGENT_VM_IMAGE into
	// cloud-init user-data as AGENT_IMAGE (matching the real AMI-bootstrap
	// contract), so that is what launches. Falls back to the requested ImageId
	// only when user-data carried none (standalone testing wiring ImageId to a
	// docker ref). There is no admin default_image: the mock never launches an
	// image the caller didn't specify.
	env := parseUserData(req.get("UserData"))
	imageToRun := strings.TrimSpace(env["AGENT_IMAGE"])
	if imageToRun == "" {
		imageToRun = strings.TrimSpace(imageID)
	}
	if imageToRun == "" {
		return writeError(c, fiber.StatusBadRequest, "InvalidParameterValue",
			"no image to launch: user-data AGENT_IMAGE and ImageId are both empty")
	}

	// Merge caller env into a full set exported to the container.
	// AGENT_IMAGE isn't re-exported (the container is already that image).
	envForContainer := map[string]string{}
	for k, v := range env {
		if k == "AGENT_IMAGE" {
			continue
		}
		envForContainer[k] = v
	}

	tags := req.tagSpecifications()

	// SpotInstanceType / InstanceMarketOptions.MarketType is how brahmi opts
	// into spot. We track that on the record so DescribeInstances reports
	// instanceLifecycle=spot, but nothing behavioural changes — spot semantics
	// don't map cleanly to docker.
	spot := strings.EqualFold(req.get("InstanceMarketOptions.MarketType"), "spot")

	instances := make([]Instance, 0, maxCount)
	for i := 0; i < maxCount; i++ {
		instanceID := newInstanceID()
		volumeName := newVolumeName(instanceID)
		launchTime := time.Now().UTC()

		// Per-instance env: point kairo's IMDS_BASE_URL at this mock's imds group
		// under a per-instance path, so the signed instance-identity document it
		// fetches names THIS instance (the id brahmi stamped from the RunInstances
		// response and binds the call-home against). The real link-local IMDS is
		// unreachable from a docker container; this is the local stand-in.
		perInstanceEnv := make(map[string]string, len(envForContainer)+1)
		maps.Copy(perInstanceEnv, envForContainer)
		perInstanceEnv["IMDS_BASE_URL"] = imdsBaseForInstance(instanceID)

		cid, err := m.runContainer(ctx, runContainerParams{
			instanceID:         instanceID,
			image:              imageToRun,
			imageID:            imageID,
			instanceType:       instanceType,
			envVars:            perInstanceEnv,
			tags:               tags,
			volumeName:         volumeName,
			entrypointOverride: m.cfg.EntrypointOverride,
			networkName:        m.cfg.Network,
			spot:               spot,
			launchTime:         launchTime,
		})
		if err != nil {
			log.Printf("aws: RunInstances: %v", err)
			return writeError(c, fiber.StatusInternalServerError, "InternalError", err.Error())
		}
		log.Printf("aws: launched %s image=%s", instanceID, imageToRun)
		record := &InstanceRecord{
			InstanceID:   instanceID,
			ContainerID:  cid,
			ImageID:      imageID,
			InstanceType: instanceType,
			Tags:         tags,
			LaunchTime:   launchTime,
			Spot:         spot,
			VolumeName:   volumeName,
		}
		m.state.Put(record)
		instances = append(instances, m.recordToInstance(ctx, record))
	}

	return writeXML(c, fiber.StatusOK, RunInstancesResponse{
		XMLNS:         ec2Namespace,
		RequestID:     newRequestID(),
		ReservationID: newReservationID(),
		OwnerID:       "000000000000",
		Instances:     instances,
	})
}

// handleDescribeInstances honors both explicit InstanceId.N filters and
// generic Filter.N.{Name,Value.M} filters. brahmi's list-and-filter path
// (`vm_reaper.go`) uses `tag:env` and `instance-state-name`.
func (m *Mock) handleDescribeInstances(c *fiber.Ctx, req *QueryRequest) error {
	ctx := context.Background()

	// Explicit instance ids narrow the candidate set BEFORE filters.
	explicitIDs := req.listValues("InstanceId")
	var candidates []*InstanceRecord
	if len(explicitIDs) > 0 {
		for _, id := range explicitIDs {
			if r, ok := m.state.Get(id); ok {
				candidates = append(candidates, r)
			}
		}
	} else {
		candidates = m.state.Filter(req.describeFilters())
	}

	// Post-filter on live docker state (instance-state-name). AWS supports
	// values like "running", "stopped", "terminated", etc.
	filters := req.describeFilters()
	stateFilter := filters["instance-state-name"]

	instances := make([]Instance, 0, len(candidates))
	for _, r := range candidates {
		inst := m.recordToInstance(ctx, r)
		if len(stateFilter) > 0 && !containsAny(stateFilter, inst.State.Name) {
			continue
		}
		instances = append(instances, inst)
	}

	// AWS groups instances into reservations; the mock returns each instance
	// as its own single-instance reservation (matches what RunInstances
	// produces on brahmi's one-at-a-time path).
	reservations := make([]Reservation, 0, len(instances))
	for _, inst := range instances {
		reservations = append(reservations, Reservation{
			ReservationID: newReservationID(),
			OwnerID:       "000000000000",
			Instances:     []Instance{inst},
		})
	}

	return writeXML(c, fiber.StatusOK, DescribeInstancesResponse{
		XMLNS:        ec2Namespace,
		RequestID:    newRequestID(),
		Reservations: reservations,
	})
}

// handleStopInstances maps to `docker stop` (or `docker pause` when
// Hibernate=true). The persistence contract matches EC2: docker's named volume
// keeps `$BENJI_HOME` intact across stop/start, matching EBS-backed root
// device semantics.
func (m *Mock) handleStopInstances(c *fiber.Ctx, req *QueryRequest) error {
	ctx := context.Background()
	ids := req.listValues("InstanceId")
	hibernate := req.hibernateFlag()

	changes := []InstanceStateChange{}
	for _, id := range ids {
		r, ok := m.state.Get(id)
		if !ok {
			continue
		}
		prev, err := m.containerState(ctx, r.ContainerID, r.Hibernated)
		if err != nil {
			continue
		}
		var opErr error
		if hibernate {
			opErr = m.pauseContainer(ctx, r.ContainerID)
			if opErr == nil {
				r.Hibernated = true
				m.state.Put(r)
			}
		} else {
			opErr = m.stopContainer(ctx, r.ContainerID)
		}
		if opErr != nil {
			log.Printf("aws: Stop %s: %v", id, opErr)
			continue
		}
		curCode := stateCodeStopping
		if hibernate {
			// Report as stopping → stopped, matching the real EC2 hibernate
			// transition (the SDK doesn't distinguish paused).
			curCode = stateCodeStopping
		}
		changes = append(changes, InstanceStateChange{
			InstanceID:    id,
			PreviousState: prev,
			CurrentState:  stateFromCode(curCode),
		})
	}
	return writeXML(c, fiber.StatusOK, StopInstancesResponse{
		XMLNS:     ec2Namespace,
		RequestID: newRequestID(),
		Changes:   changes,
	})
}

// handleStartInstances resumes a stopped or hibernated instance. Hibernated
// instances take the `docker unpause` path (RAM/process state intact), plain
// stopped ones take `docker start` (fresh boot; entrypoint re-runs).
func (m *Mock) handleStartInstances(c *fiber.Ctx, req *QueryRequest) error {
	ctx := context.Background()
	ids := req.listValues("InstanceId")

	changes := []InstanceStateChange{}
	for _, id := range ids {
		r, ok := m.state.Get(id)
		if !ok {
			continue
		}
		prev, err := m.containerState(ctx, r.ContainerID, r.Hibernated)
		if err != nil {
			continue
		}
		var opErr error
		if r.Hibernated {
			opErr = m.unpauseContainer(ctx, r.ContainerID)
			if opErr == nil {
				r.Hibernated = false
				m.state.Put(r)
			}
		} else {
			opErr = m.startContainer(ctx, r.ContainerID)
		}
		if opErr != nil {
			log.Printf("aws: Start %s: %v", id, opErr)
			continue
		}
		changes = append(changes, InstanceStateChange{
			InstanceID:    id,
			PreviousState: prev,
			CurrentState:  stateFromCode(stateCodePending),
		})
	}
	return writeXML(c, fiber.StatusOK, StartInstancesResponse{
		XMLNS:     ec2Namespace,
		RequestID: newRequestID(),
		Changes:   changes,
	})
}

// handleTerminateInstances maps to `docker rm -f`. The docker volume is left
// on disk unless the caller explicitly opts into removal via a
// mock-specific query param (`X-EC2Mock-RemoveVolume=true`) — this matches
// brahmi's `DeleteOnTermination` semantics without requiring us to parse the
// BlockDeviceMapping tree.
func (m *Mock) handleTerminateInstances(c *fiber.Ctx, req *QueryRequest) error {
	ctx := context.Background()
	ids := req.listValues("InstanceId")
	removeVolume := strings.EqualFold(req.get("X-EC2Mock-RemoveVolume"), "true")

	changes := []InstanceStateChange{}
	for _, id := range ids {
		r, ok := m.state.Get(id)
		if !ok {
			continue
		}
		prev, err := m.containerState(ctx, r.ContainerID, r.Hibernated)
		if err != nil {
			continue
		}
		if err := m.removeContainer(ctx, r.ContainerID, r.VolumeName, removeVolume); err != nil {
			log.Printf("aws: Terminate %s: %v", id, err)
			continue
		}
		m.state.Delete(id)
		changes = append(changes, InstanceStateChange{
			InstanceID:    id,
			PreviousState: prev,
			CurrentState:  stateFromCode(stateCodeShuttingDown),
		})
	}
	return writeXML(c, fiber.StatusOK, TerminateInstancesResponse{
		XMLNS:     ec2Namespace,
		RequestID: newRequestID(),
		Changes:   changes,
	})
}

// handleRebootInstances runs `docker restart` under the hood.
func (m *Mock) handleRebootInstances(c *fiber.Ctx, req *QueryRequest) error {
	ctx := context.Background()
	ids := req.listValues("InstanceId")
	for _, id := range ids {
		r, ok := m.state.Get(id)
		if !ok {
			continue
		}
		if err := m.stopContainer(ctx, r.ContainerID); err != nil {
			log.Printf("aws: Reboot(stop) %s: %v", id, err)
			continue
		}
		if err := m.startContainer(ctx, r.ContainerID); err != nil {
			log.Printf("aws: Reboot(start) %s: %v", id, err)
			continue
		}
	}
	return writeXML(c, fiber.StatusOK, RebootInstancesResponse{
		XMLNS: ec2Namespace, RequestID: newRequestID(), Return: true,
	})
}

// handleCancelSpotRequests is a no-op. brahmi calls this on terminate for
// spot-launched instances; the mock doesn't model spot request lifecycles.
func (m *Mock) handleCancelSpotRequests(c *fiber.Ctx, _ *QueryRequest) error {
	return writeXML(c, fiber.StatusOK, CancelSpotInstanceRequestsResponse{
		XMLNS: ec2Namespace, RequestID: newRequestID(),
	})
}

// handleDescribeInstanceAttribute returns a minimally-populated skeleton so
// callers checking hibernation eligibility (etc.) see something sane.
func (m *Mock) handleDescribeInstanceAttribute(c *fiber.Ctx, req *QueryRequest) error {
	return writeXML(c, fiber.StatusOK, DescribeInstanceAttributeResponse{
		XMLNS:      ec2Namespace,
		RequestID:  newRequestID(),
		InstanceID: req.get("InstanceId"),
	})
}

// handleDescribeSubnets returns an empty set. There are no subnets in a
// docker world; the response exists so that callers issuing an
// unconditional DescribeSubnets on boot don't see a 501. Callers that pass
// a selector expecting a hit will surface a clear "no subnet matched"
// error on their side — the operator's cue to unset the selector.
func (m *Mock) handleDescribeSubnets(c *fiber.Ctx, _ *QueryRequest) error {
	return writeXML(c, fiber.StatusOK, DescribeSubnetsResponse{
		XMLNS:     ec2Namespace,
		RequestID: newRequestID(),
	})
}

// handleDescribeSecurityGroups returns an empty set. Same shape and
// rationale as handleDescribeSubnets.
func (m *Mock) handleDescribeSecurityGroups(c *fiber.Ctx, _ *QueryRequest) error {
	return writeXML(c, fiber.StatusOK, DescribeSecurityGroupsResponse{
		XMLNS:     ec2Namespace,
		RequestID: newRequestID(),
	})
}

// recordToInstance projects one InstanceRecord to the AWS wire type, resolving
// live state via docker and IP allocation via the container's network config.
func (m *Mock) recordToInstance(ctx context.Context, r *InstanceRecord) Instance {
	state, err := m.containerState(ctx, r.ContainerID, r.Hibernated)
	if err != nil {
		state = stateFromCode(stateCodeStopped)
	}
	ip := m.containerIP(ctx, r.ContainerID)
	tags := make([]Tag, 0, len(r.Tags))
	for k, v := range r.Tags {
		tags = append(tags, Tag{Key: k, Value: v})
	}
	inst := Instance{
		InstanceID:        r.InstanceID,
		ImageID:           r.ImageID,
		State:             state,
		InstanceType:      r.InstanceType,
		LaunchTime:        r.LaunchTime.Format(time.RFC3339),
		Placement:         Placement{AvailabilityZone: availabilityZone},
		PrivateIPAddress:  ip,
		PublicIPAddress:   ip,
		RootDeviceType:    "ebs",
		RootDeviceName:    "/dev/sda1",
		VirtualizationTyp: "hvm",
		Architecture:      "x86_64",
		Hypervisor:        "docker",
		Tags:              tags,
	}
	if r.Spot {
		inst.InstanceLifecycle = "spot"
	}
	return inst
}

// parseIntSafe is a small helper for max-count parsing; returns 0 on any
// error so callers can default sensibly.
func parseIntSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
