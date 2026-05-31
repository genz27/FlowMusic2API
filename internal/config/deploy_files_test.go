package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type composeSpec struct {
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
}

type composeService struct {
	Build       any               `yaml:"build"`
	Image       string            `yaml:"image"`
	Restart     string            `yaml:"restart"`
	Ports       []string          `yaml:"ports"`
	Environment map[string]string `yaml:"environment"`
	Volumes     []string          `yaml:"volumes"`
	DependsOn   map[string]any    `yaml:"depends_on"`
	Healthcheck map[string]any    `yaml:"healthcheck"`
	SecurityOpt []string          `yaml:"security_opt"`
	Logging     map[string]any    `yaml:"logging"`
}

func TestDockerComposePostgresFile(t *testing.T) {
	t.Parallel()

	spec := readComposeSpec(t, "docker-compose.yml")
	app, ok := spec.Services["flowmusic2api"]
	if !ok {
		t.Fatalf("docker-compose.yml 缺少 flowmusic2api 服务")
	}
	if _, ok := spec.Services["postgres"]; !ok {
		t.Fatalf("docker-compose.yml 缺少 postgres 服务")
	}
	if app.Environment["FLOWMUSIC_DB_DRIVER"] != "postgres" {
		t.Fatalf("postgres compose should set FLOWMUSIC_DB_DRIVER=postgres, got %q", app.Environment["FLOWMUSIC_DB_DRIVER"])
	}
	assertComposeServiceHardened(t, "flowmusic2api", app)
	assertComposeHTTPPort(t, "docker-compose.yml", app)
	assertRuntimeEnvKeys(t, "docker-compose.yml", app.Environment, commonComposeEnvKeys()...)
	assertRuntimeEnvKeys(t, "docker-compose.yml", app.Environment,
		"FLOWMUSIC_POSTGRES_HOST",
		"FLOWMUSIC_POSTGRES_PORT",
		"FLOWMUSIC_POSTGRES_USER",
		"FLOWMUSIC_POSTGRES_PASSWORD",
		"FLOWMUSIC_POSTGRES_DB",
		"FLOWMUSIC_POSTGRES_SSLMODE",
		"FLOWMUSIC_POSTGRES_MAX_OPEN_CONNS",
	)
	databaseURL := app.Environment["FLOWMUSIC_DATABASE_URL"]
	if databaseURL != "${FLOWMUSIC_DATABASE_URL:-}" {
		t.Fatalf("postgres compose database URL should be optional override, got %q", app.Environment["FLOWMUSIC_DATABASE_URL"])
	}
	if app.Environment["FLOWMUSIC_POSTGRES_HOST"] != "${FLOWMUSIC_POSTGRES_HOST:-postgres}" ||
		app.Environment["FLOWMUSIC_POSTGRES_PORT"] != "${FLOWMUSIC_POSTGRES_PORT:-5432}" ||
		app.Environment["FLOWMUSIC_POSTGRES_SSLMODE"] != "${FLOWMUSIC_POSTGRES_SSLMODE:-disable}" {
		t.Fatalf("postgres compose should pass split host/port/sslmode, got %#v", app.Environment)
	}
	dependency, ok := app.DependsOn["postgres"]
	if !ok {
		t.Fatalf("flowmusic2api service should depend on postgres health")
	}
	if condition := composeMapString(dependency, "condition"); condition != "service_healthy" {
		t.Fatalf("flowmusic2api postgres dependency condition = %q, want service_healthy", condition)
	}
	if len(app.Healthcheck) == 0 {
		t.Fatalf("flowmusic2api service should define a healthcheck")
	}
	postgres := spec.Services["postgres"]
	assertComposeServiceHardened(t, "postgres", postgres)
	for _, pair := range []struct {
		appKey      string
		postgresKey string
		want        string
	}{
		{"FLOWMUSIC_POSTGRES_USER", "POSTGRES_USER", "${FLOWMUSIC_POSTGRES_USER:-flowmusic}"},
		{"FLOWMUSIC_POSTGRES_PASSWORD", "POSTGRES_PASSWORD", "${FLOWMUSIC_POSTGRES_PASSWORD:-flowmusic}"},
		{"FLOWMUSIC_POSTGRES_DB", "POSTGRES_DB", "${FLOWMUSIC_POSTGRES_DB:-flowmusic2api}"},
	} {
		if app.Environment[pair.appKey] != pair.want || postgres.Environment[pair.postgresKey] != pair.want {
			t.Fatalf("postgres compose should reuse %s/%s=%q, got app=%q postgres=%q",
				pair.appKey, pair.postgresKey, pair.want, app.Environment[pair.appKey], postgres.Environment[pair.postgresKey])
		}
	}
	if !containsString(postgres.Volumes, "postgres-data:/var/lib/postgresql/data") {
		t.Fatalf("postgres service should persist postgres-data volume, got %#v", postgres.Volumes)
	}
	if !healthcheckContains(postgres.Healthcheck, "pg_isready") {
		t.Fatalf("postgres service healthcheck should use pg_isready, got %#v", postgres.Healthcheck)
	}
	if !healthcheckContains(postgres.Healthcheck, "$${POSTGRES_USER}") ||
		!healthcheckContains(postgres.Healthcheck, "$${POSTGRES_DB}") {
		t.Fatalf("postgres service healthcheck should use configured database credentials, got %#v", postgres.Healthcheck)
	}
	if _, ok := spec.Volumes["postgres-data"]; !ok {
		t.Fatalf("docker-compose.yml 缺少 postgres-data volume")
	}
}

