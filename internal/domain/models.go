package domain

import (
	"fmt"
	"strings"
	"time"
)

const DefaultGenerationModelID = "lyria"

const (
	ProtocolModeRefreshToken = "refresh_token"
	ProtocolModeBearer       = "bearer"
	ProtocolModeProtocol     = "protocol"
)

type GenerationModel struct {
	ID                 string
	Name               string
	Description        string
	FlowMusicModelName string
	FlowMusicMode      string
	// SelectedModel is FlowMusic client_context.selected_model (public_name).
	// HAR 2026-07-30: "Lyria 3.5" (flagship default), "Lyria 3 Pro" (legacy).
	SelectedModel      string
	GhostwriterVersion string
	Aliases            []string
}

var generationModels = []GenerationModel{
	{
		ID:                 "lyria",
		Name:               "Lyria 3.5",
		Description:        "Top performing, flagship model (Lyria 3.5)",
		FlowMusicModelName: "producer:standard",
		FlowMusicMode:      "standard",
		SelectedModel:      "Lyria 3.5",
		GhostwriterVersion: "standard",
		Aliases:            []string{"lyria-3.5", "lyria-standard", "flowmusic-producer-standard", "flowmusic-standard", "flowmusic"},
	},
	{
		ID:                 "lyria-fast",
		Name:               "Lyria 3.5 Fast",
		Description:        "Lyria 3.5 alias using FlowMusic standard mode",
		FlowMusicModelName: "producer:standard",
		FlowMusicMode:      "standard",
		SelectedModel:      "Lyria 3.5",
		GhostwriterVersion: "standard",
		Aliases:            []string{"lyria-3.5-fast"},
	},
	{
		ID:                 "lyria-pro",
		Name:               "Lyria 3 Pro",
		Description:        "Legacy model (Lyria 3 Pro), deeper composition reasoning",
		FlowMusicModelName: "producer:standard",
		FlowMusicMode:      "standard",
		SelectedModel:      "Lyria 3 Pro",
		GhostwriterVersion: "pro",
		Aliases:            []string{"lyria-3-pro", "lyria-3.pro"},
	},
	{
		ID:                 "lyria-pro-fast",
		Name:               "Lyria 3 Pro Fast",
		Description:        "Legacy Lyria 3 Pro alias using FlowMusic standard mode",
		FlowMusicModelName: "producer:standard",
		FlowMusicMode:      "standard",
		SelectedModel:      "Lyria 3 Pro",
		GhostwriterVersion: "pro",
		Aliases:            []string{"lyria-3-pro-fast"},
	},
}

func GenerationModels() []GenerationModel {
	out := make([]GenerationModel, len(generationModels))
	for i, model := range generationModels {
		model.Aliases = append([]string(nil), model.Aliases...)
		out[i] = model
	}
	return out
}

func ResolveGenerationModel(modelID string) (GenerationModel, bool) {
	normalized := NormalizeGenerationModelID(modelID)
	if normalized == "" {
		normalized = DefaultGenerationModelID
	}
	for _, model := range generationModels {
		if model.ID == normalized {
			return model, true
		}
		for _, alias := range model.Aliases {
			if NormalizeGenerationModelID(alias) == normalized {
				return model, true
			}
		}
	}
	return GenerationModel{}, false
}

func NormalizeGenerationModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

type AdminConfig struct {
	ID                int64      `json:"id"`
	Username          string     `json:"admin_username"`
	PasswordHash      string     `json:"-"`
	APIKey            string     `json:"api_key"`
	DebugEnabled      bool       `json:"debug_enabled"`
	ErrorBan          int        `json:"error_ban_threshold"`
	GuestTrialEnabled bool       `json:"guest_trial_enabled"`
	MaxDailyGuestUses     int `json:"max_daily_guest_uses"`
	GuestGlobalDailyLimit int `json:"guest_global_daily_limit"`
	CreatedAt         *time.Time `json:"created_at,omitempty"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
}

type CacheConfig struct {
	ID               int64      `json:"id"`
	Enabled          bool       `json:"enabled"`
	Timeout          int        `json:"timeout"`
	BaseURL          string     `json:"base_url"`
	StorageMode      string     `json:"storage_mode"`
	S3Endpoint       string     `json:"s3_endpoint"`
	S3Region         string     `json:"s3_region"`
	S3Bucket         string     `json:"s3_bucket"`
	S3AccessKey      string     `json:"s3_access_key"`
	S3SecretKey      string     `json:"s3_secret_key"`
	S3UseSSL         bool       `json:"s3_use_ssl"`
	S3ForcePathStyle bool       `json:"s3_force_path_style"`
	S3Prefix         string     `json:"s3_prefix"`
	S3PublicBaseURL  string     `json:"s3_public_base_url"`
	EffectiveBaseURL string     `json:"effective_base_url,omitempty"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
}

func NormalizeCacheStorageMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "local":
		return "local", nil
	case "s3":
		return "s3", nil
	case "r2":
		return "r2", nil
	default:
		return "", fmt.Errorf("unsupported cache storage_mode %q (allowed: local, s3, r2)", mode)
	}
}

func IsObjectStorageMode(mode string) bool {
	normalized, err := NormalizeCacheStorageMode(mode)
	return err == nil && (normalized == "s3" || normalized == "r2")
}

