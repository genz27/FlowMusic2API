package httpapi

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
	"flowmusic2api/internal/service"
	"flowmusic2api/internal/store"

	"github.com/go-chi/chi/v5"
)

type Server struct {
	cfg        config.Config
	db         *store.Store
	flow       *service.FlowMusicClient
	accounts   *service.AccountService
	generation *service.GenerationService
	guestMu    sync.Mutex
	guestBusy  map[string]int
}

func New(cfg config.Config, db *store.Store, flow *service.FlowMusicClient, accounts *service.AccountService, generation *service.GenerationService) *Server {
	return &Server{cfg: cfg, db: db, flow: flow, accounts: accounts, generation: generation, guestBusy: make(map[string]int)}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Get("/", s.handleRoot)
	r.Get("/login", s.servePage("login.html"))
	r.Get("/manage", s.servePage("manage.html"))
	r.Get("/test", s.servePage("test.html"))
	r.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/health", s.handleHealth)
	r.Get("/healthz", s.handleHealth)
	r.Get("/healthz-v2", s.handleHealth)
	r.Get("/v1/models", s.handleListModels)
	r.Handle("/tmp/*", http.StripPrefix("/tmp/", http.FileServer(http.Dir(s.cfg.CacheDir))))

	r.Post("/api/login", s.handleLogin)
	r.Post("/api/admin/login", s.handleLogin)
	r.Get("/api/guest/config", s.handleGuestConfig)
	r.Post("/api/guest/chat/completions", s.handleGuestChatCompletions)
	r.Post("/api/guest/audio/generations", s.handleGuestAudioGenerations)
	r.Post("/api/guest/music/generations", s.handleGuestAudioGenerations)

	r.Group(func(ar chi.Router) {
		ar.Use(s.adminAuthMiddleware)
		ar.Post("/api/logout", s.handleLogout)
		ar.Post("/api/admin/logout", s.handleLogout)
		ar.Get("/api/stats", s.handleStats)
		ar.Get("/api/system/info", s.handleSystemInfo)
		ar.Get("/api/tokens", s.handleListTokens)
		ar.Post("/api/tokens", s.handleCreateToken)
		ar.Get("/api/tokens/export", s.handleExportTokens)
		ar.Post("/api/tokens/import", s.handleImportTokens)
		ar.Post("/api/tokens/st2at", s.handleSTToAT)
		ar.Get("/api/tokens/{tokenID}", s.handleGetToken)
		ar.Post("/api/tokens/{tokenID}/test", s.handleTestToken)
		ar.Post("/api/tokens/{tokenID}/refresh-credits", s.handleRefreshCredits)
		ar.Post("/api/tokens/{tokenID}/refresh-at", s.handleRefreshAT)
		ar.Post("/api/tokens/{tokenID}/enable", s.handleEnableToken)
		ar.Post("/api/tokens/{tokenID}/disable", s.handleDisableToken)
		ar.Put("/api/tokens/{tokenID}/status", s.handleUpdateTokenStatus)
		ar.Put("/api/tokens/{tokenID}", s.handleUpdateToken)
		ar.Delete("/api/tokens/{tokenID}", s.handleDeleteToken)
		ar.Post("/api/tokens/{tokenID}/sora2/activate", s.handleSora2Activate)

		ar.Get("/api/cache/config", s.handleGetCacheConfig)
		ar.Post("/api/cache/config", s.handleUpdateCacheConfig)
		ar.Post("/api/cache/test", s.handleTestCacheConfig)
		ar.Post("/api/cache/enabled", s.handleUpdateCacheConfig)
		ar.Post("/api/cache/base-url", s.handleUpdateCacheConfig)
		ar.Post("/api/cache/yun139/test", s.handleYun139Unsupported)
		ar.Get("/api/cache/yun139/files", s.handleYun139Unsupported)
		ar.Get("/api/cache/yun139/download", s.handleYun139Unsupported)
		ar.Post("/api/cache/yun139/delete", s.handleYun139Unsupported)
		ar.Get("/api/generation/timeout", s.handleGetGenerationConfig)
		ar.Post("/api/generation/timeout", s.handleUpdateGenerationConfig)
		ar.Get("/api/token-refresh/config", s.handleGetTokenRefreshConfig)
		ar.Post("/api/token-refresh/config", s.handleUpdateTokenRefreshConfig)
		ar.Post("/api/token-refresh/enabled", s.handleUpdateTokenRefreshEnabled)
		ar.Get("/api/admin/config", s.handleGetAdminConfig)
		ar.Post("/api/admin/config", s.handleUpdateAdminConfig)
		ar.Post("/api/admin/password", s.handleAdminPassword)
		ar.Post("/api/admin/apikey", s.handleUpdateAPIKey)
		ar.Post("/api/admin/debug", s.handleUpdateDebug)
		ar.Get("/api/logs", s.handleListLogs)
		ar.Get("/api/logs/active", s.handleActiveLogs)
		ar.Get("/api/logs/{logID}", s.handleLogDetail)
		ar.Delete("/api/logs", s.handleClearLogs)
		ar.Get("/api/proxy/config", s.handleProxyConfig)
		ar.Post("/api/proxy/config", s.handleProxyConfig)
		ar.Post("/api/proxy/test", s.handleProxyTest)
		ar.Get("/api/call-logic/config", s.handleGetCallLogicConfig)
		ar.Post("/api/call-logic/config", s.handleUpdateCallLogicConfig)
		ar.Post("/api/tokens/pkce", s.handlePKCEToken)
		ar.Get("/api/captcha/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, map[string]any{}) })
		ar.Post("/api/captcha/config", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"success": true})
		})
	})

	r.Group(func(pr chi.Router) {
		pr.Use(s.apiKeyAuthMiddleware)
		pr.Get("/v1/models/aliases", s.handleListModelAliases)
		pr.Get("/v1/models/{modelID}", s.handleGetModel)
		pr.Post("/v1/chat/completions", s.handleChatCompletions)
		pr.Post("/v1/audio/generations", s.handleAudioGenerations)
		pr.Post("/v1/audio/results", s.handleMusicResults)
		pr.Post("/v1/music/generations", s.handleAudioGenerations)
		pr.Post("/v1/audio/speech", s.handleAudioGenerations)
		pr.Post("/v1/music/results", s.handleMusicResults)
	})
	return r
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.db.GetAdminConfig(r.Context())
	if err != nil || cfg == nil || !cfg.GuestTrialEnabled {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.servePage("guest.html")(w, r)
}

