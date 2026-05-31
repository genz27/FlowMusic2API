package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerProcessSQLiteSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process smoke test in short mode")
	}
	root := projectRoot(t)
	port := freeTCPPort(t)
	dataDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "tmp")
	apiKey := "fm-smoke-key"
	binary := buildSmokeServerBinary(t, root)
	mediaServer := newSmokeMediaServer(t)
	flowServer, flowState := newSmokeFlowMusicServer(t, mediaServer.URL)
	supabaseServer := newSmokeSupabaseServer(t)
	s3Server, s3State := newSmokeS3Server(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary)
	cmd.Dir = root
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(),
		"FLOWMUSIC_ROOT="+root,
		"FLOWMUSIC_STATIC_DIR="+filepath.Join(root, "web", "static"),
		"FLOWMUSIC_DATA_DIR="+dataDir,
		"FLOWMUSIC_CACHE_DIR="+cacheDir,
		"FLOWMUSIC_DB_DRIVER=sqlite",
		"FLOWMUSIC_DATABASE_URL="+filepath.Join(dataDir, "flowmusic2api.db"),
		"FLOWMUSIC_HTTP_HOST=127.0.0.1",
		fmt.Sprintf("FLOWMUSIC_HTTP_PORT=%d", port),
		"FLOWMUSIC_ADMIN_USER=admin",
		"FLOWMUSIC_ADMIN_PASSWORD=admin",
		"FLOWMUSIC_DEFAULT_API_KEY="+apiKey,
		"FLOWMUSIC_ADMIN_JWT_SECRET=smoke-test-admin-secret",
		"FLOWMUSIC_BASE_URL="+flowServer.URL,
		"FLOWMUSIC_SUPABASE_BASE_URL="+supabaseServer.URL,
		"FLOWMUSIC_SUPABASE_ANON_KEY=smoke-anon-key",
		"FLOWMUSIC_DISABLE_WORKERS=true",
		"FLOWMUSIC_UPSTREAM_TIMEOUT_SECONDS=1",
		"FLOWMUSIC_GENERATION_TIMEOUT_SECONDS=1",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server process: %v", err)
	}
	waitDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(waitDone)
	}()
	defer func() {
		cancel()
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-waitDone
		}
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}
	waitForSmokeHealth(t, ctx, client, baseURL, waitDone, func() error { return waitErr }, &stdout, &stderr)

	assertSmokeGET(t, client, baseURL+"/login", http.StatusOK)
	assertSmokeGET(t, client, baseURL+"/manage", http.StatusOK)

	modelsResp := assertSmokeGET(t, client, baseURL+"/v1/models", http.StatusOK)
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decodeSmokeJSON(t, modelsResp, &models)
	if len(models.Data) == 0 {
		t.Fatalf("GET /v1/models returned no models")
	}

	modelResp := smokeJSONRequest(t, client, http.MethodGet, baseURL+"/v1/models/flowmusic", apiKey, nil)
	if modelResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models/flowmusic status = %d, body = %s", modelResp.StatusCode, modelResp.Body)
	}

	loginResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/login", "", map[string]string{
		"username": "admin",
		"password": "admin",
	})
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/login status = %d, body = %s", loginResp.StatusCode, loginResp.Body)
	}
	var login struct {
		Token string `json:"token"`
	}
	decodeSmokeJSON(t, loginResp, &login)
	if strings.TrimSpace(login.Token) == "" {
		t.Fatalf("POST /api/login returned empty token: %s", loginResp.Body)
	}

	cacheResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/cache/test", login.Token, map[string]any{
		"storage_mode": "local",
	})
	if cacheResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/cache/test status = %d, body = %s", cacheResp.StatusCode, cacheResp.Body)
	}
	var cachePayload struct {
		Success bool `json:"success"`
		Media   struct {
			URL string `json:"url"`
		} `json:"media"`
	}
	decodeSmokeJSON(t, cacheResp, &cachePayload)
	if !cachePayload.Success || !strings.HasPrefix(cachePayload.Media.URL, "/tmp/") {
		t.Fatalf("unexpected cache test payload: %s", cacheResp.Body)
	}
	assertSmokeGET(t, client, baseURL+cachePayload.Media.URL, http.StatusOK)

	audioResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/v1/audio/speech", apiKey, map[string]any{
		"model": "lyria",
		"input": "smoke test prompt",
	})
	if audioResp.StatusCode != http.StatusBadGateway {
		t.Fatalf("POST /v1/audio/speech without account status = %d, body = %s", audioResp.StatusCode, audioResp.Body)
	}
	var audioError struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	decodeSmokeJSON(t, audioResp, &audioError)
	if audioError.Error.Message == "" || audioError.Error.Code != http.StatusBadGateway {
		t.Fatalf("unexpected OpenAI error payload: %s", audioResp.Body)
	}

	createTokenResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/tokens", login.Token, map[string]any{
		"email":                "process-smoke@example.test",
		"flow_bearer":          "flow-bearer",
		"protocol_mode":        "bearer",
		"auto_refresh_enabled": false,
	})
	if createTokenResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens status = %d, body = %s", createTokenResp.StatusCode, createTokenResp.Body)
	}
	var createdBearerToken struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	decodeSmokeJSON(t, createTokenResp, &createdBearerToken)
	if createdBearerToken.Token.ID == 0 {
		t.Fatalf("created bearer token response did not include id: %s", createTokenResp.Body)
	}

	saveCacheResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/cache/config", login.Token, map[string]any{
		"enabled":      true,
		"timeout":      3600,
		"storage_mode": "local",
	})
	if saveCacheResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/cache/config status = %d, body = %s", saveCacheResp.StatusCode, saveCacheResp.Body)
	}

	chatResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/v1/chat/completions", apiKey, map[string]any{
		"model": "lyria",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "make process smoke music",
		}},
	})
	if chatResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions status = %d, body = %s", chatResp.StatusCode, chatResp.Body)
	}
	var chatPayload struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		FlowMusic struct {
			Clips []struct {
				Audio struct {
					URL         string `json:"url"`
					OriginalURL string `json:"original_url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeSmokeJSON(t, chatResp, &chatPayload)
	if chatPayload.Object != "chat.completion" || len(chatPayload.Choices) != 1 || !strings.Contains(chatPayload.Choices[0].Message.Content, "/tmp/") {
		t.Fatalf("unexpected chat completion payload: %s", chatResp.Body)
	}
	if len(chatPayload.FlowMusic.Clips) != 1 || !strings.HasPrefix(chatPayload.FlowMusic.Clips[0].Audio.URL, "/tmp/") || chatPayload.FlowMusic.Clips[0].Audio.OriginalURL != mediaServer.URL+"/song.mp3" {
		t.Fatalf("unexpected cached FlowMusic payload: %s", chatResp.Body)
	}
	assertSmokeGET(t, client, baseURL+chatPayload.FlowMusic.Clips[0].Audio.URL, http.StatusOK)
	if !flowState.sawConversation.Load() || !flowState.sawBearer.Load() {
		t.Fatalf("mock FlowMusic did not observe expected conversation or bearer: sawConversation=%v sawBearer=%v", flowState.sawConversation.Load(), flowState.sawBearer.Load())
	}

	disableResp := smokeJSONRequest(t, client, http.MethodPost, fmt.Sprintf("%s/api/tokens/%d/disable", baseURL, createdBearerToken.Token.ID), login.Token, map[string]any{})
	if disableResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens/%d/disable status = %d, body = %s", createdBearerToken.Token.ID, disableResp.StatusCode, disableResp.Body)
	}

	refreshTokenResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/tokens", login.Token, map[string]any{
		"email":                    "process-refresh@example.test",
		"refresh_token":            "old-refresh-token",
		"protocol_mode":            "refresh_token",
		"auto_refresh_enabled":     true,
		"refresh_interval_minutes": 5,
	})
	if refreshTokenResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens refresh account status = %d, body = %s", refreshTokenResp.StatusCode, refreshTokenResp.Body)
	}
	var createdRefreshToken struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	decodeSmokeJSON(t, refreshTokenResp, &createdRefreshToken)
	if createdRefreshToken.Token.ID == 0 {
		t.Fatalf("created refresh token response did not include id: %s", refreshTokenResp.Body)
	}

	refreshChatResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/v1/chat/completions", apiKey, map[string]any{
		"model": "lyria",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "make process smoke music",
		}},
	})
	if refreshChatResp.StatusCode != http.StatusOK {
		t.Fatalf("refresh-token POST /v1/chat/completions status = %d, body = %s", refreshChatResp.StatusCode, refreshChatResp.Body)
	}
	var refreshChatPayload struct {
		FlowMusic struct {
			AccountID int64 `json:"account_id"`
			Clips     []struct {
				Audio struct {
					URL string `json:"url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeSmokeJSON(t, refreshChatResp, &refreshChatPayload)
	if refreshChatPayload.FlowMusic.AccountID == 0 || len(refreshChatPayload.FlowMusic.Clips) != 1 || !strings.HasPrefix(refreshChatPayload.FlowMusic.Clips[0].Audio.URL, "/tmp/") {
		t.Fatalf("unexpected refresh-token generation payload: %s", refreshChatResp.Body)
	}
	if !flowState.sawGoogleSave.Load() || !flowState.sawRefreshedBearer.Load() {
		t.Fatalf("mock FlowMusic did not observe refresh flow: sawGoogleSave=%v sawRefreshedBearer=%v", flowState.sawGoogleSave.Load(), flowState.sawRefreshedBearer.Load())
	}

	disableRefreshResp := smokeJSONRequest(t, client, http.MethodPost, fmt.Sprintf("%s/api/tokens/%d/disable", baseURL, createdRefreshToken.Token.ID), login.Token, map[string]any{})
	if disableRefreshResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens/%d/disable status = %d, body = %s", createdRefreshToken.Token.ID, disableRefreshResp.StatusCode, disableRefreshResp.Body)
	}

	cookieTokenResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/tokens", login.Token, map[string]any{
		"email":                    "process-cookie@example.test",
		"protocol_mode":            "protocol",
		"google_cookies":           "flow_cookie=smoke",
		"auto_refresh_enabled":     true,
		"refresh_interval_minutes": 5,
	})
	if cookieTokenResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens cookie account status = %d, body = %s", cookieTokenResp.StatusCode, cookieTokenResp.Body)
	}
	var createdCookieToken struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	decodeSmokeJSON(t, cookieTokenResp, &createdCookieToken)
	if createdCookieToken.Token.ID == 0 {
		t.Fatalf("created cookie token response did not include id: %s", cookieTokenResp.Body)
	}

	cookieChatResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/v1/chat/completions", apiKey, map[string]any{
		"model": "lyria",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "make process smoke music",
		}},
	})
	if cookieChatResp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-protocol POST /v1/chat/completions status = %d, body = %s", cookieChatResp.StatusCode, cookieChatResp.Body)
	}
	var cookieChatPayload struct {
		FlowMusic struct {
			AccountID int64 `json:"account_id"`
			Clips     []struct {
				Audio struct {
					URL string `json:"url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeSmokeJSON(t, cookieChatResp, &cookieChatPayload)
	if cookieChatPayload.FlowMusic.AccountID == 0 || len(cookieChatPayload.FlowMusic.Clips) != 1 || !strings.HasPrefix(cookieChatPayload.FlowMusic.Clips[0].Audio.URL, "/tmp/") {
		t.Fatalf("unexpected cookie-protocol generation payload: %s", cookieChatResp.Body)
	}
	if !flowState.sawCookieSession.Load() || !flowState.sawCookieBearer.Load() {
		t.Fatalf("mock FlowMusic did not observe cookie protocol flow: sawCookieSession=%v sawCookieBearer=%v", flowState.sawCookieSession.Load(), flowState.sawCookieBearer.Load())
	}

	disableCookieResp := smokeJSONRequest(t, client, http.MethodPost, fmt.Sprintf("%s/api/tokens/%d/disable", baseURL, createdCookieToken.Token.ID), login.Token, map[string]any{})
	if disableCookieResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens/%d/disable status = %d, body = %s", createdCookieToken.Token.ID, disableCookieResp.StatusCode, disableCookieResp.Body)
	}

	providerTokenResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/tokens", login.Token, map[string]any{
		"email":                    "process-provider@example.test",
		"provider_token":           "direct-provider-token",
		"provider_refresh_token":   "direct-provider-refresh",
		"protocol_mode":            "refresh_token",
		"auto_refresh_enabled":     true,
		"refresh_interval_minutes": 5,
	})
	if providerTokenResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens provider account status = %d, body = %s", providerTokenResp.StatusCode, providerTokenResp.Body)
	}
	var createdProviderToken struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	decodeSmokeJSON(t, providerTokenResp, &createdProviderToken)
	if createdProviderToken.Token.ID == 0 {
		t.Fatalf("created provider token response did not include id: %s", providerTokenResp.Body)
	}

	providerChatResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/v1/chat/completions", apiKey, map[string]any{
		"model": "lyria",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "make process smoke music",
		}},
	})
	if providerChatResp.StatusCode != http.StatusOK {
		t.Fatalf("provider-token POST /v1/chat/completions status = %d, body = %s", providerChatResp.StatusCode, providerChatResp.Body)
	}
	var providerChatPayload struct {
		FlowMusic struct {
			AccountID int64 `json:"account_id"`
			Clips     []struct {
				Audio struct {
					URL string `json:"url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeSmokeJSON(t, providerChatResp, &providerChatPayload)
	if providerChatPayload.FlowMusic.AccountID != createdProviderToken.Token.ID || len(providerChatPayload.FlowMusic.Clips) != 1 || !strings.HasPrefix(providerChatPayload.FlowMusic.Clips[0].Audio.URL, "/tmp/") {
		t.Fatalf("unexpected provider-token generation payload: %s", providerChatResp.Body)
	}
	if !flowState.sawDirectProviderSave.Load() || !flowState.sawDirectProviderBearer.Load() {
		t.Fatalf("mock FlowMusic did not observe provider token flow: sawDirectProviderSave=%v sawDirectProviderBearer=%v", flowState.sawDirectProviderSave.Load(), flowState.sawDirectProviderBearer.Load())
	}

	saveR2CacheResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/cache/config", login.Token, map[string]any{
		"enabled":             true,
		"timeout":             3600,
		"storage_mode":        "r2",
		"s3_endpoint":         s3Server.URL,
		"s3_bucket":           "flowmusic-cache",
		"s3_access_key":       "smoke-access-key",
		"s3_secret_key":       "smoke-secret-key",
		"s3_use_ssl":          false,
		"s3_force_path_style": true,
		"s3_prefix":           "flow-assets",
	})
	if saveR2CacheResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/cache/config r2 status = %d, body = %s", saveR2CacheResp.StatusCode, saveR2CacheResp.Body)
	}

	r2AudioResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/v1/audio/generations", apiKey, map[string]any{
		"model": "lyria",
		"input": "make process smoke music",
	})
	if r2AudioResp.StatusCode != http.StatusOK {
		t.Fatalf("r2 POST /v1/audio/generations status = %d, body = %s", r2AudioResp.StatusCode, r2AudioResp.Body)
	}
	var r2AudioPayload struct {
		Data []struct {
			URL    string `json:"url"`
			ClipID string `json:"clip_id"`
		} `json:"data"`
		FlowMusic struct {
			Clips []struct {
				Audio struct {
					URL         string `json:"url"`
					OriginalURL string `json:"original_url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeSmokeJSON(t, r2AudioResp, &r2AudioPayload)
	if len(r2AudioPayload.Data) != 1 || r2AudioPayload.Data[0].ClipID != "clip-process" || !strings.HasPrefix(r2AudioPayload.Data[0].URL, s3Server.URL+"/flowmusic-cache/flow-assets/") || !strings.Contains(r2AudioPayload.Data[0].URL, "X-Amz-Signature=") {
		t.Fatalf("unexpected r2 audio payload: %s", r2AudioResp.Body)
	}
	if len(r2AudioPayload.FlowMusic.Clips) != 1 || r2AudioPayload.FlowMusic.Clips[0].Audio.URL != r2AudioPayload.Data[0].URL || r2AudioPayload.FlowMusic.Clips[0].Audio.OriginalURL != mediaServer.URL+"/song.mp3" {
		t.Fatalf("unexpected r2 flowmusic payload: %s", r2AudioResp.Body)
	}
	putPath, putBody := s3State.put()
	if !strings.HasPrefix(putPath, "/flowmusic-cache/flow-assets/") || !strings.HasSuffix(putPath, ".mp3") {
		t.Fatalf("unexpected R2/S3 put path: %s", putPath)
	}
	if !strings.Contains(putBody, "mock-mp3") {
		t.Fatalf("unexpected R2/S3 put body: %q", putBody)
	}

	expiredTokenResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/tokens", login.Token, map[string]any{
		"email":                "process-expired@example.test",
		"flow_bearer":          "expired-flow-bearer",
		"protocol_mode":        "bearer",
		"auto_refresh_enabled": false,
	})
	if expiredTokenResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens expired bearer status = %d, body = %s", expiredTokenResp.StatusCode, expiredTokenResp.Body)
	}
	fallbackTokenResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/api/tokens", login.Token, map[string]any{
		"email":                "process-fallback@example.test",
		"flow_bearer":          "fallback-flow-bearer",
		"protocol_mode":        "bearer",
		"auto_refresh_enabled": false,
	})
	if fallbackTokenResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/tokens fallback bearer status = %d, body = %s", fallbackTokenResp.StatusCode, fallbackTokenResp.Body)
	}

	fallbackResp := smokeJSONRequest(t, client, http.MethodPost, baseURL+"/v1/chat/completions", apiKey, map[string]any{
		"model": "lyria",
		"messages": []map[string]string{{
			"role":    "user",
			"content": "make process smoke music",
		}},
	})
	if fallbackResp.StatusCode != http.StatusOK {
		t.Fatalf("fallback POST /v1/chat/completions status = %d, body = %s", fallbackResp.StatusCode, fallbackResp.Body)
	}
	var fallbackPayload struct {
		FlowMusic struct {
			AccountID int64 `json:"account_id"`
			Clips     []struct {
				Audio struct {
					URL string `json:"url"`
				} `json:"audio"`
			} `json:"clips"`
		} `json:"flowmusic"`
	}
	decodeSmokeJSON(t, fallbackResp, &fallbackPayload)
	if fallbackPayload.FlowMusic.AccountID == 0 || len(fallbackPayload.FlowMusic.Clips) != 1 || !strings.HasPrefix(fallbackPayload.FlowMusic.Clips[0].Audio.URL, s3Server.URL+"/flowmusic-cache/flow-assets/") || !strings.Contains(fallbackPayload.FlowMusic.Clips[0].Audio.URL, "X-Amz-Signature=") {
		t.Fatalf("unexpected fallback generation payload: %s", fallbackResp.Body)
	}
	if !flowState.sawExpiredBearer.Load() || !flowState.sawFallbackBearer.Load() {
		t.Fatalf("mock FlowMusic did not observe invalid-bearer fallback: sawExpiredBearer=%v sawFallbackBearer=%v", flowState.sawExpiredBearer.Load(), flowState.sawFallbackBearer.Load())
	}
}

type smokeResponse struct {
	StatusCode int
	Body       []byte
}

func projectRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve project root: %v", err)
	}
	return root
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func buildSmokeServerBinary(t *testing.T, root string) string {
	t.Helper()
	name := "flowmusic2api-smoke"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/server")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build smoke server binary: %v\n%s", err, out)
	}
	return binary
}

func newSmokeMediaServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/song.mp3" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("mock-mp3"))
	}))
	t.Cleanup(server.Close)
	return server
}

type smokeFlowMusicState struct {
	sawConversation         atomic.Bool
	sawBearer               atomic.Bool
	sawGoogleSave           atomic.Bool
	sawRefreshedBearer      atomic.Bool
	sawDirectProviderSave   atomic.Bool
	sawDirectProviderBearer atomic.Bool
	sawCookieSession        atomic.Bool
	sawCookieBearer         atomic.Bool
	sawExpiredBearer        atomic.Bool
	sawFallbackBearer       atomic.Bool
}

func newSmokeFlowMusicServer(t *testing.T, mediaBaseURL string) (*httptest.Server, *smokeFlowMusicState) {
	t.Helper()
	state := &smokeFlowMusicState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer flow-bearer" {
			state.sawBearer.Store(true)
		}
		if r.Header.Get("Authorization") == "Bearer refreshed-flow-bearer" {
			state.sawRefreshedBearer.Store(true)
		}
		if r.Header.Get("Authorization") == "Bearer direct-provider-flow-bearer" {
			state.sawDirectProviderBearer.Store(true)
		}
		if r.Header.Get("Authorization") == "Bearer cookie-flow-bearer" {
			state.sawCookieBearer.Store(true)
		}
		if r.Header.Get("Authorization") == "Bearer fallback-flow-bearer" {
			state.sawFallbackBearer.Store(true)
		}
		switch r.URL.Path {
		case "/__api/conversation":
			state.sawConversation.Store(true)
			if r.Header.Get("Authorization") == "Bearer expired-flow-bearer" {
				state.sawExpiredBearer.Store(true)
				http.Error(w, "expired bearer", http.StatusUnauthorized)
				return
			}
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				Parts []struct {
					Content  string `json:"content"`
					PartKind string `json:"part_kind"`
				} `json:"parts"`
				ModelName string `json:"model_name"`
				Mode      string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if req.ModelName != "producer:standard" || req.Mode != "standard" || len(req.Parts) != 1 || req.Parts[0].PartKind != "user-prompt" || !strings.Contains(req.Parts[0].Content, "make process smoke music") {
				http.Error(w, fmt.Sprintf("unexpected conversation request: %+v", req), http.StatusBadRequest)
				return
			}
			writeSmokeJSON(w, map[string]any{"job_id": "job-smoke"})
		case "/__api/auth/google/save":
			state.sawGoogleSave.Store(true)
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if auth := r.Header.Get("Authorization"); auth != "" {
				http.Error(w, "google save should not send stale Authorization: "+auth, http.StatusBadRequest)
				return
			}
			var req struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				Platform     string `json:"platform"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			switch {
			case req.AccessToken == "google-provider-token" && req.RefreshToken == "google-provider-refresh" && req.Platform == "web":
				writeSmokeJSON(w, map[string]any{
					"data": map[string]any{
						"access_token": "refreshed-flow-bearer",
					},
				})
			case req.AccessToken == "direct-provider-token" && req.RefreshToken == "direct-provider-refresh" && req.Platform == "web":
				state.sawDirectProviderSave.Store(true)
				writeSmokeJSON(w, map[string]any{
					"data": map[string]any{
						"access_token": "direct-provider-flow-bearer",
					},
				})
			default:
				http.Error(w, fmt.Sprintf("unexpected google save request: %+v", req), http.StatusBadRequest)
			}
		case "/__api/auth/session":
			state.sawCookieSession.Store(true)
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if auth := r.Header.Get("Authorization"); auth != "" {
				http.Error(w, "session should not send stale Authorization: "+auth, http.StatusBadRequest)
				return
			}
			if cookie := r.Header.Get("Cookie"); cookie != "flow_cookie=smoke" {
				http.Error(w, "unexpected cookie: "+cookie, http.StatusBadRequest)
				return
			}
			writeSmokeJSON(w, map[string]any{
				"user": map[string]any{
					"email": "process-cookie@example.test",
				},
				"session": map[string]any{
					"flowAccessToken": "cookie-flow-bearer",
					"expiresIn":       3600,
				},
			})
		case "/__api/messages/job-smoke/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"clip_id\":\"clip-process\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/__api/clips":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeSmokeJSON(w, map[string]any{
				"clips": map[string]any{
					"clip-process": map[string]any{
						"id":        "clip-process",
						"title":     "Process Smoke Song",
						"audio_url": mediaBaseURL + "/song.mp3",
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, state
}

func newSmokeSupabaseServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/v1/token" || r.URL.Query().Get("grant_type") != "refresh_token" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("apikey") != "smoke-anon-key" || r.Header.Get("Authorization") != "Bearer smoke-anon-key" {
			http.Error(w, "missing supabase anon auth", http.StatusUnauthorized)
			return
		}
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.RefreshToken != "old-refresh-token" {
			http.Error(w, "unexpected refresh token", http.StatusBadRequest)
			return
		}
		writeSmokeJSON(w, map[string]any{
			"refresh_token":          "new-refresh-token",
			"provider_token":         "google-provider-token",
			"provider_refresh_token": "google-provider-refresh",
			"expires_in":             3600,
			"user": map[string]any{
				"email": "process-refresh@example.test",
			},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

type smokeS3State struct {
	mu      sync.Mutex
	putPath string
	putBody string
}

func (s *smokeS3State) put() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putPath, s.putBody
}

func newSmokeS3Server(t *testing.T) (*httptest.Server, *smokeS3State) {
	t.Helper()
	state := &smokeS3State{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if r.Method != http.MethodPut {
			http.Error(w, "unexpected S3 method: "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		state.putPath = r.URL.Path
		state.putBody = string(body)
		state.mu.Unlock()
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server, state
}

func writeSmokeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func waitForSmokeHealth(t *testing.T, ctx context.Context, client *http.Client, baseURL string, waitDone <-chan struct{}, waitErr func() error, stdout, stderr *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-waitDone:
			t.Fatalf("server process exited before health check: %v\nstdout:\n%s\nstderr:\n%s", waitErr(), stdout.String(), stderr.String())
		default:
		}
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("server process context ended before health check: %v\nstdout:\n%s\nstderr:\n%s", ctx.Err(), stdout.String(), stderr.String())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("server did not become healthy: %v\nstdout:\n%s\nstderr:\n%s", lastErr, stdout.String(), stderr.String())
}

func assertSmokeGET(t *testing.T, client *http.Client, url string, wantStatus int) *smokeResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new GET %s: %v", url, err)
	}
	return doSmokeRequest(t, client, req, wantStatus)
}

func smokeJSONRequest(t *testing.T, client *http.Client, method, url, bearer string, body any) *smokeResponse {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return doSmokeRequest(t, client, req, 0)
}

func doSmokeRequest(t *testing.T, client *http.Client, req *http.Request, wantStatus int) *smokeResponse {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if wantStatus != 0 && resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d, body = %s", req.Method, req.URL, resp.StatusCode, wantStatus, data)
	}
	return &smokeResponse{StatusCode: resp.StatusCode, Body: data}
}

func decodeSmokeJSON(t *testing.T, resp *smokeResponse, out any) {
	t.Helper()
	if err := json.Unmarshal(resp.Body, out); err != nil {
		t.Fatalf("decode JSON response %q: %v", string(resp.Body), err)
	}
}
