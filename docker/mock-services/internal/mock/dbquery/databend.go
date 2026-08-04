package dbquery

// databend.go talks to Databend over its HTTP query API (the /v1/query
// handler). Databend is rarely queried from here, so this stays a thin client
// rather than pulling in a SQL driver: it POSTs the statement, drains any
// paginated result pages, and returns rows as an array of objects keyed by the
// response schema's column names. With no endpoint configured it returns a clear
// error so Postgres/Redis never depend on it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// databendResponse is the subset of Databend's HTTP query reply we consume.
type databendResponse struct {
	Schema []struct {
		Name string `json:"name"`
	} `json:"schema"`
	Data    [][]any `json:"data"`
	NextURI string  `json:"next_uri"`
	Error   any     `json:"error"`
}

// queryDatabend runs the statement against Databend's HTTP API and returns rows
// as an array of objects.
func (src *sources) queryDatabend(ctx context.Context, q string) (any, error) {
	if src.cfg.databendEndpoint == "" {
		return nil, fmt.Errorf("databend not configured: set DB_MCP_DATABEND_ENDPOINT (e.g. http://databend:8000)")
	}
	base := strings.TrimRight(src.cfg.databendEndpoint, "/")

	body, _ := json.Marshal(map[string]any{"sql": q})
	first, err := src.databendPost(ctx, base+"/v1/query", body)
	if err != nil {
		return nil, err
	}

	cols := make([]string, len(first.Schema))
	for i, c := range first.Schema {
		cols[i] = c.Name
	}

	out := []map[string]any{}
	appendRows := func(r *databendResponse) {
		for _, row := range r.Data {
			m := make(map[string]any, len(cols))
			for i, v := range row {
				name := fmt.Sprintf("col_%d", i)
				if i < len(cols) {
					name = cols[i]
				}
				m[name] = v
			}
			out = append(out, m)
		}
	}
	appendRows(first)

	// Drain paginated pages (Databend returns data across next_uri hops).
	next := first.NextURI
	for next != "" {
		page, err := src.databendGet(ctx, base+next)
		if err != nil {
			return nil, err
		}
		appendRows(page)
		next = page.NextURI
	}
	return out, nil
}

func (src *sources) databendPost(ctx context.Context, url string, body []byte) (*databendResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return src.databendDo(req)
}

func (src *sources) databendGet(ctx context.Context, url string) (*databendResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return src.databendDo(req)
}

func (src *sources) databendDo(req *http.Request) (*databendResponse, error) {
	req.SetBasicAuth(src.cfg.databendUser, src.cfg.databendPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("databend request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("databend %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out databendResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("databend decode: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("databend error: %v", out.Error)
	}
	return &out, nil
}