type GenerationConfig struct {
	ID           int64      `json:"id"`
	Timeout      int        `json:"timeout"`
	MaxRetries   int        `json:"max_retries"`
	ImageTimeout int        `json:"image_timeout"`
	VideoTimeout int        `json:"video_timeout"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
}

type TokenRefreshConfig struct {
	ID                    int64      `json:"id"`
	Enabled               bool       `json:"enabled"`
	ATAutoRefreshEnabled  bool       `json:"at_auto_refresh_enabled"`
	RefreshIntervalMins   int        `json:"refresh_interval_minutes"`
	RefreshBeforeExpiryMs int        `json:"refresh_before_expiry_seconds"`
	UpdatedAt             *time.Time `json:"updated_at,omitempty"`
}

type CallLogicConfig struct {
	ID        int64      `json:"id"`
	CallMode  string     `json:"call_mode"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

func NormalizeCallMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "default":
		return "default", nil
	case "polling":
		return "polling", nil
	default:
		return "", fmt.Errorf("unsupported call_mode %q (allowed: default, polling)", mode)
	}
}

type ProxyConfig struct {
	ID                int64      `json:"id"`
	ProxyEnabled      bool       `json:"proxy_enabled"`
	ProxyURL          string     `json:"proxy_url"`
	MediaProxyEnabled bool       `json:"media_proxy_enabled"`
	MediaProxyURL     string     `json:"media_proxy_url"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
}

type Account struct {
	ID                    int64      `json:"id"`
	Email                 string     `json:"email"`
	Name                  string     `json:"name"`
	Remark                string     `json:"remark"`
	IsActive              bool       `json:"is_active"`
	ProtocolMode          string     `json:"protocol_mode"`
	RefreshToken          string     `json:"refresh_token,omitempty"`
	AccessToken           string     `json:"access_token,omitempty"`
	ProviderToken         string     `json:"provider_token,omitempty"`
	ProviderRefreshToken  string     `json:"provider_refresh_token,omitempty"`
	FlowBearer            string     `json:"flow_bearer,omitempty"`
	Cookies               string     `json:"google_cookies,omitempty"`
	LoginAccount          string     `json:"login_account"`
	LoginPassword         string     `json:"-"`
	ProxyURL              string     `json:"proxy_url"`
	AutoRefreshEnabled    bool       `json:"auto_refresh_enabled"`
	RefreshIntervalMins   int        `json:"refresh_interval_minutes"`
	ExpiresAt             *time.Time `json:"expires_at,omitempty"`
	ATExpires             *time.Time `json:"at_expires,omitempty"`
	LastRefreshAt         *time.Time `json:"last_st_refresh_at,omitempty"`
	LastRefreshResult     string     `json:"last_st_refresh_result"`
	LastUsedAt            *time.Time `json:"last_used_at,omitempty"`
	Credits               int        `json:"credits"`
	TokensRemaining       float64    `json:"tokens_remaining"`
	SubscriptionTier      string     `json:"user_paygate_tier"`
	UseCount              int        `json:"use_count"`
	MusicCount            int        `json:"music_count"`
	TodayMusicCount       int        `json:"today_music_count"`
	ImageCount            int        `json:"image_count"`
	VideoCount            int        `json:"video_count"`
	ErrorCount            int        `json:"error_count"`
	TodayErrorCount       int        `json:"today_error_count"`
	ConsecutiveErrorCount int        `json:"consecutive_error_count"`
	CreatedAt             *time.Time `json:"created_at,omitempty"`
	UpdatedAt             *time.Time `json:"updated_at,omitempty"`

	// 导入导出兼容字段：保留历史 st/at 命名。
	ST                 string          `json:"st,omitempty"`
	AT                 string          `json:"at,omitempty"`
	ImageEnabled       bool            `json:"image_enabled"`
	VideoEnabled       bool            `json:"video_enabled"`
	UpscaleEnabled     bool            `json:"upscale_enabled"`
	CapabilityFlagsSet bool            `json:"-"`
	ImageConcurrency   int             `json:"image_concurrency"`
	VideoConcurrency   int             `json:"video_concurrency"`
	ExplicitFields     map[string]bool `json:"-"`
	ClearFields        map[string]bool `json:"-"`
}

func NormalizeProtocolMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "session", "refresh", "supabase":
		return "refresh_token"
	case "at", "access_token", "flow_bearer":
		return "bearer"
	case "protocol", "cookie", "cookies":
		return "protocol"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

type RequestLog struct {
	ID              int64      `json:"id"`
	AccountID       *int64     `json:"token_id,omitempty"`
	AccountEmail    string     `json:"token_email,omitempty"`
	Operation       string     `json:"operation"`
	RequestBody     string     `json:"request_body,omitempty"`
	ResponseBody    string     `json:"response_body,omitempty"`
	ResponseExcerpt string     `json:"response_body_excerpt,omitempty"`
	StatusCode      int        `json:"status_code"`
	DurationMS      int64      `json:"duration_ms"`
	Duration        float64    `json:"duration"`
	StatusText      string     `json:"status_text"`
	Progress        int        `json:"progress"`
	ErrorSummary    string     `json:"error_summary"`
	CreatedAt       *time.Time `json:"created_at,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

type DashboardStats struct {
	TotalTokens  int `json:"total_tokens"`
	ActiveTokens int `json:"active_tokens"`
	TotalMusic   int `json:"total_music"`
	TodayMusic   int `json:"today_music"`
	TotalImages  int `json:"total_images"`
	TodayImages  int `json:"today_images"`
	TotalVideos  int `json:"total_videos"`
	TodayVideos  int `json:"today_videos"`
	TotalErrors  int `json:"total_errors"`
	TodayErrors  int `json:"today_errors"`
}

type MediaRef struct {
	OriginalURL string `json:"original_url"`
	URL         string `json:"url"`
	LocalURL    string `json:"local_url,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
}
