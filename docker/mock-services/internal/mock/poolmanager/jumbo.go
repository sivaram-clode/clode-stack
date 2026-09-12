package poolmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// jumboClient drives the same jumbo endpoints the real pool-manager uses to
// create + deploy + transfer a service. All four calls hit the authenticated
// /api/v1 routes with the minted pool-owner token — jumbo derives the
// creator/owner from that token (the /internal/services/bulk route instead
// requires an explicit createdByMemberId, which is why pool-manager uses the
// public create).
type jumboClient struct {
	baseURL string
	hc      *http.Client
}

func newJumboClient(baseURL string) *jumboClient {
	return &jumboClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 60 * time.Second},
	}
}

// ---- wire types (subset of jumbo's contract we depend on) -------------------

type varItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type regionConfig struct {
	Region   string `json:"region"`
	Replicas int    `json:"replicas"`
}

// standardSettings is the slice of jumbo's StandardSettings that narnia's
// deployer actually reads (image, port, replicas, root) plus what jumbo's
// image validator needs. Everything else (volumeMounts, publicNet, health) is
// k8s-only and omitted — the local narnia deployer ignores it.
type standardSettings struct {
	Image           string         `json:"image"`
	ImagePullPolicy string         `json:"imagePullPolicy,omitempty"`
	ContainerPort   int            `json:"containerPort,omitempty"`
	RunAsRoot       bool           `json:"runAsRoot,omitempty"`
	RestartPolicy   string         `json:"restartPolicy,omitempty"`
	Regions         []regionConfig `json:"regions,omitempty"`
}

type bulkConfigEntry struct {
	ServiceIdentifier string            `json:"serviceIdentifier"`
	Settings          *standardSettings `json:"settings,omitempty"`
	Vars              []varItem         `json:"vars,omitempty"`
}

// createServiceRequest is the body for the authed POST /api/v1/services — the
// creator/owner is derived from the bearer token (the pool owner).
type createServiceRequest struct {
	IDType                string  `json:"idType"`
	Name                  string  `json:"name"`
	Type                  string  `json:"type"`
	ApplicationIdentifier string  `json:"applicationIdentifier"`
	ProjectIdentifier     *string `json:"projectIdentifier,omitempty"`
}

// serviceItem is the created service jumbo returns (id is a string uuid).
type serviceItem struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

type draftRequest struct {
	IdType  string            `json:"idType"`
	Configs []bulkConfigEntry `json:"configs"`
}

type deployRequest struct {
	ApplicationIdentifier string   `json:"applicationIdentifier"`
	ServiceIdentifiers    []string `json:"serviceIdentifiers"`
	IdType                string   `json:"idType"`
}

type transferRequest struct {
	OrgID         string `json:"orgId"`
	ProjectID     string `json:"projectId"`
	ApplicationID string `json:"applicationId"`
}

// ---- calls ------------------------------------------------------------------

// createService creates one service under the pool org via the authed
// POST /api/v1/services endpoint, returning the real jumbo service id + slug.
func (c *jumboClient) createService(ctx context.Context, cfg Config, name, serviceType, token string) (uuid.UUID, string, error) {
	project := cfg.ProjectID.String()
	body := createServiceRequest{
		IDType:                "id",
		Name:                  name,
		Type:                  serviceType,
		ApplicationIdentifier: cfg.ApplicationID.String(),
		ProjectIdentifier:     &project,
	}
	var out serviceItem
	if err := c.do(ctx, http.MethodPost, "/api/v1/services", token, body, &out); err != nil {
		return uuid.Nil, "", err
	}
	id, err := uuid.Parse(out.ID)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("jumbo: create returned unparseable id %q: %w", out.ID, err)
	}
	return id, out.Slug, nil
}

// pushConfig writes the service-config draft (image + merged env) via the authed
// PUT /api/v1/service-configurations/drafts/bulk endpoint.
func (c *jumboClient) pushConfig(ctx context.Context, entry bulkConfigEntry, token string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/service-configurations/drafts/bulk", token,
		draftRequest{IdType: "id", Configs: []bulkConfigEntry{entry}}, nil)
}

// triggerDeploy applies the draft — jumbo queues a deployment and posts a batch
// to narnia (this same mock server), which runs the container. Authenticated.
func (c *jumboClient) triggerDeploy(ctx context.Context, cfg Config, serviceID uuid.UUID, token string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/deployments/services", token, deployRequest{
		ApplicationIdentifier: cfg.ApplicationID.String(),
		ServiceIdentifiers:    []string{serviceID.String()},
		IdType:                "id",
	}, nil)
}

// transferOwnership moves the service to the claimant's org/project/app.
// Authenticated as the current (pool) owner via the minted token.
func (c *jumboClient) transferOwnership(ctx context.Context, serviceID uuid.UUID, token, orgID, projectID, appID string) error {
	path := fmt.Sprintf("/api/v1/services/%s/transfer-ownership?idType=id", serviceID.String())
	return c.do(ctx, http.MethodPost, path, token, transferRequest{
		OrgID:         orgID,
		ProjectID:     projectID,
		ApplicationID: appID,
	}, nil)
}

// do fires one JSON request, decoding into out when non-nil. token empty ⇒ no
// Authorization header (the /internal routes are unauthenticated).
func (c *jumboClient) do(ctx context.Context, method, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("jumbo: marshal %s: %w", path, err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("jumbo: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jumbo: %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("jumbo: decode %s: %w", path, err)
		}
	}
	return nil
}
