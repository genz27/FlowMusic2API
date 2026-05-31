package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
	"flowmusic2api/internal/storage"
	"flowmusic2api/internal/store"
)

func TestIsAuthFailureUsesStructuredHTTPStatus(t *testing.T) {
	if !isAuthFailure(&upstreamHTTPError{Operation: "test", StatusCode: http.StatusUnauthorized, Body: "expired"}) {
		t.Fatalf("401 upstream error should be treated as authentication failure")
	}
	if !isAuthFailure(fmt.Errorf("wrapped: %w", &upstreamHTTPError{Operation: "test", StatusCode: http.StatusForbidden, Body: "blocked"})) {
		t.Fatalf("wrapped 403 upstream error should be treated as authentication failure")
	}
	if isAuthFailure(&upstreamHTTPError{Operation: "test", StatusCode: http.StatusInternalServerError, Body: "forbidden by transient policy"}) {
		t.Fatalf("non-auth upstream status should not fall back only because body mentions forbidden")
	}
}

func TestFlowHeadersNormalizePastedBearerPrefix(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		writeTestJSON(t, w, map[string]any{"data": map[string]any{"credits_remaining": 12}})
	}))
	t.Cleanup(server.Close)

	client := NewFlowMusicClient(config.Config{FlowMusicBaseURL: server.URL, UpstreamTimeout: time.Second})
	if _, err := client.GetCredits(context.Background(), domain.Account{FlowBearer: " bearer flow-bearer "}); err != nil {
		t.Fatalf("GetCredits() error = %v", err)
	}
	if auth != "Bearer flow-bearer" {
		t.Fatalf("Authorization = %q, want Bearer flow-bearer", auth)
	}
}

func TestRefreshSupabaseUpdatesFlowBearer(t *testing.T) {
	var flowSaveCalled bool
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		flowSaveCalled = true
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/v1/token" || r.URL.Query().Get("grant_type") != "refresh_token" {
			t.Fatalf("unexpected Supabase request: %s", r.URL.String())
		}
		if r.Header.Get("apikey") != "anon-key" {
			t.Fatalf("missing anon key header")
		}
		writeTestJSON(t, w, map[string]any{
			"access_token":           "supabase-access",
			"refresh_token":          "new-refresh",
			"provider_token":         "google-access",
			"provider_refresh_token": "google-refresh",
			"expires_in":             3600,
			"user": map[string]any{
				"email": "user@example.test",
				"user_metadata": map[string]string{
					"name": "Test User",
				},
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL:  flowServer.URL,
		SupabaseBaseURL:   supabaseServer.URL,
		SupabaseAnonKey:   "anon-key",
		UpstreamTimeout:   time.Second,
		GenerationTimeout: time.Second,
	})

	updated, err := client.RefreshSupabase(context.Background(), domain.Account{RefreshToken: "old-refresh"})
	if err != nil {
		t.Fatalf("RefreshSupabase() error = %v", err)
	}
	if !flowSaveCalled {
		t.Fatalf("expected SaveGoogle to be called")
	}
	if updated.RefreshToken != "new-refresh" || updated.FlowBearer != "flow-bearer" || updated.AT != "supabase-access" || updated.AccessToken != "flow-bearer" {
		t.Fatalf("unexpected token fields: %+v", updated)
	}
	if updated.Email != "user@example.test" || updated.Name != "Test User" {
		t.Fatalf("unexpected user fields: %+v", updated)
	}
	if updated.ATExpires == nil || time.Until(*updated.ATExpires) <= 0 {
		t.Fatalf("expected future expiry, got %+v", updated.ATExpires)
	}
}

func TestRefreshSupabaseKeepsStoredProviderTokenWhenRefreshOmitsIt(t *testing.T) {
	var saveGoogleBody map[string]string
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&saveGoogleBody); err != nil {
			t.Fatalf("decode SaveGoogle body: %v", err)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"access_token":  "supabase-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(supabaseServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		SupabaseBaseURL:  supabaseServer.URL,
		SupabaseAnonKey:  "anon-key",
		UpstreamTimeout:  time.Second,
	})

	updated, err := client.RefreshSupabase(context.Background(), domain.Account{
		RefreshToken:          "old-refresh",
		ProviderToken:         "stored-google-access",
		ProviderRefreshToken:  "stored-google-refresh",
		FlowBearer:            "old-flow-bearer",
		AT:                    "old-flow-bearer",
		AutoRefreshEnabled:    true,
		RefreshIntervalMins:   60,
		CapabilityFlagsSet:    true,
		ImageEnabled:          true,
		VideoEnabled:          true,
		UpscaleEnabled:        true,
		ImageConcurrency:      1,
		VideoConcurrency:      1,
		ExplicitFields:        nil,
		ClearFields:           nil,
		ConsecutiveErrorCount: 0,
	})
	if err != nil {
		t.Fatalf("RefreshSupabase() error = %v", err)
	}
	if saveGoogleBody["access_token"] != "stored-google-access" || saveGoogleBody["refresh_token"] != "stored-google-refresh" {
		t.Fatalf("SaveGoogle body did not use stored provider credentials: %+v", saveGoogleBody)
	}
	if updated.RefreshToken != "new-refresh" ||
		updated.ProviderToken != "stored-google-access" ||
		updated.ProviderRefreshToken != "stored-google-refresh" ||
		updated.FlowBearer != "flow-bearer" {
		t.Fatalf("stored provider credentials were not preserved: %+v", updated)
	}
}

func TestRefreshSupabaseFailsWithoutFlowBearer(t *testing.T) {
	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"access_token":  "supabase-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(supabaseServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: "http://127.0.0.1",
		SupabaseBaseURL:  supabaseServer.URL,
		SupabaseAnonKey:  "anon-key",
		UpstreamTimeout:  time.Second,
	})

	updated, err := client.RefreshSupabase(context.Background(), domain.Account{RefreshToken: "old-refresh"})
	if err == nil {
		t.Fatalf("RefreshSupabase() error = nil, want missing provider_token error")
	}
	if updated.FlowBearer != "" || updated.AT != "supabase-access" || updated.AccessToken != "" {
		t.Fatalf("Supabase access token must not be treated as FlowMusic bearer: %+v", updated)
	}
}

func TestSaveGoogleAcceptsFlowBearerFallbackFields(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Fatalf("SaveGoogle should not send stale Authorization header: %q", auth)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"flow_bearer": "flow-bearer-fallback"},
		})
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	bearer, err := client.SaveGoogle(context.Background(), domain.Account{
		ProviderToken: "google-access",
		FlowBearer:    "stale-flow-bearer",
		AT:            "stale-flow-bearer",
	})
	if err != nil {
		t.Fatalf("SaveGoogle() error = %v", err)
	}
	if bearer != "flow-bearer-fallback" {
		t.Fatalf("SaveGoogle() = %q, want flow-bearer-fallback", bearer)
	}
}

func TestSaveGoogleAcceptsCamelCaseBearerFields(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"flowAccessToken": "flow-bearer-camel"},
		})
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	bearer, err := client.SaveGoogle(context.Background(), domain.Account{ProviderToken: "google-access"})
	if err != nil {
		t.Fatalf("SaveGoogle() error = %v", err)
	}
	if bearer != "flow-bearer-camel" {
		t.Fatalf("SaveGoogle() = %q, want flow-bearer-camel", bearer)
	}
}

func TestRefreshFromCookiesUpdatesFlowBearerViaProviderToken(t *testing.T) {
	var sessionCookie string
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/auth/session":
			if auth := r.Header.Get("Authorization"); auth != "" {
				t.Fatalf("session refresh should not send stale Authorization header: %q", auth)
			}
			sessionCookie = r.Header.Get("Cookie")
			writeTestJSON(t, w, map[string]any{
				"data": map[string]any{
					"provider_token":         "google-access",
					"provider_refresh_token": "google-refresh",
					"refresh_token":          "refresh-token",
					"expires_in":             3600,
					"user": map[string]string{
						"email": "cookie@example.test",
						"name":  "Cookie User",
					},
				},
			})
		case "/__api/auth/google/save":
			if auth := r.Header.Get("Authorization"); auth != "" {
				t.Fatalf("google save should not send stale Authorization header: %q", auth)
			}
			writeTestJSON(t, w, map[string]any{
				"data": map[string]string{"access_token": "flow-bearer-from-cookie"},
			})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	updated, err := client.RefreshFromCookies(context.Background(), domain.Account{
		ProtocolMode: "protocol",
		Cookies:      "session=value",
		FlowBearer:   "stale-flow-bearer",
	})
	if err != nil {
		t.Fatalf("RefreshFromCookies() error = %v", err)
	}
	if sessionCookie != "session=value" {
		t.Fatalf("Cookie header = %q, want session=value", sessionCookie)
	}
	if updated.FlowBearer != "flow-bearer-from-cookie" || updated.AT != "flow-bearer-from-cookie" || updated.AccessToken != "flow-bearer-from-cookie" {
		t.Fatalf("unexpected bearer fields: %+v", updated)
	}
	if updated.RefreshToken != "refresh-token" || updated.ProviderToken != "google-access" {
		t.Fatalf("unexpected session fields: %+v", updated)
	}
}

