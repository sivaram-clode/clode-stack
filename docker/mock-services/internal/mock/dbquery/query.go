package dbquery

// query.go holds the datasource executors and the MCP tool wiring. Connections
// are opened per call and closed on return — the server is a low-traffic dev
// convenience, so a fresh dial keeps state simple and always reflects the
// current datastore rather than a cached pool.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	redis "github.com/redis/go-redis/v9"
)

// safeIdent guards a Postgres database name we interpolate into a DSN. It mirrors
// the composio group's identifier rule.
var safeIdent = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// callTimeout bounds any single query so a wedged datastore can't hang the tool.
const callTimeout = 30 * time.Second

// sources executes queries against the configured datastores.
type sources struct {
	cfg config
}

func newSources(cfg config) *sources { return &sources{cfg: cfg} }

// ── MCP tools ───────────────────────────────────────────────────────────────

// registerTools declares the `query` and `list_datasources` tools on the server.
func registerTools(s *mcpserver.MCPServer, src *sources) {
	s.AddTool(mcp.NewTool("query",
		mcp.WithDescription("Run a statement/command verbatim against a local-stack datasource and return the result as JSON. "+
			"SQL rows come back as an array of objects; Redis returns the command's reply."),
		mcp.WithString("datasource",
			mcp.Required(),
			mcp.Description("Which datastore to run against."),
			mcp.Enum("postgres", "redis", "databend"),
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("The full statement (SQL) or command (Redis, e.g. `KEYS *`) to execute as-is."),
		),
		mcp.WithString("database",
			mcp.Description("For postgres: the logical DB to run against (e.g. raksha, brahmi, jumbo; default postgres). "+
				"For redis: the numeric logical DB index (default 0). Ignored for databend."),
		),
	), src.handleQuery)

	s.AddTool(mcp.NewTool("list_datasources",
		mcp.WithDescription("List the configured datasources and, for postgres, the reachable logical database names."),
	), src.handleListDatasources)
}

func (src *sources) handleQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	datasource, err := req.RequireString("datasource")
	if err != nil {
		return mcp.NewToolResultError("datasource is required"), nil
	}
	q, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query is required"), nil
	}
	database := req.GetString("database", "")

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	var result any
	switch datasource {
	case "postgres":
		result, err = src.queryPostgres(ctx, database, q)
	case "redis":
		result, err = src.queryRedis(ctx, database, q)
	case "databend":
		result, err = src.queryDatabend(ctx, q)
	default:
		return mcp.NewToolResultError(fmt.Sprintf("unknown datasource %q", datasource)), nil
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func (src *sources) handleListDatasources(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	pg := map[string]any{"host": src.cfg.pgHost, "port": src.cfg.pgPort}
	if dbs, err := src.listPostgresDBs(ctx); err != nil {
		pg["error"] = err.Error()
	} else {
		pg["databases"] = dbs
	}

	out := map[string]any{
		"postgres": pg,
		"redis":    map[string]any{"addr": src.cfg.redisAddr, "logical_dbs": []int{0, 1}},
		"databend": map[string]any{"endpoint": src.cfg.databendEndpoint, "configured": src.cfg.databendEndpoint != ""},
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

// ── Postgres ──────────────────────────────────────────────────────────────────

func (src *sources) pgDSN(dbName string) string {
	ssl := src.cfg.pgSSLMode
	if ssl == "" {
		ssl = "disable"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		src.cfg.pgUser, src.cfg.pgPassword, src.cfg.pgHost, src.cfg.pgPort, dbName, ssl)
}

// queryPostgres runs the statement against the selected logical DB and returns
// rows as an array of objects (for statements that return rows) or an affected
// -rows summary (for INSERT/UPDATE/DELETE/DDL).
func (src *sources) queryPostgres(ctx context.Context, database, q string) (any, error) {
	dbName := database
	if dbName == "" {
		dbName = "postgres"
	}
	if !safeIdent.MatchString(dbName) {
		return nil, fmt.Errorf("invalid database name %q", dbName)
	}

	conn, err := pgx.Connect(ctx, src.pgDSN(dbName))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", dbName, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	out := []map[string]any{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		row := make(map[string]any, len(fields))
		for i, f := range fields {
			row[string(f.Name)] = normalizePGValue(vals[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	// A statement that returned no columns (e.g. UPDATE/DDL) reports affected rows.
	if len(fields) == 0 {
		return map[string]any{"affected_rows": rows.CommandTag().RowsAffected()}, nil
	}
	return out, nil
}

// listPostgresDBs returns the reachable non-template logical database names.
func (src *sources) listPostgresDBs(ctx context.Context) ([]string, error) {
	conn, err := pgx.Connect(ctx, src.pgDSN("postgres"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, `SELECT datname FROM pg_database WHERE datistemplate = false ORDER BY datname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// normalizePGValue coerces pgx's richer types into plain JSON-friendly values;
// []byte becomes a string so bytea/text land as readable JSON.
func normalizePGValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

// ── Redis ─────────────────────────────────────────────────────────────────────

// queryRedis parses the command line into arguments and runs it via the generic
// Do, returning the reply as JSON. database selects the numeric logical DB.
func (src *sources) queryRedis(ctx context.Context, database, cmdLine string) (any, error) {
	db := 0
	if database != "" {
		n, err := parseRedisDB(database)
		if err != nil {
			return nil, err
		}
		db = n
	}

	args, err := tokenize(cmdLine)
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("empty redis command")
	}

	client := redis.NewClient(&redis.Options{
		Addr:     src.cfg.redisAddr,
		Password: src.cfg.redisPassword,
		DB:       db,
	})
	defer func() { _ = client.Close() }()

	ifaceArgs := make([]any, len(args))
	for i, a := range args {
		ifaceArgs[i] = a
	}
	res, err := client.Do(ctx, ifaceArgs...).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	return normalizeRedisValue(res), nil
}

func parseRedisDB(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("invalid redis database index %q", s)
	}
	return n, nil
}

// normalizeRedisValue makes go-redis replies JSON-friendly: []byte → string, and
// nested slices/maps are walked so binary values become readable.
func normalizeRedisValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalizeRedisValue(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeRedisValue(val)
		}
		return out
	default:
		return v
	}
}

// tokenize splits a command line into arguments, honoring single/double quotes so
// values with spaces (e.g. `SET k "a b"`) stay one argument. It is intentionally
// minimal — no escape sequences beyond quote grouping.
func tokenize(s string) ([]string, error) {
	var args []string
	var cur []rune
	var quote rune
	inWord := false

	flush := func() {
		if inWord {
			args = append(args, string(cur))
			cur = cur[:0]
			inWord = false
		}
	}

	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur = append(cur, r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur = append(cur, r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in command")
	}
	flush()
	return args, nil
}
