// Package config loads the unified deployer's runtime configuration from
// command-line flags (with environment-variable fallbacks). One Load() call
// parses everything; the returned Config is passed down to each API group.
package config

import (
	"flag"
	"os"
)

// Config is the process-wide configuration shared by every API group.
type Config struct {
	// Addr is the HTTP listen address (host:port).
	Addr string
	// DockerSocket is the docker daemon socket path. Empty defers to DOCKER_HOST
	// / DOCKER_API_VERSION env vars, then /var/run/docker.sock.
	DockerSocket string
	// Network is the docker network every launched container attaches to.
	Network string
	// EntrypointOverride, if set, replaces the image ENTRYPOINT on aws
	// RunInstances launches (see the aws group for the rationale).
	EntrypointOverride string
	// PullPolicy mirrors Kubernetes imagePullPolicy: IfNotPresent | Always | Never.
	PullPolicy string
	// JumboBaseURL is jumbo's base URL — the narnia group pulls each
	// deployment's config from it and posts status callbacks back to it.
	JumboBaseURL string
}

// Load parses flags (with env fallbacks) and returns the resolved Config.
func Load() Config {
	var c Config
	flag.StringVar(&c.Addr, "addr", envOr("EC2MOCK_ADDR", ":8080"), "listen address (host:port)")
	flag.StringVar(&c.DockerSocket, "docker-socket", "", "docker daemon socket (empty = DOCKER_HOST env or /var/run/docker.sock)")
	flag.StringVar(&c.Network, "network", "bridge", "docker network launched containers attach to")
	flag.StringVar(&c.EntrypointOverride, "entrypoint-override", "", "optional /path/to/entrypoint.sh — replaces the image entrypoint on aws launches")
	flag.StringVar(&c.PullPolicy, "pull-policy", "IfNotPresent", "image pull policy: IfNotPresent (default) | Always | Never")
	flag.StringVar(&c.JumboBaseURL, "jumbo-base-url", envOr("JUMBO_BASE_URL", "http://jumbo:8080"), "jumbo base URL for narnia config-pull + status callbacks")
	flag.Parse()
	return c
}

// envOr returns the environment variable value for key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