func (s *Server) guestTrialEnabled(r *http.Request) bool {
	cfg, err := s.db.GetAdminConfig(r.Context())
	return err == nil && cfg != nil && cfg.GuestTrialEnabled
}

func (s *Server) guestMaxDaily(r *http.Request) int {
	cfg, err := s.db.GetAdminConfig(r.Context())
	if err != nil || cfg == nil || cfg.MaxDailyGuestUses <= 0 {
		return 0
	}
	return cfg.MaxDailyGuestUses
}

func (s *Server) guestGlobalMaxDaily(r *http.Request) int {
	cfg, err := s.db.GetAdminConfig(r.Context())
	if err != nil || cfg == nil || cfg.GuestGlobalDailyLimit <= 0 {
		return 0
	}
	return cfg.GuestGlobalDailyLimit
}

func (s *Server) guestDailyLimitReached(r *http.Request) bool {
	// Check per-IP limit
	maxDaily := s.guestMaxDaily(r)
	if maxDaily > 0 {
		used, err := s.db.GetTodayGuestUsage(r.Context(), guestClientKey(r))
		if err == nil && used >= maxDaily {
			return true
		}
	}
	// Check global limit
	globalMax := s.guestGlobalMaxDaily(r)
	if globalMax > 0 {
		globalUsed, err := s.db.GetTodayGlobalGuestUsage(r.Context())
		if err == nil && globalUsed >= globalMax {
			return true
		}
	}
	return false
}

