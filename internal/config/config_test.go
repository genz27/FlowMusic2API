package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadParsesRuntimeEnvironment(t *testing.T) {
	clearFlowMusicEnv(t)

	root := filepath.Join(t.TempDir(), "app")
	staticDir := filepath.Join(root, "static")
	dataDir := filepath.Join(root, "data")
	cacheDir := filepath.Join(root, "cache")

	t.Setenv("FLOWMUSIC_ROOT", " "+root+" ")
	t.Setenv("FLOWMUSIC_HTTP_HOST", " 127.0.0.1 ")
	t.Setenv("FLOWMUSIC_HTTP_PORT", "18080")
	t.Setenv("FLOWMUSIC_STATIC_DIR", " "+staticDir+" ")
	t.Setenv("FLOWMUSIC_DATA_DIR", " "+dataDir+" ")
	t.Setenv("FLOWMUSIC_CACHE_DIR", " "+cacheDir+" ")
	t.Setenv("FLOWMUSIC_CACHE_ENABLED", "true")
	t.Setenv("FLOWMUSIC_CACHE_TIMEOUT_SECONDS", "3600")
	t.Setenv("FLOWMUSIC_CACHE_BASE_URL", " https://local.example.test/tmp ")
	t.Setenv("FLOWMUSIC_CACHE_STORAGE_MODE", " r2 ")
	t.Setenv("FLOWMUSIC_S3_ENDPOINT", " https://r2.example.test ")
	t.Setenv("FLOWMUSIC_S3_REGION", " auto ")
	t.Setenv("FLOWMUSIC_S3_BUCKET", " flowmusic-cache ")
	t.Setenv("FLOWMUSIC_S3_ACCESS_KEY", " access-key ")
	t.Setenv("FLOWMUSIC_S3_SECRET_KEY", " secret-key ")
	t.Setenv("FLOWMUSIC_S3_USE_SSL", "true")
	t.Setenv("FLOWMUSIC_S3_FORCE_PATH_STYLE", "1")
	t.Setenv("FLOWMUSIC_S3_PREFIX", " flow-assets ")
	t.Setenv("FLOWMUSIC_S3_PUBLIC_BASE_URL", " https://cdn.example.test/cache ")
	t.Setenv("FLOWMUSIC_DB_DRIVER", " PostgreS ")
	t.Setenv("FLOWMUSIC_DATABASE_URL", " postgres://user:pass@db:5432/app?sslmode=disable ")
	t.Setenv("FLOWMUSIC_POSTGRES_HOST", " postgres ")
	t.Setenv("FLOWMUSIC_POSTGRES_PORT", "5433")
	t.Setenv("FLOWMUSIC_POSTGRES_USER", " flow-user ")
	t.Setenv("FLOWMUSIC_POSTGRES_PASSWORD", " flow-password ")
	t.Setenv("FLOWMUSIC_POSTGRES_DB", " flow-db ")
	t.Setenv("FLOWMUSIC_POSTGRES_SSLMODE", " require ")
	t.Setenv("FLOWMUSIC_POSTGRES_MAX_OPEN_CONNS", "12")
	t.Setenv("FLOWMUSIC_BASE_URL", " https://flow.example.test/ ")
	t.Setenv("FLOWMUSIC_SUPABASE_BASE_URL", " https://supabase.example.test/ ")
	t.Setenv("FLOWMUSIC_SUPABASE_ANON_KEY", " anon-key ")
	t.Setenv("FLOWMUSIC_GOOGLE_OAUTH_TOKEN_URL", " https://oauth.example.test/token ")
	t.Setenv("FLOWMUSIC_GOOGLE_OAUTH_CLIENT_ID", " google-client-id ")
	t.Setenv("FLOWMUSIC_GOOGLE_OAUTH_CLIENT_SECRET", " google-client-secret ")
	t.Setenv("FLOWMUSIC_UPSTREAM_TIMEOUT_SECONDS", "7")
	t.Setenv("FLOWMUSIC_GENERATION_TIMEOUT_SECONDS", "11")
	t.Setenv("FLOWMUSIC_STREAM_IDLE_TIMEOUT_SECONDS", "13")
	t.Setenv("FLOWMUSIC_TLS_INSECURE_SKIP_VERIFY", "yes")
	t.Setenv("FLOWMUSIC_ADMIN_JWT_SECRET", " jwt-secret ")
	t.Setenv("FLOWMUSIC_DISABLE_WORKERS", "on")
	t.Setenv("FLOWMUSIC_PROXY_URL", " http://proxy.example.test:8080 ")
	t.Setenv("FLOWMUSIC_DEFAULT_API_KEY", " api-key ")
	t.Setenv("FLOWMUSIC_ADMIN_USER", " root ")
	t.Setenv("FLOWMUSIC_ADMIN_PASSWORD", " password ")
	t.Setenv("FLOWMUSIC_INITIAL_ACCOUNT_EMAIL", " seed@example.test ")
	t.Setenv("FLOWMUSIC_INITIAL_ACCOUNT_NAME", " Seed Account ")
	t.Setenv("FLOWMUSIC_INITIAL_ACCOUNT_REMARK", " seeded by env ")
	t.Setenv("FLOWMUSIC_INITIAL_PROTOCOL_MODE", " protocol ")
	t.Setenv("FLOWMUSIC_INITIAL_REFRESH_TOKEN", " refresh-token ")
	t.Setenv("FLOWMUSIC_INITIAL_FLOW_BEARER", " flow-bearer ")
	t.Setenv("FLOWMUSIC_INITIAL_PROVIDER_TOKEN", " provider-token ")
	t.Setenv("FLOWMUSIC_INITIAL_PROVIDER_REFRESH_TOKEN", " provider-refresh-token ")
	t.Setenv("FLOWMUSIC_INITIAL_COOKIES", " cookie=value ")
	t.Setenv("FLOWMUSIC_INITIAL_ACCOUNT_PROXY_URL", " http://account-proxy.example.test:8080 ")
	t.Setenv("FLOWMUSIC_INITIAL_AUTO_REFRESH_ENABLED", "false")
	t.Setenv("FLOWMUSIC_INITIAL_REFRESH_INTERVAL_MINUTES", "15")
	t.Setenv("FLOWMUSIC_TOKEN_REFRESH_LEAD_SECONDS", "17")
	t.Setenv("FLOWMUSIC_TOKEN_REFRESH_INTERVAL_SECONDS", "19")
	t.Setenv("FLOWMUSIC_STORAGE_PRESIGN_SECONDS", "23")

	cfg := Load()

	if cfg.ListenAddr() != "127.0.0.1:18080" {
		t.Fatalf("ListenAddr() = %q", cfg.ListenAddr())
	}
	if cfg.RootDir != root || cfg.StaticDir != staticDir || cfg.DataDir != dataDir || cfg.CacheDir != cacheDir {
		t.Fatalf("unexpected dirs: root=%q static=%q data=%q cache=%q", cfg.RootDir, cfg.StaticDir, cfg.DataDir, cfg.CacheDir)
	}
	if !cfg.CacheEnabled || cfg.CacheTimeout != 3600 || cfg.CacheBaseURL != "https://local.example.test/tmp" || cfg.CacheStorageMode != "r2" {
		t.Fatalf("unexpected cache config: enabled=%v timeout=%d base=%q mode=%q", cfg.CacheEnabled, cfg.CacheTimeout, cfg.CacheBaseURL, cfg.CacheStorageMode)
	}
	if cfg.CacheS3Endpoint != "https://r2.example.test" ||
		cfg.CacheS3Region != "auto" ||
		cfg.CacheS3Bucket != "flowmusic-cache" ||
		cfg.CacheS3AccessKey != "access-key" ||
		cfg.CacheS3SecretKey != "secret-key" ||
		!cfg.CacheS3UseSSL ||
		!cfg.CacheS3ForcePathStyle ||
		cfg.CacheS3Prefix != "flow-assets" ||
		cfg.CacheS3PublicBaseURL != "https://cdn.example.test/cache" {
		t.Fatalf("unexpected S3/R2 env config: %+v", cfg)
	}
	if cfg.DatabaseDriver != "postgres" || cfg.DatabaseURL != "postgres://user:pass@db:5432/app?sslmode=disable" || cfg.PostgresMaxOpenConns != 12 {
		t.Fatalf("unexpected database config: driver=%q url=%q max=%d", cfg.DatabaseDriver, cfg.DatabaseURL, cfg.PostgresMaxOpenConns)
	}
	if cfg.PostgresHost != "postgres" ||
		cfg.PostgresPort != 5433 ||
		cfg.PostgresUser != "flow-user" ||
		cfg.PostgresPassword != "flow-password" ||
		cfg.PostgresDB != "flow-db" ||
		cfg.PostgresSSLMode != "require" {
		t.Fatalf("unexpected postgres split config: %+v", cfg)
	}
	if cfg.FlowMusicBaseURL != "https://flow.example.test" || cfg.SupabaseBaseURL != "https://supabase.example.test" {
		t.Fatalf("unexpected upstream URLs: flow=%q supabase=%q", cfg.FlowMusicBaseURL, cfg.SupabaseBaseURL)
	}
	if cfg.SupabaseAnonKey != "anon-key" || cfg.AdminJWTSecret != "jwt-secret" || cfg.DefaultProxyURL != "http://proxy.example.test:8080" {
		t.Fatalf("unexpected string config: anon=%q jwt=%q proxy=%q", cfg.SupabaseAnonKey, cfg.AdminJWTSecret, cfg.DefaultProxyURL)
	}
	if cfg.GoogleOAuthTokenURL != "https://oauth.example.test/token" || cfg.GoogleOAuthClientID != "google-client-id" || cfg.GoogleOAuthClientSecret != "google-client-secret" {
		t.Fatalf("unexpected Google OAuth config: token=%q client_id=%q secret=%q", cfg.GoogleOAuthTokenURL, cfg.GoogleOAuthClientID, cfg.GoogleOAuthClientSecret)
	}
	if cfg.DefaultAPIKey != "api-key" || cfg.DefaultAdminUser != "root" || cfg.DefaultAdminPassword != "password" {
		t.Fatalf("unexpected default credentials: key=%q user=%q password=%q", cfg.DefaultAPIKey, cfg.DefaultAdminUser, cfg.DefaultAdminPassword)
	}
	if cfg.InitialAccountEmail != "seed@example.test" ||
		cfg.InitialAccountName != "Seed Account" ||
		cfg.InitialAccountRemark != "seeded by env" ||
		cfg.InitialProtocolMode != "protocol" ||
		cfg.InitialRefreshToken != "refresh-token" ||
		cfg.InitialFlowBearer != "flow-bearer" ||
		cfg.InitialProviderToken != "provider-token" ||
		cfg.InitialProviderRT != "provider-refresh-token" ||
		cfg.InitialCookies != "cookie=value" ||
		cfg.InitialAccountProxy != "http://account-proxy.example.test:8080" ||
		cfg.InitialAutoRefresh ||
		cfg.InitialRefreshMins != 15 {
		t.Fatalf("unexpected initial account env config: %+v", cfg)
	}
	if !cfg.TLSInsecureSkipVerify || !cfg.DisableWorkers {
		t.Fatalf("expected bool env values to parse true: tls=%v workers=%v", cfg.TLSInsecureSkipVerify, cfg.DisableWorkers)
	}
	if cfg.UpstreamTimeout != 7*time.Second ||
		cfg.GenerationTimeout != 11*time.Second ||
		cfg.StreamIdleTimeout != 13*time.Second ||
		cfg.TokenRefreshLead != 17*time.Second ||
		cfg.TokenRefreshInterval != 19*time.Second ||
		cfg.StoragePresignDuration != 23*time.Second {
		t.Fatalf("unexpected durations: upstream=%s generation=%s idle=%s lead=%s interval=%s presign=%s",
			cfg.UpstreamTimeout, cfg.GenerationTimeout, cfg.StreamIdleTimeout,
			cfg.TokenRefreshLead, cfg.TokenRefreshInterval, cfg.StoragePresignDuration)
	}
}

