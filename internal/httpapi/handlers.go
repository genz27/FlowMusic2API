package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"flowmusic2api/internal/domain"
	svc "flowmusic2api/internal/service"
	"flowmusic2api/internal/storage"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleGetCacheConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetCacheConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "config": publicCacheConfig(cfg)})
}

func (s *Server) handleUpdateCacheConfig(w http.ResponseWriter, r *http.Request) {
	cfg, _ := s.db.GetCacheConfig(r.Context())
	if cfg == nil {
		cfg = &domain.CacheConfig{}
	}
	var req domain.CacheConfig
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if r.URL.Path == "/api/cache/enabled" {
		cfg.Enabled = req.Enabled
	} else if r.URL.Path == "/api/cache/base-url" {
		cfg.BaseURL = req.BaseURL
	} else {
		if req.S3SecretKey == "" {
			req.S3SecretKey = cfg.S3SecretKey
		}
		cfg = &req
	}
	if err := s.db.UpdateCacheConfig(r.Context(), *cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	fresh, _ := s.db.GetCacheConfig(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "config": publicCacheConfig(fresh)})
}

func publicCacheConfig(cfg *domain.CacheConfig) *domain.CacheConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.S3SecretKey = ""
	if strings.EqualFold(strings.TrimSpace(out.StorageMode), "r2") {
		out.S3PublicBaseURL = ""
		out.EffectiveBaseURL = ""
	} else {
		out.EffectiveBaseURL = firstNonEmpty(out.BaseURL, out.S3PublicBaseURL)
	}
	return &out
}

func (s *Server) handleTestCacheConfig(w http.ResponseWriter, r *http.Request) {
	current, err := s.db.GetCacheConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	testCfg := domain.CacheConfig{StorageMode: "local"}
	if current != nil {
		testCfg = *current
	}
	if r.Body != nil && r.ContentLength != 0 {
		var req domain.CacheConfig
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.S3SecretKey == "" && current != nil {
			req.S3SecretKey = current.S3SecretKey
		}
		testCfg = req
	} else if r.Body != nil {
		_ = r.Body.Close()
	}

	storageMode, err := domain.NormalizeCacheStorageMode(testCfg.StorageMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	testCfg.StorageMode = storageMode

	start := time.Now()
	cache := storage.NewCache(s.cfg, s.db, svc.NewHTTPClient(s.cfg, s.cfg.DefaultProxyURL))
	ref, err := cache.TestConfig(r.Context(), &testCfg)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":    false,
			"message":    err.Error(),
			"elapsed_ms": time.Since(start).Milliseconds(),
			"mode":       firstNonEmpty(testCfg.StorageMode, "local"),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"message":    "缓存配置测试成功",
		"elapsed_ms": time.Since(start).Milliseconds(),
		"mode":       firstNonEmpty(testCfg.StorageMode, "local"),
		"media":      ref,
	})
}

func (s *Server) handleGetGenerationConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetGenerationConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "config": cfg})
}

func (s *Server) handleUpdateGenerationConfig(w http.ResponseWriter, r *http.Request) {
	var req domain.GenerationConfig
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.UpdateGenerationConfig(r.Context(), req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleGetTokenRefreshConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetTokenRefreshConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "config": cfg})
}

func (s *Server) handleUpdateTokenRefreshConfig(w http.ResponseWriter, r *http.Request) {
	var req domain.TokenRefreshConfig
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.UpdateTokenRefreshConfig(r.Context(), req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleUpdateTokenRefreshEnabled(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetTokenRefreshConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cfg.Enabled = req.Enabled
	cfg.ATAutoRefreshEnabled = req.Enabled
	if err := s.db.UpdateTokenRefreshConfig(r.Context(), *cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "config": cfg})
}

func (s *Server) handleGetCallLogicConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetCallLogicConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "config": cfg})
}

func (s *Server) handleUpdateCallLogicConfig(w http.ResponseWriter, r *http.Request) {
	var req domain.CallLogicConfig
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.UpdateCallLogicConfig(r.Context(), req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cfg, _ := s.db.GetCallLogicConfig(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "config": cfg})
}

