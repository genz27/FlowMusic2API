package config

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultGoogleOAuthClientID = "1032626174130-533micbc9tgsei76mqhtguq07lpoe4je.apps.googleusercontent.com"

type Config struct {
	AppName                 string
	HTTPHost                string
	HTTPPort                int
	RootDir                 string
	StaticDir               string
	DataDir                 string
	CacheDir                string
	CacheEnabled            bool
	CacheTimeout            int
	CacheBaseURL            string
	CacheStorageMode        string
	CacheS3Endpoint         string
	CacheS3Region           string
	CacheS3Bucket           string
	CacheS3AccessKey        string
	CacheS3SecretKey        string
	CacheS3UseSSL           bool
	CacheS3ForcePathStyle   bool
	CacheS3Prefix           string
	CacheS3PublicBaseURL    string
	DatabaseDriver          string
	DatabaseURL             string
	PostgresHost            string
	PostgresPort            int
	PostgresUser            string
	PostgresPassword        string
	PostgresDB              string
	PostgresSSLMode         string
	PostgresMaxOpenConns    int
	FlowMusicBaseURL        string
	SupabaseBaseURL         string
	SupabaseAnonKey         string
	GoogleOAuthTokenURL     string
	GoogleOAuthClientID     string
	GoogleOAuthClientSecret string
	UpstreamTimeout         time.Duration
	GenerationTimeout       time.Duration
	StreamIdleTimeout       time.Duration
	TLSInsecureSkipVerify   bool
	AdminJWTSecret          string
	DisableWorkers          bool
	DefaultProxyURL         string
	DefaultAPIKey           string
	DefaultAdminUser        string
	DefaultAdminPassword    string
	InitialAccountEmail     string
	InitialAccountName      string
	InitialAccountRemark    string
	InitialProtocolMode     string
	InitialRefreshToken     string
	InitialFlowBearer       string
	InitialProviderToken    string
	InitialProviderRT       string
	InitialCookies          string
	InitialAccountProxy     string
	InitialAutoRefresh      bool
	InitialRefreshMins      int
	TokenRefreshLead        time.Duration
	TokenRefreshInterval    time.Duration
	StoragePresignDuration  time.Duration
	GuestDailyLimit         int
	GuestGlobalDailyLimit   int
}

