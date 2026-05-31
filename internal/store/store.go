package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"flowmusic2api/internal/config"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	cfg     config.Config
	db      *sql.DB
	dialect string
}

func New(ctx context.Context, cfg config.Config) (*Store, error) {
	dialect := strings.ToLower(strings.TrimSpace(cfg.DatabaseDriver))
	if dialect == "" {
		dialect = "sqlite"
	}
	if dialect == "pg" || dialect == "postgresql" {
		dialect = "postgres"
	}
	if dialect != "sqlite" && dialect != "postgres" {
		return nil, fmt.Errorf("unsupported database driver %q", cfg.DatabaseDriver)
	}

	dsn := strings.TrimSpace(cfg.DatabaseURL)
	if dialect == "sqlite" {
		if dsn == "" {
			dsn = filepath.Join(cfg.DataDir, "flowmusic2api.db")
		}
		if err := os.MkdirAll(filepath.Dir(dsn), 0o755); err != nil {
			return nil, err
		}
	}

	driver := dialect
	if dialect == "postgres" {
		driver = "pgx"
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if dialect == "sqlite" {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(4)
	} else if cfg.PostgresMaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.PostgresMaxOpenConns)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{cfg: cfg, db: db, dialect: dialect}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Migrate(ctx context.Context) error {
	schema := sqliteSchema
	if s.dialect == "postgres" {
		schema = postgresSchema
	}
	stmts := splitSQL(schema)
	for _, stmt := range stmts {
		if isIndexStatement(stmt) {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %w\n%s", err, stmt)
		}
	}
	if err := s.ensureCompatibilityMigrations(ctx); err != nil {
		return err
	}
	for _, stmt := range stmts {
		if !isIndexStatement(stmt) {
			continue
		}
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema index: %w\n%s", err, stmt)
		}
	}
	return nil
}

func isIndexStatement(stmt string) bool {
	stmt = strings.ToUpper(strings.TrimSpace(stmt))
	return strings.HasPrefix(stmt, "CREATE INDEX") || strings.HasPrefix(stmt, "CREATE UNIQUE INDEX")
}

func (s *Store) ensureCompatibilityMigrations(ctx context.Context) error {
	for _, item := range compatibilityColumns() {
		if err := s.ensureColumn(ctx, item.table, item.column, item.sqliteDDL, item.postgresDDL); err != nil {
			return err
		}
	}
	if s.dialect == "postgres" {
		for _, table := range []string{"accounts", "request_logs"} {
			if err := s.ensurePostgresIDDefault(ctx, table); err != nil {
				return err
			}
		}
	}
	return nil
}

type compatibilityColumn struct {
	table       string
	column      string
	sqliteDDL   string
	postgresDDL string
}

