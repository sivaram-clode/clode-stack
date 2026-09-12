package poolmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// mintPoolOwnerToken mints a short-lived org service-account token for the pool
// owner via raksha's frozen internal contract
// POST /internal/orgs/{orgId}/service-account/token (unauthenticated). The token
// authorizes the deploy trigger and the ownership transfer as the pool owner —
// the same credential the real pool-manager mints at boot.
func mintPoolOwnerToken(ctx context.Context, rakshaBaseURL string, orgID, callerMemberID uuid.UUID) (string, error) {
	url := fmt.Sprintf("%s/internal/orgs/%s/service-account/token", strings.TrimRight(rakshaBaseURL, "/"), orgID.String())
	reqBody, _ := json.Marshal(map[string]any{
		"caller_member_id": callerMemberID.String(),
		"expiresInMinutes": 30,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("raksha: mint request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("raksha: mint HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("raksha: decode mint response: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("raksha: empty token")
	}
	return out.Token, nil
}

// orgFromToken decodes (does NOT verify) a bearer JWT's payload and returns its
// organization_id claim — the target org for the ownership transfer. Verification
// is unnecessary here: we only read whom the caller says they are so we can hand
// the service to their org. Returns "" when the token is absent or carries no org.
func orgFromToken(authHeader string) string {
	tok := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if tok == "" {
		return ""
	}
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		OrganizationID string `json:"organization_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.OrganizationID
}
