package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
	"flowmusic2api/internal/service"
	"flowmusic2api/internal/storage"
	"flowmusic2api/internal/store"
)

type httpTestRig struct {
	Server *httptest.Server
	DB     *store.Store
}

func newHTTPTestServer(t *testing.T) *httptest.Server {
	return newHTTPTestRig(t).Server
}

func newHTTPTestRig(t *testing.T, mutate ...func(*config.Config)) *httpTestRig {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		AppName:                "FlowMusic2API Test",
		RootDir:                dir,
		StaticDir:              filepath.Join(dir, "web", "static"),
		DataDir:                dir,
		CacheDir:               filepath.Join(dir, "tmp"),
		DatabaseDriver:         "sqlite",
		DatabaseURL:            filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:       "http://127.0.0.1",
		SupabaseBaseURL:        "http://127.0.0.1",
		UpstreamTimeout:        time.Second,
		GenerationTimeout:      time.Second,
		AdminJWTSecret:         "test-admin-secret",
		DefaultAPIKey:          "test-api-key",
		DefaultAdminUser:       "admin",
		DefaultAdminPassword:   "admin",
		TokenRefreshInterval:   time.Hour,
		TokenRefreshLead:       time.Minute,
		StoragePresignDuration: time.Hour,
	}
	for _, fn := range mutate {
		fn(&cfg)
	}

	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}

	flow := service.NewFlowMusicClient(cfg)
	accounts := service.NewAccountService(cfg, db, flow)
	httpClient := service.NewHTTPClient(cfg, "")
	cache := storage.NewCache(cfg, db, httpClient)
	generation := service.NewGenerationService(cfg, db, accounts, flow, cache)
	api := New(cfg, db, flow, accounts, generation)

	return &httpTestRig{Server: httptest.NewServer(api.Handler()), DB: db}
}

func fakeSupabaseAccessToken(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://sb.flowmusic.app/auth/v1","aud":"authenticated"}`))
	signature := base64.RawURLEncoding.EncodeToString([]byte("signature"))
	return header + "." + payload + "." + signature
}

func fakeFlowMusicCookieJSON(t *testing.T, email, refreshToken, providerToken, providerRefreshToken string) string {
	t.Helper()
	session := map[string]any{
		"access_token":           fakeSupabaseAccessToken(t),
		"refresh_token":          refreshToken,
		"provider_token":         providerToken,
		"provider_refresh_token": providerRefreshToken,
		"user": map[string]any{
			"email": email,
			"user_metadata": map[string]any{
				"name": "Cookie User",
			},
		},
	}
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("json.Marshal(session) error = %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(sessionJSON)
	splitAt := len(encoded) / 2
	cookies := []map[string]any{
		{"domain": ".flowmusic.app", "name": "_ga", "value": "GA1.1.0.0"},
		{"domain": "www.flowmusic.app", "name": "sb-sb-auth-token.1", "value": encoded[splitAt:]},
		{"domain": "www.flowmusic.app", "name": "sb-sb-auth-token.0", "value": "base64-" + encoded[:splitAt]},
	}
	cookieJSON, err := json.Marshal(cookies)
	if err != nil {
		t.Fatalf("json.Marshal(cookies) error = %v", err)
	}
	return string(cookieJSON)
}

func TestHealthChecksDatabase(t *testing.T) {
	rig := newHTTPTestRig(t)
	ts := rig.Server
	t.Cleanup(ts.Close)

	healthyResp := doJSONRequest(t, http.MethodGet, ts.URL+"/health", "", nil)
	if healthyResp.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, body = %s", healthyResp.Code, healthyResp.Body.String())
	}
	var healthy struct {
		Status string `json:"status"`
		App    string `json:"app"`
	}
	decodeResponse(t, healthyResp, &healthy)
	if healthy.Status != "ok" || healthy.App == "" {
		t.Fatalf("unexpected healthy payload: %+v", healthy)
	}

	if err := rig.DB.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	unhealthyResp := doJSONRequest(t, http.MethodGet, ts.URL+"/healthz", "", nil)
	if unhealthyResp.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz after DB close status = %d, want %d, body = %s", unhealthyResp.Code, http.StatusServiceUnavailable, unhealthyResp.Body.String())
	}
	var unhealthy struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	decodeResponse(t, unhealthyResp, &unhealthy)
	if unhealthy.Status != "error" || unhealthy.Error == "" {
		t.Fatalf("unexpected unhealthy payload: %+v", unhealthy)
	}
}

func TestGuestTrialRootAndPublicEndpoint(t *testing.T) {
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "guest.html"), []byte("guest trial page"), 0o644); err != nil {
		t.Fatalf("WriteFile(guest.html) error = %v", err)
	}
	rig := newHTTPTestRig(t, func(cfg *config.Config) { cfg.StaticDir = staticDir })
	ts := rig.Server
	t.Cleanup(ts.Close)

	noRedirect := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	rootResp, err := noRedirect.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / error = %v", err)
	}
	_ = rootResp.Body.Close()
	if rootResp.StatusCode != http.StatusFound || rootResp.Header.Get("Location") != "/login" {
		t.Fatalf("GET / disabled status/location = %d/%q, want 302 /login", rootResp.StatusCode, rootResp.Header.Get("Location"))
	}

	disabledResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/guest/chat/completions", "", map[string]any{
		"model": "lyria",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "make music",
		}},
	})
	if disabledResp.Code != http.StatusForbidden {
		t.Fatalf("guest chat disabled status = %d, body = %s", disabledResp.Code, disabledResp.Body.String())
	}

	if err := rig.DB.UpdateAdminConfig(context.Background(), "admin", "test-api-key", false, 3, true, 0, 0); err != nil {
		t.Fatalf("UpdateAdminConfig() error = %v", err)
	}
	enabledRootResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / enabled error = %v", err)
	}
	enabledRootBody, err := io.ReadAll(enabledRootResp.Body)
	_ = enabledRootResp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll(enabled root) error = %v", err)
	}
	if enabledRootResp.StatusCode != http.StatusOK || !strings.Contains(string(enabledRootBody), "guest trial page") {
		t.Fatalf("GET / enabled status/body = %d/%q", enabledRootResp.StatusCode, string(enabledRootBody))
	}

	configResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/guest/config", "", nil)
	var guestConfig struct {
		GuestTrialEnabled bool `json:"guest_trial_enabled"`
		Models            []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	decodeResponse(t, configResp, &guestConfig)
	if !guestConfig.GuestTrialEnabled || len(guestConfig.Models) == 0 {
		t.Fatalf("unexpected guest config: %+v", guestConfig)
	}

	noKeyResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/guest/chat/completions", "", map[string]any{
		"model":    "lyria",
		"messages": []map[string]string{},
	})
	if noKeyResp.Code != http.StatusBadRequest {
		t.Fatalf("guest chat should be reachable without API key and validate payload, status = %d, body = %s", noKeyResp.Code, noKeyResp.Body.String())
	}
}

func TestAdminAuthAndTokenCRUD(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	if resp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/stats", "", nil); resp.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/stats without token status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	if loginResp.Code != http.StatusOK {
		t.Fatalf("POST /api/login status = %d, body = %s", loginResp.Code, loginResp.Body.String())
	}
	var login struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)
	if !login.Success || login.Token == "" {
		t.Fatalf("unexpected login response: %+v", login)
	}

	statsResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/stats", login.Token, nil)
	if statsResp.Code != http.StatusOK {
		t.Fatalf("GET /api/stats status = %d, body = %s", statsResp.Code, statsResp.Body.String())
	}

	createResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens", login.Token, map[string]any{
		"email":         "user@example.test",
		"st":            "refresh-token",
		"at":            "flow-bearer",
		"protocol_mode": "bearer",
	})
	if createResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Success bool `json:"success"`
		Token   struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	decodeResponse(t, createResp, &created)
	if !created.Success || created.Token.ID == 0 {
		t.Fatalf("unexpected create response: %+v", created)
	}

	listResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/tokens", login.Token, nil)
	if listResp.Code != http.StatusOK {
		t.Fatalf("GET /api/tokens status = %d, body = %s", listResp.Code, listResp.Body.String())
	}
	var tokens []map[string]any
	decodeResponse(t, listResp, &tokens)
	if len(tokens) != 1 {
		t.Fatalf("token count = %d, want 1", len(tokens))
	}
	if tokens[0]["st"] != nil || tokens[0]["at"] != nil || tokens[0]["refresh_token"] != nil || tokens[0]["flow_bearer"] != nil || tokens[0]["google_cookies"] != nil {
		t.Fatalf("GET /api/tokens leaked sensitive fields: %+v", tokens[0])
	}

	detailResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/tokens/1", login.Token, nil)
	if detailResp.Code != http.StatusOK {
		t.Fatalf("GET /api/tokens/1 status = %d, body = %s", detailResp.Code, detailResp.Body.String())
	}
	var detail struct {
		Success bool `json:"success"`
		Token   struct {
			ST         string `json:"st"`
			AT         string `json:"at"`
			FlowBearer string `json:"flow_bearer"`
		} `json:"token"`
	}
	decodeResponse(t, detailResp, &detail)
	if !detail.Success || detail.Token.ST != "refresh-token" || detail.Token.AT != "flow-bearer" || detail.Token.FlowBearer != "flow-bearer" {
		t.Fatalf("unexpected token detail response: %+v", detail)
	}

	exportResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/tokens/export", login.Token, nil)
	if exportResp.Code != http.StatusOK {
		t.Fatalf("GET /api/tokens/export status = %d, body = %s", exportResp.Code, exportResp.Body.String())
	}
	var exported struct {
		Success bool `json:"success"`
		Tokens  []struct {
			ST string `json:"st"`
			AT string `json:"at"`
		} `json:"tokens"`
	}
	decodeResponse(t, exportResp, &exported)
	if !exported.Success || len(exported.Tokens) != 1 || exported.Tokens[0].ST != "refresh-token" || exported.Tokens[0].AT != "flow-bearer" {
		t.Fatalf("unexpected token export response: %+v", exported)
	}

	deleteResp := doJSONRequest(t, http.MethodDelete, ts.URL+"/api/tokens/1", login.Token, nil)
	if deleteResp.Code != http.StatusOK {
		t.Fatalf("DELETE /api/tokens/1 status = %d, body = %s", deleteResp.Code, deleteResp.Body.String())
	}
}

func TestStaticPagesAreServedFromProjectWebDir(t *testing.T) {
	rig := newHTTPTestRig(t, func(cfg *config.Config) {
		cfg.StaticDir = filepath.Join("..", "..", "web", "static")
	})
	ts := rig.Server
	t.Cleanup(ts.Close)

	for _, tc := range []struct {
		path     string
		contains string
	}{
		{path: "/login", contains: "管理员控制台"},
		{path: "/manage", contains: "管理控制台 - FlowMusic2API"},
		{path: "/test", contains: "FlowMusic2API 模型测试"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s error = %v", tc.path, err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read %s body: %v", tc.path, err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, body = %s", tc.path, resp.StatusCode, string(data))
			}
			if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
				t.Fatalf("GET %s content-type = %q, want text/html", tc.path, resp.Header.Get("Content-Type"))
			}
			if !strings.Contains(string(data), tc.contains) {
				t.Fatalf("GET %s body missing %q", tc.path, tc.contains)
			}
		})
	}
}