func Load() Config {
	root := defaultRoot()
	loadDotEnv(root)
	dataDir := getenv("FLOWMUSIC_DATA_DIR", filepath.Join(root, "data"))
	dbDriver := strings.ToLower(getenv("FLOWMUSIC_DB_DRIVER", "sqlite"))
	if dbDriver == "pg" || dbDriver == "postgresql" {
		dbDriver = "postgres"
	}
	postgresHost := getenv("FLOWMUSIC_POSTGRES_HOST", "127.0.0.1")
	postgresPort := getenvInt("FLOWMUSIC_POSTGRES_PORT", 5432)
	postgresUser := getenv("FLOWMUSIC_POSTGRES_USER", "flowmusic")
	postgresPassword := getenv("FLOWMUSIC_POSTGRES_PASSWORD", "flowmusic")
	postgresDB := getenv("FLOWMUSIC_POSTGRES_DB", "flowmusic2api")
	postgresSSLMode := getenv("FLOWMUSIC_POSTGRES_SSLMODE", "disable")
	databaseURL := getenv("FLOWMUSIC_DATABASE_URL", "")
	if databaseURL == "" {
		if dbDriver == "postgres" {
			databaseURL = buildPostgresURL(postgresHost, postgresPort, postgresUser, postgresPassword, postgresDB, postgresSSLMode)
		} else {
			databaseURL = filepath.Join(dataDir, "flowmusic2api.db")
		}
	}
	return Config{
		AppName:                 "FlowMusic2API",
		HTTPHost:                getenv("FLOWMUSIC_HTTP_HOST", "0.0.0.0"),
		HTTPPort:                getenvInt("FLOWMUSIC_HTTP_PORT", 8000),
		RootDir:                 root,
		StaticDir:               getenv("FLOWMUSIC_STATIC_DIR", filepath.Join(root, "web", "static")),
		DataDir:                 dataDir,
		CacheDir:                getenv("FLOWMUSIC_CACHE_DIR", filepath.Join(root, "tmp")),
		CacheEnabled:            getenvBool("FLOWMUSIC_CACHE_ENABLED", false),
		CacheTimeout:            getenvInt("FLOWMUSIC_CACHE_TIMEOUT_SECONDS", 7200),
		CacheBaseURL:            getenv("FLOWMUSIC_CACHE_BASE_URL", ""),
		CacheStorageMode:        getenv("FLOWMUSIC_CACHE_STORAGE_MODE", "local"),
		CacheS3Endpoint:         getenv("FLOWMUSIC_S3_ENDPOINT", ""),
		CacheS3Region:           getenv("FLOWMUSIC_S3_REGION", ""),
		CacheS3Bucket:           getenv("FLOWMUSIC_S3_BUCKET", ""),
		CacheS3AccessKey:        getenv("FLOWMUSIC_S3_ACCESS_KEY", ""),
		CacheS3SecretKey:        getenv("FLOWMUSIC_S3_SECRET_KEY", ""),
		CacheS3UseSSL:           getenvBool("FLOWMUSIC_S3_USE_SSL", true),
		CacheS3ForcePathStyle:   getenvBool("FLOWMUSIC_S3_FORCE_PATH_STYLE", false),
		CacheS3Prefix:           getenv("FLOWMUSIC_S3_PREFIX", ""),
		CacheS3PublicBaseURL:    getenv("FLOWMUSIC_S3_PUBLIC_BASE_URL", ""),
		DatabaseDriver:          dbDriver,
		DatabaseURL:             databaseURL,
		PostgresHost:            postgresHost,
		PostgresPort:            postgresPort,
		PostgresUser:            postgresUser,
		PostgresPassword:        postgresPassword,
		PostgresDB:              postgresDB,
		PostgresSSLMode:         postgresSSLMode,
		PostgresMaxOpenConns:    getenvInt("FLOWMUSIC_POSTGRES_MAX_OPEN_CONNS", 32),
		FlowMusicBaseURL:        strings.TrimRight(getenv("FLOWMUSIC_BASE_URL", "https://www.flowmusic.app"), "/"),
		SupabaseBaseURL:         strings.TrimRight(getenv("FLOWMUSIC_SUPABASE_BASE_URL", "https://sb.flowmusic.app"), "/"),
		SupabaseAnonKey:         getenv("FLOWMUSIC_SUPABASE_ANON_KEY", ""),
		GoogleOAuthTokenURL:     getenv("FLOWMUSIC_GOOGLE_OAUTH_TOKEN_URL", "https://oauth2.googleapis.com/token"),
		GoogleOAuthClientID:     getenv("FLOWMUSIC_GOOGLE_OAUTH_CLIENT_ID", DefaultGoogleOAuthClientID),
		GoogleOAuthClientSecret: getenv("FLOWMUSIC_GOOGLE_OAUTH_CLIENT_SECRET", ""),
		UpstreamTimeout:         time.Duration(getenvInt("FLOWMUSIC_UPSTREAM_TIMEOUT_SECONDS", 120)) * time.Second,
		GenerationTimeout:       time.Duration(getenvInt("FLOWMUSIC_GENERATION_TIMEOUT_SECONDS", 600)) * time.Second,
		StreamIdleTimeout:       time.Duration(getenvInt("FLOWMUSIC_STREAM_IDLE_TIMEOUT_SECONDS", 90)) * time.Second,
		TLSInsecureSkipVerify:   getenvBool("FLOWMUSIC_TLS_INSECURE_SKIP_VERIFY", false),
		AdminJWTSecret:          getenv("FLOWMUSIC_ADMIN_JWT_SECRET", "flowmusic2api-dev-secret"),
		DisableWorkers:          getenvBool("FLOWMUSIC_DISABLE_WORKERS", false),
		DefaultProxyURL:         getenv("FLOWMUSIC_PROXY_URL", ""),
		DefaultAPIKey:           getenv("FLOWMUSIC_DEFAULT_API_KEY", "fm123456"),
		DefaultAdminUser:        getenv("FLOWMUSIC_ADMIN_USER", "admin"),
		DefaultAdminPassword:    getenv("FLOWMUSIC_ADMIN_PASSWORD", "admin"),
		InitialAccountEmail:     getenv("FLOWMUSIC_INITIAL_ACCOUNT_EMAIL", ""),
		InitialAccountName:      getenv("FLOWMUSIC_INITIAL_ACCOUNT_NAME", ""),
		InitialAccountRemark:    getenv("FLOWMUSIC_INITIAL_ACCOUNT_REMARK", ""),
		InitialProtocolMode:     getenv("FLOWMUSIC_INITIAL_PROTOCOL_MODE", ""),
		InitialRefreshToken:     getenv("FLOWMUSIC_INITIAL_REFRESH_TOKEN", ""),
		InitialFlowBearer:       getenv("FLOWMUSIC_INITIAL_FLOW_BEARER", ""),
		InitialProviderToken:    getenv("FLOWMUSIC_INITIAL_PROVIDER_TOKEN", ""),
		InitialProviderRT:       getenv("FLOWMUSIC_INITIAL_PROVIDER_REFRESH_TOKEN", ""),
		InitialCookies:          getenv("FLOWMUSIC_INITIAL_COOKIES", ""),
		InitialAccountProxy:     getenv("FLOWMUSIC_INITIAL_ACCOUNT_PROXY_URL", ""),
		InitialAutoRefresh:      getenvBool("FLOWMUSIC_INITIAL_AUTO_REFRESH_ENABLED", true),
		InitialRefreshMins:      getenvInt("FLOWMUSIC_INITIAL_REFRESH_INTERVAL_MINUTES", 60),
		TokenRefreshLead:        time.Duration(getenvInt("FLOWMUSIC_TOKEN_REFRESH_LEAD_SECONDS", 600)) * time.Second,
		TokenRefreshInterval:    time.Duration(getenvInt("FLOWMUSIC_TOKEN_REFRESH_INTERVAL_SECONDS", 60)) * time.Second,
		StoragePresignDuration:  time.Duration(getenvInt("FLOWMUSIC_STORAGE_PRESIGN_SECONDS", 604800)) * time.Second,
		GuestDailyLimit:         getenvInt("FLOWMUSIC_GUEST_DAILY_LIMIT", 3),
		GuestGlobalDailyLimit:   getenvInt("FLOWMUSIC_GUEST_GLOBAL_DAILY_LIMIT", 0),
	}
}