func (s *Server) incrementGuestUsage(r *http.Request) {
	_ = s.db.IncrementGuestUsage(r.Context(), guestClientKey(r))
}

func (s *Server) acquireGuestSlot(r *http.Request) (func(), bool) {
	key := guestClientKey(r)
	s.guestMu.Lock()
	defer s.guestMu.Unlock()
	if s.guestBusy[key] > 0 {
		return func() {}, false
	}
	s.guestBusy[key] = 1
	return func() {
		s.guestMu.Lock()
		defer s.guestMu.Unlock()
		delete(s.guestBusy, key)
	}, true
}

func guestClientKey(r *http.Request) string {
	for _, header := range []string{"CF-Connecting-IP", "X-Real-IP"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value
		}
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		if first := strings.TrimSpace(strings.Split(forwarded, ",")[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) servePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(s.cfg.StaticDir, name))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if err := s.db.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "app": s.cfg.AppName})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": err.Error()})
		return
	}
	cfg, err := s.db.GetAdminConfig(r.Context())
	if err != nil || cfg == nil || cfg.Username != strings.TrimSpace(req.Username) || comparePassword(cfg.PasswordHash, req.Password) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "message": "用户名或密码错误"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "token": s.signAdminToken(cfg.Username, time.Now().Add(24*time.Hour))})
}

func (s *Server) adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" || !s.verifyAdminToken(token) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) apiKeyAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg, err := s.db.GetAdminConfig(r.Context())
		if err != nil || cfg == nil {
			writeOpenAIError(w, http.StatusUnauthorized, fmt.Errorf("api key unavailable"))
			return
		}
		key := apiKeyFromRequest(r)
		if key != cfg.APIKey {
			writeOpenAIError(w, http.StatusUnauthorized, fmt.Errorf("invalid api key"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.GetDashboardStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"app": s.cfg.AppName, "database_driver": s.cfg.DatabaseDriver, "storage_dir": s.cfg.CacheDir,
	})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.db.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]domain.Account, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, publicAccount(token, false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleExportTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.db.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tokens": tokens})
}

func (s *Server) handleGetToken(w http.ResponseWriter, r *http.Request) {
	id, err := routeID(r, "tokenID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	token, err := s.db.GetAccount(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "token": token})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	account, err := readAccountRequest(r, true, defaultAccountCapabilityDefaults())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// 检查邮箱是否已存在
	if email := strings.TrimSpace(firstNonEmpty(account.Email, account.LoginAccount)); email != "" {
		if existing, _ := s.db.GetAccountByEmail(r.Context(), email); existing != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("邮箱 %s 已存在", email))
			return
		}
	}
	// 纯 Cookie 模式自动检测
	if account.ProtocolMode == "" {
		if strings.TrimSpace(account.Cookies) != "" &&
			strings.TrimSpace(firstNonEmpty(account.FlowBearer, account.AT, account.AccessToken)) == "" &&
			strings.TrimSpace(firstNonEmpty(account.RefreshToken, account.ST)) == "" {
			account.ProtocolMode = "protocol"
		}
	}
	id, err := s.db.CreateAccount(r.Context(), account)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	created, _ := s.db.GetAccount(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "token": created})
}

