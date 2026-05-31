package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"flowmusic2api/internal/domain"

	"golang.org/x/crypto/bcrypt"
)

func (s *Store) EnsureDefaults(ctx context.Context) error {
	admin, err := s.GetAdminConfig(ctx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if admin == nil {
		hash, err := bcrypt.GenerateFromPassword([]byte(s.cfg.DefaultAdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, s.bind(`INSERT INTO admin_config (id, username, password_hash, api_key, debug_enabled, error_ban_threshold, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`), 1, s.cfg.DefaultAdminUser, string(hash), s.cfg.DefaultAPIKey, false, 3)
		if err != nil {
			return err
		}
	}
	cacheCfg, err := s.defaultCacheConfig()
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO cache_config (id, enabled, timeout, base_url, storage_mode, s3_endpoint, s3_region, s3_bucket, s3_access_key, s3_secret_key, s3_use_ssl, s3_force_path_style, s3_prefix, s3_public_base_url, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ON CONFLICT(id) DO NOTHING`),
		1,
		cacheCfg.Enabled,
		cacheCfg.Timeout,
		cacheCfg.BaseURL,
		cacheCfg.StorageMode,
		cacheCfg.S3Endpoint,
		cacheCfg.S3Region,
		cacheCfg.S3Bucket,
		cacheCfg.S3AccessKey,
		cacheCfg.S3SecretKey,
		cacheCfg.S3UseSSL,
		cacheCfg.S3ForcePathStyle,
		cacheCfg.S3Prefix,
		cacheCfg.S3PublicBaseURL,
	); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO generation_config (id, timeout, max_retries, image_timeout, video_timeout, created_at, updated_at) VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ON CONFLICT(id) DO NOTHING`), 1, int(s.cfg.GenerationTimeout.Seconds()), 3, int(s.cfg.GenerationTimeout.Seconds()), int(s.cfg.GenerationTimeout.Seconds())); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO token_refresh_config (id, enabled, at_auto_refresh_enabled, refresh_interval_minutes, refresh_before_expiry_seconds, created_at, updated_at) VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ON CONFLICT(id) DO NOTHING`), 1, true, true, int(s.cfg.TokenRefreshInterval.Minutes()), int(s.cfg.TokenRefreshLead.Seconds())); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO proxy_config (id, proxy_enabled, proxy_url, media_proxy_enabled, media_proxy_url, created_at, updated_at) VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ON CONFLICT(id) DO NOTHING`), 1, s.cfg.DefaultProxyURL != "", s.cfg.DefaultProxyURL, s.cfg.DefaultProxyURL != "", s.cfg.DefaultProxyURL); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO call_logic_config (id, call_mode, created_at, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ON CONFLICT(id) DO NOTHING`), 1, "default"); err != nil {
		return err
	}
	if err := s.ensureInitialAccount(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) defaultCacheConfig() (domain.CacheConfig, error) {
	storageMode, err := domain.NormalizeCacheStorageMode(s.cfg.CacheStorageMode)
	if err != nil {
		return domain.CacheConfig{}, err
	}
	cfg := domain.CacheConfig{
		Enabled:          s.cfg.CacheEnabled,
		Timeout:          s.cfg.CacheTimeout,
		BaseURL:          strings.TrimSpace(s.cfg.CacheBaseURL),
		StorageMode:      storageMode,
		S3Endpoint:       strings.TrimSpace(s.cfg.CacheS3Endpoint),
		S3Region:         strings.TrimSpace(s.cfg.CacheS3Region),
		S3Bucket:         strings.TrimSpace(s.cfg.CacheS3Bucket),
		S3AccessKey:      strings.TrimSpace(s.cfg.CacheS3AccessKey),
		S3SecretKey:      s.cfg.CacheS3SecretKey,
		S3UseSSL:         s.cfg.CacheS3UseSSL,
		S3ForcePathStyle: s.cfg.CacheS3ForcePathStyle,
		S3Prefix:         strings.Trim(strings.TrimSpace(s.cfg.CacheS3Prefix), "/"),
		S3PublicBaseURL:  strings.TrimSpace(s.cfg.CacheS3PublicBaseURL),
	}
	if cfg.StorageMode == "r2" {
		cfg.S3PublicBaseURL = ""
	}
	if cfg.Timeout < 0 {
		return domain.CacheConfig{}, errors.New("FLOWMUSIC_CACHE_TIMEOUT_SECONDS must be >= 0")
	}
	if domain.IsObjectStorageMode(cfg.StorageMode) {
		if cfg.S3Endpoint == "" {
			return domain.CacheConfig{}, errors.New("FLOWMUSIC_S3_ENDPOINT is required when FLOWMUSIC_CACHE_STORAGE_MODE is s3 or r2")
		}
		if cfg.S3Bucket == "" {
			return domain.CacheConfig{}, errors.New("FLOWMUSIC_S3_BUCKET is required when FLOWMUSIC_CACHE_STORAGE_MODE is s3 or r2")
		}
	}
	return cfg, nil
}

func (s *Store) ensureInitialAccount(ctx context.Context) error {
	account, ok, err := s.initialAccountFromConfig()
	if err != nil || !ok {
		return err
	}
	_, _, err = s.UpsertAccount(ctx, account)
	return err
}

func (s *Store) initialAccountFromConfig() (domain.Account, bool, error) {
	refreshToken := strings.TrimSpace(s.cfg.InitialRefreshToken)
	flowBearer := strings.TrimSpace(s.cfg.InitialFlowBearer)
	providerToken := strings.TrimSpace(s.cfg.InitialProviderToken)
	providerRefreshToken := strings.TrimSpace(s.cfg.InitialProviderRT)
	cookies := strings.TrimSpace(s.cfg.InitialCookies)
	if refreshToken == "" && flowBearer == "" && providerToken == "" && providerRefreshToken == "" && cookies == "" {
		return domain.Account{}, false, nil
	}
	protocolMode := domain.NormalizeProtocolMode(s.cfg.InitialProtocolMode)
	if protocolMode == "refresh_token" && refreshToken == "" {
		switch {
		case cookies != "":
			protocolMode = "protocol"
		case flowBearer != "":
			protocolMode = "bearer"
		}
	}
	if protocolMode == "bearer" && flowBearer == "" {
		return domain.Account{}, false, errors.New("FLOWMUSIC_INITIAL_FLOW_BEARER is required when FLOWMUSIC_INITIAL_PROTOCOL_MODE is bearer")
	}
	if protocolMode == "protocol" && cookies == "" {
		return domain.Account{}, false, errors.New("FLOWMUSIC_INITIAL_COOKIES is required when FLOWMUSIC_INITIAL_PROTOCOL_MODE is protocol")
	}
	if protocolMode == "refresh_token" && refreshToken == "" && providerToken == "" && providerRefreshToken == "" {
		return domain.Account{}, false, errors.New("FLOWMUSIC_INITIAL_REFRESH_TOKEN, FLOWMUSIC_INITIAL_PROVIDER_TOKEN, or FLOWMUSIC_INITIAL_PROVIDER_REFRESH_TOKEN is required when FLOWMUSIC_INITIAL_PROTOCOL_MODE is refresh_token")
	}
	email := strings.TrimSpace(s.cfg.InitialAccountEmail)
	if email == "" {
		email = "initial-account@local"
	}
	refreshInterval := s.cfg.InitialRefreshMins
	if refreshInterval <= 0 {
		refreshInterval = 60
	}
	return domain.Account{
		Email:                email,
		Name:                 strings.TrimSpace(s.cfg.InitialAccountName),
		Remark:               strings.TrimSpace(s.cfg.InitialAccountRemark),
		ProtocolMode:         protocolMode,
		RefreshToken:         refreshToken,
		ST:                   refreshToken,
		AccessToken:          flowBearer,
		AT:                   flowBearer,
		FlowBearer:           flowBearer,
		ProviderToken:        providerToken,
		ProviderRefreshToken: providerRefreshToken,
		Cookies:              cookies,
		ProxyURL:             strings.TrimSpace(s.cfg.InitialAccountProxy),
		AutoRefreshEnabled:   s.cfg.InitialAutoRefresh,
		RefreshIntervalMins:  refreshInterval,
		ImageEnabled:         true,
		VideoEnabled:         true,
		UpscaleEnabled:       true,
		CapabilityFlagsSet:   true,
		ImageConcurrency:     -1,
		VideoConcurrency:     -1,
	}, true, nil
}

func (s *Store) GetAdminConfig(ctx context.Context) (*domain.AdminConfig, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT id, username, password_hash, api_key, debug_enabled, error_ban_threshold, guest_trial_enabled, max_daily_guest_uses, guest_global_daily_limit, created_at, updated_at FROM admin_config WHERE id = ?`), 1)
	var cfg domain.AdminConfig
	var created, updated nullableTime
	if err := row.Scan(&cfg.ID, &cfg.Username, &cfg.PasswordHash, &cfg.APIKey, &cfg.DebugEnabled, &cfg.ErrorBan, &cfg.GuestTrialEnabled, &cfg.MaxDailyGuestUses, &cfg.GuestGlobalDailyLimit, &created, &updated); err != nil {
		return nil, translateErr(err)
	}
	cfg.CreatedAt = created.Ptr()
	cfg.UpdatedAt = updated.Ptr()
	return &cfg, nil
}

