// Package poolmanager is the on-demand, origin-aware stand-in for the real
// pool-manager, mounted at /pool-manager on the unified mock server. The real
// pool-manager keeps a warm pool per service type and hands out a pre-baked
// container on claim — useless for fork/worktree testing, where a `fix/ikki`
// worktree needs a browser built from the `fix/browser` worktree image.
//
// This group does two things per claim (POST /pool-manager/api/v1/claim):
//
//  1. resolveImage — map the CALLER (source IP → its docker container →
//     clode.fork / compose-service label) to an image via a small list
//     (provision.yaml), so each fork gets its own image.
//  2. provision — run the SAME flow the real pool-manager runs, on demand and
//     with no warm pool: mint a pool-owner service token, CreateService in
//     jumbo under the pool org, push the rebuilt service-config (image + merged
//     env), deploy VIA JUMBO (jumbo → this server's /narnia group actually runs
//     the container), wait until it is up, then transfer ownership to the org
//     resolved from the claim token.
//
// It holds no warm pool, no svc_configs, and does no `docker run` itself — the
// deploy still flows through jumbo/narnia. The docker client is used only to
// map the caller IP to its container (origin) and to read replica health.
package poolmanager

import (
	"fmt"
	"os"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Config is the poolmanager group's runtime configuration, sourced from env
// (the compose block feeds it the same *admin-ids + *service-urls the real
// pool-manager uses).
type Config struct {
	JumboBaseURL  string
	RakshaBaseURL string

	// Pool identity — the service is CREATED under these (pool org/project/app),
	// then transferred to the claimant. OrgID falls back to OwnerID, mirroring
	// pool-manager's getUUIDFromEnvWithFallback(POOL_ORG_ID, POOL_OWNER_ID).
	OrgID         uuid.UUID
	OwnerID       uuid.UUID // caller_member_id for the raksha SA-token mint
	ProjectID     uuid.UUID
	ApplicationID uuid.UUID

	// ProvisionPath is the origin→image list file; empty/missing falls back to
	// the built-in default map.
	ProvisionPath string
}

// LoadConfig reads the group config from the environment.
func LoadConfig() (Config, error) {
	owner, err := parseUUIDEnv("POOL_OWNER_ID")
	if err != nil {
		return Config{}, err
	}
	project, err := parseUUIDEnv("POOL_PROJECT_ID")
	if err != nil {
		return Config{}, err
	}
	app, err := parseUUIDEnv("POOL_APPLICATION_ID")
	if err != nil {
		return Config{}, err
	}
	org := owner
	if v := os.Getenv("POOL_ORG_ID"); v != "" {
		if org, err = uuid.Parse(v); err != nil {
			return Config{}, fmt.Errorf("poolmanager: invalid POOL_ORG_ID: %w", err)
		}
	}
	return Config{
		JumboBaseURL:  envOr("JUMBO_BASE_URL", "http://jumbo:8080"),
		RakshaBaseURL: envOr("RAKSHA_URL", "http://raksha:8080"),
		OrgID:         org,
		OwnerID:       owner,
		ProjectID:     project,
		ApplicationID: app,
		ProvisionPath: envOr("PROVISION_CONFIG", "/etc/mock-services/provision.yaml"),
	}, nil
}

// ProvisionEntry is one (origin, serviceType) mapping: the image to deploy plus
// env overrides merged into the service-config's env block (e.g. IKKI_GRPC_ADDR
// / BRAHMI_URL pointed at the caller's fork).
type ProvisionEntry struct {
	Image string            `yaml:"image"`
	Env   map[string]string `yaml:"env"`
}

// ProvisionList is the origin→serviceType→entry list read per claim so edits
// take effect without a mock restart.
type ProvisionList struct {
	Default map[string]ProvisionEntry            `yaml:"default"`
	Origins map[string]map[string]ProvisionEntry `yaml:"origins"`
}

// builtinProvision is the fallback used when no provision.yaml is mounted: the
// baseline local images, calling home to baseline ikki/brahmi. Tags are `:main`
// — the versioned tag `stack up --browser`/`--agent` build (POOL_BUILDS tags
// `<repo>:<SVC>_TAG|main`, never `:latest`).
var builtinProvision = ProvisionList{
	Default: map[string]ProvisionEntry{
		"aramb-browser":   {Image: "clode-stack/brave-head:main", Env: map[string]string{"IKKI_GRPC_ADDR": "ikki:9000"}},
		"kairo":           {Image: "clode-stack/benji:main", Env: map[string]string{"BRAHMI_URL": "http://brahmi:8080"}},
		"kairo-ephemeral": {Image: "clode-stack/benji:main", Env: map[string]string{"BRAHMI_URL": "http://brahmi:8080"}},
	},
}

// loadProvision reads the list from path, falling back to builtin on any error
// (missing file, parse failure) so a fresh stack works with no config.
func loadProvision(path string) ProvisionList {
	if path == "" {
		return builtinProvision
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return builtinProvision
	}
	var list ProvisionList
	if err := yaml.Unmarshal(raw, &list); err != nil || len(list.Default) == 0 {
		return builtinProvision
	}
	return list
}

// resolve returns the entry for (origin, serviceType), falling back to the
// default block when the origin has no specific mapping for that type.
func (l ProvisionList) resolve(origin, serviceType string) (ProvisionEntry, bool) {
	if byType, ok := l.Origins[origin]; ok {
		if e, ok := byType[serviceType]; ok && e.Image != "" {
			return e, true
		}
	}
	if e, ok := l.Default[serviceType]; ok && e.Image != "" {
		return e, true
	}
	return ProvisionEntry{}, false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseUUIDEnv(key string) (uuid.UUID, error) {
	v := os.Getenv(key)
	if v == "" {
		return uuid.Nil, fmt.Errorf("poolmanager: %s is required", key)
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, fmt.Errorf("poolmanager: invalid %s: %w", key, err)
	}
	return id, nil
}
