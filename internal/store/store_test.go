package store

import (
	"context"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"

	"golang.org/x/crypto/bcrypt"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	return config.Config{
		AppName:              "FlowMusic2API Test",
		DataDir:              dir,
		CacheDir:             filepath.Join(dir, "tmp"),
		CacheTimeout:         7200,
		CacheStorageMode:     "local",
		CacheS3UseSSL:        true,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin-secret",
		DefaultAPIKey:        "test-api-key",
		TokenRefreshInterval: time.Hour,
		TokenRefreshLead:     10 * time.Minute,
		GenerationTimeout:    15 * time.Minute,
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	db, err := New(ctx, testConfig(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	return db
}

func TestSQLiteMigrateEnsureDefaults(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	admin, err := db.GetAdminConfig(ctx)
	if err != nil {
		t.Fatalf("GetAdminConfig() error = %v", err)
	}
	if admin.Username != "admin" || admin.APIKey != "test-api-key" {
		t.Fatalf("unexpected admin config: %+v", admin)
	}
	if admin.ErrorBan != 3 {
		t.Fatalf("default error ban threshold = %d, want 3", admin.ErrorBan)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte("admin-secret")); err != nil {
		t.Fatalf("default password hash mismatch: %v", err)
	}

	cacheCfg, err := db.GetCacheConfig(ctx)
	if err != nil {
		t.Fatalf("GetCacheConfig() error = %v", err)
	}
	if cacheCfg.StorageMode != "local" || cacheCfg.Enabled {
		t.Fatalf("unexpected cache defaults: %+v", cacheCfg)
	}
	if cacheCfg.Timeout != 7200 || !cacheCfg.S3UseSSL {
		t.Fatalf("unexpected cache timeout/S3 defaults: %+v", cacheCfg)
	}

	refreshCfg, err := db.GetTokenRefreshConfig(ctx)
	if err != nil {
		t.Fatalf("GetTokenRefreshConfig() error = %v", err)
	}
	if !refreshCfg.Enabled || !refreshCfg.ATAutoRefreshEnabled || refreshCfg.RefreshIntervalMins != 60 {
		t.Fatalf("unexpected refresh defaults: %+v", refreshCfg)
	}

	proxyCfg, err := db.GetProxyConfig(ctx)
	if err != nil {
		t.Fatalf("GetProxyConfig() error = %v", err)
	}
	if proxyCfg.ProxyEnabled || proxyCfg.ProxyURL != "" || proxyCfg.MediaProxyEnabled || proxyCfg.MediaProxyURL != "" {
		t.Fatalf("unexpected proxy defaults: %+v", proxyCfg)
	}

	callLogicCfg, err := db.GetCallLogicConfig(ctx)
	if err != nil {
		t.Fatalf("GetCallLogicConfig() error = %v", err)
	}
	if callLogicCfg.CallMode != "default" {
		t.Fatalf("unexpected call logic defaults: %+v", callLogicCfg)
	}
	if err := db.UpdateCallLogicConfig(ctx, domain.CallLogicConfig{CallMode: "polling"}); err != nil {
		t.Fatalf("UpdateCallLogicConfig() error = %v", err)
	}
	callLogicCfg, err = db.GetCallLogicConfig(ctx)
	if err != nil {
		t.Fatalf("GetCallLogicConfig() after update error = %v", err)
	}
	if callLogicCfg.CallMode != "polling" {
		t.Fatalf("call logic config was not persisted: %+v", callLogicCfg)
	}
}

func TestEnsureDefaultsSeedsCacheConfigFromEnvironmentConfig(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.CacheEnabled = true
	cfg.CacheTimeout = 3600
	cfg.CacheBaseURL = " https://local.example.test/tmp "
	cfg.CacheStorageMode = " R2 "
	cfg.CacheS3Endpoint = " https://r2.example.test "
	cfg.CacheS3Region = "auto"
	cfg.CacheS3Bucket = " flowmusic-cache "
	cfg.CacheS3AccessKey = " access-key "
	cfg.CacheS3SecretKey = "secret-key "
	cfg.CacheS3UseSSL = true
	cfg.CacheS3ForcePathStyle = true
	cfg.CacheS3Prefix = " /flow-assets/ "
	cfg.CacheS3PublicBaseURL = " https://cdn.example.test/cache "

	db, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}

	cacheCfg, err := db.GetCacheConfig(ctx)
	if err != nil {
		t.Fatalf("GetCacheConfig() error = %v", err)
	}
	if !cacheCfg.Enabled || cacheCfg.Timeout != 3600 || cacheCfg.StorageMode != "r2" {
		t.Fatalf("unexpected seeded cache mode: %+v", cacheCfg)
	}
	if cacheCfg.BaseURL != "https://local.example.test/tmp" ||
		cacheCfg.S3Endpoint != "https://r2.example.test" ||
		cacheCfg.S3Bucket != "flowmusic-cache" ||
		cacheCfg.S3AccessKey != "access-key" ||
		cacheCfg.S3SecretKey != "secret-key " ||
		!cacheCfg.S3UseSSL ||
		!cacheCfg.S3ForcePathStyle ||
		cacheCfg.S3Prefix != "flow-assets" ||
		cacheCfg.S3PublicBaseURL != "" {
		t.Fatalf("unexpected seeded S3/R2 cache config: %+v", cacheCfg)
	}

	cfg.CacheStorageMode = "local"
	cfg.CacheEnabled = false
	db.cfg = cfg
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() second call error = %v", err)
	}
	cacheCfg, err = db.GetCacheConfig(ctx)
	if err != nil {
		t.Fatalf("GetCacheConfig() after second defaults error = %v", err)
	}
	if !cacheCfg.Enabled || cacheCfg.StorageMode != "r2" {
		t.Fatalf("EnsureDefaults should not overwrite existing cache config: %+v", cacheCfg)
	}
}

func TestEnsureDefaultsRejectsInvalidSeededObjectStorageCacheConfig(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.CacheStorageMode = "r2"
	cfg.CacheS3Bucket = "flowmusic-cache"
	cfg.CacheS3Endpoint = ""

	db, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err == nil || !strings.Contains(err.Error(), "FLOWMUSIC_S3_ENDPOINT") {
		t.Fatalf("EnsureDefaults() error = %v, want FLOWMUSIC_S3_ENDPOINT validation", err)
	}
}

func TestEnsureDefaultsSeedsInitialAccountFromEnvironmentConfig(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.InitialAccountEmail = "seed@example.test"
	cfg.InitialAccountName = "Seed"
	cfg.InitialAccountRemark = "from env"
	cfg.InitialProtocolMode = "refresh"
	cfg.InitialRefreshToken = "refresh-token"
	cfg.InitialFlowBearer = "flow-bearer"
	cfg.InitialProviderToken = "provider-token"
	cfg.InitialProviderRT = "provider-refresh-token"
	cfg.InitialCookies = "cookie=value"
	cfg.InitialAccountProxy = "http://proxy.example.test:8080"
	cfg.InitialAutoRefresh = true
	cfg.InitialRefreshMins = 15

	db, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}

	account, err := db.GetAccountByEmail(ctx, "seed@example.test")
	if err != nil {
		t.Fatalf("GetAccountByEmail() error = %v", err)
	}
	if account.ProtocolMode != "refresh_token" ||
		account.RefreshToken != "refresh-token" ||
		account.FlowBearer != "flow-bearer" ||
		account.ProviderToken != "provider-token" ||
		account.ProviderRefreshToken != "provider-refresh-token" ||
		account.Cookies != "cookie=value" ||
		account.ProxyURL != "http://proxy.example.test:8080" ||
		!account.AutoRefreshEnabled ||
		account.RefreshIntervalMins != 15 ||
		!account.ImageEnabled ||
		!account.VideoEnabled ||
		!account.UpscaleEnabled {
		t.Fatalf("unexpected seeded account: %+v", account)
	}

	cfg.InitialFlowBearer = "rotated-flow-bearer"
	cfg.InitialRefreshToken = "rotated-refresh-token"
	db.cfg = cfg
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() second call error = %v", err)
	}
	accounts, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("seeded account should be upserted, got %d accounts", len(accounts))
	}
	account, err = db.GetAccountByEmail(ctx, "seed@example.test")
	if err != nil {
		t.Fatalf("GetAccountByEmail() after upsert error = %v", err)
	}
	if account.FlowBearer != "rotated-flow-bearer" || account.RefreshToken != "rotated-refresh-token" {
		t.Fatalf("seeded account was not updated from env: %+v", account)
	}
}