func (s *Server) handleGetAdminConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetAdminConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleUpdateAdminConfig(w http.ResponseWriter, r *http.Request) {
	current, err := s.db.GetAdminConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var req struct {
		Username              string `json:"admin_username"`
		APIKey                string `json:"api_key"`
		DebugEnabled          *bool  `json:"debug_enabled"`
		ErrorBanThreshold     int    `json:"error_ban_threshold"`
		GuestTrialEnabled     *bool  `json:"guest_trial_enabled"`
		MaxDailyGuestUses     *int   `json:"max_daily_guest_uses"`
		GuestGlobalDailyLimit *int   `json:"guest_global_daily_limit"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	username := firstNonEmpty(req.Username, current.Username)
	apiKey := firstNonEmpty(req.APIKey, current.APIKey)
	debugEnabled := current.DebugEnabled
	if req.DebugEnabled != nil {
		debugEnabled = *req.DebugEnabled
	}
	errorBanThreshold := current.ErrorBan
	if req.ErrorBanThreshold > 0 {
		errorBanThreshold = req.ErrorBanThreshold
	}
	if errorBanThreshold <= 0 {
		errorBanThreshold = 3
	}
	guestTrialEnabled := current.GuestTrialEnabled
	if req.GuestTrialEnabled != nil {
		guestTrialEnabled = *req.GuestTrialEnabled
	}
	maxDailyGuestUses := current.MaxDailyGuestUses
	if req.MaxDailyGuestUses != nil {
		maxDailyGuestUses = *req.MaxDailyGuestUses
	}
	guestGlobalDailyLimit := current.GuestGlobalDailyLimit
	if req.GuestGlobalDailyLimit != nil {
		guestGlobalDailyLimit = *req.GuestGlobalDailyLimit
	}
	if err := s.db.UpdateAdminConfig(r.Context(), username, apiKey, debugEnabled, errorBanThreshold, guestTrialEnabled, maxDailyGuestUses, guestGlobalDailyLimit); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	updated, _ := s.db.GetAdminConfig(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "config": updated})
}

func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username    string `json:"username"`
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cfg, err := s.db.GetAdminConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if comparePassword(cfg.PasswordHash, req.OldPassword) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "detail": "旧密码不正确"})
		return
	}
	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Username == "" {
		req.Username = cfg.Username
	}
	if err := s.db.UpdateAdminPassword(r.Context(), req.Username, hash); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleUpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NewAPIKey string `json:"new_api_key"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.NewAPIKey) == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("new_api_key is required"))
		return
	}
	if err := s.db.UpdateAPIKey(r.Context(), strings.TrimSpace(req.NewAPIKey)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleUpdateDebug(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.UpdateDebug(r.Context(), req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	s.finalizeStaleActiveLogs(r.Context())
	logs, err := s.db.GetLogs(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

func (s *Server) handleActiveLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	s.finalizeStaleActiveLogs(r.Context())
	logs, err := s.db.GetActiveLogs(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(logs))
	for _, log := range logs {
		startedAt := log.CreatedAt
		if startedAt != nil && log.DurationMS <= 0 {
			log.DurationMS = now.Sub(startedAt.UTC()).Milliseconds()
			if log.DurationMS < 0 {
				log.DurationMS = 0
			}
			log.Duration = float64(log.DurationMS) / 1000
		}
		out = append(out, map[string]any{
			"id":            log.ID,
			"token_id":      log.AccountID,
			"token_email":   log.AccountEmail,
			"operation":     log.Operation,
			"status_code":   log.StatusCode,
			"status_text":   log.StatusText,
			"progress":      log.Progress,
			"duration_ms":   log.DurationMS,
			"duration":      log.Duration,
			"error_summary": log.ErrorSummary,
			"started_at":    startedAt,
			"created_at":    log.CreatedAt,
			"updated_at":    log.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) finalizeStaleActiveLogs(ctx context.Context) {
	staleAfter := s.cfg.GenerationTimeout
	if dbCfg, err := s.db.GetGenerationConfig(ctx); err == nil && dbCfg != nil {
		totalSeconds := dbCfg.Timeout
		if totalSeconds <= 0 {
			totalSeconds = dbCfg.ImageTimeout + dbCfg.VideoTimeout
		}
		if totalSeconds > 0 {
			staleAfter = time.Duration(totalSeconds) * time.Second
		}
	}
	if staleAfter <= 0 {
		staleAfter = 30 * time.Minute
	}
	_ = s.db.FinalizeStaleActiveLogs(ctx, time.Now().UTC().Add(-(staleAfter + 2*time.Minute)))
}

func (s *Server) handleLogDetail(w http.ResponseWriter, r *http.Request) {
	id, err := routeID(r, "logID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	log, err := s.db.GetLogDetail(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, log)
}

func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	if err := s.db.ClearLogs(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleProxyConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req domain.ProxyConfig
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !req.ProxyEnabled {
			req.ProxyURL = ""
		}
		if !req.MediaProxyEnabled {
			req.MediaProxyURL = ""
		}
		if err := s.db.UpdateProxyConfig(r.Context(), req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	cfg, err := s.db.GetProxyConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"proxy_enabled":       cfg.ProxyEnabled,
		"proxy_url":           cfg.ProxyURL,
		"media_proxy_enabled": cfg.MediaProxyEnabled,
		"media_proxy_url":     cfg.MediaProxyURL,
		"success":             true,
	})
}

func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxy_url"`
		TestURL  string `json:"test_url"`
	}
	_ = readJSON(r, &req)
	start := time.Now()
	targetURL := strings.TrimSpace(req.TestURL)
	if targetURL == "" {
		targetURL = "https://www.flowmusic.app/"
	}
	client := svc.NewHTTPClient(s.cfg, req.ProxyURL)
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": err.Error(), "elapsed_ms": time.Since(start).Milliseconds()})
		return
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": err.Error(), "elapsed_ms": time.Since(start).Milliseconds()})
		return
	}
	defer resp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]any{
		"success":     resp.StatusCode < 400,
		"message":     resp.Status,
		"status_code": resp.StatusCode,
		"elapsed_ms":  time.Since(start).Milliseconds(),
		"final_url":   resp.Request.URL.String(),
	})
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	models := flowMusicModels(false)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleListModelAliases(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": flowMusicModels(true)})
}

func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request) {
	modelID := strings.TrimSpace(chi.URLParam(r, "modelID"))
	model, ok := flowMusicModel(modelID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, fmt.Errorf("model %q not found", modelID))
		return
	}
	writeJSON(w, http.StatusOK, model)
}