func TestTokenCreateDefaultsAndUpdatePreservesAutoRefresh(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	createResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens", login.Token, map[string]any{
		"email":         "default-refresh@example.test",
		"flow_bearer":   "flow-bearer",
		"protocol_mode": "bearer",
	})
	if createResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Token struct {
			ID                 int64  `json:"id"`
			AT                 string `json:"at"`
			FlowBearer         string `json:"flow_bearer"`
			AutoRefreshEnabled bool   `json:"auto_refresh_enabled"`
		} `json:"token"`
	}
	decodeResponse(t, createResp, &created)
	if created.Token.ID == 0 || !created.Token.AutoRefreshEnabled || created.Token.AT != "flow-bearer" || created.Token.FlowBearer != "flow-bearer" {
		t.Fatalf("create without auto_refresh_enabled should default true: %+v", created)
	}

	disableResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/1", login.Token, map[string]any{
		"email":                "default-refresh@example.test",
		"flow_bearer":          "flow-bearer",
		"protocol_mode":        "bearer",
		"auto_refresh_enabled": false,
	})
	if disableResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/tokens/1 disable status = %d, body = %s", disableResp.Code, disableResp.Body.String())
	}

	updateResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/1", login.Token, map[string]any{
		"remark":        "edited",
		"protocol_mode": "bearer",
	})
	if updateResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/tokens/1 update status = %d, body = %s", updateResp.Code, updateResp.Body.String())
	}
	var updated struct {
		Token struct {
			Remark             string `json:"remark"`
			AutoRefreshEnabled bool   `json:"auto_refresh_enabled"`
		} `json:"token"`
	}
	decodeResponse(t, updateResp, &updated)
	if updated.Token.Remark != "edited" || updated.Token.AutoRefreshEnabled {
		t.Fatalf("update should preserve explicit false auto refresh: %+v", updated)
	}
}

func TestTokenCookieJSONForcesRefreshTokenMode(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	createCookieJSON := fakeFlowMusicCookieJSON(t, "cookie-create@example.test", "cookie-create-refresh", "cookie-create-provider", "cookie-create-provider-refresh")
	createResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens", login.Token, map[string]any{
		"protocol_mode":  "protocol",
		"google_cookies": createCookieJSON,
	})
	if createResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens cookie JSON status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Token exportedTokenView `json:"token"`
	}
	decodeResponse(t, createResp, &created)
	if created.Token.ProtocolMode != "refresh_token" ||
		created.Token.ST != "cookie-create-refresh" ||
		created.Token.ProviderToken != "cookie-create-provider" ||
		created.Token.ProviderRefreshToken != "cookie-create-provider-refresh" ||
		!strings.Contains(created.Token.GoogleCookies, "sb-sb-auth-token.0=") {
		t.Fatalf("cookie create should switch to refresh_token and extract credentials: %+v", created.Token)
	}

	bearerResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens", login.Token, map[string]any{
		"email":         "cookie-update@example.test",
		"flow_bearer":   "old-flow-bearer",
		"protocol_mode": "bearer",
	})
	if bearerResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens bearer status = %d, body = %s", bearerResp.Code, bearerResp.Body.String())
	}
	var bearerCreated struct {
		Token exportedTokenView `json:"token"`
	}
	decodeResponse(t, bearerResp, &bearerCreated)

	updateCookieJSON := fakeFlowMusicCookieJSON(t, "cookie-update@example.test", "cookie-update-refresh", "cookie-update-provider", "cookie-update-provider-refresh")
	updateResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/"+strconv.FormatInt(bearerCreated.Token.ID, 10), login.Token, map[string]any{
		"protocol_mode":  "protocol",
		"google_cookies": updateCookieJSON,
	})
	if updateResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/tokens/%d cookie JSON status = %d, body = %s", bearerCreated.Token.ID, updateResp.Code, updateResp.Body.String())
	}
	var updated struct {
		Token exportedTokenView `json:"token"`
	}
	decodeResponse(t, updateResp, &updated)
	if updated.Token.ProtocolMode != "refresh_token" ||
		updated.Token.ST != "cookie-update-refresh" ||
		updated.Token.ProviderToken != "cookie-update-provider" ||
		updated.Token.ProviderRefreshToken != "cookie-update-provider-refresh" ||
		!strings.Contains(updated.Token.GoogleCookies, "sb-sb-auth-token.0=") {
		t.Fatalf("cookie update should switch to refresh_token and extract credentials: %+v", updated.Token)
	}
}

func TestTokenUpdateClearsExplicitCredentialFields(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	createResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens", login.Token, map[string]any{
		"email":                  "clear-http@example.test",
		"st":                     "old-refresh",
		"at":                     "old-bearer",
		"provider_token":         "old-provider",
		"provider_refresh_token": "old-provider-refresh",
		"google_cookies":         "cookie=value",
		"proxy_url":              "http://proxy.example.test:8080",
		"protocol_mode":          "refresh_token",
	})
	if createResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	decodeResponse(t, createResp, &created)

	updateResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/"+strconv.FormatInt(created.Token.ID, 10), login.Token, map[string]any{
		"protocol_mode":            "bearer",
		"st":                       nil,
		"at":                       "new-bearer",
		"provider_token":           nil,
		"provider_refresh_token":   nil,
		"google_cookies":           nil,
		"proxy_url":                "",
		"auto_refresh_enabled":     true,
		"refresh_interval_minutes": 60,
		"image_enabled":            true,
		"video_enabled":            true,
		"upscale_enabled":          true,
	})
	if updateResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/tokens/%d status = %d, body = %s", created.Token.ID, updateResp.Code, updateResp.Body.String())
	}
	var updated struct {
		Token struct {
			ProtocolMode         string `json:"protocol_mode"`
			ST                   string `json:"st"`
			AT                   string `json:"at"`
			RefreshToken         string `json:"refresh_token"`
			FlowBearer           string `json:"flow_bearer"`
			ProviderToken        string `json:"provider_token"`
			ProviderRefreshToken string `json:"provider_refresh_token"`
			GoogleCookies        string `json:"google_cookies"`
			ProxyURL             string `json:"proxy_url"`
		} `json:"token"`
	}
	decodeResponse(t, updateResp, &updated)
	if updated.Token.ProtocolMode != "bearer" ||
		updated.Token.ST != "" ||
		updated.Token.RefreshToken != "" ||
		updated.Token.AT != "new-bearer" ||
		updated.Token.FlowBearer != "new-bearer" ||
		updated.Token.ProviderToken != "" ||
		updated.Token.ProviderRefreshToken != "" ||
		updated.Token.GoogleCookies != "" ||
		updated.Token.ProxyURL != "" {
		t.Fatalf("explicit credential clear did not persist: %+v", updated.Token)
	}
}

func TestTokenUpdateClearFieldsCanClearLoginPassword(t *testing.T) {
	rig := newHTTPTestRig(t)
	ts := rig.Server
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	createResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens", login.Token, map[string]any{
		"email":          "clear-password-http@example.test",
		"at":             "flow-bearer",
		"protocol_mode":  "bearer",
		"login_password": "old-password",
	})
	if createResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	decodeResponse(t, createResp, &created)

	blankResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/"+strconv.FormatInt(created.Token.ID, 10), login.Token, map[string]any{
		"protocol_mode":            "bearer",
		"login_password":           nil,
		"auto_refresh_enabled":     true,
		"refresh_interval_minutes": 60,
		"image_enabled":            true,
		"video_enabled":            true,
		"upscale_enabled":          true,
	})
	if blankResp.Code != http.StatusOK {
		t.Fatalf("blank PUT /api/tokens/%d status = %d, body = %s", created.Token.ID, blankResp.Code, blankResp.Body.String())
	}
	account, err := rig.DB.GetAccount(context.Background(), created.Token.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.LoginPassword != "old-password" {
		t.Fatalf("blank login_password should preserve old password: %+v", account)
	}

	clearResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/"+strconv.FormatInt(created.Token.ID, 10), login.Token, map[string]any{
		"protocol_mode":            "bearer",
		"clear_fields":             []string{"login_password"},
		"auto_refresh_enabled":     true,
		"refresh_interval_minutes": 60,
		"image_enabled":            true,
		"video_enabled":            true,
		"upscale_enabled":          true,
	})
	if clearResp.Code != http.StatusOK {
		t.Fatalf("clear_fields PUT /api/tokens/%d status = %d, body = %s", created.Token.ID, clearResp.Code, clearResp.Body.String())
	}
	account, err = rig.DB.GetAccount(context.Background(), created.Token.ID)
	if err != nil {
		t.Fatalf("GetAccount() after clear error = %v", err)
	}
	if account.LoginPassword != "" {
		t.Fatalf("clear_fields login_password should clear old password: %+v", account)
	}
}

func TestTokenCreateUpdatePreservesCapabilityFlags(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	createResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens", login.Token, map[string]any{
		"email":         "capabilities@example.test",
		"flow_bearer":   "flow-bearer",
		"protocol_mode": "bearer",
		"image_enabled": false,
	})
	if createResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Token struct {
			ID             int64 `json:"id"`
			ImageEnabled   bool  `json:"image_enabled"`
			VideoEnabled   bool  `json:"video_enabled"`
			UpscaleEnabled bool  `json:"upscale_enabled"`
		} `json:"token"`
	}
	decodeResponse(t, createResp, &created)
	if created.Token.ID == 0 || created.Token.ImageEnabled || !created.Token.VideoEnabled || !created.Token.UpscaleEnabled {
		t.Fatalf("create should preserve explicit false and default omitted capability flags: %+v", created)
	}

	partialResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/"+strconv.FormatInt(created.Token.ID, 10), login.Token, map[string]any{
		"video_enabled": false,
	})
	if partialResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/tokens/%d partial status = %d, body = %s", created.Token.ID, partialResp.Code, partialResp.Body.String())
	}
	var partial struct {
		Token struct {
			ImageEnabled   bool `json:"image_enabled"`
			VideoEnabled   bool `json:"video_enabled"`
			UpscaleEnabled bool `json:"upscale_enabled"`
		} `json:"token"`
	}
	decodeResponse(t, partialResp, &partial)
	if partial.Token.ImageEnabled || partial.Token.VideoEnabled || !partial.Token.UpscaleEnabled {
		t.Fatalf("partial update should preserve omitted capability flags: %+v", partial)
	}

	updateAllFalseResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/"+strconv.FormatInt(created.Token.ID, 10), login.Token, map[string]any{
		"upscale_enabled": false,
	})
	if updateAllFalseResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/tokens/%d update all false status = %d, body = %s", created.Token.ID, updateAllFalseResp.Code, updateAllFalseResp.Body.String())
	}
	var disabled struct {
		Token struct {
			ImageEnabled   bool `json:"image_enabled"`
			VideoEnabled   bool `json:"video_enabled"`
			UpscaleEnabled bool `json:"upscale_enabled"`
		} `json:"token"`
	}
	decodeResponse(t, updateAllFalseResp, &disabled)
	if disabled.Token.ImageEnabled || disabled.Token.VideoEnabled || disabled.Token.UpscaleEnabled {
		t.Fatalf("update should allow disabling all capability flags: %+v", disabled)
	}

	reEnableResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/"+strconv.FormatInt(created.Token.ID, 10), login.Token, map[string]any{
		"image_enabled": true,
	})
	if reEnableResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/tokens/%d re-enable status = %d, body = %s", created.Token.ID, reEnableResp.Code, reEnableResp.Body.String())
	}
	var reEnabled struct {
		Token struct {
			ImageEnabled   bool `json:"image_enabled"`
			VideoEnabled   bool `json:"video_enabled"`
			UpscaleEnabled bool `json:"upscale_enabled"`
		} `json:"token"`
	}
	decodeResponse(t, reEnableResp, &reEnabled)
	if !reEnabled.Token.ImageEnabled || reEnabled.Token.VideoEnabled || reEnabled.Token.UpscaleEnabled {
		t.Fatalf("partial true update should preserve omitted false capability flags: %+v", reEnabled)
	}
}