func TestRefreshFromCookiesAcceptsCamelCaseSessionAndNestedExpiry(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/auth/session":
			writeTestJSON(t, w, map[string]any{
				"data": map[string]any{
					"providerToken":        "google-access",
					"providerRefreshToken": "google-refresh",
					"refreshToken":         "refresh-token",
					"expiresIn":            3600,
					"user": map[string]string{
						"email": "camel-cookie@example.test",
					},
				},
			})
		case "/__api/auth/google/save":
			writeTestJSON(t, w, map[string]any{
				"data": map[string]string{"accessToken": "flow-bearer-camel-cookie"},
			})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	updated, err := client.RefreshFromCookies(context.Background(), domain.Account{
		ProtocolMode: "protocol",
		Cookies:      "session=value",
	})
	if err != nil {
		t.Fatalf("RefreshFromCookies() error = %v", err)
	}
	if updated.FlowBearer != "flow-bearer-camel-cookie" || updated.RefreshToken != "refresh-token" || updated.ProviderToken != "google-access" {
		t.Fatalf("unexpected refreshed account: %+v", updated)
	}
	if updated.ATExpires == nil || time.Until(*updated.ATExpires) <= 30*time.Minute {
		t.Fatalf("nested expiresIn was not persisted as a future bearer expiry: %+v", updated.ATExpires)
	}
}

func TestParseExpiresDoesNotTreatISOTimeAsUnixSeconds(t *testing.T) {
	want := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	got := parseExpires(map[string]any{"expires_at": want.Format(time.RFC3339)})
	if got == nil || !got.Equal(want) {
		t.Fatalf("parseExpires(ISO expires_at) = %v, want %v", got, want)
	}
}

func TestRefreshFromCookiesDoesNotUseSessionAccessTokenAsFlowBearer(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/session" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]any{
				"access_token": "supabase-session-token",
				"user": map[string]string{
					"email": "cookie@example.test",
				},
			},
		})
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	updated, err := client.RefreshFromCookies(context.Background(), domain.Account{
		ProtocolMode: "protocol",
		Cookies:      "session=value",
		FlowBearer:   "stale-flow-bearer",
	})
	if err == nil {
		t.Fatalf("RefreshFromCookies() error = nil, want missing FlowMusic bearer error")
	}
	if updated.FlowBearer == "supabase-session-token" || updated.AT == "supabase-session-token" || updated.AccessToken == "supabase-session-token" {
		t.Fatalf("session access_token must not become FlowMusic bearer: %+v", updated)
	}
}

func TestStreamMessagesHonorsIdleTimeout(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/messages/job-1/stream" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL:  flowServer.URL,
		UpstreamTimeout:   time.Second,
		StreamIdleTimeout: 50 * time.Millisecond,
	})
	_, err := client.StreamMessages(context.Background(), domain.Account{FlowBearer: "flow-bearer"}, "job-1")
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("StreamMessages() error = %v, want idle timeout", err)
	}
}

func TestStreamMessagesStopsOnDoneEvent(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/messages/job-1/stream" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL:  flowServer.URL,
		UpstreamTimeout:   time.Second,
		StreamIdleTimeout: time.Second,
	})
	_, err := client.StreamMessages(context.Background(), domain.Account{FlowBearer: "flow-bearer"}, "job-1")
	if err != nil {
		t.Fatalf("StreamMessages() error = %v, want nil after [DONE]", err)
	}
}

func TestBuildConversationRequestUsesStandardFlowMusicShape(t *testing.T) {
	req := BuildConversationRequest("write a warm city pop song", "lyria")
	if req.ModelName != "producer:standard" || req.Mode != "standard" {
		t.Fatalf("unexpected model/mode: %+v", req)
	}
	if len(req.Parts) != 1 || !strings.Contains(req.Parts[0].Content, "warm city pop music") || req.Parts[0].PartKind != "user-prompt" {
		t.Fatalf("unexpected parts: %+v", req.Parts)
	}
	if strings.Contains(req.Parts[0].Content, "audio__create_song") {
		t.Fatalf("conversation prompt should not expose tool internals, got %q", req.Parts[0].Content)
	}
	if !strings.HasPrefix(req.Parts[0].Content, "直接生成") {
		t.Fatalf("conversation prompt should be a direct generation request, got %q", req.Parts[0].Content)
	}
	if req.ClientContext.GhostwriterVersion != "standard" || req.ClientContext.LyricsIDMap == nil || req.ClientContext.SongQueue == nil {
		t.Fatalf("unexpected client context: %+v", req.ClientContext)
	}
}

func TestBuildConversationRequestCompactsDistractingPrompt(t *testing.T) {
	req := BuildConversationRequest("一首可爱猫猫电子流行歌曲，中文，轻快，副歌抓耳；必须直接生成音乐，不要只回复文字。", "lyria-fast")
	got := req.Parts[0].Content
	if got != "直接生成可爱猫猫电子流行音乐 中文 轻快 副歌抓耳" {
		t.Fatalf("compacted prompt = %q", got)
	}
	for _, forbidden := range []string{"必须", "不要", "回复文字", "audio__create_song", "dalle", "图片"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("compacted prompt leaked %q: %q", forbidden, got)
		}
	}
}

func TestBuildConversationRequestPreservesExplicitLyrics(t *testing.T) {
	lyrics := strings.Repeat("喵喵穿过霓虹街\n", 40)
	req := BuildConversationRequest("中文女声电子流行\n歌词：\n[Verse]\n"+lyrics+"[Chorus]\n一起去月光下跳舞", "lyria")
	got := req.Parts[0].Content
	if !strings.Contains(got, "歌词：\n[Verse]\n") || !strings.Contains(got, "\n[Chorus]\n一起去月光下跳舞") {
		t.Fatalf("explicit lyrics layout was not preserved: %q", got)
	}
	if strings.Contains(got, "必须") || strings.Contains(got, "不要只回复文字") {
		t.Fatalf("distracting instructions should still be removed: %q", got)
	}
	if len([]rune(got)) <= 220 {
		t.Fatalf("explicit lyrics prompt should not use the short prompt limit, got %d chars", len([]rune(got)))
	}
}

func TestBuildConversationRequestForcesImagePromptToMusic(t *testing.T) {
	req := BuildConversationRequest("A vibrant, high-energy anime-style album cover for an Electropop song titled Miao Miao Pop", "lyria")
	got := req.Parts[0].Content
	if !strings.HasPrefix(got, "直接生成") || !strings.Contains(got, "Electropop music") {
		t.Fatalf("unexpected music prompt: %q", got)
	}
	for _, forbidden := range []string{"album cover", "image", "picture", "poster", "dalle", "text2im"} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Fatalf("image prompt leaked %q: %q", forbidden, got)
		}
	}
}

func TestBuildConversationRequestMapsLyriaVariants(t *testing.T) {
	tests := []struct {
		model              string
		wantModelName      string
		wantMode           string
		wantGhostwriterVer string
	}{
		{model: "lyria", wantModelName: "producer:standard", wantMode: "standard", wantGhostwriterVer: "standard"},
		{model: "Lyria-fast", wantModelName: "producer:standard", wantMode: "standard", wantGhostwriterVer: "standard"},
		{model: "lyria-pro", wantModelName: "producer:standard", wantMode: "standard", wantGhostwriterVer: "pro"},
		{model: "lyria-pro-fast", wantModelName: "producer:standard", wantMode: "standard", wantGhostwriterVer: "pro"},
		{model: "flowmusic-producer-standard", wantModelName: "producer:standard", wantMode: "standard", wantGhostwriterVer: "standard"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			req := BuildConversationRequest("prompt", tt.model)
			if req.ModelName != tt.wantModelName || req.Mode != tt.wantMode || req.ClientContext.GhostwriterVersion != tt.wantGhostwriterVer {
				t.Fatalf("BuildConversationRequest(%q) model_name=%q mode=%q ghostwriter=%q, want %q %q %q",
					tt.model,
					req.ModelName,
					req.Mode,
					req.ClientContext.GhostwriterVersion,
					tt.wantModelName,
					tt.wantMode,
					tt.wantGhostwriterVer,
				)
			}
		})
	}
}

func TestConversationJobIDScopesGenericIDFallback(t *testing.T) {
	if got := conversationJobID(map[string]any{"data": map[string]any{"id": "job-data-id"}}); got != "job-data-id" {
		t.Fatalf("conversationJobID(data.id) = %q, want job-data-id", got)
	}
	got := conversationJobID(map[string]any{
		"data": map[string]any{
			"user": map[string]any{"id": "user-id"},
		},
	})
	if got != "" {
		t.Fatalf("conversationJobID nested user id = %q, want empty", got)
	}
}

func TestGetClipsParsesMediaURLs(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/clips" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"clips": map[string]any{
				"clip-1": map[string]string{
					"id":        "clip-1",
					"title":     "Song",
					"audio_url": "https://cdn.example.test/song.mp3",
					"wav_url":   "https://cdn.example.test/song.wav",
					"image_url": "https://cdn.example.test/song.jpg",
					"video_url": "https://cdn.example.test/song.mp4",
					"op_id":     "op-1",
					"op_type":   "audio__create_song",
				},
			},
		})
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	clips, err := client.GetClips(context.Background(), domain.Account{FlowBearer: "flow-bearer"}, []string{"clip-1"})
	if err != nil {
		t.Fatalf("GetClips() error = %v", err)
	}
	if len(clips) != 1 {
		t.Fatalf("clip count = %d, want 1", len(clips))
	}
	if clips[0].AudioURL == "" || clips[0].WavURL == "" || clips[0].ImageURL == "" || clips[0].VideoURL == "" {
		t.Fatalf("media URLs not parsed: %+v", clips[0])
	}
	if clips[0].OperationID != "op-1" || clips[0].OperationType != "audio__create_song" {
		t.Fatalf("operation metadata not parsed: %+v", clips[0])
	}
}