func flowMusicModels(includeAliases bool) []map[string]any {
	modelDefs := domain.GenerationModels()
	models := make([]map[string]any, 0, len(modelDefs))
	for _, model := range modelDefs {
		models = append(models, map[string]any{
			"id":          model.ID,
			"object":      "model",
			"name":        model.Name,
			"description": model.Description,
		})
	}
	if includeAliases {
		for _, model := range modelDefs {
			for _, alias := range model.Aliases {
				models = append(models, map[string]any{
					"id":          alias,
					"object":      "model",
					"description": "Alias of " + model.ID,
					"is_alias":    true,
					"target":      model.ID,
				})
			}
		}
	}
	return models
}

func flowMusicModel(modelID string) (map[string]any, bool) {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	for _, model := range flowMusicModels(true) {
		if strings.ToLower(fmt.Sprint(model["id"])) == modelID {
			return model, true
		}
	}
	return nil, false
}

func (s *Server) handleSora2Activate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false,
		"message": "FlowMusic2API 不支持 Sora2 激活；该路由仅用于兼容旧客户端。",
	})
}

func (s *Server) handleYun139Unsupported(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": false,
		"message": "FlowMusic2API 当前仅支持 local 与 S3/R2 缓存；139 云盘路由仅用于兼容旧客户端。",
		"files":   []any{},
	})
}

func (s *Server) handleGuestConfig(w http.ResponseWriter, r *http.Request) {
	enabled := s.guestTrialEnabled(r)
	maxDaily := s.guestMaxDaily(r)
	globalMax := s.guestGlobalMaxDaily(r)
	remaining := 0
	globalRemaining := 0
	if enabled && maxDaily > 0 {
		used, _ := s.db.GetTodayGuestUsage(r.Context(), guestClientKey(r))
		remaining = maxDaily - used
		if remaining < 0 {
			remaining = 0
		}
	}
	if enabled && globalMax > 0 {
		globalUsed, _ := s.db.GetTodayGlobalGuestUsage(r.Context())
		globalRemaining = globalMax - globalUsed
		if globalRemaining < 0 {
			globalRemaining = 0
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":                     true,
		"guest_trial_enabled":         enabled,
		"max_daily_guest_uses":        maxDaily,
		"remaining_daily_uses":        remaining,
		"remaining_global_daily_uses": globalRemaining,
		"models":                      flowMusicModels(true),
	})
}

func (s *Server) handleGuestChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.guestTrialEnabled(r) {
		writeOpenAIError(w, http.StatusForbidden, fmt.Errorf("guest trial is disabled"))
		return
	}
	if s.guestDailyLimitReached(r) {
		writeOpenAIError(w, http.StatusTooManyRequests, fmt.Errorf("daily guest trial limit reached"))
		return
	}
	release, ok := s.acquireGuestSlot(r)
	if !ok {
		writeOpenAIError(w, http.StatusTooManyRequests, fmt.Errorf("guest trial already has a running generation from this IP"))
		return
	}
	defer release()
	s.handleChatCompletions(w, r)
	s.incrementGuestUsage(r)
}

func (s *Server) handleGuestAudioGenerations(w http.ResponseWriter, r *http.Request) {
	if !s.guestTrialEnabled(r) {
		writeOpenAIError(w, http.StatusForbidden, fmt.Errorf("guest trial is disabled"))
		return
	}
	if s.guestDailyLimitReached(r) {
		writeOpenAIError(w, http.StatusTooManyRequests, fmt.Errorf("daily guest trial limit reached"))
		return
	}
	release, ok := s.acquireGuestSlot(r)
	if !ok {
		writeOpenAIError(w, http.StatusTooManyRequests, fmt.Errorf("guest trial already has a running generation from this IP"))
		return
	}
	defer release()
	s.handleAudioGenerations(w, r)
	s.incrementGuestUsage(r)
}

func (s *Server) handleAudioGenerations(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string        `json:"model"`
		Prompt   string        `json:"prompt"`
		Input    string        `json:"input"`
		Messages []chatMessage `json:"messages"`
	}
	if err := readJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err)
		return
	}
	prompt := firstNonEmpty(sanitizeGenerationPrompt(req.Prompt), sanitizeGenerationPrompt(req.Input), extractPrompt(req.Messages))
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Errorf("prompt, input, or messages is required"))
		return
	}
	model, err := normalizeGenerationModel(req.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.generation.Generate(r.Context(), prompt, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created":   time.Now().Unix(),
		"data":      audioGenerationData(out),
		"clips":     audioGenerationData(out),
		"flowmusic": out,
	})
}