func TestTokenUpdatePreservesExpiryMetadata(t *testing.T) {
	rig := newHTTPTestRig(t)
	ts := rig.Server
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	atExpires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	id, err := rig.DB.CreateAccount(context.Background(), domain.Account{
		Email:              "expiry@example.test",
		FlowBearer:         "flow-bearer",
		ProtocolMode:       "bearer",
		AutoRefreshEnabled: true,
		ExpiresAt:          &expiresAt,
		ATExpires:          &atExpires,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	updateResp := doJSONRequest(t, http.MethodPut, ts.URL+"/api/tokens/"+strconv.FormatInt(id, 10), login.Token, map[string]any{
		"remark":        "metadata edit",
		"protocol_mode": "bearer",
	})
	if updateResp.Code != http.StatusOK {
		t.Fatalf("PUT /api/tokens/%d status = %d, body = %s", id, updateResp.Code, updateResp.Body.String())
	}

	updated, err := rig.DB.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expires_at should be preserved, got %+v want %s", updated.ExpiresAt, expiresAt)
	}
	if updated.ATExpires == nil || !updated.ATExpires.Equal(atExpires) {
		t.Fatalf("at_expires should be preserved, got %+v want %s", updated.ATExpires, atExpires)
	}
}

func TestOpenAIAuthBoundaries(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	faviconResp, err := http.Get(ts.URL + "/favicon.ico")
	if err != nil {
		t.Fatalf("GET /favicon.ico error = %v", err)
	}
	_ = faviconResp.Body.Close()
	if faviconResp.StatusCode != http.StatusNoContent {
		t.Fatalf("GET /favicon.ico status = %d, want %d", faviconResp.StatusCode, http.StatusNoContent)
	}

	modelsResp := doJSONRequest(t, http.MethodGet, ts.URL+"/v1/models", "", nil)
	if modelsResp.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, body = %s", modelsResp.Code, modelsResp.Body.String())
	}
	var modelsPayload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decodeResponse(t, modelsResp, &modelsPayload)
	modelIDs := map[string]bool{}
	for _, model := range modelsPayload.Data {
		modelIDs[model.ID] = true
	}
	for _, id := range []string{"lyria", "lyria-fast", "lyria-pro", "lyria-pro-fast"} {
		if !modelIDs[id] {
			t.Fatalf("GET /v1/models missing %q: %+v", id, modelsPayload.Data)
		}
	}
	aliasesNoAuth := doJSONRequest(t, http.MethodGet, ts.URL+"/v1/models/aliases", "", nil)
	if aliasesNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/models/aliases without key status = %d, want %d", aliasesNoAuth.Code, http.StatusUnauthorized)
	}
	aliasesResp := doJSONRequest(t, http.MethodGet, ts.URL+"/v1/models/aliases", "test-api-key", nil)
	if aliasesResp.Code != http.StatusOK {
		t.Fatalf("GET /v1/models/aliases status = %d, body = %s", aliasesResp.Code, aliasesResp.Body.String())
	}
	modelNoAuth := doJSONRequest(t, http.MethodGet, ts.URL+"/v1/models/lyria", "", nil)
	if modelNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/models/{model} without key status = %d, want %d", modelNoAuth.Code, http.StatusUnauthorized)
	}
	modelResp := doJSONRequest(t, http.MethodGet, ts.URL+"/v1/models/Lyria", "test-api-key", nil)
	if modelResp.Code != http.StatusOK {
		t.Fatalf("GET /v1/models/Lyria status = %d, body = %s", modelResp.Code, modelResp.Body.String())
	}
	var modelPayload struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		IsAlias bool   `json:"is_alias"`
		Target  string `json:"target"`
	}
	decodeResponse(t, modelResp, &modelPayload)
	if modelPayload.ID != "lyria" || modelPayload.Object != "model" || modelPayload.IsAlias || modelPayload.Target != "" {
		t.Fatalf("unexpected model detail payload: %+v", modelPayload)
	}
	legacyModelResp := doJSONRequest(t, http.MethodGet, ts.URL+"/v1/models/flowmusic", "test-api-key", nil)
	if legacyModelResp.Code != http.StatusOK {
		t.Fatalf("GET /v1/models/flowmusic status = %d, body = %s", legacyModelResp.Code, legacyModelResp.Body.String())
	}
	var legacyModelPayload struct {
		ID      string `json:"id"`
		IsAlias bool   `json:"is_alias"`
		Target  string `json:"target"`
	}
	decodeResponse(t, legacyModelResp, &legacyModelPayload)
	if legacyModelPayload.ID != "flowmusic" || !legacyModelPayload.IsAlias || legacyModelPayload.Target != "lyria" {
		t.Fatalf("unexpected legacy alias detail payload: %+v", legacyModelPayload)
	}
	missingModelResp := doJSONRequest(t, http.MethodGet, ts.URL+"/v1/models/missing-model", "test-api-key", nil)
	if missingModelResp.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/models/missing-model status = %d, want %d", missingModelResp.Code, http.StatusNotFound)
	}
	assertOpenAIErrorEnvelope(t, missingModelResp, http.StatusNotFound)
	xAPIKeyReq, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/models/aliases", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	xAPIKeyReq.Header.Set("x-api-key", "test-api-key")
	xAPIKeyResp, err := http.DefaultClient.Do(xAPIKeyReq)
	if err != nil {
		t.Fatalf("http.Do(x-api-key) error = %v", err)
	}
	_ = xAPIKeyResp.Body.Close()
	if xAPIKeyResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models/aliases with x-api-key status = %d, want %d", xAPIKeyResp.StatusCode, http.StatusOK)
	}

	body := map[string]any{
		"model": "lyria",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "test prompt",
		}},
	}
	noAuth := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/chat/completions", "", body)
	if noAuth.Code != http.StatusUnauthorized {
		t.Fatalf("POST /v1/chat/completions without key status = %d, want %d", noAuth.Code, http.StatusUnauthorized)
	}
	assertOpenAIErrorEnvelope(t, noAuth, http.StatusUnauthorized)

	withKey := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/chat/completions", "test-api-key", body)
	if withKey.Code != http.StatusBadGateway {
		t.Fatalf("POST /v1/chat/completions without accounts status = %d, want %d", withKey.Code, http.StatusBadGateway)
	}
	assertOpenAIErrorEnvelope(t, withKey, http.StatusBadGateway)

	badModelBody := map[string]any{
		"model": "missing-model",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "test prompt",
		}},
	}
	badModelResp := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/chat/completions", "test-api-key", badModelBody)
	if badModelResp.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/chat/completions with bad model status = %d, want %d", badModelResp.Code, http.StatusBadRequest)
	}
	assertOpenAIErrorEnvelope(t, badModelResp, http.StatusBadRequest)

	streamBody := map[string]any{
		"model": "lyria",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "test prompt",
		}},
		"stream": true,
	}
	streamWithKey := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/chat/completions", "test-api-key", streamBody)
	if streamWithKey.Code != http.StatusOK {
		t.Fatalf("stream POST /v1/chat/completions without accounts status = %d, want %d, body = %s", streamWithKey.Code, http.StatusOK, streamWithKey.Body.String())
	}
	if body := streamWithKey.Body.String(); !strings.Contains(body, `"object":"chat.completion.chunk"`) ||
		!strings.Contains(body, `"type":"upstream_error"`) ||
		!strings.Contains(body, "no active FlowMusic account with usable Bearer token") ||
		!strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected stream error response body: %s", body)
	}

	audioSpeechBody := map[string]any{
		"model": "lyria",
		"input": "test prompt",
	}
	audioSpeechNoAuth := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/audio/speech", "", audioSpeechBody)
	if audioSpeechNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("POST /v1/audio/speech without key status = %d, want %d", audioSpeechNoAuth.Code, http.StatusUnauthorized)
	}
	assertOpenAIErrorEnvelope(t, audioSpeechNoAuth, http.StatusUnauthorized)
	audioSpeechWithKey := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/audio/speech", "test-api-key", audioSpeechBody)
	if audioSpeechWithKey.Code != http.StatusBadGateway {
		t.Fatalf("POST /v1/audio/speech without accounts status = %d, want %d", audioSpeechWithKey.Code, http.StatusBadGateway)
	}
	assertOpenAIErrorEnvelope(t, audioSpeechWithKey, http.StatusBadGateway)
	audioSpeechBadModel := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/audio/speech", "test-api-key", map[string]any{
		"model": "missing-model",
		"input": "test prompt",
	})
	if audioSpeechBadModel.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/audio/speech with bad model status = %d, want %d", audioSpeechBadModel.Code, http.StatusBadRequest)
	}
	assertOpenAIErrorEnvelope(t, audioSpeechBadModel, http.StatusBadRequest)
}