func (s *Store) UpdateAdminPassword(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE admin_config SET username = ?, password_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), username, passwordHash, 1)
	return err
}

func (s *Store) UpdateAPIKey(ctx context.Context, apiKey string) error {
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE admin_config SET api_key = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), apiKey, 1)
	return err
}

func (s *Store) UpdateAdminConfig(ctx context.Context, username, apiKey string, debugEnabled bool, errorBanThreshold int, guestTrialEnabled bool, maxDailyGuestUses int, guestGlobalDailyLimit int) error {
	if errorBanThreshold <= 0 {
		errorBanThreshold = 3
	}
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE admin_config SET username = ?, api_key = ?, debug_enabled = ?, error_ban_threshold = ?, guest_trial_enabled = ?, max_daily_guest_uses = ?, guest_global_daily_limit = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), username, apiKey, debugEnabled, errorBanThreshold, guestTrialEnabled, maxDailyGuestUses, guestGlobalDailyLimit, 1)
	return err
}

func (s *Store) UpdateDebug(ctx context.Context, enabled bool) error {
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE admin_config SET debug_enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), enabled, 1)
	return err
}

func (s *Store) GetCacheConfig(ctx context.Context) (*domain.CacheConfig, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT id, enabled, timeout, base_url, storage_mode, s3_endpoint, s3_region, s3_bucket, s3_access_key, s3_secret_key, s3_use_ssl, s3_force_path_style, s3_prefix, s3_public_base_url, updated_at FROM cache_config WHERE id = ?`), 1)
	var cfg domain.CacheConfig
	var updated nullableTime
	if err := row.Scan(&cfg.ID, &cfg.Enabled, &cfg.Timeout, &cfg.BaseURL, &cfg.StorageMode, &cfg.S3Endpoint, &cfg.S3Region, &cfg.S3Bucket, &cfg.S3AccessKey, &cfg.S3SecretKey, &cfg.S3UseSSL, &cfg.S3ForcePathStyle, &cfg.S3Prefix, &cfg.S3PublicBaseURL, &updated); err != nil {
		return nil, translateErr(err)
	}
	cfg.UpdatedAt = updated.Ptr()
	return &cfg, nil
}

func (s *Store) UpdateCacheConfig(ctx context.Context, cfg domain.CacheConfig) error {
	storageMode, err := domain.NormalizeCacheStorageMode(cfg.StorageMode)
	if err != nil {
		return err
	}
	cfg.StorageMode = storageMode
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.S3Endpoint = strings.TrimSpace(cfg.S3Endpoint)
	cfg.S3Region = strings.TrimSpace(cfg.S3Region)
	cfg.S3Bucket = strings.TrimSpace(cfg.S3Bucket)
	cfg.S3AccessKey = strings.TrimSpace(cfg.S3AccessKey)
	cfg.S3Prefix = strings.Trim(strings.TrimSpace(cfg.S3Prefix), "/")
	cfg.S3PublicBaseURL = strings.TrimSpace(cfg.S3PublicBaseURL)
	if cfg.StorageMode == "r2" {
		cfg.S3PublicBaseURL = ""
	}
	if domain.IsObjectStorageMode(cfg.StorageMode) {
		if cfg.S3Endpoint == "" {
			return errors.New("S3/R2 endpoint is required")
		}
		if cfg.S3Bucket == "" {
			return errors.New("S3/R2 bucket is required")
		}
	}
	_, err = s.db.ExecContext(ctx, s.bind(`UPDATE cache_config SET enabled = ?, timeout = ?, base_url = ?, storage_mode = ?, s3_endpoint = ?, s3_region = ?, s3_bucket = ?, s3_access_key = ?, s3_secret_key = ?, s3_use_ssl = ?, s3_force_path_style = ?, s3_prefix = ?, s3_public_base_url = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), cfg.Enabled, cfg.Timeout, cfg.BaseURL, cfg.StorageMode, cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3UseSSL, cfg.S3ForcePathStyle, cfg.S3Prefix, cfg.S3PublicBaseURL, 1)
	return err
}