func TestDockerComposeSQLiteFile(t *testing.T) {
	t.Parallel()

	spec := readComposeSpec(t, "docker-compose.sqlite.yml")
	app, ok := spec.Services["flowmusic2api"]
	if !ok {
		t.Fatalf("docker-compose.sqlite.yml 缺少 flowmusic2api 服务")
	}
	if _, ok := spec.Services["postgres"]; ok {
		t.Fatalf("sqlite compose should not include postgres service")
	}
	if app.Environment["FLOWMUSIC_DB_DRIVER"] != "sqlite" {
		t.Fatalf("sqlite compose should set FLOWMUSIC_DB_DRIVER=sqlite, got %q", app.Environment["FLOWMUSIC_DB_DRIVER"])
	}
	assertComposeServiceHardened(t, "flowmusic2api", app)
	assertComposeHTTPPort(t, "docker-compose.sqlite.yml", app)
	assertRuntimeEnvKeys(t, "docker-compose.sqlite.yml", app.Environment, commonComposeEnvKeys()...)
	if app.Environment["FLOWMUSIC_DATABASE_URL"] != "/app/data/flowmusic2api.db" {
		t.Fatalf("sqlite compose should persist DB under /app/data, got %q", app.Environment["FLOWMUSIC_DATABASE_URL"])
	}
	if !containsString(app.Volumes, "./data:/app/data") || !containsString(app.Volumes, "./tmp:/app/tmp") {
		t.Fatalf("sqlite compose should persist ./data and ./tmp, got %#v", app.Volumes)
	}
	if len(app.Healthcheck) == 0 {
		t.Fatalf("flowmusic2api sqlite service should define a healthcheck")
	}
}