func TestOpenAIErrorEnvelopeAndPromptExtraction(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	resp := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/chat/completions", "test-api-key", map[string]any{
		"model":    "lyria",
		"messages": []any{},
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/chat/completions empty messages status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
	assertOpenAIErrorEnvelope(t, resp, http.StatusBadRequest)

	prompt := extractPrompt([]chatMessage{
		{Role: "system", Content: "keep it short"},
		{Role: "developer", Content: []any{map[string]any{"type": "input_text", "text": "return URLs"}}},
		{Role: "assistant", Content: "previous answer"},
		{Role: "user", Content: []any{map[string]any{"type": "text", "text": "<tools>ignored</tools>\nmake city pop"}}},
	})
	want := "make city pop"
	if prompt != want {
		t.Fatalf("extractPrompt() = %q, want %q", prompt, want)
	}

	audioResp := doJSONRequest(t, http.MethodPost, ts.URL+"/v1/audio/generations", "test-api-key", map[string]any{
		"model":  "lyria",
		"prompt": "   ",
	})
	if audioResp.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/audio/generations empty prompt status = %d, want %d", audioResp.Code, http.StatusBadRequest)
	}
	assertOpenAIErrorEnvelope(t, audioResp, http.StatusBadRequest)
}

func TestChatCompletionsEndToEndWithMockFlowMusic(t *testing.T) {
	var mediaServer *httptest.Server
	mediaServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/song.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("mock-mp3"))
		case "/song.wav":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("mock-wav"))
		case "/cover.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("mock-jpg"))
		default:
			t.Fatalf("unexpected media path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(mediaServer.Close)

	var sawConversation bool
	var sawBearer bool
	var sawProFast bool
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/conversation":
			sawConversation = true
			if r.Header.Get("Authorization") == "Bearer flow-bearer" {
				sawBearer = true
			}
			var req struct {
				Parts []struct {
					Content  string `json:"content"`
					PartKind string `json:"part_kind"`
				} `json:"parts"`
				ClientContext struct {
					GhostwriterVersion string `json:"ghostwriter_version"`
				} `json:"client_context"`
				ModelName string `json:"model_name"`
				Mode      string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode conversation request: %v", err)
			}
			if req.ModelName != "producer:standard" || len(req.Parts) != 1 || req.Parts[0].PartKind != "user-prompt" || !strings.Contains(req.Parts[0].Content, "make city pop") {
				t.Fatalf("unexpected conversation request: %+v", req)
			}
			if strings.Contains(req.Parts[0].Content, "pro fast") {
				if req.Mode != "standard" || req.ClientContext.GhostwriterVersion != "pro" {
					t.Fatalf("unexpected pro fast request: %+v", req)
				}
				sawProFast = true
			} else if req.Mode != "standard" || req.ClientContext.GhostwriterVersion != "standard" {
				t.Fatalf("unexpected standard request: %+v", req)
			}
			writeJSON(w, http.StatusOK, map[string]any{"job_id": "job-1"})
		case "/__api/messages/job-1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"clip_id\":\"clip-1\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/__api/audio-create-song-status/op-lookup":
			writeJSON(w, http.StatusOK, map[string]any{
				"operation_id": "op-lookup",
				"status":       "complete",
				"clip_id":      "clip-1",
			})
		case "/__api/clips":
			writeJSON(w, http.StatusOK, map[string]any{
				"clips": map[string]any{
					"clip-1": map[string]any{
						"id":        "clip-1",
						"title":     "Mock Song",
						"audio_url": mediaServer.URL + "/song.mp3",
						"wav_url":   mediaServer.URL + "/song.wav",
						"image_url": mediaServer.URL + "/cover.jpg",
						"lyrics": map[string]any{
							"status": "completed",
							"value":  map[string]any{"id": "lyrics-1", "text": "la la mock lyrics"},
						},
						"duration": map[string]any{"status": "completed", "value": "12.5"},
						"operation": map[string]any{
							"op_type":      "audio__create_song",
							"sound_prompt": "mock city pop sound",
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	rig := newHTTPTestRig(t, func(cfg *config.Config) {
		cfg.FlowMusicBaseURL = flowServer.URL
	})
	t.Cleanup(rig.Server.Close)
	ctx := context.Background()
	if _, err := rig.DB.CreateAccount(ctx, domain.Account{
		Email:              "mock@example.test",
		FlowBearer:         "flow-bearer",
		ProtocolMode:       "bearer",
		AutoRefreshEnabled: true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := rig.DB.UpdateCacheConfig(ctx, domain.CacheConfig{
		Enabled:     true,
		Timeout:     3600,
		StorageMode: "local",
	}); err != nil {
		t.Fatalf("UpdateCacheConfig() error = %v", err)
	}

	resp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/chat/completions", "test-api-key", map[string]any{
		"model": "lyria",
		"messages": []map[string]any{{
			"role":    "user",
			"content": "make city pop",
		}},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Data []struct {
			ClipID   string `json:"clip_id"`
			ImageURL string `json:"image_url"`
			Lyrics   string `json:"lyrics"`
		} `json:"data"`
		Clips []struct {
			ClipID string `json:"clip_id"`
		} `json:"clips"`
		FlowMusic struct {
			Clips []struct {
				Lyrics string `json:"lyrics"`
				Audio  struct {
					URL         string `json:"url"`
					OriginalURL string `json:"original_url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeResponse(t, resp, &payload)
	if payload.Object != "chat.completion" || len(payload.Choices) != 1 || payload.Choices[0].FinishReason != "stop" {
		t.Fatalf("unexpected chat response: %+v", payload)
	}
	if payload.Usage == nil || payload.Usage.PromptTokens != 0 || payload.Usage.CompletionTokens != 0 || payload.Usage.TotalTokens != 0 {
		t.Fatalf("unexpected chat usage payload: %+v", payload.Usage)
	}
	if !strings.Contains(payload.Choices[0].Message.Content, "/tmp/") || !strings.Contains(payload.Choices[0].Message.Content, "- Lyrics:") || !strings.Contains(payload.Choices[0].Message.Content, "la la mock lyrics") || !strings.Contains(payload.Choices[0].Message.Content, "Image:") || len(payload.FlowMusic.Clips) != 1 || !strings.HasPrefix(payload.FlowMusic.Clips[0].Audio.URL, "/tmp/") {
		t.Fatalf("cached audio URL not returned: %+v", payload)
	}
	if len(payload.Data) != 1 || payload.Data[0].ClipID != "clip-1" || payload.Data[0].Lyrics != "la la mock lyrics" || payload.Data[0].ImageURL == "" || len(payload.Clips) != 1 {
		t.Fatalf("chat response did not include rich clip data: %+v", payload)
	}
	if payload.FlowMusic.Clips[0].Audio.OriginalURL != mediaServer.URL+"/song.mp3" {
		t.Fatalf("original media URL mismatch: %+v", payload.FlowMusic.Clips[0].Audio)
	}

	proFastResp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/chat/completions", "test-api-key", map[string]any{
		"model": "Lyria-pro-fast",
		"messages": []map[string]any{{
			"role":    "user",
			"content": "make city pop pro fast",
		}},
	})
	if proFastResp.Code != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions pro fast status = %d, body = %s", proFastResp.Code, proFastResp.Body.String())
	}
	if !sawProFast {
		t.Fatalf("FlowMusic request did not use lyria-pro-fast mapping")
	}

	audioResp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/audio/generations", "test-api-key", map[string]any{
		"model": "lyria",
		"input": "make city pop",
	})
	if audioResp.Code != http.StatusOK {
		t.Fatalf("POST /v1/audio/generations status = %d, body = %s", audioResp.Code, audioResp.Body.String())
	}
	var audioPayload struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL      string `json:"url"`
			ClipID   string `json:"clip_id"`
			Title    string `json:"title"`
			Format   string `json:"format"`
			AudioURL string `json:"audio_url"`
			WavURL   string `json:"wav_url"`
			ImageURL string `json:"image_url"`
			Lyrics   string `json:"lyrics"`
			Audio    struct {
				URL         string `json:"url"`
				OriginalURL string `json:"original_url"`
				Format      string `json:"format"`
			} `json:"audio"`
			Wav struct {
				URL         string `json:"url"`
				OriginalURL string `json:"original_url"`
				Format      string `json:"format"`
			} `json:"wav"`
			Image struct {
				URL         string `json:"url"`
				OriginalURL string `json:"original_url"`
				Format      string `json:"format"`
			} `json:"image"`
		} `json:"data"`
		Clips []struct {
			ClipID string `json:"clip_id"`
		} `json:"clips"`
		FlowMusic struct {
			Clips []struct {
				ID     string `json:"id"`
				Title  string `json:"title"`
				Lyrics string `json:"lyrics"`
				Audio  struct {
					URL         string `json:"url"`
					OriginalURL string `json:"original_url"`
				} `json:"audio"`
				Image *struct {
					URL string `json:"url"`
				} `json:"image"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeResponse(t, audioResp, &audioPayload)
	if audioPayload.Created == 0 || len(audioPayload.Data) != 1 {
		t.Fatalf("unexpected audio response envelope: %+v", audioPayload)
	}
	if audioPayload.Data[0].ClipID != "clip-1" || audioPayload.Data[0].Title != "Mock Song" || audioPayload.Data[0].Format != "mp3" || !strings.HasPrefix(audioPayload.Data[0].URL, "/tmp/") {
		t.Fatalf("unexpected audio data payload: %+v", audioPayload.Data)
	}
	if len(audioPayload.Clips) != 1 || audioPayload.Clips[0].ClipID != "clip-1" || audioPayload.Data[0].Audio.OriginalURL != mediaServer.URL+"/song.mp3" || audioPayload.Data[0].Wav.OriginalURL != mediaServer.URL+"/song.wav" || audioPayload.Data[0].Image.OriginalURL != mediaServer.URL+"/cover.jpg" || audioPayload.Data[0].Lyrics != "la la mock lyrics" {
		t.Fatalf("unexpected rich audio data payload: %+v", audioPayload.Data[0])
	}
	if len(audioPayload.FlowMusic.Clips) != 1 || audioPayload.FlowMusic.Clips[0].Audio.OriginalURL != mediaServer.URL+"/song.mp3" || audioPayload.FlowMusic.Clips[0].Image == nil || audioPayload.FlowMusic.Clips[0].Lyrics != "la la mock lyrics" {
		t.Fatalf("unexpected audio flowmusic payload: %+v", audioPayload.FlowMusic)
	}

	audioMessagesResp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/audio/generations", "test-api-key", map[string]any{
		"model": "lyria",
		"messages": []map[string]any{{
			"role":    "user",
			"content": "make city pop",
		}},
	})
	if audioMessagesResp.Code != http.StatusOK {
		t.Fatalf("POST /v1/audio/generations with messages status = %d, body = %s", audioMessagesResp.Code, audioMessagesResp.Body.String())
	}
	var audioMessagesPayload struct {
		Data []struct {
			URL    string `json:"url"`
			ClipID string `json:"clip_id"`
		} `json:"data"`
	}
	decodeResponse(t, audioMessagesResp, &audioMessagesPayload)
	if len(audioMessagesPayload.Data) != 1 || audioMessagesPayload.Data[0].ClipID != "clip-1" || !strings.HasPrefix(audioMessagesPayload.Data[0].URL, "/tmp/") {
		t.Fatalf("unexpected messages audio data payload: %+v", audioMessagesPayload.Data)
	}

	musicGenerationResp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/music/generations", "test-api-key", map[string]any{
		"model": "lyria",
		"input": "make city pop",
	})
	if musicGenerationResp.Code != http.StatusOK {
		t.Fatalf("POST /v1/music/generations status = %d, body = %s", musicGenerationResp.Code, musicGenerationResp.Body.String())
	}
	var musicGenerationPayload struct {
		Data []struct {
			ClipID   string `json:"clip_id"`
			ImageURL string `json:"image_url"`
			Lyrics   string `json:"lyrics"`
		} `json:"data"`
	}
	decodeResponse(t, musicGenerationResp, &musicGenerationPayload)
	if len(musicGenerationPayload.Data) != 1 || musicGenerationPayload.Data[0].ClipID != "clip-1" || musicGenerationPayload.Data[0].ImageURL == "" || musicGenerationPayload.Data[0].Lyrics != "la la mock lyrics" {
		t.Fatalf("unexpected music generation data payload: %+v", musicGenerationPayload.Data)
	}

	resultResp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/music/results", "test-api-key", map[string]any{
		"operation_id": "op-lookup",
	})
	if resultResp.Code != http.StatusOK {
		t.Fatalf("POST /v1/music/results status = %d, body = %s", resultResp.Code, resultResp.Body.String())
	}
	var resultPayload struct {
		Data []struct {
			URL    string `json:"url"`
			ClipID string `json:"clip_id"`
		} `json:"data"`
		FlowMusic struct {
			ClipIDs []string `json:"clip_ids"`
			Clips   []struct {
				ID    string `json:"id"`
				Audio struct {
					URL string `json:"url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeResponse(t, resultResp, &resultPayload)
	if len(resultPayload.Data) != 1 || resultPayload.Data[0].ClipID != "clip-1" || !strings.HasPrefix(resultPayload.Data[0].URL, "/tmp/") {
		t.Fatalf("unexpected lookup data payload: %+v", resultPayload.Data)
	}
	if len(resultPayload.FlowMusic.ClipIDs) != 1 || resultPayload.FlowMusic.ClipIDs[0] != "clip-1" || len(resultPayload.FlowMusic.Clips) != 1 || resultPayload.FlowMusic.Clips[0].ID != "clip-1" {
		t.Fatalf("unexpected lookup flowmusic payload: %+v", resultPayload.FlowMusic)
	}

	audioResultResp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/audio/results", "test-api-key", map[string]any{
		"operation_id": "op-lookup",
	})
	if audioResultResp.Code != http.StatusOK {
		t.Fatalf("POST /v1/audio/results status = %d, body = %s", audioResultResp.Code, audioResultResp.Body.String())
	}

	audioSpeechResp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/audio/speech", "test-api-key", map[string]any{
		"model": "lyria",
		"input": "make city pop",
	})
	if audioSpeechResp.Code != http.StatusOK {
		t.Fatalf("POST /v1/audio/speech status = %d, body = %s", audioSpeechResp.Code, audioSpeechResp.Body.String())
	}
	var audioSpeechPayload struct {
		Data []struct {
			URL    string `json:"url"`
			ClipID string `json:"clip_id"`
			Format string `json:"format"`
		} `json:"data"`
		FlowMusic struct {
			Clips []struct {
				Audio struct {
					OriginalURL string `json:"original_url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeResponse(t, audioSpeechResp, &audioSpeechPayload)
	if len(audioSpeechPayload.Data) != 1 || audioSpeechPayload.Data[0].ClipID != "clip-1" || audioSpeechPayload.Data[0].Format != "mp3" || !strings.HasPrefix(audioSpeechPayload.Data[0].URL, "/tmp/") {
		t.Fatalf("unexpected audio speech data payload: %+v", audioSpeechPayload.Data)
	}
	if len(audioSpeechPayload.FlowMusic.Clips) != 1 || audioSpeechPayload.FlowMusic.Clips[0].Audio.OriginalURL != mediaServer.URL+"/song.mp3" {
		t.Fatalf("unexpected audio speech flowmusic payload: %+v", audioSpeechPayload.FlowMusic)
	}

	streamResp := doJSONRequest(t, http.MethodPost, rig.Server.URL+"/v1/chat/completions", "test-api-key", map[string]any{
		"model": "lyria",
		"messages": []map[string]any{{
			"role":    "user",
			"content": "make city pop",
		}},
		"stream": true,
	})
	if streamResp.Code != http.StatusOK {
		t.Fatalf("stream POST /v1/chat/completions status = %d, body = %s", streamResp.Code, streamResp.Body.String())
	}
	streamBody := streamResp.Body.String()
	if !strings.Contains(streamBody, `"object":"chat.completion.chunk"`) ||
		!strings.Contains(streamBody, `"reasoning_content"`) ||
		!strings.Contains(streamBody, "/tmp/") ||
		!strings.Contains(streamBody, "- Lyrics:") ||
		!strings.Contains(streamBody, "la la mock lyrics") ||
		!strings.Contains(streamBody, "Image:") ||
		!strings.Contains(streamBody, `"finish_reason":"stop"`) ||
		!strings.Contains(streamBody, "data: [DONE]") {
		t.Fatalf("unexpected stream response body: %s", streamBody)
	}
	if !sawConversation || !sawBearer {
		t.Fatalf("FlowMusic request did not use expected conversation or bearer: sawConversation=%v sawBearer=%v", sawConversation, sawBearer)
	}
}

func TestManagementErrorsKeepStringCompatibility(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	resp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens", login.Token, nil)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/tokens empty body status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Success bool   `json:"success"`
	}
	decodeResponse(t, resp, &payload)
	if payload.Error == "" || payload.Message == "" || payload.Detail == "" || payload.Success {
		t.Fatalf("unexpected management error payload: %+v", payload)
	}
}

func TestConfigWritesRejectInvalidJSON(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/cache/config"},
		{http.MethodPost, "/api/token-refresh/enabled"},
		{http.MethodPost, "/api/admin/debug"},
	} {
		req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader("{"))
		if err != nil {
			t.Fatalf("http.NewRequest(%s) error = %v", tc.path, err)
		}
		req.Header.Set("Authorization", "Bearer "+login.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("http.Do(%s) error = %v", tc.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s invalid JSON status = %d, want %d", tc.path, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

func TestJSONBodyReaderRejectsExtraValuesAndOversize(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "extra value", body: `{"enabled":true}{}`},
		{name: "oversize", body: `{"enabled":true}` + strings.Repeat(" ", maxJSONBodyBytes)},
	} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/admin/debug", strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("http.NewRequest(%s) error = %v", tc.name, err)
		}
		req.Header.Set("Authorization", "Bearer "+login.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("http.Do(%s) error = %v", tc.name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want %d", tc.name, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

func TestManagementCompatibilityRoutes(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	for _, path := range []string{
		"/api/cache/yun139/test",
		"/api/tokens/1/sora2/activate",
		"/api/logout",
	} {
		resp := doJSONRequest(t, http.MethodPost, ts.URL+path, login.Token, map[string]any{})
		if resp.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d, body = %s", path, resp.Code, resp.Body.String())
		}
	}
}

func TestActiveLogsEndpointReturnsRunningTasks(t *testing.T) {
	rig := newHTTPTestRig(t)
	ts := rig.Server
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	activeID, err := rig.DB.CreateRequestLog(context.Background(), domain.RequestLog{Operation: "music.generate", StatusCode: 102, StatusText: "polling", Progress: 60})
	if err != nil {
		t.Fatalf("CreateRequestLog(active) error = %v", err)
	}
	if _, err := rig.DB.CreateRequestLog(context.Background(), domain.RequestLog{Operation: "music.generate", StatusCode: 200, StatusText: "success", Progress: 100}); err != nil {
		t.Fatalf("CreateRequestLog(done) error = %v", err)
	}

	resp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/logs/active", login.Token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /api/logs/active status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var tasks []struct {
		ID         int64  `json:"id"`
		Operation  string `json:"operation"`
		StatusCode int    `json:"status_code"`
		StatusText string `json:"status_text"`
		Progress   int    `json:"progress"`
		StartedAt  string `json:"started_at"`
	}
	decodeResponse(t, resp, &tasks)
	if len(tasks) != 1 || tasks[0].ID != activeID || tasks[0].StatusCode != 102 || tasks[0].StatusText != "polling" || tasks[0].Progress != 60 || tasks[0].StartedAt == "" {
		t.Fatalf("unexpected active tasks: %+v", tasks)
	}
}

func TestProxyConfigPersists(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	saveResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/proxy/config", login.Token, map[string]any{
		"proxy_enabled":       true,
		"proxy_url":           "http://127.0.0.1:7890",
		"media_proxy_enabled": true,
		"media_proxy_url":     "socks5://127.0.0.1:1080",
	})
	if saveResp.Code != http.StatusOK {
		t.Fatalf("POST /api/proxy/config status = %d, body = %s", saveResp.Code, saveResp.Body.String())
	}

	getResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/proxy/config", login.Token, nil)
	var cfg struct {
		ProxyEnabled      bool   `json:"proxy_enabled"`
		ProxyURL          string `json:"proxy_url"`
		MediaProxyEnabled bool   `json:"media_proxy_enabled"`
		MediaProxyURL     string `json:"media_proxy_url"`
	}
	decodeResponse(t, getResp, &cfg)
	if !cfg.ProxyEnabled || cfg.ProxyURL != "http://127.0.0.1:7890" || !cfg.MediaProxyEnabled || cfg.MediaProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxy config was not persisted: %+v", cfg)
	}
}

func TestCallLogicConfigPersistsAndValidates(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	saveResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/call-logic/config", login.Token, map[string]any{"call_mode": "polling"})
	if saveResp.Code != http.StatusOK {
		t.Fatalf("POST /api/call-logic/config status = %d, body = %s", saveResp.Code, saveResp.Body.String())
	}
	var saveResult struct {
		Success bool `json:"success"`
		Config  struct {
			CallMode string `json:"call_mode"`
		} `json:"config"`
	}
	decodeResponse(t, saveResp, &saveResult)
	if !saveResult.Success || saveResult.Config.CallMode != "polling" {
		t.Fatalf("unexpected saved call logic config: %+v", saveResult)
	}

	getResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/call-logic/config", login.Token, nil)
	var getResult struct {
		Success bool `json:"success"`
		Config  struct {
			CallMode string `json:"call_mode"`
		} `json:"config"`
	}
	decodeResponse(t, getResp, &getResult)
	if !getResult.Success || getResult.Config.CallMode != "polling" {
		t.Fatalf("call logic config was not persisted: %+v", getResult)
	}

	badResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/call-logic/config", login.Token, map[string]any{"call_mode": "random"})
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/call-logic/config invalid mode status = %d, want %d, body = %s", badResp.Code, http.StatusBadRequest, badResp.Body.String())
	}
}

func TestSTToATAppliesProxyConfig(t *testing.T) {
	seen := map[string]bool{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Host
		if host == "" {
			host = r.Host
		}
		seen[host+r.URL.Path] = true
		switch host + r.URL.Path {
		case "supabase.test/auth/v1/token":
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token":           "supabase-access",
				"refresh_token":          "new-refresh-token",
				"provider_token":         "google-access",
				"provider_refresh_token": "google-refresh",
				"expires_in":             3600,
				"user": map[string]any{
					"email": "proxy@example.test",
				},
			})
		case "flowmusic.test/__api/auth/google/save":
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]string{"access_token": "flow-bearer-through-proxy"},
			})
		default:
			http.Error(w, "unexpected proxied request "+host+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(proxy.Close)

	rig := newHTTPTestRig(t, func(cfg *config.Config) {
		cfg.SupabaseBaseURL = "http://supabase.test"
		cfg.SupabaseAnonKey = "anon-key"
		cfg.FlowMusicBaseURL = "http://flowmusic.test"
	})
	ts := rig.Server
	t.Cleanup(ts.Close)
	if err := rig.DB.UpdateProxyConfig(context.Background(), domain.ProxyConfig{ProxyEnabled: true, ProxyURL: proxy.URL}); err != nil {
		t.Fatalf("UpdateProxyConfig() error = %v", err)
	}

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	resp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/st2at", login.Token, map[string]string{
		"st": "refresh-token",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens/st2at status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var result struct {
		Success     bool   `json:"success"`
		AccessToken string `json:"access_token"`
	}
	decodeResponse(t, resp, &result)
	if !result.Success || result.AccessToken != "flow-bearer-through-proxy" {
		t.Fatalf("unexpected ST to AT response: %+v", result)
	}
	if !seen["supabase.test/auth/v1/token"] || !seen["flowmusic.test/__api/auth/google/save"] {
		t.Fatalf("expected Supabase and FlowMusic calls through proxy, saw %+v", seen)
	}
}

func TestTokenTestRefreshesMissingBearerBeforeBilling(t *testing.T) {
	ctx := context.Background()
	var creditsAuth string
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/auth/google/save":
			if auth := r.Header.Get("Authorization"); auth != "" {
				t.Fatalf("SaveGoogle should not send stale Authorization header: %q", auth)
			}
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"access_token": "fresh-flow-bearer"}})
		case "/__api/billing/credits":
			creditsAuth = r.Header.Get("Authorization")
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"credits_remaining": 11, "tokens_remaining": 3}})
		case "/__api/billing/subscription":
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"subscription_tier": "PAYGATE_TIER_ONE"}})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":           "supabase-access",
			"refresh_token":          "fresh-refresh-token",
			"provider_token":         "google-access",
			"provider_refresh_token": "google-refresh",
			"expires_in":             3600,
			"user":                   map[string]string{"email": "test-token@example.test"},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	rig := newHTTPTestRig(t, func(cfg *config.Config) {
		cfg.FlowMusicBaseURL = flowServer.URL
		cfg.SupabaseBaseURL = supabaseServer.URL
		cfg.SupabaseAnonKey = "anon-key"
	})
	ts := rig.Server
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	id, err := rig.DB.CreateAccount(ctx, domain.Account{
		Email:              "test-token@example.test",
		RefreshToken:       "old-refresh-token",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	resp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/"+strconv.FormatInt(id, 10)+"/test", login.Token, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens/%d/test status = %d, body = %s", id, resp.Code, resp.Body.String())
	}
	var payload struct {
		Success bool   `json:"success"`
		Status  string `json:"status"`
		Email   string `json:"email"`
		Credits int    `json:"credits"`
	}
	decodeResponse(t, resp, &payload)
	if !payload.Success || payload.Status != "success" || payload.Credits != 11 {
		t.Fatalf("unexpected token test response: %+v", payload)
	}
	if creditsAuth != "Bearer supabase-access" {
		t.Fatalf("billing request used Authorization %q, want fresh bearer", creditsAuth)
	}
	stored, err := rig.DB.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.FlowBearer != "fresh-flow-bearer" || stored.RefreshToken != "fresh-refresh-token" {
		t.Fatalf("token test did not persist refreshed bearer: %+v", stored)
	}
}