func (s *Server) handleUpdateToken(w http.ResponseWriter, r *http.Request) {
	id, err := routeID(r, "tokenID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	existing, err := s.db.GetAccount(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	account, err := readAccountRequest(r, existing.AutoRefreshEnabled, accountCapabilityDefaultsFromAccount(*existing))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.UpdateAccount(r.Context(), id, account); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	updated, _ := s.db.GetAccount(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "token": updated})
}

func (s *Server) handleImportTokens(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := readJSON(r, &raw); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tokens, err := parseImportTokenItems(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	added := 0
	updated := 0
	for idx, item := range tokens {
		raw, _ := json.Marshal(item)
		var account domain.Account
		if err := json.Unmarshal(raw, &account); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid token at index %d: %w", idx, err))
			return
		}
		if account.Email == "" {
			account.Email = stringFromMap(item, "email")
		}
		if account.RefreshToken == "" {
			account.RefreshToken = firstNonEmpty(stringFromMap(item, "refresh_token"), stringFromMap(item, "st"), stringFromMap(item, "session_token"))
			account.ST = account.RefreshToken
		}
		if isSupabaseAccessToken(account.AccessToken) {
			account.AccessToken = ""
		}
		if account.FlowBearer == "" {
			flowBearer := firstNonEmpty(stringFromMap(item, "flow_bearer"), stringFromMap(item, "at"), stringFromMap(item, "access_token"), stringFromMap(item, "token"))
			if isSupabaseAccessToken(flowBearer) {
				flowBearer = ""
			}
			account.FlowBearer = flowBearer
			account.AT = account.FlowBearer
		}
		account.ClearFields = clearFieldsFromMap(item)
		account.ExplicitFields = account.ClearFields
		applyClearFields(&account, account.ClearFields)
		account = autoParseCookieJSON(account)
		var existing *domain.Account
		if account.ProtocolMode == "" || account.ProtocolMode == "session" {
			if account.Email != "" {
				existing, err = s.db.GetAccountByEmail(r.Context(), strings.TrimSpace(account.Email))
				if err != nil && err != store.ErrNotFound {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			}
			hasNewCredential := account.FlowBearer != "" || account.RefreshToken != "" || account.ProviderToken != "" || account.ProviderRefreshToken != "" || account.Cookies != ""
			if existing != nil && !hasNewCredential {
				account.ProtocolMode = ""
			} else if account.FlowBearer != "" && account.RefreshToken == "" {
				account.ProtocolMode = "bearer"
			} else {
				account.ProtocolMode = "refresh_token"
			}
		}
		capabilityDefaults := defaultAccountCapabilityDefaults()
		if account.Email != "" {
			if existing == nil {
				existing, err = s.db.GetAccountByEmail(r.Context(), strings.TrimSpace(account.Email))
				if err != nil && err != store.ErrNotFound {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			}
			if existing != nil {
				capabilityDefaults = accountCapabilityDefaultsFromAccount(*existing)
			}
		}
		applyAccountCapabilityMap(&account, item, capabilityDefaults)
		account.AutoRefreshEnabled = boolWithDefault(item, "auto_refresh_enabled", true)
		id, didUpdate, err := s.db.UpsertAccount(r.Context(), account)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("import token at index %d: %w", idx, err))
			return
		}
		if id <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("import token at index %d returned empty account id", idx))
			return
		}
		if active, ok := boolFromMap(item, "is_active"); ok {
			if err := s.db.SetAccountActive(r.Context(), id, active); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		if didUpdate {
			updated++
			continue
		}
		added++
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "added": added, "updated": updated})
}

func parseImportTokenItems(raw json.RawMessage) ([]map[string]any, error) {
	var wrapped struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Tokens != nil {
		if items, ok, err := importItemsFromFlowMusicCookieExport(wrapped.Tokens); ok || err != nil {
			return items, err
		}
		return wrapped.Tokens, nil
	}
	var tokens []map[string]any
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return nil, fmt.Errorf("import payload must be an array or object with tokens array")
	}
	if items, ok, err := importItemsFromFlowMusicCookieExport(tokens); ok || err != nil {
		return items, err
	}
	return tokens, nil
}

func publicAccount(account domain.Account, includeSensitive bool) domain.Account {
	out := account
	if includeSensitive {
		return out
	}
	out.RefreshToken = ""
	out.AccessToken = ""
	out.ProviderToken = ""
	out.ProviderRefreshToken = ""
	out.FlowBearer = ""
	out.Cookies = ""
	out.LoginPassword = ""
	out.ST = ""
	out.AT = ""
	return out
}