func TestGetClipsParsesLyricsAndMetadata(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/clips" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"clips": map[string]any{
				"clip-1": map[string]any{
					"id":        "clip-1",
					"title":     "Song",
					"audio_url": "https://cdn.example.test/song.m4a",
					"lyrics": map[string]any{
						"status": "completed",
						"value":  map[string]any{"id": "lyrics-1", "text": "hello lyrics"},
					},
					"duration":   map[string]any{"status": "completed", "value": "123.5"},
					"created_at": "2026-05-30T15:47:08Z",
					"operation": map[string]any{
						"op_type":      "audio__create_song",
						"sound_prompt": "warm city pop",
					},
				},
			},
		})
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	clips, err := client.GetClips(context.Background(), domain.Account{FlowBearer: "flow-bearer"}, []string{"clip-1"})
	if err != nil {
		t.Fatalf("GetClips() error = %v", err)
	}
	if len(clips) != 1 {
		t.Fatalf("clip count = %d, want 1", len(clips))
	}
	if clips[0].Lyrics != "hello lyrics" || clips[0].LyricsID != "lyrics-1" || clips[0].DurationSeconds != 123.5 || clips[0].SoundPrompt != "warm city pop" || clips[0].CreatedAt == "" {
		t.Fatalf("lyrics metadata not parsed: %+v", clips[0])
	}
}

func TestGetClipsParsesNestedMediaObjects(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/clips" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]any{
				"clips": []any{
					map[string]any{
						"clipId": "clip-1",
						"title":  "Nested Song",
						"audio":  map[string]any{"id": "audio-object-id", "url": "https://cdn.example.test/song.mp3"},
						"wav":    map[string]any{"download_url": "https://cdn.example.test/song.wav"},
						"image":  map[string]any{"src": "https://cdn.example.test/song.jpg"},
						"video":  []any{map[string]any{"url": "https://cdn.example.test/song.mp4"}},
					},
				},
			},
		})
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	clips, err := client.GetClips(context.Background(), domain.Account{FlowBearer: "flow-bearer"}, []string{"clip-1"})
	if err != nil {
		t.Fatalf("GetClips() error = %v", err)
	}
	if len(clips) != 1 {
		t.Fatalf("clip count = %d, want 1", len(clips))
	}
	if clips[0].ID != "clip-1" {
		t.Fatalf("clip id = %q, want clip-1", clips[0].ID)
	}
	if clips[0].AudioURL != "https://cdn.example.test/song.mp3" || clips[0].WavURL != "https://cdn.example.test/song.wav" || clips[0].ImageURL != "https://cdn.example.test/song.jpg" || clips[0].VideoURL != "https://cdn.example.test/song.mp4" {
		t.Fatalf("nested media URLs not parsed: %+v", clips[0])
	}
}

func TestGetClipsParsesAlternateMediaURLAliases(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/clips" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"clips": []any{
				map[string]any{
					"id":       "clip-1",
					"title":    "Alias Song",
					"mp3Url":   "https://cdn.example.test/song.mp3",
					"waveUrl":  "https://cdn.example.test/song.wav",
					"coverUrl": "https://cdn.example.test/song.jpg",
					"avi_url":  "https://cdn.example.test/song.avi",
				},
			},
		})
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	clips, err := client.GetClips(context.Background(), domain.Account{FlowBearer: "flow-bearer"}, []string{"clip-1"})
	if err != nil {
		t.Fatalf("GetClips() error = %v", err)
	}
	if len(clips) != 1 {
		t.Fatalf("clip count = %d, want 1", len(clips))
	}
	if clips[0].AudioURL != "https://cdn.example.test/song.mp3" || clips[0].WavURL != "https://cdn.example.test/song.wav" || clips[0].ImageURL != "https://cdn.example.test/song.jpg" || clips[0].VideoURL != "https://cdn.example.test/song.avi" {
		t.Fatalf("alternate media URLs not parsed: %+v", clips[0])
	}
}

func TestPollClipsWithProgressSuppressesRepeatedStatusErrors(t *testing.T) {
	var statusCalls int
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/audio-create-song-status/op-1" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		http.Error(w, `{"detail":"Operation not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(flowServer.Close)

	client := NewFlowMusicClient(config.Config{
		FlowMusicBaseURL: flowServer.URL,
		UpstreamTimeout:  time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.PollClipsWithProgress(ctx, domain.Account{FlowBearer: "flow-bearer"}, []string{"op-1", "op-1"}, time.Now().Add(time.Minute), func(status ClipPollStatus) {
		if status.Error != "" {
			statusCalls++
			if msg := status.ProgressMessage(); strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "Operation not found") {
				t.Fatalf("ProgressMessage leaked raw polling error: %q", msg)
			}
		}
	})
	if err == nil {
		t.Fatalf("PollClipsWithProgress() error = nil, want timeout after suppressed status error")
	}
	if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "Operation not found") {
		t.Fatalf("PollClipsWithProgress() leaked raw polling error: %v", err)
	}
	if statusCalls != 1 {
		t.Fatalf("status error callbacks = %d, want 1", statusCalls)
	}
}

func TestFindClipIDsIgnoresClipMetadata(t *testing.T) {
	ids := findClipIDs(map[string]any{
		"clip_status": "queued",
		"clip_type":   "audio",
		"clip_id":     "clip-main",
		"clips": []any{
			map[string]any{"id": "clip-nested", "clip_status": "ready"},
		},
	})
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got["clip-main"] || !got["clip-nested"] {
		t.Fatalf("clip ids = %#v, want clip-main and clip-nested", ids)
	}
	if got["queued"] || got["audio"] || got["ready"] {
		t.Fatalf("metadata values must not be clip ids: %#v", ids)
	}
}

func TestCollectIDsOnlyUsesExplicitOperationIDs(t *testing.T) {
	var result ConversationResult
	collectIDs(`{"job_id":"11111111-1111-1111-1111-111111111111","conversation_id":"22222222-2222-2222-2222-222222222222","id":"33333333-3333-3333-3333-333333333333"}`, &result)
	if len(result.OperationIDs) != 0 {
		t.Fatalf("conversation/job UUIDs must not be operation ids: %#v", result.OperationIDs)
	}

	collectIDs(`{"part":{"part_kind":"tool-return","tool_name":"audio__create_song","content":{"status":"success","operation_id":"op-a","operation_id_b":"op-b","clip_id":"clip-a","clip_id_b":"clip-b","a_b_test_id":"ab-test"}}}`, &result)
	if got, want := strings.Join(result.OperationIDs, ","), "op-a,op-b"; got != want {
		t.Fatalf("operation ids = %q, want %q", got, want)
	}
	if got, want := strings.Join(result.ClipIDs, ","), "clip-a,clip-b"; got != want {
		t.Fatalf("clip ids = %q, want %q", got, want)
	}
}