func TestCacheConfigMasksSecretAndPreservesExistingSecret(t *testing.T) {
	rig := newHTTPTestRig(t)
	ts := rig.Server
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	saveResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/cache/config", login.Token, map[string]any{
		"enabled":            true,
		"timeout":            7200,
		"storage_mode":       "s3",
		"s3_endpoint":        "https://s3.example.test",
		"s3_region":          "auto",
		"s3_bucket":          "flowmusic-cache",
		"s3_access_key":      "dummy-access-key",
		"s3_secret_key":      "dummy-secret-key",
		"s3_use_ssl":         true,
		"s3_public_base_url": "https://cdn.example.test",
	})
	if saveResp.Code != http.StatusOK {
		t.Fatalf("POST /api/cache/config status = %d, body = %s", saveResp.Code, saveResp.Body.String())
	}
	var saveResult struct {
		Success bool `json:"success"`
		Config  struct {
			StorageMode      string `json:"storage_mode"`
			S3SecretKey      string `json:"s3_secret_key"`
			EffectiveBaseURL string `json:"effective_base_url"`
		} `json:"config"`
	}
	decodeResponse(t, saveResp, &saveResult)
	if !saveResult.Success || saveResult.Config.StorageMode != "s3" || saveResult.Config.S3SecretKey != "" {
		t.Fatalf("unexpected cache config response: %+v", saveResult)
	}
	if saveResult.Config.EffectiveBaseURL != "https://cdn.example.test" {
		t.Fatalf("unexpected effective base URL: %+v", saveResult.Config)
	}
	stored, err := rig.DB.GetCacheConfig(context.Background())
	if err != nil {
		t.Fatalf("GetCacheConfig() error = %v", err)
	}
	if stored.S3SecretKey != "dummy-secret-key" {
		t.Fatalf("secret was not stored: %+v", stored)
	}

	updateResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/cache/config", login.Token, map[string]any{
		"enabled":            true,
		"timeout":            3600,
		"storage_mode":       "R2 ",
		"s3_endpoint":        " https://r2.example.test ",
		"s3_region":          "auto",
		"s3_bucket":          " flowmusic-cache ",
		"s3_access_key":      "dummy-access-key",
		"s3_use_ssl":         true,
		"s3_public_base_url": "https://cdn.example.test",
	})
	if updateResp.Code != http.StatusOK {
		t.Fatalf("second POST /api/cache/config status = %d, body = %s", updateResp.Code, updateResp.Body.String())
	}
	stored, err = rig.DB.GetCacheConfig(context.Background())
	if err != nil {
		t.Fatalf("GetCacheConfig() after update error = %v", err)
	}
	if stored.Timeout != 3600 || stored.StorageMode != "r2" || stored.S3Endpoint != "https://r2.example.test" || stored.S3Bucket != "flowmusic-cache" || stored.S3SecretKey != "dummy-secret-key" || stored.S3PublicBaseURL != "" {
		t.Fatalf("R2 update should normalize mode/fields, clear public prefix, and preserve stored secret: %+v", stored)
	}

	getResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/cache/config", login.Token, nil)
	var getResult struct {
		Success bool `json:"success"`
		Config  struct {
			StorageMode      string `json:"storage_mode"`
			S3SecretKey      string `json:"s3_secret_key"`
			EffectiveBaseURL string `json:"effective_base_url"`
		} `json:"config"`
	}
	decodeResponse(t, getResp, &getResult)
	if !getResult.Success || getResult.Config.StorageMode != "r2" || getResult.Config.S3SecretKey != "" {
		t.Fatalf("GET /api/cache/config returned unexpected public config: %+v", getResult)
	}
	if getResult.Config.EffectiveBaseURL != "" {
		t.Fatalf("GET /api/cache/config effective base URL = %q, want empty for R2", getResult.Config.EffectiveBaseURL)
	}

	badResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/cache/config", login.Token, map[string]any{
		"enabled":      true,
		"timeout":      7200,
		"storage_mode": "bad-storage",
	})
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/cache/config invalid storage_mode status = %d, want %d, body = %s", badResp.Code, http.StatusBadRequest, badResp.Body.String())
	}
}