func (s *Server) handleSTToAT(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ST string `json:"st"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	accountReq := domain.Account{RefreshToken: req.ST, ST: req.ST}
	if cfg, err := s.db.GetProxyConfig(r.Context()); err == nil && cfg != nil && cfg.ProxyEnabled {
		accountReq.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
	} else {
		accountReq.ProxyURL = strings.TrimSpace(s.cfg.DefaultProxyURL)
	}
	account, err := s.flow.RefreshSupabase(r.Context(), accountReq)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "access_token": account.FlowBearer, "token": account})
}

func (s *Server) handlePKCEToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AuthCode     string `json:"auth_code"`
		CodeVerifier string `json:"code_verifier"`
		AccountID    int64  `json:"account_id,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.AuthCode) == "" || strings.TrimSpace(req.CodeVerifier) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "detail": "auth_code 和 code_verifier 都是必填项"})
		return
	}
	payload, err := s.flow.ExchangePKCEToken(r.Context(), strings.TrimSpace(req.AuthCode), strings.TrimSpace(req.CodeVerifier))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "detail": err.Error()})
		return
	}
	supabaseAccessToken := stringFromMap(payload, "access_token")
	refreshToken := stringFromMap(payload, "refresh_token")
	providerToken := stringFromMap(payload, "provider_token")
	providerRefreshToken := stringFromMap(payload, "provider_refresh_token")
	flowBearer := providerToken
	if providerToken != "" {
		flowAccount := domain.Account{
			ProviderToken:        providerToken,
			ProviderRefreshToken: providerRefreshToken,
		}
		if cfg, err := s.db.GetProxyConfig(r.Context()); err == nil && cfg != nil && cfg.ProxyEnabled {
			flowAccount.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
		} else {
			flowAccount.ProxyURL = strings.TrimSpace(s.cfg.DefaultProxyURL)
		}
		if bearer, err := s.flow.SaveGoogle(r.Context(), flowAccount); err == nil && bearer != "" {
			flowBearer = bearer
		}
	}
	result := map[string]any{
		"success":                true,
		"access_token":           supabaseAccessToken,
		"refresh_token":          refreshToken,
		"provider_token":         providerToken,
		"provider_refresh_token": providerRefreshToken,
		"flow_bearer":            flowBearer,
	}
	if req.AccountID > 0 {
		fields := map[string]any{
			"refresh_token":          refreshToken,
			"provider_token":         providerToken,
			"provider_refresh_token": providerRefreshToken,
			"flow_bearer":            flowBearer,
			"access_token":           flowBearer,
			"last_refresh_result":    "pkce_token_exchange_success",
			"last_refresh_at":        time.Now().UTC(),
		}
		_ = s.db.UpdateAccountFields(r.Context(), req.AccountID, fields)
		account, _ := s.db.GetAccount(r.Context(), req.AccountID)
		result["account"] = account
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleRefreshAT(w http.ResponseWriter, r *http.Request) {
	id, err := routeID(r, "tokenID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	account, err := s.accounts.RefreshAccount(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "token": account})
}

func (s *Server) handleRefreshCredits(w http.ResponseWriter, r *http.Request) {
	id, err := routeID(r, "tokenID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	account, err := s.accounts.RefreshCredits(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "credits": account.Credits, "token": account})
}

func (s *Server) handleTestToken(w http.ResponseWriter, r *http.Request) {
	id, err := routeID(r, "tokenID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	account, err := s.accounts.RefreshCredits(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "status": "failed", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "status": "success", "email": account.Email, "credits": account.Credits})
}

func (s *Server) handleEnableToken(w http.ResponseWriter, r *http.Request) {
	s.setTokenActive(w, r, true)
}
func (s *Server) handleDisableToken(w http.ResponseWriter, r *http.Request) {
	s.setTokenActive(w, r, false)
}

func (s *Server) handleUpdateTokenStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IsActive bool `json:"is_active"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.setTokenActiveValue(w, r, req.IsActive)
}

func (s *Server) setTokenActive(w http.ResponseWriter, r *http.Request, active bool) {
	s.setTokenActiveValue(w, r, active)
}

func (s *Server) setTokenActiveValue(w http.ResponseWriter, r *http.Request, active bool) {
	id, err := routeID(r, "tokenID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.SetAccountActive(r.Context(), id, active); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id, err := routeID(r, "tokenID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.db.DeleteAccount(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
