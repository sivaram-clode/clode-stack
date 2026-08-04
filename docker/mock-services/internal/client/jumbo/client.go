// Package jumbo is a tiny client for the two jumbo internal endpoints the
// narnia group depends on: pulling a deployment's resolved configuration and
// posting status callbacks. Both live under jumbo's unauthenticated
// /internal/* route group, so no auth is threaded here.
package jumbo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to jumbo's /internal/* deployment endpoints.
type Client struct {
	baseURL string
	hc      *http.Client
}

// New returns a Client for the given jumbo base URL (e.g. http://jumbo:8080).
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 30 * time.Second},
	}
}

// DeploymentConfig is the slice of jumbo's DeploymentConfigResponse the
// deployer needs. settings/vars/secrets are left as raw JSON — the narnia
// group extracts only image, replicas, containerPort, and the env maps, so
// there is no need to model the full settings schema here.
type DeploymentConfig struct {
	DeploymentID string          `json:"deploymentId"`
	ServiceID    string          `json:"serviceId"`
	ServiceSlug  string          `json:"serviceSlug"`
	ServiceType  string          `json:"serviceType"`
	Settings     json.RawMessage `json:"settings"`
	Vars         json.RawMessage `json:"vars"`
	Secrets      json.RawMessage `json:"secrets"`
}

// GetDeploymentConfig fetches GET /internal/deployments/:id/config.
func (c *Client) GetDeploymentConfig(ctx context.Context, deploymentID string) (*DeploymentConfig, error) {
	url := fmt.Sprintf("%s/internal/deployments/%s/config", c.baseURL, deploymentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jumbo config %s: HTTP %d: %s", deploymentID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cfg DeploymentConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	return &cfg, nil
}

// PutDeploymentStatus posts PUT /internal/deployments/:id/status. payload is
// marshaled verbatim, so the caller controls the exact status/outputs shape.
func (c *Client) PutDeploymentStatus(ctx context.Context, deploymentID string, payload any) error {
	url := fmt.Sprintf("%s/internal/deployments/%s/status", c.baseURL, deploymentID)
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jumbo status %s: HTTP %d", deploymentID, resp.StatusCode)
	}
	return nil
}