func TestCacheTestEndpointUsesStoredSecretAndDoesNotLeakIt(t *testing.T) {
	var putPath string
	var deletePath string
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		switch r.Method {
		case http.MethodPut:
			putPath = r.URL.Path
			w.Header().Set("ETag", `"test-etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			deletePath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected S3 method: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(s3.Close)

	rig := newHTTPTestRig(t)
	ts := rig.Server
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	saveResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/cache/config", login.Token, map[string]any{
		"enabled":             true,
		"timeout":             7200,
		"storage_mode":        "s3",
		"s3_endpoint":         s3.URL,
		"s3_bucket":           "flowmusic-cache",
		"s3_access_key":       "dummy-access-key",
		"s3_secret_key":       "dummy-secret-key",
		"s3_use_ssl":          false,
		"s3_force_path_style": true,
		"s3_prefix":           "flow-assets",
		"s3_public_base_url":  "https://cdn.example.test/cache",
	})
	if saveResp.Code != http.StatusOK {
		t.Fatalf("POST /api/cache/config status = %d, body = %s", saveResp.Code, saveResp.Body.String())
	}
	unauthorizedResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/cache/test", "", map[string]any{
		"storage_mode": "s3",
	})
	if unauthorizedResp.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/cache/test without auth status = %d, want %d", unauthorizedResp.Code, http.StatusUnauthorized)
	}

	testResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/cache/test", login.Token, map[string]any{
		"enabled":             true,
		"timeout":             7200,
		"storage_mode":        "s3",
		"s3_endpoint":         s3.URL,
		"s3_bucket":           "flowmusic-cache",
		"s3_access_key":       "dummy-access-key",
		"s3_use_ssl":          false,
		"s3_force_path_style": true,
		"s3_prefix":           "healthcheck-only",
		"s3_public_base_url":  "https://cdn.example.test/cache",
	})
	if testResp.Code != http.StatusOK {
		t.Fatalf("POST /api/cache/test status = %d, body = %s", testResp.Code, testResp.Body.String())
	}
	if strings.Contains(testResp.Body.String(), "dummy-secret-key") {
		t.Fatalf("POST /api/cache/test leaked secret: %s", testResp.Body.String())
	}
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Mode    string `json:"mode"`
		Media   struct {
			URL         string `json:"url"`
			OriginalURL string `json:"original_url"`
		} `json:"media"`
	}
	decodeResponse(t, testResp, &result)
	if !result.Success || result.Mode != "s3" || !strings.HasPrefix(result.Media.URL, "https://cdn.example.test/cache/healthcheck-only/.flowmusic2api-healthcheck/") {
		t.Fatalf("unexpected cache test response: %+v", result)
	}
	if !strings.Contains(putPath, "/flowmusic-cache/healthcheck-only/.flowmusic2api-healthcheck/") {
		t.Fatalf("unexpected S3 healthcheck put path: %s", putPath)
	}
	if deletePath != putPath {
		t.Fatalf("healthcheck object should be removed: put=%s delete=%s", putPath, deletePath)
	}
	stored, err := rig.DB.GetCacheConfig(context.Background())
	if err != nil {
		t.Fatalf("GetCacheConfig() error = %v", err)
	}
	if stored.S3Prefix != "flow-assets" || stored.S3SecretKey != "dummy-secret-key" {
		t.Fatalf("cache test should not persist request config or clear secret: %+v", stored)
	}
}

func TestCacheTestEndpointLocalMediaURLIsServed(t *testing.T) {
	rig := newHTTPTestRig(t)
	ts := rig.Server
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	testResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/cache/test", login.Token, map[string]any{
		"enabled":      true,
		"timeout":      7200,
		"storage_mode": "local",
	})
	if testResp.Code != http.StatusOK {
		t.Fatalf("POST /api/cache/test local status = %d, body = %s", testResp.Code, testResp.Body.String())
	}
	var result struct {
		Success bool   `json:"success"`
		Mode    string `json:"mode"`
		Media   struct {
			URL         string `json:"url"`
			OriginalURL string `json:"original_url"`
		} `json:"media"`
	}
	decodeResponse(t, testResp, &result)
	if !result.Success || result.Mode != "local" || !strings.HasPrefix(result.Media.URL, "/tmp/.flowmusic2api-healthcheck-") {
		t.Fatalf("unexpected local cache test response: %+v", result)
	}
	mediaResp, err := http.Get(ts.URL + result.Media.URL)
	if err != nil {
		t.Fatalf("GET local healthcheck media error = %v", err)
	}
	defer mediaResp.Body.Close()
	body, err := io.ReadAll(mediaResp.Body)
	if err != nil {
		t.Fatalf("read local healthcheck media error = %v", err)
	}
	if mediaResp.StatusCode != http.StatusOK || !strings.Contains(string(body), "flowmusic2api cache healthcheck") {
		t.Fatalf("unexpected local healthcheck media response: status=%d body=%q", mediaResp.StatusCode, string(body))
	}
}

func TestGenerationConfigPersistsExplicitTotalTimeout(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	saveResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/generation/timeout", login.Token, map[string]any{
		"timeout":       180,
		"image_timeout": 60,
		"video_timeout": 120,
		"max_retries":   4,
	})
	if saveResp.Code != http.StatusOK {
		t.Fatalf("POST /api/generation/timeout status = %d, body = %s", saveResp.Code, saveResp.Body.String())
	}
	var saveResult struct {
		Success bool `json:"success"`
	}
	decodeResponse(t, saveResp, &saveResult)
	if !saveResult.Success {
		t.Fatalf("unexpected save result: %+v", saveResult)
	}

	getResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/generation/timeout", login.Token, nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("GET /api/generation/timeout status = %d, body = %s", getResp.Code, getResp.Body.String())
	}
	var getResult struct {
		Success bool `json:"success"`
		Config  struct {
			Timeout      int `json:"timeout"`
			ImageTimeout int `json:"image_timeout"`
			VideoTimeout int `json:"video_timeout"`
			MaxRetries   int `json:"max_retries"`
		} `json:"config"`
	}
	decodeResponse(t, getResp, &getResult)
	if !getResult.Success || getResult.Config.Timeout != 180 || getResult.Config.ImageTimeout != 60 || getResult.Config.VideoTimeout != 120 || getResult.Config.MaxRetries != 4 {
		t.Fatalf("generation config was not persisted with explicit total timeout: %+v", getResult)
	}
}

func TestAdminConfigPersistsErrorBanThreshold(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	debugResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/admin/debug", login.Token, map[string]any{
		"enabled": true,
	})
	if debugResp.Code != http.StatusOK {
		t.Fatalf("POST /api/admin/debug status = %d, body = %s", debugResp.Code, debugResp.Body.String())
	}

	saveResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/admin/config", login.Token, map[string]any{
		"error_ban_threshold": 7,
	})
	if saveResp.Code != http.StatusOK {
		t.Fatalf("POST /api/admin/config status = %d, body = %s", saveResp.Code, saveResp.Body.String())
	}
	var saved struct {
		Success bool `json:"success"`
		Config  struct {
			ErrorBanThreshold int  `json:"error_ban_threshold"`
			DebugEnabled      bool `json:"debug_enabled"`
		} `json:"config"`
	}
	decodeResponse(t, saveResp, &saved)
	if !saved.Success || saved.Config.ErrorBanThreshold != 7 || !saved.Config.DebugEnabled {
		t.Fatalf("unexpected admin config save response: %+v", saved)
	}

	getResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/admin/config", login.Token, nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/config status = %d, body = %s", getResp.Code, getResp.Body.String())
	}
	var cfg struct {
		ErrorBanThreshold int  `json:"error_ban_threshold"`
		DebugEnabled      bool `json:"debug_enabled"`
	}
	decodeResponse(t, getResp, &cfg)
	if cfg.ErrorBanThreshold != 7 || !cfg.DebugEnabled {
		t.Fatalf("admin config was not persisted: %+v", cfg)
	}
}

func TestImportTokensMapsRefreshAndBearerAliases(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	importResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, map[string]any{
		"tokens": []map[string]any{
			{
				"email":         "import@example.test",
				"session_token": "refresh-token",
				"access_token":  "flow-bearer",
				"image_enabled": false,
			},
			{
				"email":         "default-auto@example.test",
				"session_token": "refresh-token-auto",
				"access_token":  "flow-bearer-auto",
				"is_active":     false,
			},
		},
	})
	if importResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens/import status = %d, body = %s", importResp.Code, importResp.Body.String())
	}
	var importResult struct {
		Added   int `json:"added"`
		Updated int `json:"updated"`
	}
	decodeResponse(t, importResp, &importResult)
	if importResult.Added != 2 || importResult.Updated != 0 {
		t.Fatalf("unexpected first import counts: %+v", importResult)
	}

	importUpdateResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, map[string]any{
		"tokens": []map[string]any{{
			"email":                "import@example.test",
			"refresh_token":        "refresh-token-2",
			"flow_bearer":          "flow-bearer-2",
			"auto_refresh_enabled": false,
			"video_enabled":        false,
		}},
	})
	if importUpdateResp.Code != http.StatusOK {
		t.Fatalf("second POST /api/tokens/import status = %d, body = %s", importUpdateResp.Code, importUpdateResp.Body.String())
	}
	decodeResponse(t, importUpdateResp, &importResult)
	if importResult.Added != 0 || importResult.Updated != 1 {
		t.Fatalf("unexpected second import counts: %+v", importResult)
	}

	listResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/tokens/export", login.Token, nil)
	var tokens []struct {
		Email              string `json:"email"`
		ST                 string `json:"st"`
		AT                 string `json:"at"`
		ProtocolMode       string `json:"protocol_mode"`
		AutoRefreshEnabled bool   `json:"auto_refresh_enabled"`
		IsActive           bool   `json:"is_active"`
		ImageEnabled       bool   `json:"image_enabled"`
		VideoEnabled       bool   `json:"video_enabled"`
		UpscaleEnabled     bool   `json:"upscale_enabled"`
	}
	var exportPayload struct {
		Tokens []struct {
			Email              string `json:"email"`
			ST                 string `json:"st"`
			AT                 string `json:"at"`
			ProtocolMode       string `json:"protocol_mode"`
			AutoRefreshEnabled bool   `json:"auto_refresh_enabled"`
			IsActive           bool   `json:"is_active"`
			ImageEnabled       bool   `json:"image_enabled"`
			VideoEnabled       bool   `json:"video_enabled"`
			UpscaleEnabled     bool   `json:"upscale_enabled"`
		} `json:"tokens"`
	}
	decodeResponse(t, listResp, &exportPayload)
	tokens = exportPayload.Tokens
	if len(tokens) != 2 {
		t.Fatalf("token count = %d, want 2", len(tokens))
	}
	byEmail := map[string]struct {
		Email              string `json:"email"`
		ST                 string `json:"st"`
		AT                 string `json:"at"`
		ProtocolMode       string `json:"protocol_mode"`
		AutoRefreshEnabled bool   `json:"auto_refresh_enabled"`
		IsActive           bool   `json:"is_active"`
		ImageEnabled       bool   `json:"image_enabled"`
		VideoEnabled       bool   `json:"video_enabled"`
		UpscaleEnabled     bool   `json:"upscale_enabled"`
	}{}
	for _, token := range tokens {
		byEmail[token.Email] = token
	}
	updatedToken := byEmail["import@example.test"]
	if updatedToken.ST != "refresh-token-2" || updatedToken.AT != "flow-bearer-2" || updatedToken.ProtocolMode != "refresh_token" || updatedToken.AutoRefreshEnabled {
		t.Fatalf("unexpected updated imported token: %+v", updatedToken)
	}
	if updatedToken.ImageEnabled || updatedToken.VideoEnabled || !updatedToken.UpscaleEnabled {
		t.Fatalf("import update should preserve omitted capability flags and apply explicit false: %+v", updatedToken)
	}
	defaultToken := byEmail["default-auto@example.test"]
	if !defaultToken.AutoRefreshEnabled {
		t.Fatalf("import without auto_refresh_enabled should default to true: %+v", defaultToken)
	}
	if defaultToken.ST != "refresh-token-auto" || defaultToken.AT != "flow-bearer-auto" {
		t.Fatalf("import aliases should not store placeholder token values: %+v", defaultToken)
	}
	if defaultToken.IsActive {
		t.Fatalf("imported is_active=false should be preserved: %+v", defaultToken)
	}
	if !defaultToken.ImageEnabled || !defaultToken.VideoEnabled || !defaultToken.UpscaleEnabled {
		t.Fatalf("import without capability flags should default all capabilities to true: %+v", defaultToken)
	}
}

func TestImportTokensAcceptsArrayPayloadAndRejectsBadTypes(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	importResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, []map[string]any{{
		"email":        "array-import@example.test",
		"access_token": "flow-bearer-array",
	}})
	if importResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens/import array status = %d, body = %s", importResp.Code, importResp.Body.String())
	}
	var importResult struct {
		Added   int `json:"added"`
		Updated int `json:"updated"`
	}
	decodeResponse(t, importResp, &importResult)
	if importResult.Added != 1 || importResult.Updated != 0 {
		t.Fatalf("unexpected array import counts: %+v", importResult)
	}
	exportResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/tokens/export", login.Token, nil)
	var exported struct {
		Tokens []struct {
			Email        string `json:"email"`
			ST           string `json:"st"`
			AT           string `json:"at"`
			ProtocolMode string `json:"protocol_mode"`
		} `json:"tokens"`
	}
	decodeResponse(t, exportResp, &exported)
	if len(exported.Tokens) != 1 || exported.Tokens[0].Email != "array-import@example.test" || exported.Tokens[0].ST != "" || exported.Tokens[0].AT != "flow-bearer-array" || exported.Tokens[0].ProtocolMode != "bearer" {
		t.Fatalf("unexpected direct array import export: %+v", exported.Tokens)
	}

	badResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, map[string]any{
		"tokens": []map[string]any{{
			"email":             "bad-import@example.test",
			"access_token":      "flow-bearer",
			"image_concurrency": "not-a-number",
		}},
	})
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/tokens/import bad type status = %d, want %d, body = %s", badResp.Code, http.StatusBadRequest, badResp.Body.String())
	}
}

func TestImportSupabaseSessionDoesNotStoreSupabaseAccessTokenAsFlowBearer(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	supabaseAccessToken := fakeSupabaseAccessToken(t)
	importResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, []map[string]any{{
		"email":                  "supabase-session@example.test",
		"access_token":           supabaseAccessToken,
		"refresh_token":          "supabase-refresh-token",
		"provider_token":         "google-provider-token",
		"provider_refresh_token": "google-provider-refresh-token",
	}})
	if importResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens/import status = %d, body = %s", importResp.Code, importResp.Body.String())
	}

	exportResp := doJSONRequest(t, http.MethodGet, ts.URL+"/api/tokens/export", login.Token, nil)
	var exported struct {
		Tokens []struct {
			Email                string `json:"email"`
			ST                   string `json:"st"`
			AT                   string `json:"at"`
			ProviderToken        string `json:"provider_token"`
			ProviderRefreshToken string `json:"provider_refresh_token"`
			ProtocolMode         string `json:"protocol_mode"`
		} `json:"tokens"`
	}
	decodeResponse(t, exportResp, &exported)
	if len(exported.Tokens) != 1 {
		t.Fatalf("token count = %d, want 1", len(exported.Tokens))
	}
	token := exported.Tokens[0]
	if token.Email != "supabase-session@example.test" ||
		token.ST != "supabase-refresh-token" ||
		token.AT != "" ||
		token.ProviderToken != "google-provider-token" ||
		token.ProviderRefreshToken != "google-provider-refresh-token" ||
		token.ProtocolMode != "refresh_token" {
		t.Fatalf("unexpected imported Supabase session token: %+v", token)
	}
}

func TestImportTokensAcceptsFlowMusicBrowserCookieExport(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	session := map[string]any{
		"access_token":            fakeSupabaseAccessToken(t),
		"refresh_token":           "cookie-refresh-token",
		"provider_token":          "cookie-provider-token",
		"provider_refresh_token":  "cookie-provider-refresh-token",
		"ignored_flow_bearer_key": "must-not-import",
		"user": map[string]any{
			"email": "cookie@example.test",
			"user_metadata": map[string]any{
				"name":      "Cookie User",
				"full_name": "Cookie Full Name",
			},
		},
	}
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(sessionJSON)
	splitAt := len(encoded) / 2
	cookies := []map[string]any{
		{"domain": ".flowmusic.app", "name": "_ga", "value": "GA1.1.0.0"},
		{"domain": "www.flowmusic.app", "name": "sb-sb-auth-token.1", "value": encoded[splitAt:]},
		{"domain": "www.flowmusic.app", "name": "sb-sb-auth-token.0", "value": "base64-" + encoded[:splitAt]},
	}

	importResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, map[string]any{
		"tokens": cookies,
	})
	if importResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens/import cookie export status = %d, body = %s", importResp.Code, importResp.Body.String())
	}
	var importResult struct {
		Added   int `json:"added"`
		Updated int `json:"updated"`
	}
	decodeResponse(t, importResp, &importResult)
	if importResult.Added != 1 || importResult.Updated != 0 {
		t.Fatalf("unexpected cookie import counts: %+v", importResult)
	}

	token := exportedTokenByEmail(t, ts.URL, login.Token, "cookie@example.test")
	if token.Name != "Cookie User" ||
		token.ST != "cookie-refresh-token" ||
		token.AT != "" ||
		token.ProviderToken != "cookie-provider-token" ||
		token.ProviderRefreshToken != "cookie-provider-refresh-token" ||
		token.ProtocolMode != "refresh_token" {
		t.Fatalf("unexpected imported cookie token: %+v", token)
	}
}

func TestImportTokenObjectCookieJSONForcesRefreshTokenMode(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	cookieJSON := fakeFlowMusicCookieJSON(t, "cookie-object@example.test", "cookie-object-refresh", "cookie-object-provider", "cookie-object-provider-refresh")
	importResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, []map[string]any{{
		"protocol_mode":  "protocol",
		"google_cookies": cookieJSON,
	}})
	if importResp.Code != http.StatusOK {
		t.Fatalf("POST /api/tokens/import token object cookie JSON status = %d, body = %s", importResp.Code, importResp.Body.String())
	}

	token := exportedTokenByEmail(t, ts.URL, login.Token, "cookie-object@example.test")
	if token.ST != "cookie-object-refresh" ||
		token.AT != "" ||
		token.ProviderToken != "cookie-object-provider" ||
		token.ProviderRefreshToken != "cookie-object-provider-refresh" ||
		token.ProtocolMode != "refresh_token" ||
		!strings.Contains(token.GoogleCookies, "sb-sb-auth-token.0=") {
		t.Fatalf("cookie object import should switch to refresh_token and extract credentials: %+v", token)
	}
}

func TestImportTokensRejectsBrokenFlowMusicBrowserCookieExport(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	badResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, []map[string]any{{
		"domain": "www.flowmusic.app",
		"name":   "sb-sb-auth-token.1",
		"value":  "base64-not-json",
	}})
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/tokens/import broken cookie status = %d, want %d, body = %s", badResp.Code, http.StatusBadRequest, badResp.Body.String())
	}
	if !strings.Contains(badResp.Body.String(), "contiguous") {
		t.Fatalf("broken cookie error should mention contiguous chunks, body = %s", badResp.Body.String())
	}
}

func TestImportTokensClearFieldsClearsSelectedCredentials(t *testing.T) {
	ts := newHTTPTestServer(t)
	t.Cleanup(ts.Close)

	loginResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, loginResp, &login)

	createResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, []map[string]any{{
		"email":                  "clear-import@example.test",
		"refresh_token":          "refresh-token",
		"flow_bearer":            "flow-bearer",
		"provider_token":         "provider-token",
		"provider_refresh_token": "provider-refresh-token",
		"google_cookies":         "cookie=value",
		"proxy_url":              "http://proxy.example.test:8080",
		"protocol_mode":          "refresh_token",
	}})
	if createResp.Code != http.StatusOK {
		t.Fatalf("initial POST /api/tokens/import status = %d, body = %s", createResp.Code, createResp.Body.String())
	}

	emptyCompatResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, []map[string]any{{
		"email":                  "clear-import@example.test",
		"refresh_token":          "",
		"flow_bearer":            "",
		"provider_token":         "",
		"provider_refresh_token": "",
		"google_cookies":         "",
		"proxy_url":              "",
	}})
	if emptyCompatResp.Code != http.StatusOK {
		t.Fatalf("empty POST /api/tokens/import status = %d, body = %s", emptyCompatResp.Code, emptyCompatResp.Body.String())
	}
	token := exportedTokenByEmail(t, ts.URL, login.Token, "clear-import@example.test")
	if token.ST != "refresh-token" ||
		token.AT != "flow-bearer" ||
		token.ProviderToken != "provider-token" ||
		token.ProviderRefreshToken != "provider-refresh-token" ||
		token.GoogleCookies != "cookie=value" ||
		token.ProxyURL != "http://proxy.example.test:8080" ||
		token.ProtocolMode != "refresh_token" {
		t.Fatalf("empty import should preserve old credentials: %+v", token)
	}

	clearResp := doJSONRequest(t, http.MethodPost, ts.URL+"/api/tokens/import", login.Token, []map[string]any{{
		"email":        "clear-import@example.test",
		"clear_fields": []string{"refresh_token", "flow_bearer", "provider_token", "provider_refresh_token", "google_cookies", "proxy_url"},
	}})
	if clearResp.Code != http.StatusOK {
		t.Fatalf("clear POST /api/tokens/import status = %d, body = %s", clearResp.Code, clearResp.Body.String())
	}
	token = exportedTokenByEmail(t, ts.URL, login.Token, "clear-import@example.test")
	if token.ST != "" ||
		token.AT != "" ||
		token.ProviderToken != "" ||
		token.ProviderRefreshToken != "" ||
		token.GoogleCookies != "" ||
		token.ProxyURL != "" ||
		token.ProtocolMode != "refresh_token" {
		t.Fatalf("clear_fields import did not clear selected credentials while preserving protocol: %+v", token)
	}
}

type exportedTokenView struct {
	ID                   int64  `json:"id"`
	Email                string `json:"email"`
	Name                 string `json:"name"`
	ST                   string `json:"st"`
	AT                   string `json:"at"`
	ProtocolMode         string `json:"protocol_mode"`
	ProviderToken        string `json:"provider_token"`
	ProviderRefreshToken string `json:"provider_refresh_token"`
	GoogleCookies        string `json:"google_cookies"`
	ProxyURL             string `json:"proxy_url"`
}

func exportedTokenByEmail(t *testing.T, baseURL, bearer, email string) exportedTokenView {
	t.Helper()
	exportResp := doJSONRequest(t, http.MethodGet, baseURL+"/api/tokens/export", bearer, nil)
	if exportResp.Code != http.StatusOK {
		t.Fatalf("GET /api/tokens/export status = %d, body = %s", exportResp.Code, exportResp.Body.String())
	}
	var exported struct {
		Tokens []exportedTokenView `json:"tokens"`
	}
	decodeResponse(t, exportResp, &exported)
	for _, token := range exported.Tokens {
		if token.Email == email {
			return token
		}
	}
	t.Fatalf("exported token %q not found in %+v", email, exported.Tokens)
	return exportedTokenView{}
}

type testHTTPResponse struct {
	Code int
	Body *bytes.Buffer
}

func doJSONRequest(t *testing.T, method, url, bearer string, body any) *testHTTPResponse {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http.Do() error = %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll() error = %v", err)
	}
	return &testHTTPResponse{Code: resp.StatusCode, Body: bytes.NewBuffer(data)}
}

func decodeResponse(t *testing.T, resp *testHTTPResponse, out any) {
	t.Helper()
	if err := json.Unmarshal(resp.Body.Bytes(), out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", resp.Body.String(), err)
	}
}

func assertOpenAIErrorEnvelope(t *testing.T, resp *testHTTPResponse, wantStatus int) {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    int    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Success bool   `json:"success"`
	}
	decodeResponse(t, resp, &payload)
	if payload.Error.Message == "" || payload.Error.Type != "invalid_request_error" || payload.Error.Code != wantStatus || payload.Message == "" || payload.Success {
		t.Fatalf("unexpected OpenAI error payload: %+v", payload)
	}
}