func (s *Server) handleMusicResults(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID    int64    `json:"account_id"`
		JobID        string   `json:"job_id"`
		OperationID  string   `json:"operation_id"`
		OperationIDs []string `json:"operation_ids"`
		ClipID       string   `json:"clip_id"`
		ClipIDs      []string `json:"clip_ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err)
		return
	}
	operationIDs := append([]string{}, req.OperationIDs...)
	if req.OperationID != "" {
		operationIDs = append(operationIDs, req.OperationID)
	}
	clipIDs := append([]string{}, req.ClipIDs...)
	if req.ClipID != "" {
		clipIDs = append(clipIDs, req.ClipID)
	}
	if strings.TrimSpace(req.JobID) == "" && len(operationIDs) == 0 && len(clipIDs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Errorf("job_id, operation_id, operation_ids, clip_id, or clip_ids is required"))
		return
	}
	out, err := s.generation.LookupResult(r.Context(), svc.GenerationResultLookup{
		AccountID:    req.AccountID,
		JobID:        req.JobID,
		OperationIDs: operationIDs,
		ClipIDs:      clipIDs,
	}, nil)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created":   time.Now().Unix(),
		"data":      audioGenerationData(out),
		"clips":     audioGenerationData(out),
		"flowmusic": out,
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatCompletionRequest
	if err := readJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err)
		return
	}
	prompt := extractPrompt(req.Messages)
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Errorf("messages prompt is required"))
		return
	}
	model, err := normalizeGenerationModel(req.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err)
		return
	}
	responseID := "chatcmpl-flowmusic-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	created := time.Now().Unix()
	if req.Stream {
		s.streamChatCompletionGeneration(r.Context(), w, responseID, created, model, prompt)
		return
	}
	out, err := s.generation.Generate(r.Context(), prompt, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err)
		return
	}
	content := renderGenerationMarkdown(out)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        responseID,
		"object":    "chat.completion",
		"created":   created,
		"model":     model,
		"choices":   []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":     map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		"data":      audioGenerationData(out),
		"clips":     audioGenerationData(out),
		"flowmusic": out,
	})
}

func normalizeGenerationModel(modelID string) (string, error) {
	model, ok := domain.ResolveGenerationModel(modelID)
	if !ok {
		return "", fmt.Errorf("model %q not found", modelID)
	}
	return model.ID, nil
}

func (s *Server) streamChatCompletionGeneration(ctx context.Context, w http.ResponseWriter, responseID string, created int64, model, prompt string) {
	stream := newChatCompletionStream(w)
	stream.writeChunk(responseID, created, model, map[string]any{"role": "assistant"}, nil)
	stream.flush()

	lastProgress := ""
	out, err := s.generation.GenerateWithProgress(ctx, prompt, model, func(progress svc.GenerationProgress) {
		message := strings.TrimSpace(progress.Message)
		if message == "" || message == lastProgress {
			return
		}
		lastProgress = message
		if progress.Progress > 0 {
			message = fmt.Sprintf("[%d%%] %s", progress.Progress, message)
		}
		stream.writeReasoningChunk(responseID, created, model, message+"\n")
	})
	if err != nil {
		stream.writeError(err)
		stream.done()
		return
	}
	stream.writeChunk(responseID, created, model, map[string]any{"content": renderGenerationMarkdown(out)}, "stop")
	stream.done()
}

func (s *Server) streamChatCompletion(w http.ResponseWriter, responseID string, created int64, model, content string) {
	stream := newChatCompletionStream(w)
	stream.writeChunk(responseID, created, model, map[string]any{"role": "assistant"}, nil)
	stream.flush()
	stream.writeChunk(responseID, created, model, map[string]any{"content": content}, nil)
	stream.writeChunk(responseID, created, model, map[string]any{}, "stop")
	stream.done()
}

type chatCompletionStream struct {
	writer  *bufio.Writer
	flusher http.Flusher
}

func newChatCompletionStream(w http.ResponseWriter) *chatCompletionStream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	stream := &chatCompletionStream{writer: bufio.NewWriter(w)}
	if flusher, ok := w.(http.Flusher); ok {
		stream.flusher = flusher
	}
	return stream
}

func (s *chatCompletionStream) flush() {
	_ = s.writer.Flush()
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *chatCompletionStream) writeChunk(responseID string, created int64, model string, delta map[string]any, finishReason any) {
	chunk := map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{"delta": delta, "index": 0, "finish_reason": finishReason}},
	}
	data, _ := json.Marshal(chunk)
	_, _ = s.writer.WriteString("data: " + string(data) + "\n\n")
}

func (s *chatCompletionStream) writeReasoningChunk(responseID string, created int64, model string, content string) {
	s.writeChunk(responseID, created, model, map[string]any{"reasoning_content": content}, nil)
	s.flush()
}

func (s *chatCompletionStream) writeError(err error) {
	if err == nil {
		err = errors.New("stream generation failed")
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    "upstream_error",
			"code":    http.StatusBadGateway,
		},
	}
	data, _ := json.Marshal(payload)
	_, _ = s.writer.WriteString("data: " + string(data) + "\n\n")
	s.flush()
}

func (s *chatCompletionStream) done() {
	_, _ = s.writer.WriteString("data: [DONE]\n\n")
	s.flush()
}

type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func audioGenerationData(out svc.GenerationOutput) []map[string]any {
	data := make([]map[string]any, 0, len(out.Clips))
	for _, clip := range out.Clips {
		item := map[string]any{
			"id":      clip.ID,
			"clip_id": clip.ID,
			"title":   clip.Title,
		}
		if clip.Audio.URL != "" {
			format := audioRefFormat(clip.Audio)
			item["url"] = clip.Audio.URL
			item["audio_url"] = clip.Audio.URL
			item["format"] = format
			item["audio"] = mediaRefData(clip.Audio, format)
		}
		if clip.Wav != nil && clip.Wav.URL != "" {
			item["wav_url"] = clip.Wav.URL
			item["wav"] = mediaRefData(*clip.Wav, "wav")
		}
		if clip.Image != nil && clip.Image.URL != "" {
			item["image_url"] = clip.Image.URL
			item["cover_url"] = clip.Image.URL
			item["image"] = mediaRefData(*clip.Image, mediaRefFormat(*clip.Image))
		}
		if clip.Video != nil && clip.Video.URL != "" {
			item["video_url"] = clip.Video.URL
			item["video"] = mediaRefData(*clip.Video, mediaRefFormat(*clip.Video))
		}
		if clip.Lyrics != "" {
			item["lyrics"] = clip.Lyrics
		}
		if clip.LyricsID != "" {
			item["lyrics_id"] = clip.LyricsID
		}
		if clip.SoundPrompt != "" {
			item["sound_prompt"] = clip.SoundPrompt
		}
		if clip.OperationID != "" {
			item["operation_id"] = clip.OperationID
		}
		if clip.OperationType != "" {
			item["operation_type"] = clip.OperationType
		}
		if clip.DurationSeconds > 0 {
			item["duration"] = clip.DurationSeconds
			item["duration_seconds"] = clip.DurationSeconds
		}
		if clip.CreatedAt != "" {
			item["created_at"] = clip.CreatedAt
		}
		data = append(data, item)
	}
	return data
}

func mediaRefData(ref domain.MediaRef, format string) map[string]any {
	out := map[string]any{
		"url":          ref.URL,
		"original_url": ref.OriginalURL,
	}
	if ref.LocalURL != "" {
		out["local_url"] = ref.LocalURL
	}
	if ref.ContentType != "" {
		out["content_type"] = ref.ContentType
	}
	if ref.Size > 0 {
		out["size"] = ref.Size
	}
	if format != "" {
		out["format"] = format
	}
	return out
}

func mediaRefFormat(ref domain.MediaRef) string {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(ref.ContentType, ";")[0]))
	switch contentType {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "video/mp4":
		return "mp4"
	case "video/x-msvideo", "video/avi", "video/msvideo":
		return "avi"
	}
	urlText := strings.ToLower(ref.URL)
	if ref.OriginalURL != "" {
		urlText += " " + strings.ToLower(ref.OriginalURL)
	}
	for _, ext := range []string{"jpg", "jpeg", "png", "webp", "gif", "mp4", "avi"} {
		if strings.Contains(urlText, "."+ext+"?") || strings.Contains(urlText, "."+ext+"#") || strings.HasSuffix(urlText, "."+ext) {
			if ext == "jpeg" {
				return "jpg"
			}
			return ext
		}
	}
	return ""
}

func audioRefFormat(ref domain.MediaRef) string {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(ref.ContentType, ";")[0]))
	switch contentType {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/mp4", "audio/x-m4a":
		return "m4a"
	case "audio/aac":
		return "aac"
	case "audio/ogg", "application/ogg":
		return "ogg"
	case "audio/flac":
		return "flac"
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav"
	}
	urlText := strings.ToLower(ref.URL)
	if ref.OriginalURL != "" {
		urlText += " " + strings.ToLower(ref.OriginalURL)
	}
	for _, ext := range []string{"mp3", "m4a", "aac", "ogg", "flac", "wav"} {
		if strings.Contains(urlText, "."+ext+"?") || strings.Contains(urlText, "."+ext+"#") || strings.HasSuffix(urlText, "."+ext) {
			return ext
		}
	}
	return "audio"
}

type accountCapabilityDefaults struct {
	ImageEnabled   bool
	VideoEnabled   bool
	UpscaleEnabled bool
}

func defaultAccountCapabilityDefaults() accountCapabilityDefaults {
	return accountCapabilityDefaults{ImageEnabled: true, VideoEnabled: true, UpscaleEnabled: true}
}

func accountCapabilityDefaultsFromAccount(account domain.Account) accountCapabilityDefaults {
	return accountCapabilityDefaults{
		ImageEnabled:   account.ImageEnabled,
		VideoEnabled:   account.VideoEnabled,
		UpscaleEnabled: account.UpscaleEnabled,
	}
}

func applyAccountCapabilityRequest(account *domain.Account, imageEnabled, videoEnabled, upscaleEnabled *bool, defaults accountCapabilityDefaults) {
	account.ImageEnabled = defaults.ImageEnabled
	account.VideoEnabled = defaults.VideoEnabled
	account.UpscaleEnabled = defaults.UpscaleEnabled
	if imageEnabled != nil {
		account.ImageEnabled = *imageEnabled
	}
	if videoEnabled != nil {
		account.VideoEnabled = *videoEnabled
	}
	if upscaleEnabled != nil {
		account.UpscaleEnabled = *upscaleEnabled
	}
	account.CapabilityFlagsSet = true
}

func applyAccountCapabilityMap(account *domain.Account, item map[string]any, defaults accountCapabilityDefaults) {
	account.ImageEnabled = defaults.ImageEnabled
	account.VideoEnabled = defaults.VideoEnabled
	account.UpscaleEnabled = defaults.UpscaleEnabled
	if value, ok := boolFromMap(item, "image_enabled"); ok {
		account.ImageEnabled = value
	}
	if value, ok := boolFromMap(item, "video_enabled"); ok {
		account.VideoEnabled = value
	}
	if value, ok := boolFromMap(item, "upscale_enabled"); ok {
		account.UpscaleEnabled = value
	}
	account.CapabilityFlagsSet = true
}

func readAccountRequest(r *http.Request, defaultAutoRefreshEnabled bool, capabilityDefaults accountCapabilityDefaults) (domain.Account, error) {
	var raw map[string]json.RawMessage
	if err := readJSON(r, &raw); err != nil {
		return domain.Account{}, err
	}
	var req struct {
		ST                   string `json:"st"`
		AT                   string `json:"at"`
		RefreshToken         string `json:"refresh_token"`
		SessionToken         string `json:"session_token"`
		AccessToken          string `json:"access_token"`
		FlowBearer           string `json:"flow_bearer"`
		Token                string `json:"token"`
		ProviderToken        string `json:"provider_token"`
		ProviderRefreshToken string `json:"provider_refresh_token"`
		Email                string `json:"email"`
		Name                 string `json:"name"`
		Remark               string `json:"remark"`
		ProtocolMode         string `json:"protocol_mode"`
		GoogleCookies        string `json:"google_cookies"`
		LoginAccount         string `json:"login_account"`
		LoginPassword        string `json:"login_password"`
		ProxyURL             string `json:"proxy_url"`
		AutoRefreshEnabled   *bool  `json:"auto_refresh_enabled"`
		RefreshIntervalMins  int    `json:"refresh_interval_minutes"`
		ImageEnabled         *bool  `json:"image_enabled"`
		VideoEnabled         *bool  `json:"video_enabled"`
		UpscaleEnabled       *bool  `json:"upscale_enabled"`
		ImageConcurrency     int    `json:"image_concurrency"`
		VideoConcurrency     int    `json:"video_concurrency"`
	}
	data, _ := json.Marshal(raw)
	if err := json.Unmarshal(data, &req); err != nil {
		return domain.Account{}, err
	}
	autoRefreshEnabled := defaultAutoRefreshEnabled
	if req.AutoRefreshEnabled != nil {
		autoRefreshEnabled = *req.AutoRefreshEnabled
	}
	explicit := explicitAccountFields(raw)
	refreshToken := firstNonEmpty(req.RefreshToken, req.ST, req.SessionToken)
	flowBearer := firstNonEmpty(req.FlowBearer, req.AT, req.AccessToken, req.Token)
	if isSupabaseAccessToken(flowBearer) {
		flowBearer = ""
	}
	protocolMode := ""
	if explicit["protocol_mode"] {
		protocolMode = domain.NormalizeProtocolMode(req.ProtocolMode)
	}
	account := domain.Account{
		Email: req.Email, Name: req.Name, Remark: req.Remark, ProtocolMode: protocolMode,
		RefreshToken: refreshToken, ST: refreshToken,
		AccessToken: flowBearer, ProviderToken: req.ProviderToken, ProviderRefreshToken: req.ProviderRefreshToken,
		FlowBearer: flowBearer, AT: flowBearer,
		Cookies: req.GoogleCookies, LoginAccount: req.LoginAccount, LoginPassword: req.LoginPassword, ProxyURL: req.ProxyURL,
		AutoRefreshEnabled: autoRefreshEnabled, RefreshIntervalMins: req.RefreshIntervalMins,
		ImageConcurrency: req.ImageConcurrency, VideoConcurrency: req.VideoConcurrency,
		ExplicitFields: explicit,
		ClearFields:    clearFieldsFromRaw(raw["clear_fields"]),
	}
	applyClearFields(&account, account.ClearFields)
	applyAccountCapabilityRequest(&account, req.ImageEnabled, req.VideoEnabled, req.UpscaleEnabled, capabilityDefaults)
	account = autoParseCookieJSON(account)
	return account, nil
}

func autoParseCookieJSON(account domain.Account) domain.Account {
	cookieStr := strings.TrimSpace(account.Cookies)
	if !strings.HasPrefix(cookieStr, "[") {
		return account
	}
	var cookieArr []map[string]any
	if err := json.Unmarshal([]byte(cookieStr), &cookieArr); err != nil {
		return account
	}
	var parts []string
	for _, c := range cookieArr {
		name := stringFromMap(c, "name")
		value := stringFromMap(c, "value")
		if strings.HasPrefix(name, "sb-sb-auth-token.") && value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	if len(parts) > 0 {
		account.Cookies = strings.Join(parts, "; ")
	}
	items, ok, err := importItemsFromFlowMusicCookieExport(cookieArr)
	if !ok || err != nil || len(items) == 0 {
		return account
	}
	item := items[0]
	if account.RefreshToken == "" {
		account.RefreshToken = stringFromMap(item, "refresh_token")
		account.ST = account.RefreshToken
	}
	if account.ProviderToken == "" {
		account.ProviderToken = stringFromMap(item, "provider_token")
	}
	if account.ProviderRefreshToken == "" {
		account.ProviderRefreshToken = stringFromMap(item, "provider_refresh_token")
	}
	if account.Email == "" {
		account.Email = stringFromMap(item, "email")
	}
	if account.Name == "" {
		account.Name = stringFromMap(item, "name")
	}
	account.ProtocolMode = "refresh_token"
	if account.RefreshIntervalMins <= 0 {
		if v, ok := item["refresh_interval_minutes"]; ok {
			if f, ok := v.(float64); ok {
				account.RefreshIntervalMins = int(f)
			}
		}
	}
	return account
}

func isSupabaseAccessToken(value string) bool {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Issuer   string `json:"iss"`
		Audience string `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return false
	}
	issuer := strings.ToLower(strings.TrimSpace(claims.Issuer))
	audience := strings.ToLower(strings.TrimSpace(claims.Audience))
	return strings.Contains(issuer, "/auth/v1") && audience == "authenticated"
}

