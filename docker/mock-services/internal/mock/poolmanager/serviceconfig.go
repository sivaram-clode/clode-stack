package poolmanager

import (
	"crypto/rand"
	"encoding/hex"
	"maps"

	"github.com/google/uuid"
)

// serviceConfigTemplate holds the per-serviceType service-config with its env
// merged in as one blob — the local, self-contained equivalent of what
// pool-manager's BuildServiceConfig produces (no svc_configs). Only the image
// is swapped per claim; origin env + pool vars are merged on top.
type serviceConfigTemplate struct {
	settings standardSettings
	vars     []varItem
	kairo    bool // kairo family → needs a per-deploy CODE_SERVER_PASSWORD
}

// templates are keyed by serviceType. kairo-ephemeral reuses the kairo blob.
var templates = map[string]serviceConfigTemplate{
	"kairo":           kairoTemplate(),
	"kairo-ephemeral": kairoTemplate(),
	"aramb-browser":   arambBrowserTemplate(),
}

// kairoTemplate mirrors pool-manager's newKairoDefaults() vars (the benji agent
// image), trimmed to the settings narnia actually reads locally.
func kairoTemplate() serviceConfigTemplate {
	return serviceConfigTemplate{
		settings: standardSettings{
			ImagePullPolicy: "IfNotPresent",
			ContainerPort:   18800,
			RunAsRoot:       false,
			RestartPolicy:   "Always",
			Regions:         []regionConfig{{Region: "eu-west-2", Replicas: 1}},
		},
		vars: []varItem{
			{Key: "HOME", Value: "/home/node"},
			{Key: "TERM", Value: "xterm-256color"},
			{Key: "KAIRO_ENABLED", Value: "1"},
			{Key: "GATEWAY_TYPE", Value: "benji"},
			{Key: "BENJI_GRPC_HOST", Value: "127.0.0.1"},
			{Key: "BENJI_GRPC_PORT", Value: "50051"},
			{Key: "BENJI_HOME", Value: "/home/node/.benji"},
			{Key: "KAIRO_DATA_DIR", Value: "/home/node/.benji/.kairo"},
			{Key: "MCPORTER_CONFIG", Value: "/home/node/.benji/mcporter.json"},
			{Key: "KAIRO_LISTEN_ADDR", Value: ":18800"},
		},
		kairo: true,
	}
}

// arambBrowserTemplate mirrors pool-manager's newArambBrowserDefaults(): the
// brave-head pool ikki drives via IKKI_CONNECT.
func arambBrowserTemplate() serviceConfigTemplate {
	return serviceConfigTemplate{
		settings: standardSettings{
			ImagePullPolicy: "IfNotPresent",
			ContainerPort:   9222,
			RunAsRoot:       true,
			RestartPolicy:   "Always",
			Regions:         []regionConfig{{Region: "eu-west-2", Replicas: 1}},
		},
		vars: []varItem{
			{Key: "IKKI_CONNECT", Value: "true"},
		},
	}
}

// buildConfigEntry assembles the jumbo bulk-config entry for one on-demand
// service: template vars + origin env (merged, origin wins) + pool identity
// vars, with the resolved image stamped into the settings.
func buildConfigEntry(serviceType string, serviceID uuid.UUID, slug, image string, originEnv map[string]string, agentID string) (bulkConfigEntry, bool) {
	tpl, ok := templates[serviceType]
	if !ok {
		return bulkConfigEntry{}, false
	}

	settings := tpl.settings
	settings.Image = image

	// Merge order: template vars → origin env (fork call-home overrides) →
	// pool identity vars. A map dedups so an origin key overrides the template.
	merged := map[string]string{}
	for _, v := range tpl.vars {
		merged[v.Key] = v.Value
	}
	maps.Copy(merged, originEnv)
	merged["JUMBO_SERVICE_ID"] = serviceID.String()
	merged["JUMBO_SERVICE_SLUG"] = slug
	merged["AGENT_ID"] = agentID
	if tpl.kairo {
		merged["CODE_SERVER_PASSWORD"] = randomHex(32)
	}

	vars := make([]varItem, 0, len(merged))
	for k, v := range merged {
		vars = append(vars, varItem{Key: k, Value: v})
	}

	return bulkConfigEntry{
		ServiceIdentifier: serviceID.String(),
		Settings:          &settings,
		Vars:              vars,
	}, true
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "insecure-fallback-password"
	}
	return hex.EncodeToString(b)
}
