package composio

// db.go is the whole persistence layer for the composio mock: it self-provisions
// its database + schema on startup and holds every SQL query. State lives in
// Postgres (the stack's shared db) so a connect-ref's data is consistent across
// requests and survives restarts — the mock is meant to be boring and solid,
// not a thing you debug.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbConfig is the resolved Postgres connection target.
type dbConfig struct {
	Host, Port, User, Password, SSLMode, DBName string
}

// safeIdent guards the database name we interpolate into CREATE DATABASE (which
// cannot be parameterized). Anything outside this is rejected rather than run.
var safeIdent = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// store is the Postgres-backed state for the composio group.
type store struct {
	pool *pgxpool.Pool
}

// connRow is a persisted mock connection. Status is never stored — it is
// derived from these fields (see deriveStatus), which keeps activation
// deterministic and restart-safe.
type connRow struct {
	AccountRef   string
	UserID       string
	Toolkit      string
	AuthConfigID string
	Alias        *string
	IsDisabled   bool
	IsDeleted    bool
	CreatedAt    time.Time
	ActivatedAt  *time.Time
}

// resourceRow is one generic per-tool record (a message, sheet row, event, …).
type resourceRow struct {
	ID        string
	Data      json.RawMessage
	CreatedAt time.Time
}

// openStore provisions the database if missing, opens a pool, and creates the
// schema. Every step is idempotent, so repeated boots are a no-op.
func openStore(ctx context.Context, cfg dbConfig) (*store, error) {
	if !safeIdent.MatchString(cfg.DBName) {
		return nil, fmt.Errorf("invalid db name %q", cfg.DBName)
	}
	if err := ensureDatabase(ctx, cfg); err != nil {
		return nil, fmt.Errorf("ensure database: %w", err)
	}
	pool, err := pgxpool.New(ctx, dsn(cfg, cfg.DBName))
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := createSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &store{pool: pool}, nil
}

// dsn builds a libpq URL for the given database.
func dsn(cfg dbConfig, dbName string) string {
	ssl := cfg.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, dbName, ssl)
}

// ensureDatabase connects to the admin `postgres` database and creates the
// target database if it does not already exist. A concurrent create (42P04) is
// treated as success.
func ensureDatabase(ctx context.Context, cfg dbConfig) error {
	conn, err := pgx.Connect(ctx, dsn(cfg, "postgres"))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", cfg.DBName).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+cfg.DBName); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P04" { // duplicate_database
			return nil
		}
		return err
	}
	return nil
}

const schemaDDL = `
CREATE TABLE IF NOT EXISTS mock_connection (
    account_ref    TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL,
    toolkit        TEXT NOT NULL,
    auth_config_id TEXT,
    alias          TEXT,
    is_disabled    BOOLEAN NOT NULL DEFAULT FALSE,
    is_deleted     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at   TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS mock_resource (
    id          TEXT PRIMARY KEY,
    account_ref TEXT NOT NULL,
    toolkit     TEXT NOT NULL,
    kind        TEXT NOT NULL,
    data        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_mock_resource_ref ON mock_resource (account_ref, toolkit, kind, created_at);
`

func createSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schemaDDL)
	return err
}

// ── Status derivation ─────────────────────────────────────────────────────────

// Composio status vocabulary toolkit-proxy's connector maps (see composioStatus
// in the proxy). We only emit these three plus DELETED.
const (
	statusActive    = "ACTIVE"
	statusInitiated = "INITIATED"
	statusInactive  = "INACTIVE"
	statusDeleted   = "DELETED"
)

// deriveStatus computes a connection's status from its row: disabled wins, then
// an explicit completion (activated_at) means ACTIVE. There is no time-based
// auto-activation — mirroring real Composio, a connection stays INITIATED until
// its OAuth URL is opened and completed (the Authorize click, or POST
// /oauth/complete for headless callers).
func (s *store) deriveStatus(r connRow) string {
	switch {
	case r.IsDeleted:
		return statusDeleted
	case r.IsDisabled:
		return statusInactive
	case r.ActivatedAt != nil:
		return statusActive
	default:
		return statusInitiated
	}
}

// ── Connections ───────────────────────────────────────────────────────────────