func TestAcquireRefreshesMissingBearerFromRefreshToken(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "fresh-flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"access_token":           "supabase-access",
			"refresh_token":          "fresh-refresh-token",
			"provider_token":         "google-access",
			"provider_refresh_token": "google-refresh",
			"expires_in":             3600,
			"user": map[string]any{
				"email": "acquire@example.test",
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
	db, err := store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(context.Background()); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	id, err := db.CreateAccount(context.Background(), domain.Account{
		Email:              "acquire@example.test",
		RefreshToken:       "old-refresh-token",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if account.ID != id || account.FlowBearer != "fresh-flow-bearer" || account.RefreshToken != "fresh-refresh-token" {
		t.Fatalf("Acquire() did not refresh missing bearer: %+v", account)
	}
	stored, err := db.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.FlowBearer != "fresh-flow-bearer" || stored.AT != "fresh-flow-bearer" {
		t.Fatalf("refreshed bearer was not persisted: %+v", stored)
	}
}

func TestAcquireRefreshesMissingBearerFromProviderToken(t *testing.T) {
	var saveGoogleCalled bool
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Fatalf("SaveGoogle should not send stale Authorization header: %q", auth)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode SaveGoogle body: %v", err)
		}
		if req["access_token"] != "google-access" || req["refresh_token"] != "google-refresh" {
			t.Fatalf("unexpected SaveGoogle body: %+v", req)
		}
		saveGoogleCalled = true
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "fresh-provider-flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
	db, err := store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(context.Background()); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	id, err := db.CreateAccount(context.Background(), domain.Account{
		Email:                "provider@example.test",
		ProtocolMode:         "refresh_token",
		ProviderToken:        "google-access",
		ProviderRefreshToken: "google-refresh",
		AutoRefreshEnabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !saveGoogleCalled {
		t.Fatalf("Acquire() did not call SaveGoogle for provider_token")
	}
	if account.ID != id || account.FlowBearer != "fresh-provider-flow-bearer" {
		t.Fatalf("Acquire() did not refresh provider_token bearer: %+v", account)
	}
	stored, err := db.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.FlowBearer != "fresh-provider-flow-bearer" || stored.AT != "fresh-provider-flow-bearer" || stored.LastRefreshResult != "provider_token_flow_bearer_success" {
		t.Fatalf("provider_token refresh was not persisted: %+v", stored)
	}
}

func TestAcquireRefreshesMissingBearerFromProviderRefreshToken(t *testing.T) {
	var oauthCalled bool
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Fatalf("unexpected OAuth path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" ||
			r.Form.Get("refresh_token") != "google-refresh" ||
			r.Form.Get("client_id") != "google-client-id" ||
			r.Form.Get("client_secret") != "google-client-secret" {
			t.Fatalf("unexpected OAuth form: %+v", r.Form)
		}
		oauthCalled = true
		writeTestJSON(t, w, map[string]any{
			"access_token":  "fresh-google-access",
			"refresh_token": "rotated-google-refresh",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(oauthServer.Close)

	var saveGoogleCalled bool
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode SaveGoogle body: %v", err)
		}
		if req["access_token"] != "fresh-google-access" || req["refresh_token"] != "rotated-google-refresh" {
			t.Fatalf("unexpected SaveGoogle body: %+v", req)
		}
		saveGoogleCalled = true
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "fresh-provider-refresh-flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:                 dir,
		DatabaseDriver:          "sqlite",
		DatabaseURL:             filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:        flowServer.URL,
		GoogleOAuthTokenURL:     oauthServer.URL + "/token",
		GoogleOAuthClientID:     "google-client-id",
		GoogleOAuthClientSecret: "google-client-secret",
		UpstreamTimeout:         time.Second,
		GenerationTimeout:       time.Second,
		TokenRefreshLead:        time.Minute,
		TokenRefreshInterval:    time.Hour,
		DefaultAdminUser:        "admin",
		DefaultAdminPassword:    "admin",
		DefaultAPIKey:           "test-api-key",
	}
	db, err := store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(context.Background()); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	id, err := db.CreateAccount(context.Background(), domain.Account{
		Email:                "provider-refresh@example.test",
		ProtocolMode:         "refresh_token",
		ProviderRefreshToken: "google-refresh",
		AutoRefreshEnabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !oauthCalled || !saveGoogleCalled {
		t.Fatalf("Acquire() did not use provider_refresh_token path: oauth=%v saveGoogle=%v", oauthCalled, saveGoogleCalled)
	}
	if account.ID != id || account.ProviderToken != "fresh-google-access" || account.ProviderRefreshToken != "rotated-google-refresh" || account.FlowBearer != "fresh-provider-refresh-flow-bearer" {
		t.Fatalf("Acquire() did not refresh provider credentials: %+v", account)
	}
	stored, err := db.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.ProviderToken != "fresh-google-access" ||
		stored.ProviderRefreshToken != "rotated-google-refresh" ||
		stored.FlowBearer != "fresh-provider-refresh-flow-bearer" ||
		stored.LastRefreshResult != "provider_refresh_token_google_refresh_and_flow_bearer_success" {
		t.Fatalf("provider_refresh_token refresh was not persisted: %+v", stored)
	}
}

func TestAcquireRetriesExpiredProviderTokenWithProviderRefreshToken(t *testing.T) {
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if r.Form.Get("refresh_token") != "google-refresh" {
			t.Fatalf("unexpected OAuth form: %+v", r.Form)
		}
		writeTestJSON(t, w, map[string]any{"access_token": "fresh-google-access"})
	}))
	t.Cleanup(oauthServer.Close)

	saveCalls := 0
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		saveCalls++
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode SaveGoogle body: %v", err)
		}
		if saveCalls == 1 {
			if req["access_token"] != "stale-google-access" {
				t.Fatalf("first SaveGoogle access_token = %q, want stale-google-access", req["access_token"])
			}
			http.Error(w, `{"detail":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if req["access_token"] != "fresh-google-access" {
			t.Fatalf("retry SaveGoogle access_token = %q, want fresh-google-access", req["access_token"])
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "fresh-provider-refresh-flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		GoogleOAuthTokenURL:  oauthServer.URL,
		GoogleOAuthClientID:  "google-client-id",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
	db, err := store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(context.Background()); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	id, err := db.CreateAccount(context.Background(), domain.Account{
		Email:                "expired-provider@example.test",
		ProtocolMode:         "refresh_token",
		ProviderToken:        "stale-google-access",
		ProviderRefreshToken: "google-refresh",
		AutoRefreshEnabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if saveCalls != 2 {
		t.Fatalf("SaveGoogle calls = %d, want 2", saveCalls)
	}
	if account.ID != id || account.ProviderToken != "fresh-google-access" || account.FlowBearer != "fresh-provider-refresh-flow-bearer" {
		t.Fatalf("Acquire() did not recover expired provider token: %+v", account)
	}
}

func TestAcquireUsesProviderRefreshReturnedBySupabaseWhenProviderTokenMissing(t *testing.T) {
	supabaseCalled := false
	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supabaseCalled = true
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode supabase refresh request: %v", err)
		}
		if req["refresh_token"] != "old-refresh-token" {
			t.Fatalf("supabase refresh_token = %q, want old-refresh-token", req["refresh_token"])
		}
		writeTestJSON(t, w, map[string]any{
			"access_token":           "supabase-access",
			"refresh_token":          "fresh-refresh-token",
			"provider_refresh_token": "google-refresh",
			"expires_in":             3600,
			"user": map[string]any{
				"email": "fallback@example.test",
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	oauthCalled := false
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthCalled = true
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "google-refresh" || r.Form.Get("client_id") != "google-client-id" {
			t.Fatalf("unexpected OAuth form: %+v", r.Form)
		}
		writeTestJSON(t, w, map[string]any{"access_token": "fresh-google-access"})
	}))
	t.Cleanup(oauthServer.Close)

	saveGoogleCalled := false
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode SaveGoogle body: %v", err)
		}
		if req["access_token"] != "fresh-google-access" || req["refresh_token"] != "google-refresh" {
			t.Fatalf("unexpected SaveGoogle body: %+v", req)
		}
		saveGoogleCalled = true
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "fresh-flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		GoogleOAuthTokenURL:  oauthServer.URL,
		GoogleOAuthClientID:  "google-client-id",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
	db, err := store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(context.Background()); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	id, err := db.CreateAccount(context.Background(), domain.Account{
		Email:              "fallback@example.test",
		ProtocolMode:       "refresh_token",
		RefreshToken:       "old-refresh-token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !supabaseCalled || !oauthCalled || !saveGoogleCalled {
		t.Fatalf("Acquire() did not use full refresh chain: supabase=%v oauth=%v saveGoogle=%v", supabaseCalled, oauthCalled, saveGoogleCalled)
	}
	if account.ID != id ||
		account.RefreshToken != "fresh-refresh-token" ||
		account.ProviderToken != "fresh-google-access" ||
		account.ProviderRefreshToken != "google-refresh" ||
		account.FlowBearer != "fresh-flow-bearer" {
		t.Fatalf("Acquire() did not preserve and refresh chained credentials: %+v", account)
	}
	stored, err := db.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.RefreshToken != "fresh-refresh-token" ||
		stored.ProviderToken != "fresh-google-access" ||
		stored.ProviderRefreshToken != "google-refresh" ||
		stored.FlowBearer != "fresh-flow-bearer" ||
		stored.LastRefreshResult != "provider_refresh_token_google_refresh_and_flow_bearer_success" {
		t.Fatalf("full refresh chain was not persisted: %+v", stored)
	}
}

func TestRefreshAccountPersistsPartialSupabaseRefreshOnProviderTokenMissing(t *testing.T) {
	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"access_token":  "supabase-access",
			"refresh_token": "rotated-refresh-token",
			"expires_in":    3600,
			"user": map[string]any{
				"email": "rotated@example.test",
				"user_metadata": map[string]any{
					"name": "Rotated User",
				},
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     "http://127.0.0.1:1",
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
	db, err := store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(context.Background()); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	id, err := db.CreateAccount(context.Background(), domain.Account{
		Email:              "old@example.test",
		ProtocolMode:       "refresh_token",
		RefreshToken:       "old-refresh-token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	_, err = accounts.RefreshAccount(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "provider_token is missing") {
		t.Fatalf("RefreshAccount() error = %v, want missing provider_token", err)
	}
	stored, err := db.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.RefreshToken != "rotated-refresh-token" || stored.Email != "rotated@example.test" || stored.Name != "Rotated User" {
		t.Fatalf("partial Supabase refresh fields were not persisted: %+v", stored)
	}
	if !strings.Contains(stored.LastRefreshResult, "provider_token is missing") {
		t.Fatalf("last refresh result = %q, want provider_token error", stored.LastRefreshResult)
	}
}

func TestRefreshGoogleProviderTokenValidatesClientAndAccessToken(t *testing.T) {
	client := NewFlowMusicClient(config.Config{GoogleOAuthTokenURL: "http://127.0.0.1:1/token"})
	_, err := client.RefreshGoogleProviderToken(context.Background(), domain.Account{ProviderRefreshToken: "google-refresh"})
	if err == nil || !strings.Contains(err.Error(), "FLOWMUSIC_GOOGLE_OAUTH_CLIENT_ID") {
		t.Fatalf("RefreshGoogleProviderToken() error = %v, want missing client id", err)
	}

	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{"expires_in": 3600})
	}))
	t.Cleanup(oauthServer.Close)
	client = NewFlowMusicClient(config.Config{
		GoogleOAuthTokenURL: oauthServer.URL,
		GoogleOAuthClientID: "google-client-id",
	})
	_, err = client.RefreshGoogleProviderToken(context.Background(), domain.Account{ProviderRefreshToken: "google-refresh"})
	if err == nil || !strings.Contains(err.Error(), "empty access_token") {
		t.Fatalf("RefreshGoogleProviderToken() error = %v, want empty access_token", err)
	}
}

func TestRefreshCreditsRefreshesMissingBearerBeforeBilling(t *testing.T) {
	ctx := context.Background()
	var saveGoogleCalled bool
	var creditsAuth string
	var subscriptionAuth string
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/auth/google/save":
			saveGoogleCalled = true
			if auth := r.Header.Get("Authorization"); auth != "" {
				t.Fatalf("SaveGoogle should not send stale Authorization header: %q", auth)
			}
			writeTestJSON(t, w, map[string]any{
				"data": map[string]string{"access_token": "fresh-flow-bearer"},
			})
		case "/__api/billing/credits":
			creditsAuth = r.Header.Get("Authorization")
			writeTestJSON(t, w, map[string]any{
				"data": map[string]any{
					"credits_remaining": 42,
					"tokens_remaining":  7.5,
				},
			})
		case "/__api/billing/subscription":
			subscriptionAuth = r.Header.Get("Authorization")
			writeTestJSON(t, w, map[string]any{
				"data": map[string]string{"subscription_tier": "PAYGATE_TIER_ONE"},
			})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"access_token":           "supabase-access",
			"refresh_token":          "fresh-refresh-token",
			"provider_token":         "google-access",
			"provider_refresh_token": "google-refresh",
			"expires_in":             3600,
			"user": map[string]any{
				"email": "credits@example.test",
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:              "credits@example.test",
		RefreshToken:       "old-refresh-token",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.RefreshCredits(ctx, id)
	if err != nil {
		t.Fatalf("RefreshCredits() error = %v", err)
	}
	if !saveGoogleCalled {
		t.Fatalf("RefreshCredits() should refresh missing bearer before billing")
	}
	if creditsAuth != "Bearer supabase-access" || subscriptionAuth != "Bearer supabase-access" {
		t.Fatalf("billing requests used wrong Authorization: credits=%q subscription=%q", creditsAuth, subscriptionAuth)
	}
	if account.FlowBearer != "fresh-flow-bearer" || account.RefreshToken != "fresh-refresh-token" {
		t.Fatalf("refreshed token fields were not persisted: %+v", account)
	}
	if account.Credits != 42 || account.TokensRemaining != 7.5 || account.SubscriptionTier != "PAYGATE_TIER_ONE" {
		t.Fatalf("billing fields were not persisted: %+v", account)
	}
}

func TestAcquireRespectsGlobalRefreshDisabled(t *testing.T) {
	ctx := context.Background()
	supabaseCalled := false
	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supabaseCalled = true
		http.Error(w, "should not refresh", http.StatusInternalServerError)
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     "http://127.0.0.1",
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	if err := db.UpdateTokenRefreshConfig(ctx, domain.TokenRefreshConfig{
		Enabled:               false,
		ATAutoRefreshEnabled:  false,
		RefreshIntervalMins:   60,
		RefreshBeforeExpiryMs: 600,
	}); err != nil {
		t.Fatalf("UpdateTokenRefreshConfig() error = %v", err)
	}
	if _, err := db.CreateAccount(ctx, domain.Account{
		Email:              "disabled-refresh@example.test",
		RefreshToken:       "refresh-token",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	_, err = accounts.Acquire(ctx)
	if err == nil || !strings.Contains(err.Error(), "no active FlowMusic account with usable Bearer token") {
		t.Fatalf("Acquire() error = %v, want no usable bearer without auto refresh", err)
	}
	if supabaseCalled {
		t.Fatalf("Acquire() should not refresh when global AT auto refresh is disabled")
	}
}

func TestAcquireUsesNearExpiryBearerWhenRefreshDisabled(t *testing.T) {
	ctx := context.Background()
	supabaseCalled := false
	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supabaseCalled = true
		http.Error(w, "should not refresh", http.StatusInternalServerError)
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     "http://127.0.0.1",
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     10 * time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	if err := db.UpdateTokenRefreshConfig(ctx, domain.TokenRefreshConfig{
		Enabled:               false,
		ATAutoRefreshEnabled:  false,
		RefreshIntervalMins:   60,
		RefreshBeforeExpiryMs: 600,
	}); err != nil {
		t.Fatalf("UpdateTokenRefreshConfig() error = %v", err)
	}
	expiresSoon := time.Now().UTC().Add(5 * time.Minute)
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:              "near-expiry-refresh-disabled@example.test",
		RefreshToken:       "refresh-token",
		FlowBearer:         "near-expiry-flow-bearer",
		ATExpires:          &expiresSoon,
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if account.ID != id || account.FlowBearer != "near-expiry-flow-bearer" {
		t.Fatalf("Acquire() should use current bearer when refresh is disabled: %+v", account)
	}
	if supabaseCalled {
		t.Fatalf("Acquire() should not refresh when global AT auto refresh is disabled")
	}
}

func TestAcquireUsesPersistedRefreshLead(t *testing.T) {
	ctx := context.Background()
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "fresh-flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"access_token":           "supabase-access",
			"refresh_token":          "fresh-refresh-token",
			"provider_token":         "google-access",
			"provider_refresh_token": "google-refresh",
			"expires_in":             3600,
			"user": map[string]any{
				"email": "lead@example.test",
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	if err := db.UpdateTokenRefreshConfig(ctx, domain.TokenRefreshConfig{
		Enabled:               true,
		ATAutoRefreshEnabled:  true,
		RefreshIntervalMins:   60,
		RefreshBeforeExpiryMs: 600,
	}); err != nil {
		t.Fatalf("UpdateTokenRefreshConfig() error = %v", err)
	}
	expiresSoon := time.Now().UTC().Add(5 * time.Minute)
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:              "lead@example.test",
		RefreshToken:       "old-refresh-token",
		FlowBearer:         "stale-flow-bearer",
		ATExpires:          &expiresSoon,
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if account.ID != id || account.FlowBearer != "fresh-flow-bearer" {
		t.Fatalf("Acquire() did not use persisted refresh lead: %+v", account)
	}
}

func TestAcquireRefreshesBearerProtocolBeforeExpiryWhenRefreshCredentialsExist(t *testing.T) {
	ctx := context.Background()
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "fresh-bearer-mode-flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"access_token":           "supabase-access",
			"refresh_token":          "fresh-bearer-mode-refresh-token",
			"provider_token":         "google-access",
			"provider_refresh_token": "google-refresh",
			"expires_in":             3600,
			"user": map[string]any{
				"email": "bearer-mode-refresh@example.test",
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	if err := db.UpdateTokenRefreshConfig(ctx, domain.TokenRefreshConfig{
		Enabled:               true,
		ATAutoRefreshEnabled:  true,
		RefreshIntervalMins:   60,
		RefreshBeforeExpiryMs: 600,
	}); err != nil {
		t.Fatalf("UpdateTokenRefreshConfig() error = %v", err)
	}
	expiresSoon := time.Now().UTC().Add(5 * time.Minute)
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:              "bearer-mode-refresh@example.test",
		RefreshToken:       "old-bearer-mode-refresh-token",
		FlowBearer:         "stale-bearer-mode-flow-bearer",
		ATExpires:          &expiresSoon,
		ProtocolMode:       "bearer",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if account.ID != id || account.FlowBearer != "fresh-bearer-mode-flow-bearer" || account.RefreshToken != "fresh-bearer-mode-refresh-token" {
		t.Fatalf("bearer protocol account was not refreshed before expiry: %+v", account)
	}
	stored, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.FlowBearer != "fresh-bearer-mode-flow-bearer" || stored.LastRefreshAt == nil {
		t.Fatalf("refreshed bearer protocol account was not persisted: %+v", stored)
	}
}

func TestAcquireUsesExistingBearerWhenRefreshTokenFails(t *testing.T) {
	ctx := context.Background()
	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refresh token expired", http.StatusUnauthorized)
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     "http://127.0.0.1",
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:              "stale-refresh-valid-bearer@example.test",
		RefreshToken:       "expired-refresh-token",
		FlowBearer:         "still-current-flow-bearer",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if account.ID != id || account.FlowBearer != "still-current-flow-bearer" {
		t.Fatalf("Acquire() should fall back to existing current bearer: %+v", account)
	}
	stored, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if stored.LastRefreshAt == nil || !strings.Contains(strings.ToLower(stored.LastRefreshResult), "http 401") {
		t.Fatalf("refresh failure should be recorded while existing bearer is used: %+v", stored)
	}
}

func TestAcquireSkipsExpiredBearerWhenRefreshTokenFails(t *testing.T) {
	ctx := context.Background()
	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refresh token expired", http.StatusUnauthorized)
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     "http://127.0.0.1",
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	expired := time.Now().UTC().Add(-time.Minute)
	badID, err := db.CreateAccount(ctx, domain.Account{
		Email:              "expired-bearer@example.test",
		RefreshToken:       "expired-refresh-token",
		FlowBearer:         "expired-flow-bearer",
		ATExpires:          &expired,
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount(bad) error = %v", err)
	}
	goodID, err := db.CreateAccount(ctx, domain.Account{
		Email:        "usable-bearer@example.test",
		FlowBearer:   "usable-flow-bearer",
		ProtocolMode: "bearer",
	})
	if err != nil {
		t.Fatalf("CreateAccount(good) error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if account.ID != goodID || account.ID == badID || account.FlowBearer != "usable-flow-bearer" {
		t.Fatalf("Acquire() should skip expired bearer after failed refresh: %+v", account)
	}
}

func TestAcquireRefreshesUnknownExpiryBearerWhenIntervalDue(t *testing.T) {
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "fresh-flow-bearer"},
		})
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(t, w, map[string]any{
			"access_token":           "supabase-access",
			"refresh_token":          "fresh-refresh-token",
			"provider_token":         "google-access",
			"provider_refresh_token": "google-refresh",
			"expires_in":             3600,
			"user": map[string]any{
				"email": "unknown-expiry@example.test",
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
	db, err := store.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(context.Background()); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	id, err := db.CreateAccount(context.Background(), domain.Account{
		Email:              "unknown-expiry@example.test",
		RefreshToken:       "old-refresh-token",
		FlowBearer:         "stale-flow-bearer",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if account.ID != id || account.FlowBearer != "fresh-flow-bearer" || account.RefreshToken != "fresh-refresh-token" {
		t.Fatalf("Acquire() did not refresh stale unknown-expiry bearer: %+v", account)
	}
}

func TestAcquireHonorsCallLogicMode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     "http://127.0.0.1",
		SupabaseBaseURL:      "http://127.0.0.1",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	highErrorID, err := db.CreateAccount(ctx, domain.Account{Email: "high-error@example.test", ProtocolMode: "bearer", FlowBearer: "high-error-bearer"})
	if err != nil {
		t.Fatalf("CreateAccount(high error) error = %v", err)
	}
	lowErrorID, err := db.CreateAccount(ctx, domain.Account{Email: "low-error@example.test", ProtocolMode: "bearer", FlowBearer: "low-error-bearer"})
	if err != nil {
		t.Fatalf("CreateAccount(low error) error = %v", err)
	}
	if err := db.UpdateAccountFields(ctx, highErrorID, map[string]any{"error_count": 5, "consecutive_error_count": 2}); err != nil {
		t.Fatalf("UpdateAccountFields() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire(default) error = %v", err)
	}
	if account.ID != lowErrorID {
		t.Fatalf("default call mode should prefer lower-error account: got %d, want %d", account.ID, lowErrorID)
	}

	if err := db.UpdateCallLogicConfig(ctx, domain.CallLogicConfig{CallMode: "polling"}); err != nil {
		t.Fatalf("UpdateCallLogicConfig() error = %v", err)
	}
	account, err = accounts.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire(polling) error = %v", err)
	}
	if account.ID != highErrorID {
		t.Fatalf("polling call mode should keep last-used/id order: got %d, want %d", account.ID, highErrorID)
	}
}

func TestAcquireBearerModeFallsBackToUsableBearerWhenRefreshFails(t *testing.T) {
	ctx := context.Background()
	supabaseCalled := false
	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supabaseCalled = true
		http.Error(w, "refresh token expired", http.StatusUnauthorized)
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     "http://127.0.0.1",
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:              "bearer-preserved-rt@example.test",
		ProtocolMode:       "bearer",
		RefreshToken:       "expired-refresh-token",
		FlowBearer:         "usable-flow-bearer",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if account.ID != id || account.FlowBearer != "usable-flow-bearer" {
		t.Fatalf("unexpected acquired account: %+v", account)
	}
	if !supabaseCalled {
		t.Fatalf("bearer mode with refresh credentials should attempt refresh before using stale/unknown-expiry bearer")
	}
}

func TestProtocolRefreshPrefersCookiesOverStoredRefreshToken(t *testing.T) {
	ctx := context.Background()
	var sessionCalled bool
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/auth/session":
			sessionCalled = true
			writeTestJSON(t, w, map[string]any{
				"data": map[string]any{
					"provider_token":         "fresh-google-access",
					"provider_refresh_token": "fresh-google-refresh",
					"refresh_token":          "cookie-refresh-token",
					"expires_in":             3600,
					"user": map[string]string{
						"email": "protocol@example.test",
					},
				},
			})
		case "/__api/auth/google/save":
			writeTestJSON(t, w, map[string]any{
				"data": map[string]string{"access_token": "fresh-cookie-flow-bearer"},
			})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		SupabaseBaseURL:      "http://127.0.0.1:1",
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:              "protocol@example.test",
		ProtocolMode:       "protocol",
		Cookies:            "session=value",
		RefreshToken:       "old-refresh-token",
		FlowBearer:         "old-flow-bearer",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	account, err := accounts.RefreshAccount(ctx, id)
	if err != nil {
		t.Fatalf("RefreshAccount() error = %v", err)
	}
	if !sessionCalled {
		t.Fatalf("protocol account should refresh via cookie session")
	}
	if account.FlowBearer != "fresh-cookie-flow-bearer" || account.RefreshToken != "cookie-refresh-token" {
		t.Fatalf("unexpected refreshed protocol account: %+v", account)
	}
}

func TestRunRefreshOnceRefreshesDueAccountsAndSkipsOthers(t *testing.T) {
	ctx := context.Background()
	var refreshedTokensMu sync.Mutex
	refreshedTokens := []string{}
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__api/auth/google/save" {
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode google save request: %v", err)
		}
		writeTestJSON(t, w, map[string]any{
			"data": map[string]string{"access_token": "flow-bearer-" + req["access_token"]},
		})
	}))
	t.Cleanup(flowServer.Close)

	supabaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode supabase request: %v", err)
		}
		refreshToken := req["refresh_token"]
		refreshedTokensMu.Lock()
		refreshedTokens = append(refreshedTokens, refreshToken)
		refreshedTokensMu.Unlock()
		writeTestJSON(t, w, map[string]any{
			"refresh_token":          "new-" + refreshToken,
			"provider_token":         "provider-" + refreshToken,
			"provider_refresh_token": "provider-refresh-" + refreshToken,
			"expires_in":             3600,
			"user": map[string]any{
				"email": refreshToken + "@example.test",
			},
		})
	}))
	t.Cleanup(supabaseServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		SupabaseBaseURL:      supabaseServer.URL,
		SupabaseAnonKey:      "anon-key",
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     10 * time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	if err := db.UpdateTokenRefreshConfig(ctx, domain.TokenRefreshConfig{
		Enabled:               true,
		ATAutoRefreshEnabled:  true,
		RefreshIntervalMins:   60,
		RefreshBeforeExpiryMs: 600,
	}); err != nil {
		t.Fatalf("UpdateTokenRefreshConfig() error = %v", err)
	}
	dueID, err := db.CreateAccount(ctx, domain.Account{
		Email:              "due@example.test",
		RefreshToken:       "due-refresh",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount(due) error = %v", err)
	}
	future := time.Now().UTC().Add(time.Hour)
	notDueID, err := db.CreateAccount(ctx, domain.Account{
		Email:              "not-due@example.test",
		RefreshToken:       "not-due-refresh",
		FlowBearer:         "still-current-bearer",
		ATExpires:          &future,
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount(not due) error = %v", err)
	}
	manualID, err := db.CreateAccount(ctx, domain.Account{
		Email:              "manual@example.test",
		RefreshToken:       "manual-refresh",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: false,
	})
	if err != nil {
		t.Fatalf("CreateAccount(manual) error = %v", err)
	}
	inactiveID, err := db.CreateAccount(ctx, domain.Account{
		Email:              "inactive@example.test",
		RefreshToken:       "inactive-refresh",
		ProtocolMode:       "refresh_token",
		AutoRefreshEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateAccount(inactive) error = %v", err)
	}
	if err := db.SetAccountActive(ctx, inactiveID, false); err != nil {
		t.Fatalf("SetAccountActive() error = %v", err)
	}

	accounts := NewAccountService(cfg, db, NewFlowMusicClient(cfg))
	accounts.RunRefreshOnce(ctx)

	refreshedTokensMu.Lock()
	gotRefreshes := append([]string(nil), refreshedTokens...)
	refreshedTokensMu.Unlock()
	if len(gotRefreshes) != 1 || gotRefreshes[0] != "due-refresh" {
		t.Fatalf("RunRefreshOnce refreshed tokens = %#v, want only due-refresh", gotRefreshes)
	}
	due, err := db.GetAccount(ctx, dueID)
	if err != nil {
		t.Fatalf("GetAccount(due) error = %v", err)
	}
	if due.FlowBearer != "flow-bearer-provider-due-refresh" || due.RefreshToken != "new-due-refresh" || due.LastRefreshAt == nil {
		t.Fatalf("due account was not refreshed correctly: %+v", due)
	}
	notDue, err := db.GetAccount(ctx, notDueID)
	if err != nil {
		t.Fatalf("GetAccount(not due) error = %v", err)
	}
	if notDue.FlowBearer != "still-current-bearer" || notDue.RefreshToken != "not-due-refresh" || notDue.LastRefreshAt != nil {
		t.Fatalf("not-due account should be unchanged: %+v", notDue)
	}
	manual, err := db.GetAccount(ctx, manualID)
	if err != nil {
		t.Fatalf("GetAccount(manual) error = %v", err)
	}
	if manual.FlowBearer != "" || manual.LastRefreshAt != nil {
		t.Fatalf("manual-refresh disabled account should be unchanged: %+v", manual)
	}
	inactive, err := db.GetAccount(ctx, inactiveID)
	if err != nil {
		t.Fatalf("GetAccount(inactive) error = %v", err)
	}
	if inactive.FlowBearer != "" || inactive.LastRefreshAt != nil || inactive.IsActive {
		t.Fatalf("inactive account should be unchanged and inactive: %+v", inactive)
	}
}

func TestConcurrentGenerateLeasesDifferentAccounts(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	auths := make([]string, 0, 2)
	bothConversations := make(chan struct{})
	closed := false
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/conversation":
			auth := r.Header.Get("Authorization")
			mu.Lock()
			auths = append(auths, auth)
			if len(auths) == 2 && !closed {
				close(bothConversations)
				closed = true
			}
			mu.Unlock()
			select {
			case <-bothConversations:
			case <-time.After(2 * time.Second):
				http.Error(w, "timed out waiting for concurrent conversations", http.StatusGatewayTimeout)
				return
			}
			switch auth {
			case "Bearer bearer-1":
				writeTestJSON(t, w, map[string]string{"job_id": "job-1"})
			case "Bearer bearer-2":
				writeTestJSON(t, w, map[string]string{"job_id": "job-2"})
			default:
				t.Fatalf("unexpected Authorization header: %q", auth)
			}
		case "/__api/messages/job-1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"clip_id":"11111111-1111-1111-1111-111111111111"}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/__api/messages/job-2/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"clip_id":"22222222-2222-2222-2222-222222222222"}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/__api/clips":
			var req struct {
				ClipIDs []string `json:"clip_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode clips request: %v", err)
			}
			clips := map[string]any{}
			for _, id := range req.ClipIDs {
				clips[id] = map[string]string{
					"id":        id,
					"title":     "Concurrent Song",
					"audio_url": "https://cdn.example.test/" + id + ".mp3",
				}
			}
			writeTestJSON(t, w, map[string]any{"clips": clips})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		CacheDir:             filepath.Join(dir, "tmp"),
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		UpstreamTimeout:      3 * time.Second,
		GenerationTimeout:    3 * time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	for _, account := range []domain.Account{
		{Email: "lease-1@example.test", ProtocolMode: "bearer", FlowBearer: "bearer-1"},
		{Email: "lease-2@example.test", ProtocolMode: "bearer", FlowBearer: "bearer-2"},
	} {
		if _, err := db.CreateAccount(ctx, account); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	flow := NewFlowMusicClient(cfg)
	accounts := NewAccountService(cfg, db, flow)
	cache := storage.NewCache(cfg, db, NewHTTPClient(cfg, ""))
	generation := NewGenerationService(cfg, db, accounts, flow, cache)
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			out, err := generation.Generate(ctx, "prompt", "flowmusic-producer-standard")
			if err != nil {
				errCh <- err
				return
			}
			if len(out.Clips) != 1 || out.Clips[0].Audio.URL == "" {
				errCh <- fmt.Errorf("unexpected generation output: %+v", out)
				return
			}
			errCh <- nil
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 {
		t.Fatalf("conversation calls = %d, want 2 (%#v)", len(auths), auths)
	}
	seen := map[string]bool{}
	for _, auth := range auths {
		seen[auth] = true
	}
	if !seen["Bearer bearer-1"] || !seen["Bearer bearer-2"] {
		t.Fatalf("concurrent generations should use distinct accounts, got %#v", auths)
	}
}

func TestGenerateRetriesNextAccountOnStartAuthFailure(t *testing.T) {
	ctx := context.Background()
	clipID := "22222222-2222-2222-2222-222222222222"
	var badConversationCalls int
	var goodConversationCalls int
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/conversation":
			switch r.Header.Get("Authorization") {
			case "Bearer bad-bearer":
				badConversationCalls++
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			case "Bearer good-bearer":
				goodConversationCalls++
				writeTestJSON(t, w, map[string]string{"job_id": "job-good"})
			default:
				t.Fatalf("unexpected Authorization header: %q", r.Header.Get("Authorization"))
			}
		case "/__api/messages/job-good/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"clip_id":"` + clipID + `"}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/__api/clips":
			writeTestJSON(t, w, map[string]any{
				"clips": map[string]any{
					clipID: map[string]string{
						"id":        clipID,
						"title":     "Fallback Song",
						"audio_url": "https://cdn.example.test/fallback.mp3",
					},
				},
			})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		CacheDir:             filepath.Join(dir, "tmp"),
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	badID, err := db.CreateAccount(ctx, domain.Account{
		Email:        "bad-start@example.test",
		ProtocolMode: "bearer",
		FlowBearer:   "bad-bearer",
	})
	if err != nil {
		t.Fatalf("CreateAccount(bad) error = %v", err)
	}
	goodID, err := db.CreateAccount(ctx, domain.Account{
		Email:        "good-start@example.test",
		ProtocolMode: "bearer",
		FlowBearer:   "good-bearer",
	})
	if err != nil {
		t.Fatalf("CreateAccount(good) error = %v", err)
	}

	flow := NewFlowMusicClient(cfg)
	accounts := NewAccountService(cfg, db, flow)
	cache := storage.NewCache(cfg, db, NewHTTPClient(cfg, ""))
	generation := NewGenerationService(cfg, db, accounts, flow, cache)
	out, err := generation.Generate(ctx, "prompt", "flowmusic-producer-standard")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if out.AccountID != goodID || len(out.Clips) != 1 || out.Clips[0].Audio.URL != "https://cdn.example.test/fallback.mp3" {
		t.Fatalf("Generate() did not finish with fallback account: %+v", out)
	}
	if badConversationCalls != 1 || goodConversationCalls != 1 {
		t.Fatalf("unexpected conversation call counts: bad=%d good=%d", badConversationCalls, goodConversationCalls)
	}
	badAccount, err := db.GetAccount(ctx, badID)
	if err != nil {
		t.Fatalf("GetAccount(bad) error = %v", err)
	}
	if badAccount.ErrorCount != 1 || badAccount.ConsecutiveErrorCount != 1 {
		t.Fatalf("bad account auth failure should be recorded: %+v", badAccount)
	}
	if badAccount.UseCount != 1 || badAccount.LastUsedAt == nil {
		t.Fatalf("bad account auth failure should touch usage: %+v", badAccount)
	}
	goodAccount, err := db.GetAccount(ctx, goodID)
	if err != nil {
		t.Fatalf("GetAccount(good) error = %v", err)
	}
	if goodAccount.UseCount != 1 || goodAccount.LastUsedAt == nil {
		t.Fatalf("good account success should touch usage: %+v", goodAccount)
	}
}

func TestGenerateRetriesNextAccountOnPostStartAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failPath  string
		wantCalls int
	}{
		{name: "stream", failPath: "stream", wantCalls: 1},
		{name: "poll", failPath: "poll", wantCalls: 1},
		{name: "clips", failPath: "clips", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			badClipID := "33333333-3333-3333-3333-333333333333"
			goodClipID := "44444444-4444-4444-4444-444444444444"
			operationID := "55555555-5555-5555-5555-555555555555"
			var authFailureCalls int
			var goodConversationCalls int
			flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				switch r.URL.Path {
				case "/__api/conversation":
					switch auth {
					case "Bearer bad-bearer":
						writeTestJSON(t, w, map[string]string{"job_id": "job-bad"})
					case "Bearer good-bearer":
						goodConversationCalls++
						writeTestJSON(t, w, map[string]string{"job_id": "job-good"})
					default:
						t.Fatalf("unexpected Authorization header: %q", auth)
					}
				case "/__api/messages/job-bad/stream":
					if tc.failPath == "stream" {
						authFailureCalls++
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.failPath == "poll" {
						_, _ = w.Write([]byte(`data: {"operation_id":"` + operationID + `"}` + "\n\n"))
					} else {
						_, _ = w.Write([]byte(`data: {"clip_id":"` + badClipID + `"}` + "\n\n"))
					}
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
				case "/__api/audio-create-song-status/" + operationID:
					authFailureCalls++
					http.Error(w, "forbidden", http.StatusForbidden)
				case "/__api/audio-create-song-status/job-bad":
					writeTestJSON(t, w, map[string]any{"data": map[string]any{}})
				case "/__api/messages/job-good/stream":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(`data: {"clip_id":"` + goodClipID + `"}` + "\n\n"))
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
				case "/__api/clips":
					var clipsReq struct {
						ClipIDs []string `json:"clip_ids"`
					}
					if err := json.NewDecoder(r.Body).Decode(&clipsReq); err != nil {
						t.Fatalf("decode clips request: %v", err)
					}
					if auth == "Bearer bad-bearer" && tc.failPath == "clips" {
						if len(clipsReq.ClipIDs) != 1 || clipsReq.ClipIDs[0] != badClipID {
							t.Fatalf("bad bearer clips request used unexpected clip_ids: %+v", clipsReq.ClipIDs)
						}
						authFailureCalls++
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					if len(clipsReq.ClipIDs) != 1 || clipsReq.ClipIDs[0] != goodClipID {
						t.Fatalf("good bearer clips request used unexpected clip_ids: %+v", clipsReq.ClipIDs)
					}
					writeTestJSON(t, w, map[string]any{
						"clips": map[string]any{
							goodClipID: map[string]string{
								"id":        goodClipID,
								"title":     "Fallback Song",
								"audio_url": "https://cdn.example.test/fallback.mp3",
							},
						},
					})
				default:
					t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
				}
			}))
			t.Cleanup(flowServer.Close)

			dir := t.TempDir()
			cfg := config.Config{
				DataDir:              dir,
				CacheDir:             filepath.Join(dir, "tmp"),
				DatabaseDriver:       "sqlite",
				DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
				FlowMusicBaseURL:     flowServer.URL,
				UpstreamTimeout:      time.Second,
				GenerationTimeout:    3 * time.Second,
				TokenRefreshLead:     time.Minute,
				TokenRefreshInterval: time.Hour,
				DefaultAdminUser:     "admin",
				DefaultAdminPassword: "admin",
				DefaultAPIKey:        "test-api-key",
			}
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
			badID, err := db.CreateAccount(ctx, domain.Account{
				Email:        "bad-" + tc.name + "@example.test",
				ProtocolMode: "bearer",
				FlowBearer:   "bad-bearer",
			})
			if err != nil {
				t.Fatalf("CreateAccount(bad) error = %v", err)
			}
			goodID, err := db.CreateAccount(ctx, domain.Account{
				Email:        "good-" + tc.name + "@example.test",
				ProtocolMode: "bearer",
				FlowBearer:   "good-bearer",
			})
			if err != nil {
				t.Fatalf("CreateAccount(good) error = %v", err)
			}

			flow := NewFlowMusicClient(cfg)
			accounts := NewAccountService(cfg, db, flow)
			cache := storage.NewCache(cfg, db, NewHTTPClient(cfg, ""))
			generation := NewGenerationService(cfg, db, accounts, flow, cache)
			out, err := generation.Generate(ctx, "prompt", "flowmusic-producer-standard")
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			if out.AccountID != goodID || len(out.Clips) != 1 || out.Clips[0].Audio.URL != "https://cdn.example.test/fallback.mp3" {
				t.Fatalf("Generate() did not finish with fallback account: %+v", out)
			}
			if authFailureCalls != tc.wantCalls || goodConversationCalls != 1 {
				t.Fatalf("unexpected call counts: authFailure=%d goodConversation=%d", authFailureCalls, goodConversationCalls)
			}
			badAccount, err := db.GetAccount(ctx, badID)
			if err != nil {
				t.Fatalf("GetAccount(bad) error = %v", err)
			}
			if badAccount.ErrorCount != 1 || badAccount.ConsecutiveErrorCount != 1 {
				t.Fatalf("bad account auth failure should be recorded: %+v", badAccount)
			}
			if badAccount.UseCount != 1 || badAccount.LastUsedAt == nil {
				t.Fatalf("bad account auth failure should touch usage: %+v", badAccount)
			}
			goodAccount, err := db.GetAccount(ctx, goodID)
			if err != nil {
				t.Fatalf("GetAccount(good) error = %v", err)
			}
			if goodAccount.UseCount != 1 || goodAccount.LastUsedAt == nil {
				t.Fatalf("good account success should touch usage: %+v", goodAccount)
			}
		})
	}
}