func explicitAccountFields(raw map[string]json.RawMessage) map[string]bool {
	fields := map[string]bool{}
	mark := func(logical string, keys ...string) {
		for _, key := range keys {
			if _, ok := raw[key]; ok {
				fields[logical] = true
				return
			}
		}
	}
	mark("email", "email")
	mark("name", "name")
	mark("remark", "remark")
	mark("protocol_mode", "protocol_mode")
	mark("refresh_token", "refresh_token", "st", "session_token")
	mark("flow_bearer", "flow_bearer", "at", "access_token", "token")
	mark("provider_token", "provider_token")
	mark("provider_refresh_token", "provider_refresh_token")
	mark("cookies", "google_cookies")
	mark("login_account", "login_account")
	mark("login_password", "login_password")
	mark("proxy_url", "proxy_url")
	for field := range clearFieldsFromRaw(raw["clear_fields"]) {
		fields[field] = true
	}
	return fields
}

func clearFieldsFromRaw(raw json.RawMessage) map[string]bool {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields []string
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	return canonicalClearFields(fields)
}

func clearFieldsFromMap(item map[string]any) map[string]bool {
	raw, ok := item["clear_fields"]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case []string:
		return canonicalClearFields(typed)
	case []any:
		values := make([]string, 0, len(typed))
		for _, value := range typed {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
				values = append(values, text)
			}
		}
		return canonicalClearFields(values)
	default:
		return nil
	}
}