func TestLoadBuildsEscapedPostgresURLFromSplitEnvironment(t *testing.T) {
	clearFlowMusicEnv(t)
	t.Setenv("FLOWMUSIC_DB_DRIVER", "postgres")
	t.Setenv("FLOWMUSIC_POSTGRES_HOST", "db.example.test")
	t.Setenv("FLOWMUSIC_POSTGRES_PORT", "15432")
	t.Setenv("FLOWMUSIC_POSTGRES_USER", "flow:user")
	t.Setenv("FLOWMUSIC_POSTGRES_PASSWORD", "p@ss:word#1")
	t.Setenv("FLOWMUSIC_POSTGRES_DB", "flowmusic2api")
	t.Setenv("FLOWMUSIC_POSTGRES_SSLMODE", "verify-full")

	cfg := Load()

	want := "postgres://flow%3Auser:p%40ss%3Aword%231@db.example.test:15432/flowmusic2api?sslmode=verify-full"
	if cfg.DatabaseURL != want {
		t.Fatalf("DatabaseURL = %q, want %q", cfg.DatabaseURL, want)
	}
}

func TestLoadUsesLoginAllGoogleOAuthClientIDByDefault(t *testing.T) {
	clearFlowMusicEnv(t)
	t.Setenv("FLOWMUSIC_ROOT", t.TempDir())

	cfg := Load()

	if cfg.GoogleOAuthClientID != DefaultGoogleOAuthClientID {
		t.Fatalf("GoogleOAuthClientID = %q, want login all HAR client id %q", cfg.GoogleOAuthClientID, DefaultGoogleOAuthClientID)
	}
}