func (s *store) createConnection(ctx context.Context, r connRow) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO mock_connection (account_ref, user_id, toolkit, auth_config_id, alias)
		 VALUES ($1, $2, $3, $4, $5)`,
		r.AccountRef, r.UserID, r.Toolkit, r.AuthConfigID, r.Alias)
	return err
}

const connCols = `account_ref, user_id, toolkit, auth_config_id, alias, is_disabled, is_deleted, created_at, activated_at`

func scanConn(row pgx.Row) (connRow, error) {
	var r connRow
	err := row.Scan(&r.AccountRef, &r.UserID, &r.Toolkit, &r.AuthConfigID, &r.Alias,
		&r.IsDisabled, &r.IsDeleted, &r.CreatedAt, &r.ActivatedAt)
	return r, err
}

// getConnection returns the connection (including soft-deleted, so callers can
// 404 correctly) and whether it was found.
func (s *store) getConnection(ctx context.Context, accountRef string) (connRow, bool, error) {
	r, err := scanConn(s.pool.QueryRow(ctx, `SELECT `+connCols+` FROM mock_connection WHERE account_ref = $1`, accountRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return connRow{}, false, nil
	}
	if err != nil {
		return connRow{}, false, err
	}
	return r, true, nil
}

// listConnections returns non-deleted connections, optionally narrowed by
// user_id and toolkit. Status filtering is applied by the caller (it depends on
// the derived status). Empty filters mean no narrowing.
func (s *store) listConnections(ctx context.Context, userIDs, toolkits []string) ([]connRow, error) {
	q := `SELECT ` + connCols + ` FROM mock_connection WHERE is_deleted = FALSE`
	args := []any{}
	if len(userIDs) > 0 {
		args = append(args, userIDs)
		q += fmt.Sprintf(" AND user_id = ANY($%d)", len(args))
	}
	if len(toolkits) > 0 {
		args = append(args, toolkits)
		q += fmt.Sprintf(" AND toolkit = ANY($%d)", len(args))
	}
	q += " ORDER BY created_at, account_ref"

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []connRow
	for rows.Next() {
		r, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) setAlias(ctx context.Context, accountRef, alias string) error {
	_, err := s.pool.Exec(ctx, `UPDATE mock_connection SET alias = $2, updated_at = now() WHERE account_ref = $1`, accountRef, alias)
	return err
}

func (s *store) setDisabled(ctx context.Context, accountRef string, disabled bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE mock_connection SET is_disabled = $2, updated_at = now() WHERE account_ref = $1`, accountRef, disabled)
	return err
}

func (s *store) softDelete(ctx context.Context, accountRef string) error {
	_, err := s.pool.Exec(ctx, `UPDATE mock_connection SET is_deleted = TRUE, updated_at = now() WHERE account_ref = $1`, accountRef)
	return err
}

// activate marks the connection completed (sets activated_at once), the instant
// flip the OAuth landing page triggers.
func (s *store) activate(ctx context.Context, accountRef string) error {
	_, err := s.pool.Exec(ctx, `UPDATE mock_connection SET activated_at = now(), updated_at = now() WHERE account_ref = $1 AND activated_at IS NULL`, accountRef)
	return err
}

// ── Resources ─────────────────────────────────────────────────────────────────

func (s *store) insertResource(ctx context.Context, id, accountRef, toolkit, kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO mock_resource (id, account_ref, toolkit, kind, data) VALUES ($1, $2, $3, $4, $5)`,
		id, accountRef, toolkit, kind, raw)
	return err
}

func (s *store) listResources(ctx context.Context, accountRef, toolkit, kind string) ([]resourceRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, data, created_at FROM mock_resource
		 WHERE account_ref = $1 AND toolkit = $2 AND kind = $3
		 ORDER BY created_at, id`,
		accountRef, toolkit, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []resourceRow
	for rows.Next() {
		var r resourceRow
		if err := rows.Scan(&r.ID, &r.Data, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) getResource(ctx context.Context, accountRef, id string) (resourceRow, bool, error) {
	var r resourceRow
	err := s.pool.QueryRow(ctx,
		`SELECT id, data, created_at FROM mock_resource WHERE account_ref = $1 AND id = $2`,
		accountRef, id).Scan(&r.ID, &r.Data, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return resourceRow{}, false, nil
	}
	if err != nil {
		return resourceRow{}, false, err
	}
	return r, true, nil
}

// deleteResources clears all rows of a kind for a connect-ref — used by the
// sheets overwrite (UPDATE_VALUES) before re-inserting.
func (s *store) deleteResources(ctx context.Context, accountRef, toolkit, kind string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM mock_resource WHERE account_ref = $1 AND toolkit = $2 AND kind = $3`,
		accountRef, toolkit, kind)
	return err
}