func canonicalClearFields(values []string) map[string]bool {
	fields := map[string]bool{}
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "st", "refresh", "refresh_token", "session_token":
			fields["refresh_token"] = true
		case "at", "access_token", "flow_bearer", "bearer", "token":
			fields["flow_bearer"] = true
		case "provider_token", "google_access_token":
			fields["provider_token"] = true
		case "provider_refresh_token", "google_refresh_token":
			fields["provider_refresh_token"] = true
		case "google_cookies", "cookies", "cookie":
			fields["cookies"] = true
		case "proxy", "proxy_url":
			fields["proxy_url"] = true
		case "login_account":
			fields["login_account"] = true
		case "login_password":
			fields["login_password"] = true
		}
	}
	return fields
}

func applyClearFields(account *domain.Account, fields map[string]bool) {
	if account == nil || len(fields) == 0 {
		return
	}
	if fields["refresh_token"] {
		account.RefreshToken = ""
		account.ST = ""
	}
	if fields["flow_bearer"] {
		account.FlowBearer = ""
		account.AT = ""
		account.AccessToken = ""
	}
	if fields["provider_token"] {
		account.ProviderToken = ""
	}
	if fields["provider_refresh_token"] {
		account.ProviderRefreshToken = ""
	}
	if fields["cookies"] {
		account.Cookies = ""
	}
	if fields["proxy_url"] {
		account.ProxyURL = ""
	}
	if fields["login_account"] {
		account.LoginAccount = ""
	}
	if fields["login_password"] {
		account.LoginPassword = ""
	}
}