func TestDeploymentFilesCoverRuntimeConfiguration(t *testing.T) {
	t.Parallel()

	envExample := readProjectFile(t, ".env.example")
	for _, fragment := range []string{
		"FLOWMUSIC_HTTP_BIND=127.0.0.1",
	} {
		if !strings.Contains(envExample, fragment) {
			t.Fatalf(".env.example 缺少片段 %q", fragment)
		}
	}
	for _, key := range []string{
		"FLOWMUSIC_HTTP_HOST=",
		"FLOWMUSIC_HTTP_BIND=",
		"FLOWMUSIC_HTTP_PORT=",
		"FLOWMUSIC_ROOT=",
		"FLOWMUSIC_STATIC_DIR=",
		"FLOWMUSIC_DATA_DIR=",
		"FLOWMUSIC_CACHE_DIR=",
		"FLOWMUSIC_CACHE_ENABLED=",
		"FLOWMUSIC_CACHE_TIMEOUT_SECONDS=",
		"FLOWMUSIC_CACHE_BASE_URL=",
		"FLOWMUSIC_CACHE_STORAGE_MODE=",
		"FLOWMUSIC_S3_ENDPOINT=",
		"FLOWMUSIC_S3_REGION=",
		"FLOWMUSIC_S3_BUCKET=",
		"FLOWMUSIC_S3_ACCESS_KEY=",
		"FLOWMUSIC_S3_SECRET_KEY=",
		"FLOWMUSIC_S3_USE_SSL=",
		"FLOWMUSIC_S3_FORCE_PATH_STYLE=",
		"FLOWMUSIC_S3_PREFIX=",
		"FLOWMUSIC_S3_PUBLIC_BASE_URL=",
		"FLOWMUSIC_DB_DRIVER=",
		"FLOWMUSIC_DATABASE_URL=",
		"FLOWMUSIC_POSTGRES_MAX_OPEN_CONNS=",
		"FLOWMUSIC_POSTGRES_HOST=",
		"FLOWMUSIC_POSTGRES_PORT=",
		"FLOWMUSIC_POSTGRES_USER=",
		"FLOWMUSIC_POSTGRES_PASSWORD=",
		"FLOWMUSIC_POSTGRES_DB=",
		"FLOWMUSIC_POSTGRES_SSLMODE=",
		"FLOWMUSIC_ADMIN_USER=",
		"FLOWMUSIC_ADMIN_PASSWORD=",
		"FLOWMUSIC_DEFAULT_API_KEY=",
		"FLOWMUSIC_ADMIN_JWT_SECRET=",
		"FLOWMUSIC_INITIAL_ACCOUNT_EMAIL=",
		"FLOWMUSIC_INITIAL_ACCOUNT_NAME=",
		"FLOWMUSIC_INITIAL_ACCOUNT_REMARK=",
		"FLOWMUSIC_INITIAL_PROTOCOL_MODE=",
		"FLOWMUSIC_INITIAL_REFRESH_TOKEN=",
		"FLOWMUSIC_INITIAL_FLOW_BEARER=",
		"FLOWMUSIC_INITIAL_PROVIDER_TOKEN=",
		"FLOWMUSIC_INITIAL_PROVIDER_REFRESH_TOKEN=",
		"FLOWMUSIC_INITIAL_COOKIES=",
		"FLOWMUSIC_INITIAL_ACCOUNT_PROXY_URL=",
		"FLOWMUSIC_INITIAL_AUTO_REFRESH_ENABLED=",
		"FLOWMUSIC_INITIAL_REFRESH_INTERVAL_MINUTES=",
		"FLOWMUSIC_BASE_URL=",
		"FLOWMUSIC_SUPABASE_BASE_URL=",
		"FLOWMUSIC_SUPABASE_ANON_KEY=",
		"FLOWMUSIC_GOOGLE_OAUTH_TOKEN_URL=",
		"FLOWMUSIC_GOOGLE_OAUTH_CLIENT_ID=",
		"FLOWMUSIC_GOOGLE_OAUTH_CLIENT_SECRET=",
		"FLOWMUSIC_PROXY_URL=",
		"FLOWMUSIC_TLS_INSECURE_SKIP_VERIFY=",
		"FLOWMUSIC_UPSTREAM_TIMEOUT_SECONDS=",
		"FLOWMUSIC_GENERATION_TIMEOUT_SECONDS=",
		"FLOWMUSIC_STREAM_IDLE_TIMEOUT_SECONDS=",
		"FLOWMUSIC_DISABLE_WORKERS=",
		"FLOWMUSIC_TOKEN_REFRESH_LEAD_SECONDS=",
		"FLOWMUSIC_TOKEN_REFRESH_INTERVAL_SECONDS=",
		"FLOWMUSIC_STORAGE_PRESIGN_SECONDS=",
		"FLOWMUSIC_LIVE_TEST=",
		"FLOWMUSIC_LIVE_PROTOCOL_MODE=",
		"FLOWMUSIC_LIVE_FLOW_BEARER=",
		"FLOWMUSIC_LIVE_REFRESH_TOKEN=",
		"FLOWMUSIC_LIVE_PROVIDER_TOKEN=",
		"FLOWMUSIC_LIVE_PROVIDER_REFRESH_TOKEN=",
		"FLOWMUSIC_LIVE_COOKIES=",
		"FLOWMUSIC_LIVE_PROMPT=",
	} {
		if !strings.Contains(envExample, key) {
			t.Fatalf(".env.example 缺少 %s", key)
		}
	}

	dockerfile := readProjectFile(t, "Dockerfile")
	for _, fragment := range []string{
		"FROM golang:",
		"go mod download",
		"go build",
		"COPY web",
		"HEALTHCHECK",
		"CMD [\"/app/flowmusic2api\"]",
	} {
		if !strings.Contains(dockerfile, fragment) {
			t.Fatalf("Dockerfile 缺少片段 %q", fragment)
		}
	}

	readme := readProjectFile(t, "README.md")
	for _, fragment := range []string{
		"FLOWMUSIC_HTTP_BIND=0.0.0.0",
		"`FLOWMUSIC_HTTP_BIND` 控制宿主机发布地址",
		"`FLOWMUSIC_HTTP_HOST` 控制容器内服务监听地址",
	} {
		if !strings.Contains(readme, fragment) {
			t.Fatalf("README.md 缺少片段 %q", fragment)
		}
	}
}