func compatibilityColumns() []compatibilityColumn {
	return []compatibilityColumn{
		{"admin_config", "debug_enabled", "debug_enabled BOOLEAN NOT NULL DEFAULT FALSE", "debug_enabled BOOLEAN NOT NULL DEFAULT FALSE"},
		{"admin_config", "error_ban_threshold", "error_ban_threshold INTEGER NOT NULL DEFAULT 3", "error_ban_threshold INTEGER NOT NULL DEFAULT 3"},
		{"admin_config", "guest_trial_enabled", "guest_trial_enabled BOOLEAN NOT NULL DEFAULT FALSE", "guest_trial_enabled BOOLEAN NOT NULL DEFAULT FALSE"},
		{"admin_config", "created_at", "created_at TIMESTAMP NULL", "created_at TIMESTAMPTZ NULL"},
		{"admin_config", "max_daily_guest_uses", "max_daily_guest_uses INTEGER NOT NULL DEFAULT 3", "max_daily_guest_uses INTEGER NOT NULL DEFAULT 3"},
			{"admin_config", "guest_global_daily_limit", "guest_global_daily_limit INTEGER NOT NULL DEFAULT 0", "guest_global_daily_limit INTEGER NOT NULL DEFAULT 0"},
		{"admin_config", "updated_at", "updated_at TIMESTAMP NULL", "updated_at TIMESTAMPTZ NULL"},
		{"cache_config", "base_url", "base_url TEXT NOT NULL DEFAULT ''", "base_url TEXT NOT NULL DEFAULT ''"},
		{"cache_config", "storage_mode", "storage_mode TEXT NOT NULL DEFAULT 'local'", "storage_mode TEXT NOT NULL DEFAULT 'local'"},
		{"cache_config", "s3_endpoint", "s3_endpoint TEXT NOT NULL DEFAULT ''", "s3_endpoint TEXT NOT NULL DEFAULT ''"},
		{"cache_config", "s3_region", "s3_region TEXT NOT NULL DEFAULT ''", "s3_region TEXT NOT NULL DEFAULT ''"},
		{"cache_config", "s3_bucket", "s3_bucket TEXT NOT NULL DEFAULT ''", "s3_bucket TEXT NOT NULL DEFAULT ''"},
		{"cache_config", "s3_access_key", "s3_access_key TEXT NOT NULL DEFAULT ''", "s3_access_key TEXT NOT NULL DEFAULT ''"},
		{"cache_config", "s3_secret_key", "s3_secret_key TEXT NOT NULL DEFAULT ''", "s3_secret_key TEXT NOT NULL DEFAULT ''"},
		{"cache_config", "s3_use_ssl", "s3_use_ssl BOOLEAN NOT NULL DEFAULT TRUE", "s3_use_ssl BOOLEAN NOT NULL DEFAULT TRUE"},
		{"cache_config", "s3_force_path_style", "s3_force_path_style BOOLEAN NOT NULL DEFAULT FALSE", "s3_force_path_style BOOLEAN NOT NULL DEFAULT FALSE"},
		{"cache_config", "s3_prefix", "s3_prefix TEXT NOT NULL DEFAULT ''", "s3_prefix TEXT NOT NULL DEFAULT ''"},
		{"cache_config", "s3_public_base_url", "s3_public_base_url TEXT NOT NULL DEFAULT ''", "s3_public_base_url TEXT NOT NULL DEFAULT ''"},
		{"cache_config", "created_at", "created_at TIMESTAMP NULL", "created_at TIMESTAMPTZ NULL"},
		{"cache_config", "updated_at", "updated_at TIMESTAMP NULL", "updated_at TIMESTAMPTZ NULL"},
		{"generation_config", "max_retries", "max_retries INTEGER NOT NULL DEFAULT 3", "max_retries INTEGER NOT NULL DEFAULT 3"},
		{"generation_config", "image_timeout", "image_timeout INTEGER NOT NULL DEFAULT 600", "image_timeout INTEGER NOT NULL DEFAULT 600"},
		{"generation_config", "video_timeout", "video_timeout INTEGER NOT NULL DEFAULT 600", "video_timeout INTEGER NOT NULL DEFAULT 600"},
		{"generation_config", "created_at", "created_at TIMESTAMP NULL", "created_at TIMESTAMPTZ NULL"},
		{"generation_config", "updated_at", "updated_at TIMESTAMP NULL", "updated_at TIMESTAMPTZ NULL"},
		{"token_refresh_config", "at_auto_refresh_enabled", "at_auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE", "at_auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE"},
		{"token_refresh_config", "refresh_before_expiry_seconds", "refresh_before_expiry_seconds INTEGER NOT NULL DEFAULT 600", "refresh_before_expiry_seconds INTEGER NOT NULL DEFAULT 600"},
		{"token_refresh_config", "created_at", "created_at TIMESTAMP NULL", "created_at TIMESTAMPTZ NULL"},
		{"token_refresh_config", "updated_at", "updated_at TIMESTAMP NULL", "updated_at TIMESTAMPTZ NULL"},
		{"proxy_config", "proxy_enabled", "proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE", "proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE"},
		{"proxy_config", "proxy_url", "proxy_url TEXT NOT NULL DEFAULT ''", "proxy_url TEXT NOT NULL DEFAULT ''"},
		{"proxy_config", "media_proxy_enabled", "media_proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE", "media_proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE"},
		{"proxy_config", "media_proxy_url", "media_proxy_url TEXT NOT NULL DEFAULT ''", "media_proxy_url TEXT NOT NULL DEFAULT ''"},
		{"proxy_config", "created_at", "created_at TIMESTAMP NULL", "created_at TIMESTAMPTZ NULL"},
		{"proxy_config", "updated_at", "updated_at TIMESTAMP NULL", "updated_at TIMESTAMPTZ NULL"},
		{"call_logic_config", "call_mode", "call_mode TEXT NOT NULL DEFAULT 'default'", "call_mode TEXT NOT NULL DEFAULT 'default'"},
		{"call_logic_config", "created_at", "created_at TIMESTAMP NULL", "created_at TIMESTAMPTZ NULL"},
		{"call_logic_config", "updated_at", "updated_at TIMESTAMP NULL", "updated_at TIMESTAMPTZ NULL"},
		{"accounts", "name", "name TEXT NOT NULL DEFAULT ''", "name TEXT NOT NULL DEFAULT ''"},
		{"accounts", "remark", "remark TEXT NOT NULL DEFAULT ''", "remark TEXT NOT NULL DEFAULT ''"},
		{"accounts", "is_active", "is_active BOOLEAN NOT NULL DEFAULT TRUE", "is_active BOOLEAN NOT NULL DEFAULT TRUE"},
		{"accounts", "protocol_mode", "protocol_mode TEXT NOT NULL DEFAULT 'refresh_token'", "protocol_mode TEXT NOT NULL DEFAULT 'refresh_token'"},
		{"accounts", "refresh_token", "refresh_token TEXT NOT NULL DEFAULT ''", "refresh_token TEXT NOT NULL DEFAULT ''"},
		{"accounts", "access_token", "access_token TEXT NOT NULL DEFAULT ''", "access_token TEXT NOT NULL DEFAULT ''"},
		{"accounts", "provider_token", "provider_token TEXT NOT NULL DEFAULT ''", "provider_token TEXT NOT NULL DEFAULT ''"},
		{"accounts", "provider_refresh_token", "provider_refresh_token TEXT NOT NULL DEFAULT ''", "provider_refresh_token TEXT NOT NULL DEFAULT ''"},
		{"accounts", "flow_bearer", "flow_bearer TEXT NOT NULL DEFAULT ''", "flow_bearer TEXT NOT NULL DEFAULT ''"},
		{"accounts", "cookies", "cookies TEXT NOT NULL DEFAULT ''", "cookies TEXT NOT NULL DEFAULT ''"},
		{"accounts", "login_account", "login_account TEXT NOT NULL DEFAULT ''", "login_account TEXT NOT NULL DEFAULT ''"},
		{"accounts", "login_password", "login_password TEXT NOT NULL DEFAULT ''", "login_password TEXT NOT NULL DEFAULT ''"},
		{"accounts", "proxy_url", "proxy_url TEXT NOT NULL DEFAULT ''", "proxy_url TEXT NOT NULL DEFAULT ''"},
		{"accounts", "auto_refresh_enabled", "auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE", "auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE"},
		{"accounts", "refresh_interval_minutes", "refresh_interval_minutes INTEGER NOT NULL DEFAULT 60", "refresh_interval_minutes INTEGER NOT NULL DEFAULT 60"},
		{"accounts", "expires_at", "expires_at TIMESTAMP NULL", "expires_at TIMESTAMPTZ NULL"},
		{"accounts", "at_expires", "at_expires TIMESTAMP NULL", "at_expires TIMESTAMPTZ NULL"},
		{"accounts", "last_refresh_at", "last_refresh_at TIMESTAMP NULL", "last_refresh_at TIMESTAMPTZ NULL"},
		{"accounts", "last_refresh_result", "last_refresh_result TEXT NOT NULL DEFAULT ''", "last_refresh_result TEXT NOT NULL DEFAULT ''"},
		{"accounts", "last_used_at", "last_used_at TIMESTAMP NULL", "last_used_at TIMESTAMPTZ NULL"},
		{"accounts", "credits", "credits INTEGER NOT NULL DEFAULT 0", "credits INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "tokens_remaining", "tokens_remaining REAL NOT NULL DEFAULT 0", "tokens_remaining DOUBLE PRECISION NOT NULL DEFAULT 0"},
		{"accounts", "subscription_tier", "subscription_tier TEXT NOT NULL DEFAULT ''", "subscription_tier TEXT NOT NULL DEFAULT ''"},
		{"accounts", "use_count", "use_count INTEGER NOT NULL DEFAULT 0", "use_count INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "music_count", "music_count INTEGER NOT NULL DEFAULT 0", "music_count INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "today_music_count", "today_music_count INTEGER NOT NULL DEFAULT 0", "today_music_count INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "image_count", "image_count INTEGER NOT NULL DEFAULT 0", "image_count INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "video_count", "video_count INTEGER NOT NULL DEFAULT 0", "video_count INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "error_count", "error_count INTEGER NOT NULL DEFAULT 0", "error_count INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "today_error_count", "today_error_count INTEGER NOT NULL DEFAULT 0", "today_error_count INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "consecutive_error_count", "consecutive_error_count INTEGER NOT NULL DEFAULT 0", "consecutive_error_count INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "today_date", "today_date TEXT NOT NULL DEFAULT ''", "today_date TEXT NOT NULL DEFAULT ''"},
		{"accounts", "image_enabled", "image_enabled BOOLEAN NOT NULL DEFAULT TRUE", "image_enabled BOOLEAN NOT NULL DEFAULT TRUE"},
		{"accounts", "video_enabled", "video_enabled BOOLEAN NOT NULL DEFAULT TRUE", "video_enabled BOOLEAN NOT NULL DEFAULT TRUE"},
		{"accounts", "upscale_enabled", "upscale_enabled BOOLEAN NOT NULL DEFAULT TRUE", "upscale_enabled BOOLEAN NOT NULL DEFAULT TRUE"},
		{"accounts", "image_concurrency", "image_concurrency INTEGER NOT NULL DEFAULT -1", "image_concurrency INTEGER NOT NULL DEFAULT -1"},
		{"accounts", "video_concurrency", "video_concurrency INTEGER NOT NULL DEFAULT -1", "video_concurrency INTEGER NOT NULL DEFAULT -1"},
		{"accounts", "created_at", "created_at TIMESTAMP NULL", "created_at TIMESTAMPTZ NULL"},
		{"accounts", "updated_at", "updated_at TIMESTAMP NULL", "updated_at TIMESTAMPTZ NULL"},
		{"request_logs", "request_body", "request_body TEXT NOT NULL DEFAULT ''", "request_body TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "response_body", "response_body TEXT NOT NULL DEFAULT ''", "response_body TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "response_body_excerpt", "response_body_excerpt TEXT NOT NULL DEFAULT ''", "response_body_excerpt TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "status_code", "status_code INTEGER NOT NULL DEFAULT 0", "status_code INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "duration_ms", "duration_ms INTEGER NOT NULL DEFAULT 0", "duration_ms BIGINT NOT NULL DEFAULT 0"},
		{"request_logs", "status_text", "status_text TEXT NOT NULL DEFAULT ''", "status_text TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "progress", "progress INTEGER NOT NULL DEFAULT 0", "progress INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "error_summary", "error_summary TEXT NOT NULL DEFAULT ''", "error_summary TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "created_at", "created_at TIMESTAMP NULL", "created_at TIMESTAMPTZ NULL"},
		{"request_logs", "updated_at", "updated_at TIMESTAMP NULL", "updated_at TIMESTAMPTZ NULL"},
	}
}

func (s *Store) ensureColumn(ctx context.Context, table, column, sqliteDDL, postgresDDL string) error {
	hasColumn, err := s.hasColumn(ctx, table, column)
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	ddl := sqliteDDL
	if s.dialect == "postgres" {
		ddl = postgresDDL
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+ddl); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) ensurePostgresIDDefault(ctx context.Context, table string) error {
	row := s.db.QueryRowContext(ctx, `
SELECT COALESCE(identity_generation, '') <> '' OR column_default IS NOT NULL
FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = $1 AND column_name = 'id'`, table)
	var hasDefault bool
	if err := row.Scan(&hasDefault); err != nil {
		return fmt.Errorf("inspect %s.id default: %w", table, err)
	}
	if hasDefault {
		return nil
	}
	for _, stmt := range postgresIDDefaultStatements(table) {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("ensure %s.id default: %w", table, err)
		}
	}
	return nil
}

func postgresIDDefaultStatements(table string) []string {
	sequence := table + "_id_seq"
	return []string{
		`CREATE SEQUENCE IF NOT EXISTS ` + sequence,
		`ALTER SEQUENCE ` + sequence + ` OWNED BY ` + table + `.id`,
		`SELECT setval('` + sequence + `', COALESCE((SELECT MAX(id) FROM ` + table + `), 0) + 1, false)`,
		`ALTER TABLE ` + table + ` ALTER COLUMN id SET DEFAULT nextval('` + sequence + `')`,
	}
}

func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	if s.dialect == "postgres" {
		row := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`, table, column)
		var exists bool
		if err := row.Scan(&exists); err != nil {
			return false, err
		}
		return exists, nil
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) bind(query string) string {
	if s.dialect != "postgres" {
		return query
	}
	var b strings.Builder
	idx := 1
	for _, r := range query {
		if r == '?' {
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(idx))
			idx++
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func splitSQL(text string) []string {
	parts := strings.Split(text, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		stmt := strings.TrimSpace(part)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func translateErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func returningID() string {
	return "RETURNING id"
}

func nullableArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func today() string {
	return time.Now().Format("2006-01-02")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

type nullableTime struct {
	value *time.Time
}

func (t *nullableTime) Scan(value any) error {
	if value == nil {
		t.value = nil
		return nil
	}
	switch v := value.(type) {
	case time.Time:
		utc := v.UTC()
		t.value = &utc
		return nil
	case string:
		return t.scanString(v)
	case []byte:
		return t.scanString(string(v))
	default:
		return fmt.Errorf("cannot scan %T as time", value)
	}
}

func (t *nullableTime) scanString(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		t.value = nil
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			utc := parsed.UTC()
			t.value = &utc
			return nil
		}
	}
	return fmt.Errorf("cannot parse time %q", value)
}

func (t nullableTime) Ptr() *time.Time {
	return t.value
}