func extractPrompt(messages []chatMessage) string {
	var lastUser string
	var lastPrompt string
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "system" || role == "developer" {
			continue
		}
		if role != "" && role != "user" {
			continue
		}
		text := sanitizeGenerationPrompt(messageContentText(message.Content))
		if text == "" {
			continue
		}
		if role == "user" || role == "" {
			lastUser = text
		}
		lastPrompt = text
	}
	if lastUser != "" {
		return lastUser
	}
	return lastPrompt
}

func sanitizeGenerationPrompt(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	for _, tag := range []string{"tools", "tool_call", "tool_calls", "function_calls"} {
		text = stripTaggedBlock(text, tag)
	}
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "system:") || strings.HasPrefix(lower, "developer:") {
			continue
		}
		if strings.Contains(lower, "you are chatgpt") || strings.Contains(lower, "function calling") {
			continue
		}
		kept = append(kept, line)
	}
	text = strings.TrimSpace(strings.Join(kept, "\n"))
	if len([]rune(text)) > 4000 {
		runes := []rune(text)
		text = string(runes[:4000])
	}
	return text
}

func stripTaggedBlock(text, tag string) string {
	startToken := "<" + strings.ToLower(tag)
	endToken := "</" + strings.ToLower(tag) + ">"
	for {
		lower := strings.ToLower(text)
		start := strings.Index(lower, startToken)
		if start < 0 {
			return text
		}
		endRel := strings.Index(lower[start:], endToken)
		if endRel < 0 {
			return strings.TrimSpace(text[:start])
		}
		end := start + endRel + len(endToken)
		text = text[:start] + text[end:]
	}
}