func TestEnsureDefaultsSeedsInitialAccountWithStableFallbackEmail(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.InitialProtocolMode = "bearer"
	cfg.InitialFlowBearer = "flow-bearer"
	cfg.InitialAutoRefresh = false

	db, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() second call error = %v", err)
	}
	accounts, err := db.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0].Email != "initial-account@local" || accounts[0].ProtocolMode != "bearer" || accounts[0].FlowBearer != "flow-bearer" {
		t.Fatalf("unexpected stable fallback seeded account: %+v", accounts)
	}
}

func TestEnsureDefaultsSeedsInitialProviderTokenAccount(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.InitialProviderToken = "provider-token"
	cfg.InitialProviderRT = "provider-refresh-token"
	cfg.InitialAutoRefresh = true

	db, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}

	account, err := db.GetAccountByEmail(ctx, "initial-account@local")
	if err != nil {
		t.Fatalf("GetAccountByEmail() error = %v", err)
	}
	if account.ProtocolMode != "refresh_token" ||
		account.ProviderToken != "provider-token" ||
		account.ProviderRefreshToken != "provider-refresh-token" ||
		!account.AutoRefreshEnabled {
		t.Fatalf("unexpected provider-token seeded account: %+v", account)
	}
}

func TestEnsureDefaultsRejectsInvalidInitialAccountSeed(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.InitialProtocolMode = "protocol"
	cfg.InitialFlowBearer = "flow-bearer"

	db, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err == nil || !strings.Contains(err.Error(), "FLOWMUSIC_INITIAL_COOKIES") {
		t.Fatalf("EnsureDefaults() error = %v, want FLOWMUSIC_INITIAL_COOKIES validation", err)
	}
}