func (s *Store) GetGenerationConfig(ctx context.Context) (*domain.GenerationConfig, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT id, timeout, max_retries, image_timeout, video_timeout, updated_at FROM generation_config WHERE id = ?`), 1)
	var cfg domain.GenerationConfig
	var updated nullableTime
	if err := row.Scan(&cfg.ID, &cfg.Timeout, &cfg.MaxRetries, &cfg.ImageTimeout, &cfg.VideoTimeout, &updated); err != nil {
		return nil, translateErr(err)
	}
	cfg.UpdatedAt = updated.Ptr()
	return &cfg, nil
}

func (s *Store) UpdateGenerationConfig(ctx context.Context, cfg domain.GenerationConfig) error {
	if cfg.Timeout <= 0 {
		switch {
		case cfg.ImageTimeout > 0 && cfg.VideoTimeout > 0:
			cfg.Timeout = cfg.ImageTimeout + cfg.VideoTimeout
		case cfg.VideoTimeout > 0:
			cfg.Timeout = cfg.VideoTimeout
		case cfg.ImageTimeout > 0:
			cfg.Timeout = cfg.ImageTimeout
		}
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 1
	}
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE generation_config SET timeout = ?, max_retries = ?, image_timeout = ?, video_timeout = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), cfg.Timeout, cfg.MaxRetries, cfg.ImageTimeout, cfg.VideoTimeout, 1)
	return err
}

func (s *Store) GetTokenRefreshConfig(ctx context.Context) (*domain.TokenRefreshConfig, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT id, enabled, at_auto_refresh_enabled, refresh_interval_minutes, refresh_before_expiry_seconds, updated_at FROM token_refresh_config WHERE id = ?`), 1)
	var cfg domain.TokenRefreshConfig
	var updated nullableTime
	if err := row.Scan(&cfg.ID, &cfg.Enabled, &cfg.ATAutoRefreshEnabled, &cfg.RefreshIntervalMins, &cfg.RefreshBeforeExpiryMs, &updated); err != nil {
		return nil, translateErr(err)
	}
	cfg.UpdatedAt = updated.Ptr()
	return &cfg, nil
}