func TestGenerateFailsFastWhenStreamOnlyChats(t *testing.T) {
	ctx := context.Background()
	var sawPrompt string
	var polledStatus bool
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/conversation":
			var req ConversationRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode conversation request: %v", err)
			}
			if len(req.Parts) != 1 {
				t.Fatalf("unexpected conversation parts: %+v", req.Parts)
			}
			sawPrompt = req.Parts[0].Content
			writeTestJSON(t, w, map[string]string{"job_id": "job-chat"})
		case "/__api/messages/job-chat/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: conversation_id\n"))
			_, _ = w.Write([]byte(`data: {"id":"22222222-2222-2222-2222-222222222222"}` + "\n\n"))
			_, _ = w.Write([]byte("event: part\n"))
			_, _ = w.Write([]byte(`data: {"index":2,"status":"final","part":{"content":"请告诉我想要什么风格，我再给你建议。","part_kind":"text"},"delta":""}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			if strings.HasPrefix(r.URL.Path, "/__api/audio-create-song-status/") {
				polledStatus = true
				http.Error(w, "must not poll job or conversation ids", http.StatusNotFound)
				return
			}
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		CacheDir:             filepath.Join(dir, "tmp"),
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	if _, err := db.CreateAccount(ctx, domain.Account{
		Email:        "chat-only@example.test",
		ProtocolMode: "bearer",
		FlowBearer:   "flow-bearer",
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	flow := NewFlowMusicClient(cfg)
	accounts := NewAccountService(cfg, db, flow)
	cache := storage.NewCache(cfg, db, NewHTTPClient(cfg, ""))
	generation := NewGenerationService(cfg, db, accounts, flow, cache)
	_, err = generation.Generate(ctx, "猫猫音乐", "flowmusic-producer-standard")
	if err == nil || !strings.Contains(err.Error(), "未调用音乐生成工具") || strings.Contains(err.Error(), "请告诉我想要什么风格") {
		t.Fatalf("Generate() error = %v, want concise chat-only error", err)
	}
	if polledStatus {
		t.Fatalf("Generate() must not poll job_id or conversation_id when no operation_id was returned")
	}
	if sawPrompt != "直接生成猫猫音乐" {
		t.Fatalf("conversation prompt = %q, want direct generation prompt", sawPrompt)
	}
}

func TestGenerateReturnsMultipleClips(t *testing.T) {
	ctx := context.Background()
	clipA := "11111111-1111-1111-1111-111111111111"
	clipB := "22222222-2222-2222-2222-222222222222"
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/conversation":
			writeTestJSON(t, w, map[string]string{"job_id": "job-1"})
		case "/__api/messages/job-1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"clip_ids":["` + clipA + `","` + clipB + `"]}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case "/__api/clips":
			writeTestJSON(t, w, map[string]any{
				"clips": map[string]any{
					clipA: map[string]string{
						"id":        clipA,
						"title":     "Song A",
						"audio_url": "https://cdn.example.test/a.mp3",
					},
					clipB: map[string]string{
						"id":        clipB,
						"title":     "Song B",
						"audio_url": "https://cdn.example.test/b.mp3",
					},
				},
			})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		CacheDir:             filepath.Join(dir, "tmp"),
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	if _, err := db.CreateAccount(ctx, domain.Account{
		Email:        "multi@example.test",
		ProtocolMode: "bearer",
		FlowBearer:   "flow-bearer",
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	flow := NewFlowMusicClient(cfg)
	accounts := NewAccountService(cfg, db, flow)
	cache := storage.NewCache(cfg, db, NewHTTPClient(cfg, ""))
	generation := NewGenerationService(cfg, db, accounts, flow, cache)
	out, err := generation.Generate(ctx, "prompt", "flowmusic-producer-standard")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(out.Clips) != 2 {
		t.Fatalf("clip count = %d, want 2: %+v", len(out.Clips), out.Clips)
	}
	got := map[string]string{}
	for _, clip := range out.Clips {
		got[clip.ID] = clip.Audio.URL
	}
	if got[clipA] != "https://cdn.example.test/a.mp3" || got[clipB] != "https://cdn.example.test/b.mp3" {
		t.Fatalf("generated clips not preserved: %+v", out.Clips)
	}
}

func TestGenerateFailsWhenFlowMusicReturnsNoAudioClips(t *testing.T) {
	ctx := context.Background()
	clipID := "11111111-1111-1111-1111-111111111111"
	flowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__api/conversation":
			writeTestJSON(t, w, map[string]string{"job_id": "job-1"})
		case "/__api/messages/job-1/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"clip_ids":["` + clipID + `"]}` + "\n\n"))
		case "/__api/clips":
			writeTestJSON(t, w, map[string]any{
				"clips": map[string]any{
					clipID: map[string]string{
						"id":    clipID,
						"title": "No Audio",
					},
				},
			})
		default:
			t.Fatalf("unexpected FlowMusic path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(flowServer.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		CacheDir:             filepath.Join(dir, "tmp"),
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		FlowMusicBaseURL:     flowServer.URL,
		UpstreamTimeout:      time.Second,
		GenerationTimeout:    time.Second,
		TokenRefreshLead:     time.Minute,
		TokenRefreshInterval: time.Hour,
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
	}
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
	accountID, err := db.CreateAccount(ctx, domain.Account{
		Email:      "generate@example.test",
		FlowBearer: "flow-bearer",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	flow := NewFlowMusicClient(cfg)
	accounts := NewAccountService(cfg, db, flow)
	cache := storage.NewCache(cfg, db, NewHTTPClient(cfg, ""))
	generation := NewGenerationService(cfg, db, accounts, flow, cache)
	_, err = generation.Generate(ctx, "prompt", "flowmusic-producer-standard")
	if err == nil {
		t.Fatalf("Generate() error = nil, want no audio error")
	}
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.ErrorCount != 1 || account.ConsecutiveErrorCount != 1 {
		t.Fatalf("generation failure should increment account errors: %+v", account)
	}
}

func TestShouldRefreshAccountHonorsExpiryAndInterval(t *testing.T) {
	now := time.Date(2026, 5, 30, 4, 0, 0, 0, time.UTC)
	future := now.Add(30 * time.Minute)
	soon := now.Add(2 * time.Minute)
	lastRefresh := now.Add(-30 * time.Minute)

	if shouldRefreshAccount(domain.Account{}, 10*time.Minute, now) != true {
		t.Fatalf("missing bearer should refresh")
	}
	if shouldRefreshAccount(domain.Account{FlowBearer: "bearer", ATExpires: &future}, 10*time.Minute, now) {
		t.Fatalf("future bearer should not refresh before lead")
	}
	if !shouldRefreshAccount(domain.Account{FlowBearer: "bearer", ATExpires: &soon}, 10*time.Minute, now) {
		t.Fatalf("near-expiry bearer should refresh")
	}
	if shouldRefreshAccount(domain.Account{FlowBearer: "bearer", LastRefreshAt: &lastRefresh, RefreshIntervalMins: 60}, 10*time.Minute, now) {
		t.Fatalf("unknown-expiry bearer should respect account interval")
	}
}

func TestRecordFailureDisablesAccountAtConfiguredThreshold(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
		TokenRefreshInterval: time.Hour,
		TokenRefreshLead:     time.Minute,
		GenerationTimeout:    time.Second,
	}
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
	if err := db.UpdateAdminConfig(ctx, "admin", "test-api-key", false, 2, false, 0, 0); err != nil {
		t.Fatalf("UpdateAdminConfig() error = %v", err)
	}
	id, err := db.CreateAccount(ctx, domain.Account{
		Email:      "ban-threshold@example.test",
		FlowBearer: "flow-bearer",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	generation := &GenerationService{db: db}
	generation.recordFailure(ctx, id)
	account, err := db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if !account.IsActive || account.ConsecutiveErrorCount != 1 {
		t.Fatalf("account should remain active after first failure: %+v", account)
	}

	generation.recordFailure(ctx, id)
	account, err = db.GetAccount(ctx, id)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.IsActive || account.ConsecutiveErrorCount != 2 {
		t.Fatalf("account should be disabled at threshold: %+v", account)
	}
}

func TestFailLogPersistsCanceledContextWithBackgroundWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{
		DataDir:              dir,
		DatabaseDriver:       "sqlite",
		DatabaseURL:          filepath.Join(dir, "flowmusic2api.db"),
		DefaultAdminUser:     "admin",
		DefaultAdminPassword: "admin",
		DefaultAPIKey:        "test-api-key",
		TokenRefreshInterval: time.Hour,
		TokenRefreshLead:     time.Minute,
		GenerationTimeout:    time.Second,
	}
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
	accountID, err := db.CreateAccount(ctx, domain.Account{Email: "fail-log@example.test", FlowBearer: "flow-bearer"})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	logID, err := db.CreateRequestLog(ctx, domain.RequestLog{Operation: "music.generate", StatusCode: 102, StatusText: "streaming", Progress: 30})
	if err != nil {
		t.Fatalf("CreateRequestLog() error = %v", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	generation := &GenerationService{db: db}
	generation.failLog(canceledCtx, logID, accountID, []byte(`{"prompt":"test"}`), time.Now(), context.Canceled)
	log, err := db.GetLogDetail(ctx, logID)
	if err != nil {
		t.Fatalf("GetLogDetail() error = %v", err)
	}
	if log.StatusCode != 499 || log.StatusText != "canceled" || log.Progress != 100 || !strings.Contains(log.ResponseBody, "context canceled") {
		t.Fatalf("canceled failLog was not persisted correctly: %+v", log)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("json encode error = %v", err)
	}
}