func TestSQLiteMigrateAddsCompatibilityColumnsToExistingTables(t *testing.T) {
	ctx := context.Background()
	db, err := New(ctx, testConfig(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	oldSchema := []string{
		`CREATE TABLE admin_config (id INTEGER PRIMARY KEY, username TEXT NOT NULL, password_hash TEXT NOT NULL, api_key TEXT NOT NULL)`,
		`CREATE TABLE cache_config (id INTEGER PRIMARY KEY, enabled BOOLEAN NOT NULL DEFAULT FALSE, timeout INTEGER NOT NULL DEFAULT 7200)`,
		`CREATE TABLE generation_config (id INTEGER PRIMARY KEY, timeout INTEGER NOT NULL DEFAULT 600)`,
		`CREATE TABLE token_refresh_config (id INTEGER PRIMARY KEY, enabled BOOLEAN NOT NULL DEFAULT TRUE, refresh_interval_minutes INTEGER NOT NULL DEFAULT 60)`,
		`CREATE TABLE proxy_config (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE call_logic_config (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE accounts (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE request_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, account_id INTEGER NULL, operation TEXT NOT NULL)`,
	}
	for _, stmt := range oldSchema {
		if _, err := db.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("create old schema: %v\n%s", err, stmt)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if _, err := db.GetAdminConfig(ctx); err != nil {
		t.Fatalf("GetAdminConfig() after compatibility migration error = %v", err)
	}
	if _, err := db.GetCacheConfig(ctx); err != nil {
		t.Fatalf("GetCacheConfig() after compatibility migration error = %v", err)
	}
	if _, err := db.GetGenerationConfig(ctx); err != nil {
		t.Fatalf("GetGenerationConfig() after compatibility migration error = %v", err)
	}
	if _, err := db.GetTokenRefreshConfig(ctx); err != nil {
		t.Fatalf("GetTokenRefreshConfig() after compatibility migration error = %v", err)
	}
	if _, err := db.GetProxyConfig(ctx); err != nil {
		t.Fatalf("GetProxyConfig() after compatibility migration error = %v", err)
	}
	if _, err := db.GetCallLogicConfig(ctx); err != nil {
		t.Fatalf("GetCallLogicConfig() after compatibility migration error = %v", err)
	}

	accountID, err := db.CreateAccount(ctx, domain.Account{Email: "compat@example.test", FlowBearer: "flow-bearer"})
	if err != nil {
		t.Fatalf("CreateAccount() after compatibility migration error = %v", err)
	}
	if _, err := db.GetAccount(ctx, accountID); err != nil {
		t.Fatalf("GetAccount() after compatibility migration error = %v", err)
	}
	logID, err := db.CreateRequestLog(ctx, domain.RequestLog{AccountID: &accountID, Operation: "compat.test", StatusCode: 102})
	if err != nil {
		t.Fatalf("CreateRequestLog() after compatibility migration error = %v", err)
	}
	if _, err := db.GetLogDetail(ctx, logID); err != nil {
		t.Fatalf("GetLogDetail() after compatibility migration error = %v", err)
	}
}

func TestGetActiveLogsReturnsOnlyRunningEntries(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	activeID, err := db.CreateRequestLog(ctx, domain.RequestLog{Operation: "music.generate", StatusCode: 102, StatusText: "streaming", Progress: 30})
	if err != nil {
		t.Fatalf("CreateRequestLog(active) error = %v", err)
	}
	if _, err := db.CreateRequestLog(ctx, domain.RequestLog{Operation: "music.generate", StatusCode: 200, StatusText: "success", Progress: 100}); err != nil {
		t.Fatalf("CreateRequestLog(done) error = %v", err)
	}

	logs, err := db.GetActiveLogs(ctx, 100)
	if err != nil {
		t.Fatalf("GetActiveLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].ID != activeID || logs[0].StatusText != "streaming" {
		t.Fatalf("unexpected active logs: %+v", logs)
	}
}

func TestFinalizeStaleActiveLogsMarksOnlyActiveRowsTimedOut(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	activeID, err := db.CreateRequestLog(ctx, domain.RequestLog{Operation: "music.generate", StatusCode: 102, StatusText: "generating", Progress: 50})
	if err != nil {
		t.Fatalf("CreateRequestLog(active) error = %v", err)
	}
	doneID, err := db.CreateRequestLog(ctx, domain.RequestLog{Operation: "music.generate", StatusCode: 200, StatusText: "success", Progress: 100})
	if err != nil {
		t.Fatalf("CreateRequestLog(done) error = %v", err)
	}

	if err := db.FinalizeStaleActiveLogs(ctx, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatalf("FinalizeStaleActiveLogs() error = %v", err)
	}
	active, err := db.GetLogDetail(ctx, activeID)
	if err != nil {
		t.Fatalf("GetLogDetail(active) error = %v", err)
	}
	if active.StatusCode != 504 || active.StatusText != "timeout" || active.Progress != 100 || active.ErrorSummary == "" {
		t.Fatalf("active log not finalized as timeout: %+v", active)
	}
	done, err := db.GetLogDetail(ctx, doneID)
	if err != nil {
		t.Fatalf("GetLogDetail(done) error = %v", err)
	}
	if done.StatusCode != 200 || done.StatusText != "success" || done.Progress != 100 {
		t.Fatalf("completed log should not be finalized: %+v", done)
	}
}

func TestPostgresIDDefaultStatements(t *testing.T) {
	for _, table := range []string{"accounts", "request_logs"} {
		t.Run(table, func(t *testing.T) {
			got := postgresIDDefaultStatements(table)
			seq := table + "_id_seq"
			want := []string{
				`CREATE SEQUENCE IF NOT EXISTS ` + seq,
				`ALTER SEQUENCE ` + seq + ` OWNED BY ` + table + `.id`,
				`SELECT setval('` + seq + `', COALESCE((SELECT MAX(id) FROM ` + table + `), 0) + 1, false)`,
				`ALTER TABLE ` + table + ` ALTER COLUMN id SET DEFAULT nextval('` + seq + `')`,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("postgresIDDefaultStatements(%q) = %#v, want %#v", table, got, want)
			}
		})
	}
}

func TestBindKeepsSQLiteAndRewritesPostgresPlaceholders(t *testing.T) {
	query := `SELECT * FROM request_logs WHERE status_code = ? OR account_id = ? ORDER BY id DESC LIMIT ?`
	if got := (&Store{dialect: "sqlite"}).bind(query); got != query {
		t.Fatalf("sqlite bind() = %q, want %q", got, query)
	}
	got := (&Store{dialect: "postgres"}).bind(query)
	want := `SELECT * FROM request_logs WHERE status_code = $1 OR account_id = $2 ORDER BY id DESC LIMIT $3`
	if got != want {
		t.Fatalf("postgres bind() = %q, want %q", got, want)
	}
}

func TestPostgresSchemaIncludesCurrentConfigTables(t *testing.T) {
	for _, fragment := range []string{
		`CREATE TABLE IF NOT EXISTS admin_config`,
		`CREATE TABLE IF NOT EXISTS cache_config`,
		`CREATE TABLE IF NOT EXISTS generation_config`,
		`CREATE TABLE IF NOT EXISTS token_refresh_config`,
		`CREATE TABLE IF NOT EXISTS proxy_config`,
		`CREATE TABLE IF NOT EXISTS call_logic_config`,
		`CREATE TABLE IF NOT EXISTS accounts`,
		`CREATE TABLE IF NOT EXISTS request_logs`,
		`id BIGINT PRIMARY KEY`,
		`id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY`,
		`created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP`,
		`guest_trial_enabled BOOLEAN NOT NULL DEFAULT FALSE`,
		`storage_mode TEXT NOT NULL DEFAULT 'local'`,
		`s3_endpoint TEXT NOT NULL DEFAULT ''`,
		`s3_region TEXT NOT NULL DEFAULT ''`,
		`s3_bucket TEXT NOT NULL DEFAULT ''`,
		`s3_access_key TEXT NOT NULL DEFAULT ''`,
		`s3_secret_key TEXT NOT NULL DEFAULT ''`,
		`s3_use_ssl BOOLEAN NOT NULL DEFAULT TRUE`,
		`s3_force_path_style BOOLEAN NOT NULL DEFAULT FALSE`,
		`s3_prefix TEXT NOT NULL DEFAULT ''`,
		`s3_public_base_url TEXT NOT NULL DEFAULT ''`,
		`max_retries INTEGER NOT NULL DEFAULT 3`,
		`image_timeout INTEGER NOT NULL DEFAULT 600`,
		`video_timeout INTEGER NOT NULL DEFAULT 600`,
		`at_auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE`,
		`refresh_before_expiry_seconds INTEGER NOT NULL DEFAULT 600`,
		`media_proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE`,
		`media_proxy_url TEXT NOT NULL DEFAULT ''`,
		`provider_refresh_token TEXT NOT NULL DEFAULT ''`,
		`tokens_remaining DOUBLE PRECISION NOT NULL DEFAULT 0`,
		`image_concurrency INTEGER NOT NULL DEFAULT -1`,
		`video_concurrency INTEGER NOT NULL DEFAULT -1`,
		`call_mode TEXT NOT NULL DEFAULT 'default'`,
		`duration_ms BIGINT NOT NULL DEFAULT 0`,
		`response_body_excerpt TEXT NOT NULL DEFAULT ''`,
		`status_text TEXT NOT NULL DEFAULT ''`,
		`progress INTEGER NOT NULL DEFAULT 0`,
		`error_summary TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at DESC, id DESC)`,
	} {
		if !strings.Contains(postgresSchema, fragment) {
			t.Fatalf("postgres schema missing fragment %q", fragment)
		}
	}
	for _, sqliteOnly := range []string{`PRAGMA`, `AUTOINCREMENT`, `REAL NOT NULL DEFAULT 0`} {
		if strings.Contains(postgresSchema, sqliteOnly) {
			t.Fatalf("postgres schema contains sqlite-only fragment %q", sqliteOnly)
		}
	}
}

func TestPostgresSchemaColumnsMatchSQLiteSchema(t *testing.T) {
	sqliteTables := parseSchemaColumns(t, sqliteSchema)
	postgresTables := parseSchemaColumns(t, postgresSchema)
	if !reflect.DeepEqual(tableNames(sqliteTables), tableNames(postgresTables)) {
		t.Fatalf("postgres tables = %#v, want sqlite tables %#v", tableNames(postgresTables), tableNames(sqliteTables))
	}
	for table, sqliteColumns := range sqliteTables {
		postgresColumns, ok := postgresTables[table]
		if !ok {
			t.Fatalf("postgres schema missing table %s", table)
		}
		if !reflect.DeepEqual(postgresColumns, sqliteColumns) {
			t.Fatalf("%s postgres columns = %#v, want sqlite columns %#v", table, postgresColumns, sqliteColumns)
		}
	}
}

func TestAccountColumnsStayAlignedWithSchema(t *testing.T) {
	accountsColumns := parseSchemaColumns(t, sqliteSchema)["accounts"]
	selectedColumns := parseAccountColumnNames(accountColumns)
	schemaSet := make(map[string]struct{}, len(accountsColumns))
	for _, column := range accountsColumns {
		schemaSet[column] = struct{}{}
	}
	for _, column := range selectedColumns {
		if _, ok := schemaSet[column]; !ok {
			t.Fatalf("accountColumns selects %q, but accounts schema does not contain it", column)
		}
		delete(schemaSet, column)
	}
	wantUnscanned := map[string]struct{}{"today_date": {}}
	if !reflect.DeepEqual(schemaSet, wantUnscanned) {
		t.Fatalf("unscanned accounts schema columns = %#v, want %#v", schemaSet, wantUnscanned)
	}
}

func TestPostgresCompatibilityColumnDDLContract(t *testing.T) {
	columns := map[string]compatibilityColumn{}
	for _, column := range compatibilityColumns() {
		columns[column.table+"."+column.column] = column
	}
	for key, wantDDL := range map[string]string{
		"admin_config.guest_trial_enabled":                   "guest_trial_enabled BOOLEAN NOT NULL DEFAULT FALSE",
		"admin_config.updated_at":                            "updated_at TIMESTAMPTZ NULL",
		"cache_config.s3_endpoint":                           "s3_endpoint TEXT NOT NULL DEFAULT ''",
		"cache_config.s3_region":                             "s3_region TEXT NOT NULL DEFAULT ''",
		"cache_config.s3_bucket":                             "s3_bucket TEXT NOT NULL DEFAULT ''",
		"cache_config.s3_access_key":                         "s3_access_key TEXT NOT NULL DEFAULT ''",
		"cache_config.s3_secret_key":                         "s3_secret_key TEXT NOT NULL DEFAULT ''",
		"cache_config.s3_use_ssl":                            "s3_use_ssl BOOLEAN NOT NULL DEFAULT TRUE",
		"cache_config.s3_force_path_style":                   "s3_force_path_style BOOLEAN NOT NULL DEFAULT FALSE",
		"cache_config.s3_prefix":                             "s3_prefix TEXT NOT NULL DEFAULT ''",
		"cache_config.s3_public_base_url":                    "s3_public_base_url TEXT NOT NULL DEFAULT ''",
		"generation_config.max_retries":                      "max_retries INTEGER NOT NULL DEFAULT 3",
		"generation_config.image_timeout":                    "image_timeout INTEGER NOT NULL DEFAULT 600",
		"generation_config.video_timeout":                    "video_timeout INTEGER NOT NULL DEFAULT 600",
		"token_refresh_config.at_auto_refresh_enabled":       "at_auto_refresh_enabled BOOLEAN NOT NULL DEFAULT TRUE",
		"token_refresh_config.refresh_before_expiry_seconds": "refresh_before_expiry_seconds INTEGER NOT NULL DEFAULT 600",
		"proxy_config.proxy_enabled":                         "proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE",
		"proxy_config.proxy_url":                             "proxy_url TEXT NOT NULL DEFAULT ''",
		"proxy_config.media_proxy_enabled":                   "media_proxy_enabled BOOLEAN NOT NULL DEFAULT FALSE",
		"proxy_config.media_proxy_url":                       "media_proxy_url TEXT NOT NULL DEFAULT ''",
		"call_logic_config.call_mode":                        "call_mode TEXT NOT NULL DEFAULT 'default'",
		"call_logic_config.created_at":                       "created_at TIMESTAMPTZ NULL",
		"call_logic_config.updated_at":                       "updated_at TIMESTAMPTZ NULL",
		"accounts.expires_at":                                "expires_at TIMESTAMPTZ NULL",
		"accounts.at_expires":                                "at_expires TIMESTAMPTZ NULL",
		"accounts.last_refresh_at":                           "last_refresh_at TIMESTAMPTZ NULL",
		"accounts.last_used_at":                              "last_used_at TIMESTAMPTZ NULL",
		"accounts.tokens_remaining":                          "tokens_remaining DOUBLE PRECISION NOT NULL DEFAULT 0",
		"accounts.image_enabled":                             "image_enabled BOOLEAN NOT NULL DEFAULT TRUE",
		"accounts.video_enabled":                             "video_enabled BOOLEAN NOT NULL DEFAULT TRUE",
		"accounts.upscale_enabled":                           "upscale_enabled BOOLEAN NOT NULL DEFAULT TRUE",
		"accounts.image_concurrency":                         "image_concurrency INTEGER NOT NULL DEFAULT -1",
		"accounts.video_concurrency":                         "video_concurrency INTEGER NOT NULL DEFAULT -1",
		"request_logs.duration_ms":                           "duration_ms BIGINT NOT NULL DEFAULT 0",
		"request_logs.response_body":                         "response_body TEXT NOT NULL DEFAULT ''",
		"request_logs.response_body_excerpt":                 "response_body_excerpt TEXT NOT NULL DEFAULT ''",
		"request_logs.status_text":                           "status_text TEXT NOT NULL DEFAULT ''",
		"request_logs.progress":                              "progress INTEGER NOT NULL DEFAULT 0",
		"request_logs.error_summary":                         "error_summary TEXT NOT NULL DEFAULT ''",
		"request_logs.updated_at":                            "updated_at TIMESTAMPTZ NULL",
	} {
		column, ok := columns[key]
		if !ok {
			t.Fatalf("compatibilityColumns() missing %s", key)
		}
		if column.postgresDDL != wantDDL {
			t.Fatalf("compatibilityColumns()[%s].postgresDDL = %q, want %q", key, column.postgresDDL, wantDDL)
		}
	}
}

func TestPostgresBoundQueriesForStoreOperations(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		placeholders int
		fragments    []string
	}{
		{
			name:         "create request log",
			query:        `INSERT INTO request_logs (account_id, operation, request_body, response_body, response_body_excerpt, status_code, duration_ms, status_text, progress, error_summary, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ` + returningID(),
			placeholders: 10,
			fragments:    []string{`VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) RETURNING id`},
		},
		{
			name:         "get logs",
			query:        `SELECT l.id, l.account_id, COALESCE(a.email, ''), l.operation, '' AS request_body, '' AS response_body, l.response_body_excerpt, l.status_code, l.duration_ms, l.status_text, l.progress, l.error_summary, l.created_at, l.updated_at FROM request_logs l LEFT JOIN accounts a ON a.id = l.account_id ORDER BY l.id DESC LIMIT ?`,
			placeholders: 1,
			fragments:    []string{`COALESCE(a.email, '')`, `LIMIT $1`},
		},
		{
			name:         "get active logs",
			query:        `SELECT l.id, l.account_id, COALESCE(a.email, ''), l.operation, '' AS request_body, '' AS response_body, l.response_body_excerpt, l.status_code, l.duration_ms, l.status_text, l.progress, l.error_summary, l.created_at, l.updated_at FROM request_logs l LEFT JOIN accounts a ON a.id = l.account_id WHERE l.status_code = ? OR LOWER(l.status_text) IN ('started', 'queued', 'token_selected', 'token_ready', 'streaming', 'polling', 'caching', 'processing', 'running') ORDER BY l.updated_at DESC, l.id DESC LIMIT ?`,
			placeholders: 2,
			fragments:    []string{`l.status_code = $1`, `LOWER(l.status_text) IN ('started', 'queued'`, `LIMIT $2`},
		},
		{
			name:         "update request log",
			query:        `UPDATE request_logs SET account_id = ?, request_body = ?, response_body = ?, response_body_excerpt = ?, status_code = ?, duration_ms = ?, status_text = ?, progress = ?, error_summary = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			placeholders: 10,
			fragments:    []string{`account_id = $1`, `error_summary = $9`, `WHERE id = $10`},
		},
		{
			name:         "update cache config",
			query:        `UPDATE cache_config SET enabled = ?, timeout = ?, base_url = ?, storage_mode = ?, s3_endpoint = ?, s3_region = ?, s3_bucket = ?, s3_access_key = ?, s3_secret_key = ?, s3_use_ssl = ?, s3_force_path_style = ?, s3_prefix = ?, s3_public_base_url = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			placeholders: 14,
			fragments:    []string{`enabled = $1`, `s3_public_base_url = $13`, `WHERE id = $14`},
		},
		{
			name:         "update generation config",
			query:        `UPDATE generation_config SET timeout = ?, max_retries = ?, image_timeout = ?, video_timeout = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			placeholders: 5,
			fragments:    []string{`timeout = $1`, `video_timeout = $4`, `WHERE id = $5`},
		},
		{
			name:         "update token refresh config",
			query:        `UPDATE token_refresh_config SET enabled = ?, at_auto_refresh_enabled = ?, refresh_interval_minutes = ?, refresh_before_expiry_seconds = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			placeholders: 5,
			fragments:    []string{`enabled = $1`, `refresh_before_expiry_seconds = $4`, `WHERE id = $5`},
		},
		{
			name:         "update proxy config",
			query:        `UPDATE proxy_config SET proxy_enabled = ?, proxy_url = ?, media_proxy_enabled = ?, media_proxy_url = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			placeholders: 5,
			fragments:    []string{`proxy_enabled = $1`, `media_proxy_url = $4`, `WHERE id = $5`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertPostgresBoundQuery(t, tc.query, tc.placeholders, tc.fragments)
		})
	}
}

var pgPlaceholderPattern = regexp.MustCompile(`\$\d+\b`)

func assertPostgresBoundQuery(t *testing.T, query string, placeholders int, fragments []string) {
	t.Helper()
	got := (&Store{dialect: "postgres"}).bind(query)
	if strings.Contains(got, "?") {
		t.Fatalf("postgres query still contains ? placeholder: %s", got)
	}
	matches := pgPlaceholderPattern.FindAllString(got, -1)
	if len(matches) != placeholders {
		t.Fatalf("postgres placeholder count = %d, want %d\n%s", len(matches), placeholders, got)
	}
	for idx, match := range matches {
		want := "$" + strconv.Itoa(idx+1)
		if match != want {
			t.Fatalf("postgres placeholder %d = %s, want %s\n%s", idx+1, match, want, got)
		}
	}
	for _, fragment := range fragments {
		if !strings.Contains(got, fragment) {
			t.Fatalf("postgres query missing fragment %q\n%s", fragment, got)
		}
	}
}

func parseSchemaColumns(t *testing.T, schema string) map[string][]string {
	t.Helper()
	tables := make(map[string][]string)
	for _, stmt := range splitSQL(schema) {
		upper := strings.ToUpper(stmt)
		if !strings.HasPrefix(upper, "CREATE TABLE IF NOT EXISTS ") {
			continue
		}
		open := strings.Index(stmt, "(")
		close := strings.LastIndex(stmt, ")")
		if open < 0 || close < open {
			t.Fatalf("cannot parse CREATE TABLE statement: %s", stmt)
		}
		table := strings.Fields(strings.TrimSpace(stmt[:open]))
		name := table[len(table)-1]
		body := stmt[open+1 : close]
		lines := strings.Split(body, "\n")
		columns := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(strings.TrimSuffix(line, ","))
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			columns = append(columns, fields[0])
		}
		tables[name] = columns
	}
	return tables
}

func tableNames(tables map[string][]string) []string {
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func parseAccountColumnNames(columns string) []string {
	parts := strings.Split(columns, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "a.")
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		out = append(out, fields[0])
	}
	return out
}

func TestUpdateAdminConfigPersistsErrorBanThreshold(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	if err := db.UpdateAdminConfig(ctx, "root", "updated-api-key", true, 5, true, 0, 0); err != nil {
		t.Fatalf("UpdateAdminConfig() error = %v", err)
	}
	admin, err := db.GetAdminConfig(ctx)
	if err != nil {
		t.Fatalf("GetAdminConfig() error = %v", err)
	}
	if admin.Username != "root" || admin.APIKey != "updated-api-key" || !admin.DebugEnabled || admin.ErrorBan != 5 || !admin.GuestTrialEnabled {
		t.Fatalf("admin config was not persisted: %+v", admin)
	}
}

func TestAccountCRUD(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	id, err := db.CreateAccount(ctx, domain.Account{
		Email:        "user@example.test",
		RefreshToken: "refresh-token",
		FlowBearer:   "flow-bearer",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	account, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.ST != "refresh-token" || account.AT != "flow-bearer" {
		t.Fatalf("compat token fields not populated: %+v", account)
	}
	if !account.IsActive || !account.ImageEnabled || !account.VideoEnabled || !account.UpscaleEnabled {
		t.Fatalf("unexpected account defaults: %+v", account)
	}

	if err := db.SetAccountActive(ctx, id, false); err != nil {
		t.Fatalf("SetAccountActive() error = %v", err)
	}
	active, err := db.GetActiveAccounts(ctx)
	if err != nil {
		t.Fatalf("GetActiveAccounts() error = %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no active accounts, got %d", len(active))
	}

	if err := db.DeleteAccount(ctx, id); err != nil {
		t.Fatalf("DeleteAccount() error = %v", err)
	}
	if _, err := db.GetAccount(ctx, id); err != ErrNotFound {
		t.Fatalf("GetAccount() after delete error = %v, want %v", err, ErrNotFound)
	}
}

func TestUpdateAccountPreservesOmittedIdentityAndTokens(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	id, err := db.CreateAccount(ctx, domain.Account{
		Email:        "preserve@example.test",
		RefreshToken: "refresh-token",
		FlowBearer:   "flow-bearer",
		Cookies:      "cookie=value",
		ProtocolMode: "refresh_token",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	if err := db.UpdateAccount(ctx, id, domain.Account{
		Remark:             "edited",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
		ImageEnabled:       true,
		VideoEnabled:       true,
		UpscaleEnabled:     true,
		ImageConcurrency:   -1,
		VideoConcurrency:   -1,
	}); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	account, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.Email != "preserve@example.test" || account.RefreshToken != "refresh-token" || account.FlowBearer != "flow-bearer" || account.Cookies != "cookie=value" {
		t.Fatalf("omitted fields were not preserved: %+v", account)
	}
	if account.Remark != "edited" {
		t.Fatalf("remark = %q, want edited", account.Remark)
	}
}

func TestUpdateAccountClearsExplicitCredentialFields(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	id, err := db.CreateAccount(ctx, domain.Account{
		Email:                "clear@example.test",
		Name:                 "Clear Me",
		Remark:               "old remark",
		RefreshToken:         "old-refresh",
		FlowBearer:           "old-bearer",
		AccessToken:          "old-bearer",
		ProviderToken:        "old-provider",
		ProviderRefreshToken: "old-provider-refresh",
		Cookies:              "cookie=value",
		LoginAccount:         "old-login",
		LoginPassword:        "old-password",
		ProxyURL:             "http://proxy.example.test:8080",
		ProtocolMode:         "refresh_token",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	if err := db.UpdateAccount(ctx, id, domain.Account{
		ProtocolMode:       "bearer",
		FlowBearer:         "new-bearer",
		AT:                 "new-bearer",
		AutoRefreshEnabled: true,
		ImageEnabled:       true,
		VideoEnabled:       true,
		UpscaleEnabled:     true,
		ImageConcurrency:   -1,
		VideoConcurrency:   -1,
		ExplicitFields: map[string]bool{
			"protocol_mode":          true,
			"refresh_token":          true,
			"flow_bearer":            true,
			"provider_token":         true,
			"provider_refresh_token": true,
			"cookies":                true,
			"login_account":          true,
			"proxy_url":              true,
			"remark":                 true,
		},
	}); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	account, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.ProtocolMode != "bearer" ||
		account.RefreshToken != "" ||
		account.FlowBearer != "new-bearer" ||
		account.AccessToken != "new-bearer" ||
		account.ProviderToken != "" ||
		account.ProviderRefreshToken != "" ||
		account.Cookies != "" ||
		account.LoginAccount != "" ||
		account.ProxyURL != "" ||
		account.Remark != "" {
		t.Fatalf("explicit empty fields were not applied: %+v", account)
	}
	if account.Email != "clear@example.test" || account.Name != "Clear Me" || account.LoginPassword != "old-password" {
		t.Fatalf("omitted identity/password fields should be preserved: %+v", account)
	}
}

func TestUpdateAccountClearsLoginPasswordOnlyWithClearField(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	id, err := db.CreateAccount(ctx, domain.Account{
		Email:         "clear-password@example.test",
		LoginPassword: "old-password",
		ProtocolMode:  "bearer",
		FlowBearer:    "flow-bearer",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	if err := db.UpdateAccount(ctx, id, domain.Account{
		ProtocolMode:       "bearer",
		AutoRefreshEnabled: true,
		ImageEnabled:       true,
		VideoEnabled:       true,
		UpscaleEnabled:     true,
		ImageConcurrency:   -1,
		VideoConcurrency:   -1,
		ExplicitFields: map[string]bool{
			"login_password": true,
		},
	}); err != nil {
		t.Fatalf("UpdateAccount() blank password error = %v", err)
	}
	account, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.LoginPassword != "old-password" {
		t.Fatalf("blank login_password should preserve old password, got %+v", account)
	}

	if err := db.UpdateAccount(ctx, id, domain.Account{
		ProtocolMode:       "bearer",
		AutoRefreshEnabled: true,
		ImageEnabled:       true,
		VideoEnabled:       true,
		UpscaleEnabled:     true,
		ImageConcurrency:   -1,
		VideoConcurrency:   -1,
		ExplicitFields: map[string]bool{
			"login_password": true,
		},
		ClearFields: map[string]bool{
			"login_password": true,
		},
	}); err != nil {
		t.Fatalf("UpdateAccount() clear_fields password error = %v", err)
	}
	account, err = db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() after clear error = %v", err)
	}
	if account.LoginPassword != "" {
		t.Fatalf("clear_fields login_password should clear old password, got %+v", account)
	}
}

func TestCreateAccountDefaultsBearerModeWhenOnlyBearerProvided(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)

	id, err := db.CreateAccount(ctx, domain.Account{
		Email:      "bearer@example.test",
		FlowBearer: "flow-bearer",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	account, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.ProtocolMode != "bearer" {
		t.Fatalf("ProtocolMode = %q, want bearer", account.ProtocolMode)
	}
}
