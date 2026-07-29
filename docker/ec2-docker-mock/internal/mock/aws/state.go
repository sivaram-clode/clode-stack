package aws

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// InstanceRecord is what the mock keeps in memory for each running "EC2
// instance". The docker container ID is the durable handle — the fake EC2
// instance ID (`i-<hex>`) is what brahmi sees.
type InstanceRecord struct {
	InstanceID   string // i-<16 hex>
	ContainerID  string // docker container id
	ImageID      string // AMI id brahmi asked for (returned as-is)
	InstanceType string // returned as-is
	Tags         map[string]string
	LaunchTime   time.Time
	Spot         bool   // true if RunInstances came from a spot request
	Hibernated   bool   // set by StopInstances(Hibernate=true)
	VolumeName   string // named docker volume mounted at $BENJI_HOME (for stop/start persistence)
}

// State is an in-memory instance registry. Not persistent across restarts —
// it's a mock, and container state on the docker daemon is the truth on disk.
// Rebuilding on boot would be a nice-to-have follow-up (scan `docker ps -a`
// for aws.mock.instance-id labels), left out here to keep the surface small.
type State struct {
	mu        sync.RWMutex
	instances map[string]*InstanceRecord // key: instance id
}

func NewState() *State { return &State{instances: map[string]*InstanceRecord{}} }

func (s *State) Put(r *InstanceRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances[r.InstanceID] = r
}

func (s *State) Get(id string) (*InstanceRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.instances[id]
	return r, ok
}

func (s *State) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, id)
}

// All returns a snapshot of every tracked record.
func (s *State) All() []*InstanceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*InstanceRecord, 0, len(s.instances))
	for _, r := range s.instances {
		out = append(out, r)
	}
	return out
}

// Filter returns records whose tags match all filters. Filter values are OR'd
// within a single filter name (AWS semantics); each filter is AND'd across
// names.
func (s *State) Filter(filters map[string][]string) []*InstanceRecord {
	all := s.All()
	if len(filters) == 0 {
		return all
	}
	out := []*InstanceRecord{}
	for _, r := range all {
		if matchFilters(r, filters) {
			out = append(out, r)
		}
	}
	return out
}

// matchFilters implements the subset of AWS filter names brahmi uses:
//   - instance-state-name — matched against the docker container's inspect state
//     (looked up separately; here we only match filters that are tag-based).
//   - tag:<key> — value must appear in the record's tags for that key.
//   - tag-key — record must have any of the requested keys.
//
// State-based filters ("instance-state-name") are honored by the caller after
// resolving via docker inspect.
func matchFilters(r *InstanceRecord, filters map[string][]string) bool {
	for name, vals := range filters {
		switch {
		case strings.HasPrefix(name, "tag:"):
			tagKey := strings.TrimPrefix(name, "tag:")
			got, ok := r.Tags[tagKey]
			if !ok {
				return false
			}
			if !containsAny(vals, got) {
				return false
			}
		case name == "tag-key":
			any := false
			for _, k := range vals {
				if _, ok := r.Tags[k]; ok {
					any = true
					break
				}
			}
			if !any {
				return false
			}
		case name == "instance-state-name":
			// Handled by the caller after docker inspect — every record
			// passes this filter here, then the caller drops rows whose
			// live state doesn't match.
			continue
		}
	}
	return true
}

func containsAny(hay []string, needle string) bool {
	return slices.Contains(hay, needle)
}

// --- ID generation ---------------------------------------------------------

// newInstanceID mints an EC2-shaped instance id: `i-` + 17 hex chars, matching
// modern AWS (their old format was 8 hex chars, current is 17).
func newInstanceID() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return "i-" + hex.EncodeToString(b)[:17]
}

// newReservationID mints an EC2-shaped reservation id.
func newReservationID() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return "r-" + hex.EncodeToString(b)[:17]
}

// newVolumeID mints a docker volume name deterministically namespaced under
// the mock, so `docker volume ls` shows what's ours.
func newVolumeName(instanceID string) string {
	return fmt.Sprintf("ec2mock-%s-benji-home", strings.TrimPrefix(instanceID, "i-"))
}

// newRequestID returns a fresh AWS-shaped request id (UUID).
func newRequestID() string { return uuid.NewString() }