func (s *Store) UpdateTokenRefreshConfig(ctx context.Context, cfg domain.TokenRefreshConfig) error {
	if cfg.RefreshIntervalMins <= 0 {
		cfg.RefreshIntervalMins = int(time.Hour.Minutes())
	}
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE token_refresh_config SET enabled = ?, at_auto_refresh_enabled = ?, refresh_interval_minutes = ?, refresh_before_expiry_seconds = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), cfg.Enabled, cfg.ATAutoRefreshEnabled, cfg.RefreshIntervalMins, cfg.RefreshBeforeExpiryMs, 1)
	return err
}

func (s *Store) GetProxyConfig(ctx context.Context) (*domain.ProxyConfig, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT id, proxy_enabled, proxy_url, media_proxy_enabled, media_proxy_url, updated_at FROM proxy_config WHERE id = ?`), 1)
	var cfg domain.ProxyConfig
	var updated nullableTime
	if err := row.Scan(&cfg.ID, &cfg.ProxyEnabled, &cfg.ProxyURL, &cfg.MediaProxyEnabled, &cfg.MediaProxyURL, &updated); err != nil {
		return nil, translateErr(err)
	}
	cfg.UpdatedAt = updated.Ptr()
	return &cfg, nil
}

func (s *Store) UpdateProxyConfig(ctx context.Context, cfg domain.ProxyConfig) error {
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE proxy_config SET proxy_enabled = ?, proxy_url = ?, media_proxy_enabled = ?, media_proxy_url = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), cfg.ProxyEnabled, cfg.ProxyURL, cfg.MediaProxyEnabled, cfg.MediaProxyURL, 1)
	return err
}

func (s *Store) GetCallLogicConfig(ctx context.Context) (*domain.CallLogicConfig, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT id, call_mode, updated_at FROM call_logic_config WHERE id = ?`), 1)
	var cfg domain.CallLogicConfig
	var updated nullableTime
	if err := row.Scan(&cfg.ID, &cfg.CallMode, &updated); err != nil {
		return nil, translateErr(err)
	}
	cfg.UpdatedAt = updated.Ptr()
	if cfg.CallMode == "" {
		cfg.CallMode = "default"
	}
	return &cfg, nil
}

func (s *Store) UpdateCallLogicConfig(ctx context.Context, cfg domain.CallLogicConfig) error {
	callMode, err := domain.NormalizeCallMode(cfg.CallMode)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.bind(`UPDATE call_logic_config SET call_mode = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), callMode, 1)
	return err
}