func loadDotEnv(root string) {
	for _, name := range []string{".env", ".env.local"} {
		envPath := filepath.Join(root, name)
		data, err := os.ReadFile(envPath)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			value = strings.Trim(value, "\"'")
			if key == "" || os.Getenv(key) != "" {
				continue
			}
			_ = os.Setenv(key, value)
		}
	}
}

func buildPostgresURL(host string, port int, user, password, database, sslMode string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "127.0.0.1"
	}
	if port <= 0 {
		port = 5432
	}
	database = strings.Trim(strings.TrimSpace(database), "/")
	if database == "" {
		database = "flowmusic2api"
	}
	sslMode = strings.TrimSpace(sslMode)
	if sslMode == "" {
		sslMode = "disable"
	}
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(strings.TrimSpace(user), password),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/" + database,
	}
	query := dsn.Query()
	query.Set("sslmode", sslMode)
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func (c Config) ListenAddr() string {
	return c.HTTPHost + ":" + strconv.Itoa(c.HTTPPort)
}

func defaultRoot() string {
	if value := strings.TrimSpace(os.Getenv("FLOWMUSIC_ROOT")); value != "" {
		return value
	}
	if cwd, err := os.Getwd(); err == nil && strings.TrimSpace(cwd) != "" {
		return cwd
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		return filepath.Dir(exe)
	}
	return "."
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