func TestDockerIgnoreExcludesLocalSecretsAndArtifacts(t *testing.T) {
	t.Parallel()

	content := readProjectFile(t, ".dockerignore")
	for _, pattern := range []string{
		"cookie.txt",
		"*.har",
		".playwright-mcp/",
		"data/",
		"tmp/",
		"*.db",
		"*.exe",
		".env.*",
		"!.env.example",
	} {
		if !strings.Contains(content, pattern) {
			t.Fatalf(".dockerignore 缺少 %s", pattern)
		}
	}
}

func TestManagementPageHidesUnusedAccountCapabilityFields(t *testing.T) {
	t.Parallel()

	content := readProjectFile(t, filepath.Join("web", "static", "manage.html"))
	for _, fragment := range []string{
		"addTokenImageEnabled",
		"addTokenVideoEnabled",
		"addTokenUpscaleEnabled",
		"editTokenImageEnabled",
		"editTokenVideoEnabled",
		"editTokenUpscaleEnabled",
		"image_enabled:form.imageEnabled",
		"video_enabled:form.videoEnabled",
		"upscale_enabled:form.upscaleEnabled",
		"image_enabled:t.image_enabled!==false",
		"video_enabled:t.video_enabled!==false",
		"upscale_enabled:t.upscale_enabled!==false",
	} {
		if strings.Contains(content, fragment) {
			t.Fatalf("manage.html 不应再暴露账号能力字段片段 %q", fragment)
		}
	}
}

func TestManagementPageCoversProviderCredentialFields(t *testing.T) {
	t.Parallel()

	content := readProjectFile(t, filepath.Join("web", "static", "manage.html"))
	for _, fragment := range []string{
		"addTokenProviderToken",
		"addTokenProviderRefreshToken",
		"editTokenProviderToken",
		"editTokenProviderRefreshToken",
		"provider_token:form.providerToken",
		"provider_refresh_token:form.providerRefreshToken",
		"token.provider_token",
		"token.provider_refresh_token",
		"provider_token:t.provider_token",
		"provider_refresh_token:t.provider_refresh_token",
	} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("manage.html 缺少 provider 凭据字段片段 %q", fragment)
		}
	}
}

func TestManagementPageCoversImportPayloadCompatibility(t *testing.T) {
	t.Parallel()

	content := readProjectFile(t, filepath.Join("web", "static", "manage.html"))
	for _, fragment := range []string{
		"Array.isArray(importData)?importData:(importData&&Array.isArray(importData.tokens)?importData.tokens:null)",
		"JSON格式错误：应为数组或包含 tokens 数组的对象",
		"body:JSON.stringify({tokens:tokens})",
		"d.detail||d.message||'未知错误'",
	} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("manage.html 缺少导入兼容片段 %q", fragment)
		}
	}
}

func TestManagementPageDebugCopyMatchesImplementedLogging(t *testing.T) {
	t.Parallel()

	content := readProjectFile(t, filepath.Join("web", "static", "manage.html"))
	if strings.Contains(content, "logs.txt") {
		t.Fatalf("manage.html should not claim debug logs are written to logs.txt")
	}
	for _, fragment := range []string{
		"管理页日志接口",
		"仅建议临时排错时开启",
	} {
		if !strings.Contains(content, fragment) {
			t.Fatalf("manage.html 缺少调试文案片段 %q", fragment)
		}
	}
}

func readComposeSpec(t *testing.T, name string) composeSpec {
	t.Helper()
	var spec composeSpec
	if err := yaml.Unmarshal([]byte(readProjectFile(t, name)), &spec); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if len(spec.Services) == 0 {
		t.Fatalf("%s 缺少 services", name)
	}
	return spec
}

func readProjectFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func commonComposeEnvKeys() []string {
	return []string{
		"FLOWMUSIC_HTTP_HOST",
		"FLOWMUSIC_HTTP_PORT",
		"FLOWMUSIC_ROOT",
		"FLOWMUSIC_STATIC_DIR",
		"FLOWMUSIC_DATA_DIR",
		"FLOWMUSIC_CACHE_DIR",
		"FLOWMUSIC_CACHE_ENABLED",
		"FLOWMUSIC_CACHE_TIMEOUT_SECONDS",
		"FLOWMUSIC_CACHE_BASE_URL",
		"FLOWMUSIC_CACHE_STORAGE_MODE",
		"FLOWMUSIC_S3_ENDPOINT",
		"FLOWMUSIC_S3_REGION",
		"FLOWMUSIC_S3_BUCKET",
		"FLOWMUSIC_S3_ACCESS_KEY",
		"FLOWMUSIC_S3_SECRET_KEY",
		"FLOWMUSIC_S3_USE_SSL",
		"FLOWMUSIC_S3_FORCE_PATH_STYLE",
		"FLOWMUSIC_S3_PREFIX",
		"FLOWMUSIC_S3_PUBLIC_BASE_URL",
		"FLOWMUSIC_ADMIN_USER",
		"FLOWMUSIC_ADMIN_PASSWORD",
		"FLOWMUSIC_DEFAULT_API_KEY",
		"FLOWMUSIC_ADMIN_JWT_SECRET",
		"FLOWMUSIC_INITIAL_ACCOUNT_EMAIL",
		"FLOWMUSIC_INITIAL_ACCOUNT_NAME",
		"FLOWMUSIC_INITIAL_ACCOUNT_REMARK",
		"FLOWMUSIC_INITIAL_PROTOCOL_MODE",
		"FLOWMUSIC_INITIAL_REFRESH_TOKEN",
		"FLOWMUSIC_INITIAL_FLOW_BEARER",
		"FLOWMUSIC_INITIAL_PROVIDER_TOKEN",
		"FLOWMUSIC_INITIAL_PROVIDER_REFRESH_TOKEN",
		"FLOWMUSIC_INITIAL_COOKIES",
		"FLOWMUSIC_INITIAL_ACCOUNT_PROXY_URL",
		"FLOWMUSIC_INITIAL_AUTO_REFRESH_ENABLED",
		"FLOWMUSIC_INITIAL_REFRESH_INTERVAL_MINUTES",
		"FLOWMUSIC_BASE_URL",
		"FLOWMUSIC_SUPABASE_BASE_URL",
		"FLOWMUSIC_SUPABASE_ANON_KEY",
		"FLOWMUSIC_GOOGLE_OAUTH_TOKEN_URL",
		"FLOWMUSIC_GOOGLE_OAUTH_CLIENT_ID",
		"FLOWMUSIC_GOOGLE_OAUTH_CLIENT_SECRET",
		"FLOWMUSIC_PROXY_URL",
		"FLOWMUSIC_TLS_INSECURE_SKIP_VERIFY",
		"FLOWMUSIC_UPSTREAM_TIMEOUT_SECONDS",
		"FLOWMUSIC_GENERATION_TIMEOUT_SECONDS",
		"FLOWMUSIC_STREAM_IDLE_TIMEOUT_SECONDS",
		"FLOWMUSIC_DISABLE_WORKERS",
		"FLOWMUSIC_TOKEN_REFRESH_LEAD_SECONDS",
		"FLOWMUSIC_TOKEN_REFRESH_INTERVAL_SECONDS",
		"FLOWMUSIC_STORAGE_PRESIGN_SECONDS",
	}
}

func assertRuntimeEnvKeys(t *testing.T, name string, env map[string]string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := env[key]; !ok {
			t.Fatalf("%s 缺少运行期环境变量 %s", name, key)
		}
	}
}

func assertComposeServiceHardened(t *testing.T, name string, svc composeService) {
	t.Helper()
	if svc.Restart != "unless-stopped" {
		t.Fatalf("%s restart = %q, want unless-stopped", name, svc.Restart)
	}
	if !containsString(svc.SecurityOpt, "no-new-privileges:true") {
		t.Fatalf("%s should enable no-new-privileges, got %#v", name, svc.SecurityOpt)
	}
	if len(svc.Logging) == 0 {
		t.Fatalf("%s should define logging rotation", name)
	}
}

func assertComposeHTTPPort(t *testing.T, name string, svc composeService) {
	t.Helper()
	want := "${FLOWMUSIC_HTTP_BIND:-127.0.0.1}:${FLOWMUSIC_HTTP_PORT:-8000}:8000"
	if !containsString(svc.Ports, want) {
		t.Fatalf("%s should publish HTTP on %q, got %#v", name, want, svc.Ports)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func composeMapString(value any, key string) string {
	switch typed := value.(type) {
	case map[string]any:
		if item, ok := typed[key].(string); ok {
			return item
		}
	case map[any]any:
		if item, ok := typed[key].(string); ok {
			return item
		}
	}
	return ""
}

func healthcheckContains(healthcheck map[string]any, fragment string) bool {
	for _, value := range healthcheck {
		if anyValueContains(value, fragment) {
			return true
		}
	}
	return false
}

func anyValueContains(value any, fragment string) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, fragment)
	case []any:
		for _, item := range typed {
			if anyValueContains(item, fragment) {
				return true
			}
		}
	case []string:
		for _, item := range typed {
			if strings.Contains(item, fragment) {
				return true
			}
		}
	}
	return false
}