func TestLoadFallsBackForInvalidScalarEnvironment(t *testing.T) {
	clearFlowMusicEnv(t)
	t.Setenv("FLOWMUSIC_ROOT", t.TempDir())
	t.Setenv("FLOWMUSIC_HTTP_PORT", "not-a-port")
	t.Setenv("FLOWMUSIC_POSTGRES_PORT", "not-a-port")
	t.Setenv("FLOWMUSIC_POSTGRES_MAX_OPEN_CONNS", "many")
	t.Setenv("FLOWMUSIC_UPSTREAM_TIMEOUT_SECONDS", "slow")
	t.Setenv("FLOWMUSIC_CACHE_TIMEOUT_SECONDS", "slow")
	t.Setenv("FLOWMUSIC_CACHE_ENABLED", "maybe")
	t.Setenv("FLOWMUSIC_INITIAL_AUTO_REFRESH_ENABLED", "maybe")
	t.Setenv("FLOWMUSIC_INITIAL_REFRESH_INTERVAL_MINUTES", "often")
	t.Setenv("FLOWMUSIC_S3_USE_SSL", "maybe")
	t.Setenv("FLOWMUSIC_S3_FORCE_PATH_STYLE", "maybe")
	t.Setenv("FLOWMUSIC_GENERATION_TIMEOUT_SECONDS", "slow")
	t.Setenv("FLOWMUSIC_STREAM_IDLE_TIMEOUT_SECONDS", "slow")
	t.Setenv("FLOWMUSIC_TOKEN_REFRESH_LEAD_SECONDS", "soon")
	t.Setenv("FLOWMUSIC_TOKEN_REFRESH_INTERVAL_SECONDS", "often")
	t.Setenv("FLOWMUSIC_STORAGE_PRESIGN_SECONDS", "long")
	t.Setenv("FLOWMUSIC_TLS_INSECURE_SKIP_VERIFY", "maybe")
	t.Setenv("FLOWMUSIC_DISABLE_WORKERS", "maybe")

	cfg := Load()

	if cfg.HTTPPort != 8000 || cfg.PostgresMaxOpenConns != 32 {
		t.Fatalf("invalid int env should fall back, got port=%d max=%d", cfg.HTTPPort, cfg.PostgresMaxOpenConns)
	}
	if cfg.CacheTimeout != 7200 {
		t.Fatalf("invalid cache timeout should fall back, got %d", cfg.CacheTimeout)
	}
	if !cfg.InitialAutoRefresh || cfg.InitialRefreshMins != 60 {
		t.Fatalf("invalid initial account env should fall back, got auto=%v interval=%d", cfg.InitialAutoRefresh, cfg.InitialRefreshMins)
	}
	if cfg.UpstreamTimeout != 120*time.Second ||
		cfg.GenerationTimeout != 600*time.Second ||
		cfg.StreamIdleTimeout != 90*time.Second ||
		cfg.TokenRefreshLead != 600*time.Second ||
		cfg.TokenRefreshInterval != 60*time.Second ||
		cfg.StoragePresignDuration != 604800*time.Second {
		t.Fatalf("invalid duration env should fall back: %+v", cfg)
	}
	if cfg.CacheEnabled || !cfg.CacheS3UseSSL || cfg.CacheS3ForcePathStyle || cfg.TLSInsecureSkipVerify || cfg.DisableWorkers {
		t.Fatalf("invalid bool env should fall back, got cache=%v s3ssl=%v pathstyle=%v tls=%v workers=%v",
			cfg.CacheEnabled, cfg.CacheS3UseSSL, cfg.CacheS3ForcePathStyle, cfg.TLSInsecureSkipVerify, cfg.DisableWorkers)
	}
}

func clearFlowMusicEnv(t *testing.T) {
	t.Helper()
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "FLOWMUSIC_") {
			t.Setenv(key, "")
		}
	}
}