func messageContentText(content any) string {
	switch typed := content.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]any); ok {
				itemType := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				if itemType == "text" || itemType == "input_text" || itemType == "" {
					if text := strings.TrimSpace(fmt.Sprint(m["text"])); text != "" && text != "<nil>" {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func renderGenerationMarkdown(out svc.GenerationOutput) string {
	if len(out.Clips) == 0 {
		if out.GeneratedRaw != "" {
			return out.GeneratedRaw
		}
		return fmt.Sprintf("FlowMusic job %s 已完成，但未解析到 clip。", out.JobID)
	}
	var b strings.Builder
	for i, clip := range out.Clips {
		title := firstNonEmpty(clip.Title, clip.ID, fmt.Sprintf("clip-%d", i+1))
		b.WriteString("### ")
		b.WriteString(title)
		b.WriteString("\n\n")
		if clip.Audio.URL != "" {
			b.WriteString("- Audio: ")
			b.WriteString(clip.Audio.URL)
			b.WriteString("\n")
		}
		if clip.Wav != nil && clip.Wav.URL != "" {
			b.WriteString("- WAV: ")
			b.WriteString(clip.Wav.URL)
			b.WriteString("\n")
		}
		if clip.Image != nil && clip.Image.URL != "" {
			b.WriteString("- Image: ")
			b.WriteString(clip.Image.URL)
			b.WriteString("\n")
		}
		if clip.Video != nil && clip.Video.URL != "" {
			b.WriteString("- Video: ")
			b.WriteString(clip.Video.URL)
			b.WriteString("\n")
		}
		if clip.DurationSeconds > 0 {
			b.WriteString("- Duration: ")
			b.WriteString(strconv.FormatFloat(clip.DurationSeconds, 'f', 2, 64))
			b.WriteString("s\n")
		}
		if clip.SoundPrompt != "" {
			b.WriteString("- Sound Prompt: ")
			b.WriteString(clip.SoundPrompt)
			b.WriteString("\n")
		}
		if clip.Lyrics != "" {
			b.WriteString("- Lyrics:\n\n```text\n")
			b.WriteString(strings.TrimSpace(clip.Lyrics))
			b.WriteString("\n```\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	if err == nil {
		err = errors.New(http.StatusText(status))
	}
	writeJSON(w, status, map[string]any{
		"error":   err.Error(),
		"message": err.Error(),
		"detail":  err.Error(),
		"success": false,
	})
}

func writeOpenAIError(w http.ResponseWriter, status int, err error) {
	if err == nil {
		err = errors.New(http.StatusText(status))
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    "invalid_request_error",
			"code":    status,
		},
		"message": err.Error(),
		"success": false,
	})
}

const maxJSONBodyBytes = 8 << 20

func readJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxJSONBodyBytes {
		return fmt.Errorf("request body exceeds %d bytes", maxJSONBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request body must contain a single JSON value")
	}
	return nil
}

func routeID(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, name), 10, 64)
}

func bearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

func apiKeyFromRequest(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	for _, key := range []string{"x-api-key", "x-goog-api-key"} {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			return value
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("key"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" && strings.TrimSpace(value) != "<nil>" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boolWithDefault(m map[string]any, key string, fallback bool) bool {
	if value, ok := boolFromMap(m, key); ok {
		return value
	}
	return fallback
}

func stringFromMap(m map[string]any, key string) string {
	value, ok := m[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	default:
		text := strings.TrimSpace(fmt.Sprint(typed))
		if text == "<nil>" {
			return ""
		}
		return text
	}
}

func boolFromMap(m map[string]any, key string) (bool, bool) {
	value, ok := m[key]
	if !ok || value == nil {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	case float64:
		return typed != 0, true
	case int:
		return typed != 0, true
	}
	return false, false
}
