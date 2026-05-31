package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
)

type FlowMusicClient struct {
	cfg        config.Config
	mu         sync.Mutex
	httpClients map[string]*http.Client
}

type CreditInfo struct {
	CreditsRemaining float64 `json:"credits_remaining"`
	TokensRemaining  float64 `json:"tokens_remaining"`
	SubscriptionTier string  `json:"subscription_tier"`
}

type ConversationResult struct {
	JobID        string
	RawEvents    []string
	ClipIDs      []string
	OperationIDs []string
}

type ConversationStreamEvent struct {
	Event        string
	Data         string
	Status       string
	PartKind     string
	ToolName     string
	TextDelta    string
	TextContent  string
	ToolTitle    string
	SoundPrompt  string
	OperationIDs []string
	ClipIDs      []string
}

type ClipPollStatus struct {
	OperationID string
	Status      string
	Progress    any
	ClipIDs     []string
	Error       string
}

type ClipResult struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	AudioURL        string  `json:"audio_url"`
	WavURL          string  `json:"wav_url"`
	ImageURL        string  `json:"image_url"`
	VideoURL        string  `json:"video_url"`
	Lyrics          string  `json:"lyrics,omitempty"`
	LyricsID        string  `json:"lyrics_id,omitempty"`
	SoundPrompt     string  `json:"sound_prompt,omitempty"`
	OperationID     string  `json:"operation_id,omitempty"`
	OperationType   string  `json:"operation_type,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	CreatedAt       string  `json:"created_at,omitempty"`
}

type upstreamHTTPError struct {
	Operation  string
	StatusCode int
	Body       string
}

func (e *upstreamHTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Operation, e.StatusCode)
	}
	return fmt.Sprintf("%s: HTTP %d %s", e.Operation, e.StatusCode, body)
}

type ConversationPart struct {
	Content  string `json:"content"`
	PartKind string `json:"part_kind"`
}

type ConversationClientContext struct {
	CurrentSongID      string         `json:"current_song_id,omitempty"`
	SongQueue          []any          `json:"song_queue"`
	SelectedModel      any            `json:"selected_model"`
	LyricsIDMap        map[string]any `json:"lyrics_id_map"`
	GhostwriterVersion string         `json:"ghostwriter_version"`
}

type ConversationRequest struct {
	Parts         []ConversationPart        `json:"parts"`
	ClientContext ConversationClientContext `json:"client_context"`
	ModelName     string                    `json:"model_name"`
	Mode          string                    `json:"mode"`
}

func NewFlowMusicClient(cfg config.Config) *FlowMusicClient {
	return &FlowMusicClient{cfg: cfg, httpClients: make(map[string]*http.Client)}
}

func (c *FlowMusicClient) RefreshSupabase(ctx context.Context, account domain.Account) (domain.Account, error) {
	refreshToken := strings.TrimSpace(firstNonEmpty(account.RefreshToken, account.ST))
	if refreshToken == "" {
		return account, fmt.Errorf("refresh_token is empty")
	}
	if strings.TrimSpace(c.cfg.SupabaseAnonKey) == "" {
		return account, fmt.Errorf("FLOWMUSIC_SUPABASE_ANON_KEY 未配置，无法刷新 Supabase token。请在 .env 文件中设置(可从 HAR 文件提取)")
	}
	body := map[string]string{"refresh_token": refreshToken}
	var payload map[string]any
	if err := c.doJSON(ctx, account.ProxyURL, http.MethodPost, c.cfg.SupabaseBaseURL+"/auth/v1/token?grant_type=refresh_token", nil, body, &payload, func(req *http.Request) {
		req.Header.Set("apikey", c.cfg.SupabaseAnonKey)
		req.Header.Set("Authorization", "Bearer "+c.cfg.SupabaseAnonKey)
		req.Header.Set("X-Client-Info", "supabase-ssr/0.5.2")
		req.Header.Set("X-Supabase-Api-Version", "2024-01-01")
	}); err != nil {
		return account, err
	}
	if rt := getString(payload, "refresh_token"); rt != "" {
		account.RefreshToken = rt
		account.ST = rt
	}
	supabasePT := getString(payload, "provider_token")
	supabasePRT := getString(payload, "provider_refresh_token")
	account.ProviderToken = firstNonEmpty(supabasePT, account.ProviderToken)
	account.ProviderRefreshToken = firstNonEmpty(supabasePRT, account.ProviderRefreshToken)
	now := time.Now().UTC()
	account.LastRefreshAt = &now
	if expiresAt := parseExpires(payload); expiresAt != nil {
		account.ExpiresAt = expiresAt
		account.ATExpires = expiresAt
	}
	if user, ok := payload["user"].(map[string]any); ok {
		if email := getString(user, "email"); email != "" {
			account.Email = email
		}
		if metadata, ok := user["user_metadata"].(map[string]any); ok {
			if name := getString(metadata, "name"); name != "" {
				account.Name = name
			}
		}
	}
	// FlowMusic API validates the Supabase JWT (access_token), not the provider_token.
	if supabaseJWT := getString(payload, "access_token"); supabaseJWT != "" {
		account.AT = supabaseJWT
	}
	if account.ProviderToken == "" {
		return account, fmt.Errorf("supabase refresh succeeded but provider_token is missing; cannot update FlowMusic bearer")
	}
	// Supabase refresh didn't return provider_token (old token is likely expired).
	// Try Google OAuth refresh directly if client_secret is configured.
	if supabasePT == "" && strings.TrimSpace(account.ProviderRefreshToken) != "" {
		if secret := strings.TrimSpace(c.cfg.GoogleOAuthClientSecret); secret != "" {
			refreshed, oauthErr := c.RefreshGoogleProviderToken(ctx, account)
			if oauthErr == nil {
				account = refreshed
				supabasePT = account.ProviderToken
			}
		}
	}
	// Try to get a valid flow_bearer via SaveGoogle.
	// If provider_token is expired, SaveGoogle will return 401.
	// Save the Supabase JWT before it gets overwritten — FlowMusic validates this JWT.
	supabaseJWT := getString(payload, "access_token")
	flowBearer, saveErr := c.saveAndResolveFlowBearer(ctx, &account, payload)
	if saveErr != nil {
		flowBearer = strings.TrimSpace(account.ProviderToken)
		if supabasePT == "" {
			if strings.TrimSpace(c.cfg.GoogleOAuthClientSecret) == "" {
				account.LastRefreshResult = "supabase_refresh_no_provider_token: 请在环境变量 FLOWMUSIC_GOOGLE_OAUTH_CLIENT_SECRET 中配置 Google OAuth client_secret，或重新导入账号"
			} else {
				account.LastRefreshResult = "supabase_refresh_no_provider_token: Google OAuth 刷新失败，provider_token 已过期"
			}
		} else {
			account.LastRefreshResult = "supabase_refresh_use_provider_token_directly"
		}
	} else if strings.TrimSpace(flowBearer) == "" {
		flowBearer = strings.TrimSpace(account.ProviderToken)
		account.LastRefreshResult = "supabase_refresh_use_provider_token_directly"
	} else {
		account.LastRefreshResult = "supabase_refresh_and_flow_bearer_success"
	}
	account.FlowBearer = flowBearer
	// AT stores the Supabase JWT (which FlowMusic's API validates)
	// instead of the provider token. This ensures API calls use the
	// correct Bearer token even when the provider_token has expired.
	if supabaseJWT != "" {
		account.AT = supabaseJWT
	} else {
		account.AT = flowBearer
	}
	account.AccessToken = flowBearer
	return account, nil
}

func (c *FlowMusicClient) saveAndResolveFlowBearer(ctx context.Context, account *domain.Account, supabasePayload map[string]any) (string, error) {
	// Build/update Supabase auth cookie on every refresh so it stays fresh,
	// while preserving existing provider_token when Supabase refresh doesn't return one.
	cookieValue := buildSupabaseAuthCookie(supabasePayload, account)
	if cookieValue != "" {
		account.Cookies = cookieValue
	}
	headerAccount := withoutFlowBearer(*account)
	headerAccount.Cookies = "" // SaveGoogle needs no auth headers at all
	var savePayload map[string]any
	saveBody := map[string]string{
		"access_token":  strings.TrimSpace(account.ProviderToken),
		"platform":      "web",
		"refresh_token": strings.TrimSpace(account.ProviderRefreshToken),
	}
	if err := c.doJSON(ctx, account.ProxyURL, http.MethodPost, c.cfg.FlowMusicBaseURL+"/__api/auth/google/save", &headerAccount, saveBody, &savePayload, nil); err != nil {
		return "", err
	}
	if data, ok := savePayload["data"].(map[string]any); ok {
		return firstNonEmpty(
			getString(data, "access_token"),
			getString(data, "flow_bearer"),
			getString(data, "flow_access_token"),
			getString(data, "token"),
		), nil
	}
	return firstNonEmpty(
		getString(savePayload, "access_token"),
		getString(savePayload, "flow_bearer"),
		getString(savePayload, "flow_access_token"),
		getString(savePayload, "token"),
	), nil
}

func buildSupabaseAuthCookie(payload map[string]any, account *domain.Account) string {
	sessionToken := getString(payload, "access_token")
	refreshToken := getString(payload, "refresh_token")
	if sessionToken == "" || refreshToken == "" {
		return ""
	}
	// Use provider_token from Supabase response, falling back to what we have in DB.
	providerToken := firstNonEmpty(getString(payload, "provider_token"), account.ProviderToken)
	providerRefreshToken := firstNonEmpty(getString(payload, "provider_refresh_token"), account.ProviderRefreshToken)
	user, _ := payload["user"].(map[string]any)
	if user == nil {
		user = map[string]any{}
	}
	expiresIn := payload["expires_in"]
	expiresAt := payload["expires_at"]
	session := map[string]any{
		"access_token":           sessionToken,
		"token_type":             "bearer",
		"expires_in":             expiresIn,
		"expires_at":             expiresAt,
		"refresh_token":          refreshToken,
		"user":                   user,
		"provider_token":         providerToken,
		"provider_refresh_token": providerRefreshToken,
	}
	data, err := json.Marshal(session)
	if err != nil {
		return ""
	}
	const chunkSize = 4096
	bare := base64.StdEncoding.EncodeToString(data)
	fullValue := "base64-" + bare
	var parts []string
	for i := 0; i < len(fullValue); i += chunkSize {
		end := i + chunkSize
		if end > len(fullValue) {
			end = len(fullValue)
		}
		parts = append(parts, fullValue[i:end])
	}
	var cookieParts []string
	for i, part := range parts {
		cookieParts = append(cookieParts, fmt.Sprintf("sb-sb-auth-token.%d=%s", i, part))
	}
	return strings.Join(cookieParts, "; ")
}

func (c *FlowMusicClient) SaveGoogle(ctx context.Context, account domain.Account) (string, error) {
	providerToken := strings.TrimSpace(account.ProviderToken)
	if providerToken == "" {
		return "", fmt.Errorf("provider_token is empty")
	}
	body := map[string]string{
		"access_token":  providerToken,
		"platform":      "web",
		"refresh_token": strings.TrimSpace(account.ProviderRefreshToken),
	}
	var payload map[string]any
	headerAccount := withoutFlowBearer(account)
	headerAccount.Cookies = "" // SaveGoogle needs no auth headers at all
	err := c.doJSON(ctx, account.ProxyURL, http.MethodPost, c.cfg.FlowMusicBaseURL+"/__api/auth/google/save", &headerAccount, body, &payload, nil)
	if err != nil {
		return "", err
	}
	if data, ok := payload["data"].(map[string]any); ok {
		return firstNonEmpty(
			getString(data, "access_token"),
			getString(data, "flow_bearer"),
			getString(data, "flow_access_token"),
			getString(data, "token"),
		), nil
	}
	return firstNonEmpty(
		getString(payload, "access_token"),
		getString(payload, "flow_bearer"),
		getString(payload, "flow_access_token"),
		getString(payload, "token"),
	), nil
}

func (c *FlowMusicClient) RefreshGoogleProviderToken(ctx context.Context, account domain.Account) (domain.Account, error) {
	refreshToken := strings.TrimSpace(account.ProviderRefreshToken)
	if refreshToken == "" {
		return account, fmt.Errorf("provider_refresh_token is empty")
	}
	clientID := strings.TrimSpace(c.cfg.GoogleOAuthClientID)
	if clientID == "" {
		return account, fmt.Errorf("FLOWMUSIC_GOOGLE_OAUTH_CLIENT_ID is required for provider_refresh_token")
	}
	tokenURL := strings.TrimSpace(c.cfg.GoogleOAuthTokenURL)
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	if secret := strings.TrimSpace(c.cfg.GoogleOAuthClientSecret); secret != "" {
		form.Set("client_secret", secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return account, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome Safari")
	resp, err := NewHTTPClient(c.cfg, account.ProxyURL).Do(req)
	if err != nil {
		return account, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return account, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return account, &upstreamHTTPError{
			Operation:  http.MethodPost + " " + tokenURL,
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	var payload map[string]any
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &payload); err != nil {
			return account, err
		}
	}
	providerToken := firstNonEmpty(getString(payload, "access_token"), getString(payload, "provider_token"))
	if providerToken == "" {
		return account, fmt.Errorf("Google OAuth refresh returned empty access_token")
	}
	account.ProviderToken = providerToken
	if nextRefreshToken := getString(payload, "refresh_token"); nextRefreshToken != "" {
		account.ProviderRefreshToken = nextRefreshToken
	}
	now := time.Now().UTC()
	account.LastRefreshAt = &now
	account.LastRefreshResult = "provider_refresh_token_google_refresh_success"
	return account, nil
}

func (c *FlowMusicClient) ExchangePKCEToken(ctx context.Context, authCode, codeVerifier string) (map[string]any, error) {
	if strings.TrimSpace(c.cfg.SupabaseAnonKey) == "" {
		return nil, fmt.Errorf("FLOWMUSIC_SUPABASE_ANON_KEY is required for PKCE token exchange (set it in .env)")
	}
	body := map[string]string{
		"auth_code":     authCode,
		"code_verifier": codeVerifier,
	}
	var payload map[string]any
	if err := c.doJSON(ctx, "", http.MethodPost, c.cfg.SupabaseBaseURL+"/auth/v1/token?grant_type=pkce", nil, body, &payload, func(req *http.Request) {
		req.Header.Set("apikey", c.cfg.SupabaseAnonKey)
		req.Header.Set("Authorization", "Bearer "+c.cfg.SupabaseAnonKey)
		req.Header.Set("X-Client-Info", "supabase-ssr/0.5.2")
		req.Header.Set("X-Supabase-Api-Version", "2024-01-01")
	}); err != nil {
		return nil, err
	}
	return payload, nil
}

func (c *FlowMusicClient) RefreshFromCookies(ctx context.Context, account domain.Account) (domain.Account, error) {
	if strings.TrimSpace(account.Cookies) == "" {
		return account, fmt.Errorf("google_cookies is empty")
	}
	var payload map[string]any
	headerAccount := withoutFlowBearer(account)
	if err := c.doJSON(ctx, account.ProxyURL, http.MethodGet, c.cfg.FlowMusicBaseURL+"/__api/auth/session", &headerAccount, nil, &payload, nil); err != nil {
		return account, err
	}
	account.RefreshToken = firstNonEmpty(findString(payload, "refresh_token"), account.RefreshToken, account.ST)
	account.ST = account.RefreshToken
	sessionProviderToken := findString(payload, "provider_token")
	account.ProviderToken = firstNonEmpty(sessionProviderToken, account.ProviderToken)
	account.ProviderRefreshToken = firstNonEmpty(findString(payload, "provider_refresh_token"), account.ProviderRefreshToken)
	refreshedFlowBearer := firstNonEmpty(findString(payload, "flow_bearer"), findString(payload, "flow_access_token"))
	if refreshedFlowBearer != "" {
		account.FlowBearer = refreshedFlowBearer
		account.AT = refreshedFlowBearer
	} else if strings.TrimSpace(sessionProviderToken) != "" {
		flowBearer, err := c.SaveGoogle(ctx, account)
		if err != nil {
			return account, fmt.Errorf("cookie protocol session found provider_token but FlowMusic bearer update failed: %w", err)
		}
		refreshedFlowBearer = flowBearer
		account.FlowBearer = refreshedFlowBearer
		account.AT = refreshedFlowBearer
	}
	if strings.TrimSpace(refreshedFlowBearer) == "" {
		return account, fmt.Errorf("cookie protocol session did not contain a FlowMusic bearer token")
	}
	account.AccessToken = refreshedFlowBearer
	now := time.Now().UTC()
	account.LastRefreshAt = &now
	account.LastRefreshResult = "cookie_protocol_flow_bearer_success"
	if expires := parseExpires(payload); expires != nil {
		account.ExpiresAt = expires
		account.ATExpires = expires
	} else if rawExpires := firstNonEmpty(findString(payload, "expires_at"), findString(payload, "expires")); rawExpires != "" {
		account.ATExpires = parseTime(rawExpires)
	}
	if email := findString(payload, "email"); email != "" {
		account.Email = email
	}
	if name := findString(payload, "name"); name != "" {
		account.Name = name
	}
	return account, nil
}

func withoutFlowBearer(account domain.Account) domain.Account {
	account.FlowBearer = ""
	account.AT = ""
	account.AccessToken = ""
	return account
}

func (c *FlowMusicClient) GetCredits(ctx context.Context, account domain.Account) (CreditInfo, error) {
	var info CreditInfo
	var credits map[string]any
	if err := c.doJSON(ctx, account.ProxyURL, http.MethodGet, c.cfg.FlowMusicBaseURL+"/__api/billing/credits", &account, nil, &credits, nil); err != nil {
		return info, err
	}
	if data, ok := credits["data"].(map[string]any); ok {
		info.CreditsRemaining = getFloat(data, "credits_remaining")
		info.TokensRemaining = getFloat(data, "tokens_remaining")
	}
	var sub map[string]any
	if err := c.doJSON(ctx, account.ProxyURL, http.MethodGet, c.cfg.FlowMusicBaseURL+"/__api/billing/subscription", &account, nil, &sub, nil); err == nil {
		if data, ok := sub["data"].(map[string]any); ok {
			info.SubscriptionTier = getString(data, "subscription_tier")
		}
	}
	return info, nil
}

func (c *FlowMusicClient) StartConversation(ctx context.Context, account domain.Account, prompt, model string) (string, error) {
	body := BuildConversationRequest(prompt, model)
	var payload map[string]any
	if err := c.doJSON(ctx, account.ProxyURL, http.MethodPost, c.cfg.FlowMusicBaseURL+"/__api/conversation", &account, body, &payload, nil); err != nil {
		return "", err
	}
	jobID := conversationJobID(payload)
	if jobID == "" {
		return "", fmt.Errorf("upstream response missing job_id")
	}
	return jobID, nil
}

func conversationJobID(payload map[string]any) string {
	return firstNonEmpty(
		findString(payload, "job_id"),
		findString(payload, "jobId"),
		findString(payload, "conversation_id"),
		getString(payload, "id"),
		getNestedString(payload, "data", "id"),
	)
}

func BuildConversationRequest(prompt, model string) ConversationRequest {
	modelSpec := flowModelSpec(model)
	return ConversationRequest{
		Parts: []ConversationPart{
			{Content: buildMusicGenerationPrompt(prompt), PartKind: "user-prompt"},
		},
		ClientContext: ConversationClientContext{
			SongQueue:          []any{},
			SelectedModel:      nil,
			LyricsIDMap:        map[string]any{},
			GhostwriterVersion: modelSpec.GhostwriterVersion,
		},
		ModelName: modelSpec.FlowMusicModelName,
		Mode:      modelSpec.FlowMusicMode,
	}
}

func buildMusicGenerationPrompt(prompt string) string {
	prompt = compactMusicPrompt(prompt)
	if prompt == "" {
		prompt = "适合直接播放的纯音乐"
	}
	runes := []rune(prompt)
	limit := 220
	if promptContainsExplicitLyrics(prompt) {
		limit = 3000
	}
	if len(runes) > limit {
		prompt = string(runes[:limit])
	}
	if promptHasMusicIntent(prompt) {
		return "直接生成" + prompt
	}
	return "直接生成" + prompt + "音乐"
}

func compactMusicPrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return ""
	}
	preserveLayout := promptContainsExplicitLyrics(prompt)
	if preserveLayout {
		prompt = strings.ReplaceAll(prompt, "\r\n", "\n")
		prompt = strings.ReplaceAll(prompt, "\r", "\n")
	} else {
		prompt = strings.Join(strings.Fields(prompt), " ")
	}
	replacements := []struct {
		old string
		new string
	}{
		{"catchy chorus", "catchy"},
		{"必须直接生成音乐", ""},
		{"必须生成音乐", ""},
		{"直接生成音乐", ""},
		{"直接生成歌曲", ""},
		{"生成音乐", ""},
		{"生成歌曲", ""},
		{"创作音乐", ""},
		{"创作歌曲", ""},
		{"一首", ""},
		{"歌曲", "音乐"},
		{"歌名", ""},
		{"作词", ""},
		{"不要只回复文字", ""},
		{"不要回复文字", ""},
		{"不要只返回文字", ""},
		{"不要返回文字", ""},
		{"不要只给文字", ""},
		{"不要给建议", ""},
		{"不要解释", ""},
		{"只输出结果", ""},
		{"工具调用", ""},
		{"调用工具", ""},
		{"audio__create_song", ""},
		{"dalle.text2im", ""},
		{"DALL-E", ""},
		{"dalle", ""},
		{"text2im", ""},
		{"图片", ""},
		{"图像", ""},
		{"封面", ""},
		{"海报", ""},
		{"album cover", ""},
		{"cover art", ""},
		{"cover image", ""},
		{"image", ""},
		{"picture", ""},
		{"poster", ""},
		{"song", "music"},
		{"write a", ""},
		{"write an", ""},
		{"API", ""},
		{"api", ""},
	}
	for _, item := range replacements {
		prompt = strings.ReplaceAll(prompt, item.old, item.new)
	}
	if preserveLayout {
		lines := strings.Split(prompt, "\n")
		out := make([]string, 0, len(lines))
		blank := false
		for _, line := range lines {
			line = strings.Join(strings.Fields(strings.TrimSpace(line)), " ")
			if line == "" {
				if !blank && len(out) > 0 {
					out = append(out, "")
				}
				blank = true
				continue
			}
			out = append(out, line)
			blank = false
		}
		prompt = strings.TrimSpace(strings.Join(out, "\n"))
	} else {
		prompt = strings.NewReplacer(
			"，", " ",
			",", " ",
			"；", " ",
			";", " ",
			"。", " ",
			".", " ",
			"：", " ",
			":", " ",
			"、", " ",
			"（", " ",
			"）", " ",
			"(", " ",
			")", " ",
		).Replace(prompt)
		prompt = strings.Trim(prompt, " \t\r\n,，;；。.!！:：-—")
		prompt = strings.Join(strings.Fields(prompt), " ")
	}
	prompt = strings.Trim(prompt, " \t\r\n,，;；。.!！:：-—")
	for strings.Contains(prompt, "音乐音乐") {
		prompt = strings.ReplaceAll(prompt, "音乐音乐", "音乐")
	}
	return prompt
}

func promptContainsExplicitLyrics(prompt string) bool {
	lower := strings.ToLower(prompt)
	if strings.Contains(lower, "歌词") || strings.Contains(lower, "lyrics") {
		return true
	}
	for _, marker := range []string{
		"[verse]", "[chorus]", "[bridge]", "[intro]", "[outro]", "[pre-chorus]",
		"[主歌]", "[副歌]", "[导歌]", "[桥段]", "[前奏]", "[尾奏]",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func promptHasMusicIntent(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, marker := range []string{
		"音乐", "歌曲", "歌", "曲", "旋律", "副歌", "歌词", "人声", "纯音乐", "器乐", "节拍", "电音", "流行", "摇滚", "爵士", "民谣", "说唱", "嘻哈", "lo-fi", "lofi", "bpm",
		"music", "song", "track", "melody", "chorus", "lyrics", "vocal", "instrumental", "beat", "pop", "rock", "jazz", "hip hop", "electro", "electropop",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (c *FlowMusicClient) StreamMessages(ctx context.Context, account domain.Account, jobID string) (ConversationResult, error) {
	return c.StreamMessagesWithEvents(ctx, account, jobID, nil)
}

func (c *FlowMusicClient) StreamMessagesWithEvents(ctx context.Context, account domain.Account, jobID string, onEvent func(ConversationStreamEvent)) (ConversationResult, error) {
	result := ConversationResult{JobID: jobID}
	var cancel context.CancelFunc
	var idleTimedOut atomic.Bool
	idleTimeout := c.cfg.StreamIdleTimeout
	if idleTimeout > 0 {
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
	}
	url := c.cfg.FlowMusicBaseURL + "/__api/messages/" + jobID + "/stream?last_id=0"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return result, err
	}
	c.applyFlowHeaders(req, &account)
	client := NewHTTPClient(c.cfg, account.ProxyURL)
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return result, &upstreamHTTPError{
			Operation:  "stream messages",
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}
	var idleTimer *time.Timer
	if idleTimeout > 0 {
		idleTimer = time.AfterFunc(idleTimeout, func() {
			idleTimedOut.Store(true)
			cancel()
		})
		defer idleTimer.Stop()
	}
	resetIdleTimer := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() && idleTimedOut.Load() {
			return
		}
		idleTimer.Reset(idleTimeout)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()
		resetIdleTimer()
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		if data == "" {
			continue
		}
		result.RawEvents = append(result.RawEvents, data)
		beforeOps := len(result.OperationIDs)
		beforeClips := len(result.ClipIDs)
		collectIDs(data, &result)
		if onEvent != nil {
			event := parseConversationStreamEvent(currentEvent, data)
			event.OperationIDs = append([]string(nil), result.OperationIDs[beforeOps:]...)
			event.ClipIDs = append([]string(nil), result.ClipIDs[beforeClips:]...)
			onEvent(event)
		}
		if currentEvent == "final" {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		if idleTimedOut.Load() {
			return result, fmt.Errorf("stream messages idle timeout after %s", idleTimeout)
		}
		return result, err
	}
	return result, nil
}

func (c *FlowMusicClient) PollClips(ctx context.Context, account domain.Account, ids []string, deadline time.Time) ([]string, error) {
	return c.PollClipsWithProgress(ctx, account, ids, deadline, nil)
}

func (c *FlowMusicClient) PollClipsWithProgress(ctx context.Context, account domain.Account, ids []string, deadline time.Time, onStatus func(ClipPollStatus)) ([]string, error) {
	seen := map[string]struct{}{}
	var lastErr error
	reportedErrors := map[string]struct{}{}
	lastHeartbeat := time.Time{}
	for time.Now().Before(deadline) {
		for _, id := range ids {
			if id == "" {
				continue
			}
			var payload map[string]any
			err := c.doJSON(ctx, account.ProxyURL, http.MethodGet, c.cfg.FlowMusicBaseURL+"/__api/audio-create-song-status/"+id, &account, nil, &payload, nil)
			if err != nil {
				if isAuthFailure(err) {
					return mapKeys(seen), err
				}
				_, reported := reportedErrors[id]
				if onStatus != nil && !reported {
					onStatus(ClipPollStatus{OperationID: id, Error: err.Error(), ClipIDs: mapKeys(seen)})
				}
				reportedErrors[id] = struct{}{}
				lastErr = err
				continue
			}
			status := ClipPollStatus{
				OperationID: id,
				Status:      firstNonEmpty(findString(payload, "status"), findString(payload, "state")),
				Progress:    findValue(payload, "progress"),
			}
			for _, clipID := range findClipIDs(payload) {
				seen[clipID] = struct{}{}
			}
			status.ClipIDs = mapKeys(seen)
			if onStatus != nil {
				onStatus(status)
			}
		}
		if len(seen) > 0 {
			return mapKeys(seen), nil
		}
		if onStatus != nil && time.Since(lastHeartbeat) >= 15*time.Second {
			onStatus(ClipPollStatus{Status: "waiting", ClipIDs: mapKeys(seen)})
			lastHeartbeat = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if lastErr != nil {
		return mapKeys(seen), fmt.Errorf("poll clips timed out while waiting for upstream song status")
	}
	return mapKeys(seen), fmt.Errorf("poll clips timed out without clip ids")
}

func (c *FlowMusicClient) GetClips(ctx context.Context, account domain.Account, clipIDs []string) ([]ClipResult, error) {
	if len(clipIDs) == 0 {
		return nil, nil
	}
	body := map[string]any{"clip_ids": clipIDs}
	var payload map[string]any
	if err := c.doJSON(ctx, account.ProxyURL, http.MethodPost, c.cfg.FlowMusicBaseURL+"/__api/clips", &account, body, &payload, nil); err != nil {
		return nil, err
	}
	out := clipsFromPayload(payload)
	return orderClipsByIDs(out, clipIDs), nil
}

func clipsFromPayload(payload map[string]any) []ClipResult {
	raw := findValue(payload, "clips")
	if raw == nil {
		raw = findValue(payload, "data")
	}
	switch typed := raw.(type) {
	case map[string]any:
		out := make([]ClipResult, 0, len(typed))
		for id, item := range typed {
			if m, ok := item.(map[string]any); ok {
				out = append(out, clipFromMap(m, id))
			}
		}
		return out
	case []any:
		out := make([]ClipResult, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]any); ok {
				out = append(out, clipFromMap(m, ""))
			}
		}
		return out
	default:
		return nil
	}
}

func orderClipsByIDs(clips []ClipResult, ids []string) []ClipResult {
	if len(clips) < 2 || len(ids) == 0 {
		return clips
	}
	byID := make(map[string][]ClipResult, len(clips))
	for _, clip := range clips {
		byID[clip.ID] = append(byID[clip.ID], clip)
	}
	out := make([]ClipResult, 0, len(clips))
	used := make(map[string]int, len(clips))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		items := byID[id]
		index := used[id]
		if index >= len(items) {
			continue
		}
		out = append(out, items[index])
		used[id] = index + 1
	}
	for _, clip := range clips {
		if used[clip.ID] > 0 {
			used[clip.ID]--
			continue
		}
		out = append(out, clip)
	}
	if len(out) == len(clips) {
		return out
	}
	return clips
}

func clipFromMap(item map[string]any, fallbackID string) ClipResult {
	lyrics, lyricsID := clipLyrics(item)
	return ClipResult{
		ID:              firstNonEmpty(getString(item, "id"), getString(item, "clip_id"), getString(item, "clipId"), fallbackID),
		Title:           findString(item, "title"),
		AudioURL:        mediaURL(item, "audio", "audio_url", "audioUrl", "mp3_url", "mp3Url", "m4a_url", "m4aUrl"),
		WavURL:          mediaURL(item, "wav", "wav_url", "wavUrl", "wave_url", "waveUrl"),
		ImageURL:        mediaURL(item, "image", "image_url", "imageUrl", "cover_url", "coverUrl"),
		VideoURL:        mediaURL(item, "video", "video_url", "videoUrl", "avi_url", "aviUrl", "mp4_url", "mp4Url"),
		Lyrics:          lyrics,
		LyricsID:        lyricsID,
		SoundPrompt:     firstNonEmpty(getNestedString(item, "operation", "sound_prompt"), getString(item, "sound_prompt"), findString(item, "sound_prompt")),
		OperationID:     firstNonEmpty(getString(item, "op_id"), getString(item, "operation_id"), getString(item, "operationId"), getNestedString(item, "operation", "id")),
		OperationType:   firstNonEmpty(getString(item, "op_type"), getString(item, "operation_type"), getNestedString(item, "operation", "op_type")),
		DurationSeconds: clipDurationSeconds(item),
		CreatedAt:       getString(item, "created_at"),
	}
}

func clipLyrics(item map[string]any) (string, string) {
	raw, ok := directValue(item, "lyrics")
	if !ok || raw == nil {
		return firstNonEmpty(getString(item, "lyrics_text"), getString(item, "lyricsText")), getString(item, "lyrics_id")
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed), getString(item, "lyrics_id")
	case map[string]any:
		if value, ok := directValue(typed, "value"); ok {
			if valueMap, ok := value.(map[string]any); ok {
				return firstNonEmpty(getString(valueMap, "text"), getString(valueMap, "lyrics")), firstNonEmpty(getString(valueMap, "id"), getString(item, "lyrics_id"))
			}
			if text := scalarString(value); text != "" {
				return text, firstNonEmpty(getString(typed, "id"), getString(item, "lyrics_id"))
			}
		}
		return firstNonEmpty(getString(typed, "text"), getString(typed, "lyrics")), firstNonEmpty(getString(typed, "id"), getString(item, "lyrics_id"))
	default:
		return scalarString(typed), getString(item, "lyrics_id")
	}
}

func clipDurationSeconds(item map[string]any) float64 {
	raw, ok := directValue(item, "duration")
	if !ok || raw == nil {
		if n, ok := findNumeric(item, "duration_seconds"); ok {
			return n
		}
		return 0
	}
	if n, ok := numericValue(raw); ok {
		return n
	}
	if m, ok := raw.(map[string]any); ok {
		if value, ok := directValue(m, "value"); ok {
			if n, ok := numericValue(value); ok {
				return n
			}
		}
	}
	return 0
}

func mediaURL(item map[string]any, objectKey string, aliases ...string) string {
	for _, key := range aliases {
		if url := findString(item, key); url != "" {
			return url
		}
	}
	if raw, ok := directValue(item, objectKey); ok {
		if url := mediaValueURL(raw, aliases...); url != "" {
			return url
		}
	}
	if url := findString(item, objectKey); url != "" {
		return url
	}
	return ""
}

func mediaValueURL(value any, aliases ...string) string {
	if s := scalarString(value); s != "" {
		return s
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := append([]string{"url", "src", "href", "download_url", "downloadUrl"}, aliases...)
		for _, key := range keys {
			if s := getString(typed, key); s != "" {
				return s
			}
		}
		for _, key := range keys {
			if s := findString(typed, key); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range typed {
			if s := mediaValueURL(child, aliases...); s != "" {
				return s
			}
		}
	}
	return ""
}

func (c *FlowMusicClient) getHTTPClient(proxyURL string) *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client, ok := c.httpClients[proxyURL]; ok {
		return client
	}
	client := NewHTTPClient(c.cfg, proxyURL)
	c.httpClients[proxyURL] = client
	return client
}

func (c *FlowMusicClient) doJSON(ctx context.Context, proxyURL, method, url string, account *domain.Account, body any, out any, mutate func(*http.Request)) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if account != nil {
		c.applyFlowHeaders(req, account)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := c.getHTTPClient(proxyURL).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &upstreamHTTPError{
			Operation:  method + " " + url,
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func (c *FlowMusicClient) applyFlowHeaders(req *http.Request, account *domain.Account) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", c.cfg.FlowMusicBaseURL)
	req.Header.Set("Referer", c.cfg.FlowMusicBaseURL+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome Safari")
	if account == nil {
		return
	}
	// FlowMusic API validates the Supabase JWT (access_token in the cookie).
	// Use Supabase JWT from cookie when available, fall back to FlowBearer/AT.
	bearer := extractSupabaseJWT(account.Cookies)
	if bearer == "" {
		bearer = normalizeBearerToken(firstNonEmpty(account.AT, account.FlowBearer))
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie := strings.TrimSpace(account.Cookies); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

func extractSupabaseJWT(cookieValue string) string {
	if cookieValue == "" {
		return ""
	}
	parts := map[string]string{}
	for _, part := range strings.Split(cookieValue, "; ") {
		if idx := strings.IndexByte(part, '='); idx > 0 {
			parts[part[:idx]] = part[idx+1:]
		}
	}
	var full string
	for i := 0; ; i++ {
		key := fmt.Sprintf("sb-sb-auth-token.%d", i)
		chunk, ok := parts[key]
		if !ok {
			break
		}
		full += chunk
	}
	if full == "" {
		return ""
	}
	full = strings.TrimPrefix(full, "base64-")
	pad := (4 - len(full)%4) % 4
	if pad > 0 {
		full += strings.Repeat("=", pad)
	}
	data, err := base64.StdEncoding.DecodeString(full)
	if err != nil {
		return ""
	}
	var session map[string]any
	if err := json.Unmarshal(data, &session); err != nil {
		return ""
	}
	token, _ := session["access_token"].(string)
	return token
}

func normalizeBearerToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.Fields(value)
	if len(parts) >= 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(strings.Join(parts[1:], " "))
	}
	return value
}

func flowModelSpec(model string) domain.GenerationModel {
	spec, ok := domain.ResolveGenerationModel(model)
	if ok {
		return spec
	}
	spec, _ = domain.ResolveGenerationModel(domain.DefaultGenerationModelID)
	return spec
}

func collectIDs(data string, result *ConversationResult) {
	var payload any
	if json.Unmarshal([]byte(data), &payload) == nil {
		for _, id := range findOperationIDs(payload) {
			result.OperationIDs = appendUnique(result.OperationIDs, id)
		}
		for _, id := range findClipIDs(payload) {
			result.ClipIDs = appendUnique(result.ClipIDs, id)
		}
	}
}

func parseConversationStreamEvent(eventName, data string) ConversationStreamEvent {
	event := ConversationStreamEvent{Event: eventName, Data: data}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return event
	}
	event.Status = getString(payload, "status")
	event.TextDelta = getString(payload, "delta")
	if part, ok := directValue(payload, "part"); ok {
		if partMap, ok := part.(map[string]any); ok {
			event.PartKind = getString(partMap, "part_kind")
			event.ToolName = getString(partMap, "tool_name")
			if args, ok := directValueOrNil(partMap, "args").(map[string]any); ok {
				event.ToolTitle = firstNonEmpty(getString(args, "title"), findString(args, "title"))
				event.SoundPrompt = firstNonEmpty(getString(args, "sound_prompt"), findString(args, "sound_prompt"))
			}
			if content := directValueOrNil(partMap, "content"); content != nil {
				event.TextContent = scalarString(content)
			}
		}
	}
	return event
}

func directValueOrNil(m map[string]any, key string) any {
	value, ok := directValue(m, key)
	if !ok {
		return nil
	}
	return value
}

func (e ConversationStreamEvent) ProgressMessages() []string {
	var messages []string
	if e.Event == "conversation_id" {
		var payload map[string]any
		_ = json.Unmarshal([]byte(e.Data), &payload)
		if id := findString(payload, "id"); id != "" {
			messages = append(messages, "FlowMusic 会话已创建: "+id)
		}
	}
	if e.Event != "part" {
		return messages
	}
	switch e.PartKind {
	case "tool-call":
		if e.ToolName == "audio__create_song" && e.Status == "start" {
			detail := firstNonEmpty(e.ToolTitle, e.SoundPrompt)
			if detail != "" {
				messages = append(messages, "上游已触发音乐生成工具: "+truncate(detail, 180))
			} else {
				messages = append(messages, "上游已触发音乐生成工具")
			}
		}
	case "tool-return":
		if e.ToolName == "audio__create_song" {
			if len(e.ClipIDs) > 0 {
				messages = append(messages, "上游已返回歌曲 clip: "+strings.Join(e.ClipIDs, ", "))
			} else if len(e.OperationIDs) > 0 {
				messages = append(messages, "上游已返回生成任务: "+strings.Join(e.OperationIDs, ", "))
			} else {
				messages = append(messages, "上游音乐生成工具已返回")
			}
		}
	case "retry-prompt":
		messages = append(messages, "上游音乐生成工具要求重试，继续等待生成结果...")
	case "text":
		return messages
	}
	return messages
}

func (s ClipPollStatus) ProgressMessage() string {
	if strings.TrimSpace(s.Error) != "" {
		return "歌曲状态暂未就绪，继续等待上游生成结果..."
	}
	if len(s.ClipIDs) > 0 {
		return "已获取歌曲 clip: " + strings.Join(s.ClipIDs, ", ")
	}
	progress := scalarString(s.Progress)
	status := strings.TrimSpace(s.Status)
	switch {
	case status != "" && progress != "":
		return fmt.Sprintf("歌曲生成状态: %s (%s)", status, progress)
	case status != "":
		return "歌曲生成状态: " + status
	case s.OperationID != "":
		return "轮询歌曲生成任务: " + s.OperationID
	default:
		return "轮询歌曲生成状态..."
	}
}

func findClipIDs(value any) []string {
	var out []string
	var walk func(any, string, []string)
	walk = func(v any, key string, ancestors []string) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range sortedMapKeys(x) {
				child := x[k]
				normalized := normalizeFieldName(k)
				if isClipIDKey(normalized) {
					out = appendClipIDValues(out, child)
					continue
				}
				if normalized == "id" && hasClipAncestor(ancestors) {
					out = appendClipIDValues(out, child)
					continue
				}
				walk(child, normalized, append(ancestors, normalized))
			}
		case []any:
			for _, child := range x {
				walk(child, key, ancestors)
			}
		case string:
			if isClipIDKey(key) {
				out = appendUnique(out, x)
			}
		}
	}
	walk(value, "", nil)
	return out
}

func findOperationIDs(value any) []string {
	var out []string
	var walk func(any, string, []string)
	walk = func(v any, key string, ancestors []string) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range sortedMapKeys(x) {
				child := x[k]
				normalized := normalizeFieldName(k)
				if isOperationIDKey(normalized) {
					out = appendOperationIDValues(out, child)
					continue
				}
				if normalized == "id" && hasOperationAncestor(ancestors) {
					out = appendOperationIDValues(out, child)
					continue
				}
				walk(child, normalized, append(ancestors, normalized))
			}
		case []any:
			for _, child := range x {
				walk(child, key, ancestors)
			}
		case string:
			if isOperationIDKey(key) {
				out = appendUnique(out, x)
			}
		}
	}
	walk(value, "", nil)
	return out
}

func isOperationIDKey(key string) bool {
	key = normalizeFieldName(key)
	return key == "operationid" || key == "operationids" || key == "operationida" || key == "operationidb" || key == "opid" || key == "opids"
}

func appendOperationIDValues(values []string, value any) []string {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			values = appendOperationIDValues(values, child)
		}
	case map[string]any:
		values = appendOperationIDValues(values, firstNonEmpty(getString(typed, "operation_id"), getString(typed, "operationId"), getString(typed, "id")))
	default:
		values = appendUnique(values, scalarString(typed))
	}
	return values
}

func hasOperationAncestor(ancestors []string) bool {
	for _, ancestor := range ancestors {
		switch normalizeFieldName(ancestor) {
		case "operation", "operations", "audiooperation", "songoperation", "generationoperation":
			return true
		}
	}
	return false
}

func isClipIDKey(key string) bool {
	key = normalizeFieldName(key)
	return key == "clipid" || key == "clipids" || key == "clipida" || key == "clipidb" || key == "clip_id"
}

func appendClipIDValues(values []string, value any) []string {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			values = appendClipIDValues(values, child)
		}
	case map[string]any:
		values = appendClipIDValues(values, firstNonEmpty(getString(typed, "id"), getString(typed, "clip_id"), getString(typed, "clipId")))
	default:
		values = appendUnique(values, scalarString(typed))
	}
	return values
}

func hasClipAncestor(ancestors []string) bool {
	for _, ancestor := range ancestors {
		switch normalizeFieldName(ancestor) {
		case "clip", "clips", "cliplist", "clipdata", "generatedclips":
			return true
		}
	}
	return false
}

func normalizeFieldName(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	return key
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func parseExpires(payload map[string]any) *time.Time {
	if seconds, ok := findNumeric(payload, "expires_in"); ok && seconds > 0 {
		t := time.Now().UTC().Add(time.Duration(seconds) * time.Second)
		return &t
	}
	if unix, ok := findNumeric(payload, "expires_at"); ok && unix > 0 {
		t := time.Unix(int64(unix), 0).UTC()
		return &t
	}
	if raw := firstNonEmpty(findString(payload, "expires_at"), findString(payload, "expires")); raw != "" {
		return parseTime(raw)
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if value, ok := directValue(m, key); ok && value != nil {
		return scalarString(value)
	}
	return ""
}

func getNestedString(m map[string]any, keys ...string) string {
	var value any = m
	for _, key := range keys {
		current, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		next, ok := directValue(current, key)
		if !ok {
			return ""
		}
		value = next
	}
	return scalarString(value)
}

func directValue(m map[string]any, key string) (any, bool) {
	target := strings.TrimSpace(key)
	for k, value := range m {
		if strings.EqualFold(strings.TrimSpace(k), target) {
			return value, true
		}
	}
	normalizedTarget := normalizeFieldName(target)
	if normalizedTarget == "" {
		return nil, false
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if normalizeFieldName(k) == normalizedTarget {
			return m[k], true
		}
	}
	return nil, false
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strings.TrimSpace(strconv.FormatFloat(typed, 'f', -1, 64))
	case float32:
		return strings.TrimSpace(strconv.FormatFloat(float64(typed), 'f', -1, 32))
	default:
		return ""
	}
}

func getFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	value, ok := directValue(m, key)
	if !ok {
		return 0
	}
	number, ok := numericValue(value)
	if !ok {
		return 0
	}
	return number
}

func numericValue(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case int32:
		return float64(value), true
	case json.Number:
		f, err := value.Float64()
		return f, err == nil
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(value, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func findString(value any, key string) string {
	key = normalizeFieldName(key)
	if key == "" {
		return ""
	}
	var walk func(any) string
	walk = func(v any) string {
		switch typed := v.(type) {
		case map[string]any:
			for k, child := range typed {
				if normalizeFieldName(k) == key {
					if s := scalarString(child); s != "" && s != "<nil>" {
						return s
					}
				}
			}
			for _, child := range typed {
				if s := walk(child); s != "" {
					return s
				}
			}
		case []any:
			for _, child := range typed {
				if s := walk(child); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return walk(value)
}

func findNumeric(value any, key string) (float64, bool) {
	key = normalizeFieldName(key)
	if key == "" {
		return 0, false
	}
	var walk func(any) (float64, bool)
	walk = func(v any) (float64, bool) {
		switch typed := v.(type) {
		case map[string]any:
			for k, child := range typed {
				if normalizeFieldName(k) == key {
					if number, ok := numericValue(child); ok {
						return number, true
					}
				}
			}
			for _, child := range typed {
				if number, ok := walk(child); ok {
					return number, true
				}
			}
		case []any:
			for _, child := range typed {
				if number, ok := walk(child); ok {
					return number, true
				}
			}
		}
		return 0, false
	}
	return walk(value)
}

func findValue(value any, key string) any {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return nil
	}
	var walk func(any) any
	walk = func(v any) any {
		switch typed := v.(type) {
		case map[string]any:
			for k, child := range typed {
				if strings.ToLower(strings.TrimSpace(k)) == key {
					return child
				}
			}
			for _, child := range typed {
				if found := walk(child); found != nil {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != nil {
					return found
				}
			}
		}
		return nil
	}
	return walk(value)
}

func parseTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
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
		if parsed, err := time.Parse(layout, raw); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
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
