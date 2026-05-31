package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
)

func TestLiveFlowMusicGenerationContract(t *testing.T) {
	if os.Getenv("FLOWMUSIC_LIVE_TEST") != "1" {
		t.Skip("set FLOWMUSIC_LIVE_TEST=1 and a live credential env var to run")
	}

	cfg := config.Load()
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = 90 * time.Second
	}
	if cfg.GenerationTimeout <= 0 {
		cfg.GenerationTimeout = 10 * time.Minute
	}
	client := NewFlowMusicClient(cfg)
	account := domain.Account{
		ProtocolMode:         strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_PROTOCOL_MODE")),
		RefreshToken:         strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_REFRESH_TOKEN")),
		ST:                   strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_REFRESH_TOKEN")),
		ProviderToken:        strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_PROVIDER_TOKEN")),
		ProviderRefreshToken: strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_PROVIDER_REFRESH_TOKEN")),
		FlowBearer:           strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_FLOW_BEARER")),
		AT:                   strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_FLOW_BEARER")),
		Cookies:              strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_COOKIES")),
		ProxyURL:             strings.TrimSpace(os.Getenv("FLOWMUSIC_PROXY_URL")),
		AutoRefreshEnabled:   true,
		RefreshIntervalMins:  60,
		ImageEnabled:         true,
		VideoEnabled:         true,
		UpscaleEnabled:       true,
		ImageConcurrency:     -1,
		VideoConcurrency:     -1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.GenerationTimeout)
	defer cancel()
	account = liveAccountWithBearer(t, ctx, client, account)

	prompt := strings.TrimSpace(os.Getenv("FLOWMUSIC_LIVE_PROMPT"))
	if prompt == "" {
		prompt = "Generate a short instrumental city-pop music clip for API contract verification."
	}
	jobID, err := client.StartConversation(ctx, account, prompt, "lyria")
	if err != nil {
		t.Fatalf("StartConversation() error = %v", err)
	}
	stream, err := client.StreamMessages(ctx, account, jobID)
	if err != nil {
		t.Fatalf("StreamMessages() error = %v", err)
	}
	clipIDs := append([]string{}, stream.ClipIDs...)
	if len(clipIDs) == 0 {
		if len(stream.OperationIDs) == 0 {
			t.Fatalf("live FlowMusic contract returned no operation ids; job_id=%s events=%d", jobID, len(stream.RawEvents))
		}
		ids := append([]string{}, stream.OperationIDs...)
		clipIDs, err = client.PollClips(ctx, account, ids, time.Now().Add(cfg.GenerationTimeout))
		if err != nil {
			t.Fatalf("PollClips() error = %v", err)
		}
	}
	if len(clipIDs) == 0 {
		t.Fatalf("live FlowMusic contract returned no clip ids; job_id=%s events=%d operations=%d", jobID, len(stream.RawEvents), len(stream.OperationIDs))
	}
	clips, err := client.GetClips(ctx, account, clipIDs)
	if err != nil {
		t.Fatalf("GetClips() error = %v", err)
	}
	if len(clips) == 0 {
		t.Fatalf("GetClips() returned no clips for %d clip ids", len(clipIDs))
	}
	for _, clip := range clips {
		if clip.AudioURL != "" || clip.WavURL != "" {
			return
		}
	}
	t.Fatalf("live FlowMusic clips did not include audio or wav URLs: clip_count=%d", len(clips))
}

func liveAccountWithBearer(t *testing.T, ctx context.Context, client *FlowMusicClient, account domain.Account) domain.Account {
	t.Helper()
	if strings.TrimSpace(account.FlowBearer) != "" {
		return account
	}
	if strings.TrimSpace(account.Cookies) != "" {
		account.ProtocolMode = "protocol"
		updated, err := client.RefreshFromCookies(ctx, account)
		if err != nil {
			t.Fatalf("RefreshFromCookies() error = %v", err)
		}
		return updated
	}
	if strings.TrimSpace(account.RefreshToken) != "" {
		if strings.TrimSpace(client.cfg.SupabaseAnonKey) == "" {
			t.Skip("set FLOWMUSIC_SUPABASE_ANON_KEY to run live refresh_token contract test")
		}
		account.ProtocolMode = "refresh_token"
		updated, err := client.RefreshSupabase(ctx, account)
		if err != nil {
			t.Fatalf("RefreshSupabase() error = %v", err)
		}
		return updated
	}
	if strings.TrimSpace(account.ProviderToken) != "" {
		flowBearer, err := client.SaveGoogle(ctx, account)
		if err != nil {
			t.Fatalf("SaveGoogle() error = %v", err)
		}
		if strings.TrimSpace(flowBearer) == "" {
			t.Fatalf("SaveGoogle() returned empty FlowMusic bearer")
		}
		account.FlowBearer = flowBearer
		account.AT = flowBearer
		account.AccessToken = flowBearer
		return account
	}
	if strings.TrimSpace(account.ProviderRefreshToken) != "" {
		if strings.TrimSpace(client.cfg.GoogleOAuthClientID) == "" {
			t.Skip("set FLOWMUSIC_GOOGLE_OAUTH_CLIENT_ID to run live provider_refresh_token contract test")
		}
		updated, err := client.RefreshGoogleProviderToken(ctx, account)
		if err != nil {
			t.Fatalf("RefreshGoogleProviderToken() error = %v", err)
		}
		flowBearer, err := client.SaveGoogle(ctx, updated)
		if err != nil {
			t.Fatalf("SaveGoogle() after provider refresh error = %v", err)
		}
		if strings.TrimSpace(flowBearer) == "" {
			t.Fatalf("SaveGoogle() returned empty FlowMusic bearer")
		}
		updated.FlowBearer = flowBearer
		updated.AT = flowBearer
		updated.AccessToken = flowBearer
		return updated
	}
	t.Skip("set FLOWMUSIC_LIVE_FLOW_BEARER, FLOWMUSIC_LIVE_COOKIES, FLOWMUSIC_LIVE_REFRESH_TOKEN, FLOWMUSIC_LIVE_PROVIDER_TOKEN, or FLOWMUSIC_LIVE_PROVIDER_REFRESH_TOKEN")
	return account
}
