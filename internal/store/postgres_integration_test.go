package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
)

func TestPostgresMigrateEnsureDefaultsIntegration(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("FLOWMUSIC_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set FLOWMUSIC_TEST_POSTGRES_DSN to run PostgreSQL integration test")
	}
	ctx := context.Background()
	cfg := config.Config{
		DatabaseDriver:       "postgres",
		DatabaseURL:          dsn,
		PostgresMaxOpenConns: 4,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin-secret",
		DefaultAPIKey:        "test-api-key",
		TokenRefreshInterval: time.Hour,
		TokenRefreshLead:     10 * time.Minute,
		GenerationTimeout:    15 * time.Minute,
	}
	db, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New(postgres) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(postgres) error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults(postgres) error = %v", err)
	}
	email := "pg-integration-" + strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339Nano), ":", "-") + "@example.test"
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:              email,
		RefreshToken:       "refresh-token",
		FlowBearer:         "flow-bearer",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount(postgres) error = %v", err)
	}
	account, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount(postgres) error = %v", err)
	}
	if account.Email != email || account.ST != "refresh-token" || account.AT != "flow-bearer" {
		t.Fatalf("unexpected postgres account: %+v", account)
	}
	if err := db.UpdateAccountFields(ctx, id, map[string]any{
		"tokens_remaining": 12.5,
		"credits":          7,
		"last_refresh_at":  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpdateAccountFields(postgres) error = %v", err)
	}
	account, err = db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount(postgres after update fields) error = %v", err)
	}
	if account.TokensRemaining != 12.5 || account.Credits != 7 || account.LastRefreshAt == nil {
		t.Fatalf("postgres account field update was not persisted: %+v", account)
	}
	if err := db.UpdateCacheConfig(ctx, domain.CacheConfig{
		Enabled:          true,
		Timeout:          3600,
		BaseURL:          "https://cdn.example.test",
		StorageMode:      "r2",
		S3Endpoint:       "https://example.r2.cloudflarestorage.com",
		S3Bucket:         "flowmusic-cache",
		S3AccessKey:      "access-key",
		S3SecretKey:      "secret-key",
		S3UseSSL:         true,
		S3ForcePathStyle: true,
		S3Prefix:         "flow-assets",
		S3PublicBaseURL:  "https://cdn.example.test/cache",
	}); err != nil {
		t.Fatalf("UpdateCacheConfig(postgres) error = %v", err)
	}
	cacheCfg, err := db.GetCacheConfig(ctx)
	if err != nil {
		t.Fatalf("GetCacheConfig(postgres) error = %v", err)
	}
	if cacheCfg.StorageMode != "r2" || cacheCfg.S3Bucket != "flowmusic-cache" || !cacheCfg.S3ForcePathStyle {
		t.Fatalf("unexpected postgres cache config: %+v", cacheCfg)
	}
	logID, err := db.CreateRequestLog(ctx, domain.RequestLog{
		AccountID:       &id,
		Operation:       "postgres.integration",
		RequestBody:     `{"prompt":"pg"}`,
		ResponseBody:    `{"status":"running"}`,
		ResponseExcerpt: `running`,
		StatusCode:      102,
		DurationMS:      123,
		StatusText:      "running",
		Progress:        40,
	})
	if err != nil {
		t.Fatalf("CreateRequestLog(postgres) error = %v", err)
	}
	active, err := db.GetActiveLogs(ctx, 10)
	if err != nil {
		t.Fatalf("GetActiveLogs(postgres) error = %v", err)
	}
	foundActive := false
	for _, item := range active {
		if item.ID == logID && item.AccountEmail == email {
			foundActive = true
		}
	}
	if !foundActive {
		t.Fatalf("postgres active logs missing log %d: %+v", logID, active)
	}
	if err := db.UpdateRequestLog(ctx, logID, domain.RequestLog{
		AccountID:       &id,
		RequestBody:     `{"prompt":"pg"}`,
		ResponseBody:    `{"status":"ok"}`,
		ResponseExcerpt: `ok`,
		StatusCode:      200,
		DurationMS:      456,
		StatusText:      "success",
		Progress:        100,
	}); err != nil {
		t.Fatalf("UpdateRequestLog(postgres) error = %v", err)
	}
	detail, err := db.GetLogDetail(ctx, logID)
	if err != nil {
		t.Fatalf("GetLogDetail(postgres) error = %v", err)
	}
	if detail.StatusCode != 200 || detail.DurationMS != 456 || detail.AccountEmail != email {
		t.Fatalf("unexpected postgres log detail: %+v", detail)
	}
	stats, err := db.GetDashboardStats(ctx)
	if err != nil {
		t.Fatalf("GetDashboardStats(postgres) error = %v", err)
	}
	if stats.TotalTokens < 1 || stats.ActiveTokens < 1 {
		t.Fatalf("unexpected postgres dashboard stats: %+v", stats)
	}
	t.Cleanup(func() { _ = db.DeleteAccount(context.Background(), id) })
}
